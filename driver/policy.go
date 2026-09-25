package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// PolicyPath is the file an operator writes a runner's settings in. The documentation
// names it for the floor and gives the reason every other setting shares with it:
// require_userns_remap "is a line in /etc/agentiik/runner.toml and not a command line
// flag, so the decision survives in something reviewable rather than in somebody's shell
// history".
const PolicyPath = "/etc/agentiik/runner.toml"

// UsernsFloor says whether a daemon without user namespace remapping is refused.
//
// It is an enumeration and not a bool, which is the whole point of the type.
// require_userns_remap defaults to true in the documentation and a Go bool defaults to
// false, so a Policy nobody filled in would read as a lifted floor, and a field whose
// zero value silently lifts a security floor is the one shape this must not have. The
// zero value here is the floor in place.
type UsernsFloor int

const (
	// RemapRequired refuses a daemon that does not remap user namespaces. It is the
	// zero value, so a Policy{} is the hardened tier and not the laptop.
	RemapRequired UsernsFloor = iota

	// RemapLifted takes work on a daemon without the remapping. An operator decided
	// that "this machine does not need the floor, which is a reasonable thing to
	// decide about a laptop and a serious thing to decide about a fleet".
	RemapLifted
)

// String writes the floor as the file's own value, so that the setting and the field
// read alike: require_userns_remap = true is RemapRequired and false is RemapLifted.
func (u UsernsFloor) String() string {
	if u == RemapLifted {
		return "false"
	}
	return "true"
}

// Lifted says whether the refusal has been lifted. It reads at the point of use as the
// question being asked there, which is not the same question the field name asks.
func (u UsernsFloor) Lifted() bool { return u == RemapLifted }

// SeccompFloor says whether a daemon that filters no system call is refused.
//
// It is an enumeration for the reason UsernsFloor is one: its zero value is the floor in
// place, so a Policy nobody filled in refuses a daemon that would run every container with
// the whole system call table open. The SecurityOpt row promises "the default seccomp
// profile", and a daemon built without seccomp, or started with
// --seccomp-profile=unconfined, applies none.
//
// Unlike the userns floor, no line of runner.toml lifts it. Docker's packages build
// seccomp in and every mainstream kernel offers it, so a daemon without it is one somebody
// switched off, which makes it a daemon to fix rather than one to configure around, and a
// runner holds the floor whatever its file says. The callers that are not runners lift
// it, agk run --local first among them, and the driver then says what the machine gives
// up instead of refusing it.
type SeccompFloor int

const (
	// SeccompRequired refuses a daemon that applies no seccomp profile. It is the zero
	// value, so a Policy{} is a runner's and not a laptop's.
	SeccompRequired SeccompFloor = iota

	// SeccompLifted takes work on such a daemon, and the driver says so once.
	SeccompLifted
)

// Lifted says whether the refusal has been lifted.
func (s SeccompFloor) Lifted() bool { return s == SeccompLifted }

// SecretsFloor says whether a secrets directory that is not a tmpfs mounted
// noexec,nosuid,nodev is refused.
//
// A secret value is written on this side and bound into the container, because a tmpfs the
// daemon creates at container start is empty and cannot be pre-populated. A bind keeps the
// flags of the mount its source sits on, so the flags the Tmpfs row promises for a secret
// mount point are the flags of the host directory, and a runner holds that directory to
// them rather than trusting whatever /dev/shm happens to be mounted with: on most
// distributions it is nosuid,nodev and not noexec.
//
// It is an enumeration for the reason the other two floors are one, and like the seccomp
// floor no line of runner.toml lifts it. The callers that are not runners lift it, agk run
// --local first among them, since a laptop has no such tmpfs and macOS has no tmpfs at
// all; the driver then says where a value lands instead.
type SecretsFloor int

const (
	// SecretsTmpfsRequired refuses a secrets directory that is not a tmpfs mounted
	// noexec,nosuid,nodev, and an empty one. It is the zero value, so a Policy{} is a
	// runner's.
	SecretsTmpfsRequired SecretsFloor = iota

	// SecretsTmpfsLifted takes whatever directory the policy names, and none.
	SecretsTmpfsLifted
)

// Lifted says whether the refusal has been lifted.
func (s SecretsFloor) Lifted() bool { return s == SecretsTmpfsLifted }

// DigestFloor says whether a task's image is held to what a server runs: an image named by
// digest, and for a step that is not a script step, one carrying /agk/brick.yaml.
//
// A server runs only name@sha256, which agk push records in place of every tag, because "a
// tag is a mutable pointer, and a commit must determine what ran": a tag resolves to
// whatever this host last pulled under it. And a server holds a brick to its manifest,
// because the manifest is what declares the account the container runs as: an image with
// none would run as its own, root included, and before publication checks manifests the
// runner is the last gate. agk run --local is held to neither, since an image built on the
// machine and never pushed has only a tag, and it validates each brick's manifest itself
// before anything runs.
//
// It is an enumeration for the reason the other floors are one, and like the seccomp floor
// no line of runner.toml lifts it: a runner is a server, and the callers that are not
// runners lift it.
type DigestFloor int

const (
	// DigestRequired refuses a task whose image is not name@sha256, and a non-script
	// step whose image carries no manifest. It is the zero value, so a Policy{} is a
	// runner's.
	DigestRequired DigestFloor = iota

	// DigestLifted runs a tag as the daemon resolves it, and an image with no manifest
	// as the account the image declares.
	DigestLifted
)

// Lifted says whether the refusal has been lifted.
func (f DigestFloor) Lifted() bool { return f == DigestLifted }

// Ulimit is one soft and hard pair, as the daemon's Ulimits carry them.
type Ulimit struct {
	Soft int64
	Hard int64
}

// Ulimits is the two the settings table names, "nofile and nproc set by the runner
// policy", and no others: a policy that could set any ulimit would be a second resources
// block nothing in the workflow language can see.
type Ulimits struct {
	NoFile Ulimit
	NProc  Ulimit
}

// Policy is what the machine says about every container it runs, as opposed to what a
// workflow says about one step. The settings table of the documentation is written
// partly in values a step asks for and partly in values the runner sets, and this is the
// second half.
//
// Its zero value is every floor in place and nothing else configured, which is what lets
// a caller with no /etc/agentiik/runner.toml pass Policy{} and still be refused on a
// daemon with no remapping or no seccomp. DefaultPolicy fills in the rest.
type Policy struct {
	// RequireUsernsRemap is the floor. It carries the file's own spelling so that
	// the setting and the field read alike.
	RequireUsernsRemap UsernsFloor

	// RequireSeccomp is the seccomp floor, which no key of the file sets.
	RequireSeccomp SeccompFloor

	// RequireSecretsTmpfs holds SecretsDir to a tmpfs mounted noexec,nosuid,nodev,
	// and no key of the file sets it either.
	RequireSecretsTmpfs SecretsFloor

	// RequireDigest holds a task's image to name@sha256, and a brick's image to its
	// manifest, and no key of the file sets it either.
	RequireDigest DigestFloor

	// StopGrace is the t of the daemon's own stop, the wait between SIGTERM and
	// SIGKILL. The escalation belongs to the daemon rather than to a timer here, so
	// that it survives this process dying between the two signals.
	StopGrace time.Duration

	// PidsLimit is the default for PidsLimit where resources.pids says nothing, and
	// the ceiling where it says more. The documentation's default is 256.
	PidsLimit int64

	// Ulimits are nofile and nproc, applied to every container. A file that sets
	// pids_limit and no nproc moves nproc with it, for the reason DefaultPolicy gives
	// the two one number.
	Ulimits Ulimits

	// MemoryCap and CPUCap are the runner's half of "capped by the runner policy and
	// the namespace quota". Zero is no cap here, which is not the same as no cap at
	// all: the quota is the other half and it is not this package's.
	MemoryCap int64
	CPUCap    float64

	// TmpSize is the size of the /tmp tmpfs. The settings table says the writable
	// paths are "mounted as sized tmpfs", so there is no unsized case to configure.
	TmpSize int64

	// Source is the file this policy was read from, and empty when nobody read one.
	//
	// It exists so that the driver can say where a setting came from without inventing
	// a provenance. LoadPolicy fills it; DefaultPolicy leaves it empty, which is the
	// case agk run --local is in, and the one sentence that would otherwise name
	// PolicyPath says "this runner does not require it" instead of claiming a file
	// said so. A message that names a file nobody opened sends its reader looking for
	// a configuration that is not there.
	Source string

	// SecretsDir is the host directory secret values are written under before they
	// are bound at /agk/secrets/<name>. It is a tmpfs where the platform has one,
	// /dev/shm on Linux, because a value that touched a disk is a value somebody has
	// to erase. Empty means the task's working directory, which is the laptop case
	// and which the driver says out loud.
	//
	// A runner holds it to more than a tmpfs, RequireSecretsTmpfs, and /dev/shm is
	// mounted without noexec on most distributions, so an installation mounts one of
	// its own and names it with secrets_dir.
	SecretsDir string

	// LogMaxBytes and LogMaxLines cap the collected standard error. A log is a
	// diagnostic and not a payload: the cap keeps one runaway task from filling the
	// store, and the truncation marker says what was dropped. Zero is no cap.
	LogMaxBytes int64
	LogMaxLines int

	// Seccomp, AppArmor and SELinuxLabel are the profiles of the SecurityOpt row,
	// "the default seccomp profile, and an AppArmor profile or SELinux label
	// depending on the host". Each is empty by default, which leaves the daemon its
	// own defaults; naming one here replaces them.
	//
	// Seccomp is the profile itself, its JSON, and not the path of a file holding it,
	// because that is what the Engine API takes. seccomp=<path> is a convenience of the
	// docker command, which reads the file and sends what is in it; a daemon handed the
	// path decodes the path as a profile and refuses to start any container carrying
	// it. LoadPolicy reads the file seccomp_profile names and keeps what it holds.
	Seccomp      string
	AppArmor     string
	SELinuxLabel string

	// AllowCapAdd is the explicit permission the CapDrop row speaks of: "CapAdd is
	// refused unless the runner policy allows it explicitly". Nothing a workflow or a
	// manifest writes can ask for a capability today, so this is a door that stays
	// shut rather than one that is guarded.
	AllowCapAdd []string

	// Shell is what a script step runs under where the step names none. Empty is
	// DefaultShell.
	Shell []string

	// Helper is the static agk binary on this host, mounted read-only at BinPath for
	// a script step: "a static helper for scripts that want to be precise rather than
	// lucky, with agk items, agk emit and agk attach. It is a convenience, never a
	// requirement". It is a runner setting because which binary is on this host is a
	// fact about the host and not about a workflow, and empty is a runner that has
	// none to offer, which binds nothing.
	Helper string

	// HooksSkipped says the file carries a [hooks] table, which this version reads and
	// runs none of: the hooks of #runner-side-hooks arrive in v0.9.0. It is kept so
	// that the driver can say so once, because an operator who wrote a pre_task that
	// attaches a licence, or a post_task that wipes a scratch disk, is relying on it
	// having run.
	HooksSkipped bool
}

// DefaultPolicy is the runner as it is installed: both floors in place, the documented
// pids default, and modest ceilings for the settings the documentation leaves to the
// runner.
func DefaultPolicy() Policy {
	return Policy{
		RequireUsernsRemap:  RemapRequired,
		RequireSeccomp:      SeccompRequired,
		RequireSecretsTmpfs: SecretsTmpfsRequired,
		RequireDigest:       DigestRequired,

		// The daemon's own default for POST /containers/{id}/stop. Taking a
		// different number here would make the driver's grace and the grace of a
		// docker stop typed by hand two different things on one host.
		StopGrace: 10 * time.Second,

		// "PidsLimit: from resources.pids, default 256."
		PidsLimit: 256,

		Ulimits: Ulimits{
			// A brick reads a handful of files and writes a handful more. The
			// soft limit is what a process sees by default and the hard limit
			// is what it may raise itself to, which is room for a step that
			// opens many files on purpose without room for one that leaks them.
			NoFile: Ulimit{Soft: 1024, Hard: 4096},
			// nproc matches the pid ceiling. A process the pids cgroup will not
			// let exist should not be one the ulimit would have allowed, so the
			// two say the same number rather than two numbers whose difference
			// nobody can explain.
			NProc: Ulimit{Soft: 256, Hard: 256},
		},

		// Enough scratch for a step that stages a file before emitting it, small
		// enough that a runaway write fails the task rather than the host. tmpfs
		// pages are host memory, which is why this is sized and not left open.
		TmpSize: 64 << 20,

		SecretsDir: defaultSecretsDir(),

		// A log is read by a person. Four megabytes is the same order as the
		// largest envelope a port may carry, so the largest thing one task hands
		// back is the same size whichever way it hands it back.
		LogMaxBytes: 4 << 20,
		LogMaxLines: 50000,
	}
}

// defaultSecretsDir answers where a secret value may be written without touching a disk.
//
// /dev/shm is a tmpfs on every Linux distribution that matters, which is what agk run
// --local writes on there. It is not what a runner writes on: most distributions mount it
// without noexec, so a runner is refused it and names a tmpfs of its own with secrets_dir.
// Elsewhere, and macOS is the case
// that matters because agk run --local has to work there, there is no equivalent path,
// so this is empty and the value lands in the task's working directory instead. That is
// a real difference and the driver announces it rather than pretending otherwise.
func defaultSecretsDir() string {
	if runtime.GOOS == "linux" {
		return "/dev/shm"
	}
	return ""
}

// LoadPolicy reads the runner's configuration file.
//
// Every setting an operator owns about the machine is a key of it, and the reading is
// strict: a key this runner does not read is refused naming its line, and so is a value of
// the wrong type or outside what its setting allows. The file belongs to one host and is
// read at every start, and the permissive reading would buy nothing there but a
// misspelled pids_limt that leaves the default in force without a word. A key is also
// held to its exact spelling, because the decoder matches a key to a setting without
// regard to case and Require_Userns_Remap would otherwise lift the floor under a name the
// documentation never wrote.
//
// [hooks] is read as strictly as the rest and run not at all. The hooks of
// #runner-side-hooks arrive in v0.9.0, and a file already written for them is not refused
// by the version before them; HooksSkipped says it was there, so that the driver can say
// none of it runs. It is still held to the three keys the documentation writes, because
// TOML puts every key after a table's header inside that table: a seccomp_profile written
// below [hooks] is a key of [hooks], and a table taking any key would drop it without a
// word.
//
// A key the file leaves out keeps its value from DefaultPolicy, and require_userns_remap
// in particular keeps the floor: an absent line is not a decision.
//
// A missing file is returned as the error it is, wrapping fs.ErrNotExist, so that a
// caller can tell "there is no file" from "the file says something I cannot read" and
// fall back to DefaultPolicy for the first only. Reading a floor out of a file that is
// not there would be the one mistake this whole shape exists to prevent. Every refusal
// comes back with DefaultPolicy beside it, so a caller that drops the error still holds
// the floor.
func LoadPolicy(path string) (Policy, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return DefaultPolicy(), err
	}
	p, err := readPolicy(path, text)
	if err != nil {
		return DefaultPolicy(), err
	}
	return p, nil
}

// readPolicy reads one file's text in three passes, each answering one question.
//
// The first reads the document and nothing else, so that a line that is not TOML is
// refused in TOML's own words and the other two passes can take a document as given. The
// second is strict and knows where things are: a key no setting has, and a value of the
// wrong type, each come back with the line they are on. The third holds every key to its
// spelling, which the second does not do.
func readPolicy(path string, text []byte) (Policy, error) {
	var raw map[string]any
	if err := toml.Unmarshal(text, &raw); err != nil {
		return Policy{}, notTOML(path, err)
	}

	var f runnerFile
	d := toml.NewDecoder(bytes.NewReader(text))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return Policy{}, misshapen(path, err)
	}

	if err := exactKeys(path, raw, ""); err != nil {
		return Policy{}, err
	}

	p := DefaultPolicy()
	// What was read, so that a message about a setting can name where the setting is
	// written rather than where it would be written if anybody had read anything.
	p.Source = path
	if err := f.apply(path, &p); err != nil {
		return Policy{}, err
	}
	// Read off the document rather than the struct: an empty [hooks] decodes into
	// nothing, and it is still a table somebody wrote.
	_, p.HooksSkipped = raw["hooks"]
	return p, nil
}

// runnerFile is runner.toml as it is written, one field per key.
//
// Every field is a pointer so that a key the file leaves out keeps its default rather than
// taking the zero value of its type. An absent require_userns_remap would otherwise read
// as false, which is the floor lifted by a line nobody wrote.
type runnerFile struct {
	RequireUsernsRemap *bool        `toml:"require_userns_remap"`
	SecretsDir         *string      `toml:"secrets_dir"`
	StopGrace          *string      `toml:"stop_grace"`
	Helper             *string      `toml:"helper"`
	SeccompProfile     *string      `toml:"seccomp_profile"`
	AppArmorProfile    *string      `toml:"apparmor_profile"`
	SELinuxLabel       *string      `toml:"selinux_label"`
	AllowCapAdd        *[]string    `toml:"allow_cap_add"`
	PidsLimit          *int64       `toml:"pids_limit"`
	MemoryCap          *string      `toml:"memory_cap"`
	CPUCap             *string      `toml:"cpu_cap"`
	TmpSize            *string      `toml:"tmp_size"`
	LogMaxBytes        *int64       `toml:"log_max_bytes"`
	LogMaxLines        *int64       `toml:"log_max_lines"`
	Ulimits            *fileUlimits `toml:"ulimits"`

	// Hooks is declared key by key so that the strict pass holds [hooks] to what the
	// documentation writes in it. What is in it is not run.
	Hooks *fileHooks `toml:"hooks"`
}

type fileHooks struct {
	Timeout  *string   `toml:"timeout"`
	PreTask  *[]string `toml:"pre_task"`
	PostTask *[]string `toml:"post_task"`
}

type fileUlimits struct {
	NoFile *fileUlimit `toml:"nofile"`
	NProc  *fileUlimit `toml:"nproc"`
}

type fileUlimit struct {
	Soft *int64 `toml:"soft"`
	Hard *int64 `toml:"hard"`
}

// fileKeys is every key of runner.toml, dotted where it sits in a table, with how its
// value is written.
//
// The description is what a refusal says. A value of the wrong type is answered with the
// type it should have had, in the file's own terms, rather than with the Go type the
// decoder failed to put it in, which is a fact about this package that the person editing
// the file has no use for.
var fileKeys = map[string]string{
	"require_userns_remap": "true or false",
	"secrets_dir":          `an absolute path in quotation marks, such as "/run/agentiik/secrets"`,
	"stop_grace":           `a whole number of seconds written as a duration in quotation marks, such as "10s"`,
	"helper":               `an absolute path in quotation marks, such as "/usr/local/lib/agentiik/agk-helper"`,
	"seccomp_profile":      `the absolute path of a JSON seccomp profile in quotation marks, such as "/etc/agentiik/seccomp.json"`,
	"apparmor_profile":     `the name of an AppArmor profile loaded on the host, in quotation marks, such as "agentiik-brick"`,
	"selinux_label":        `one SELinux label option in quotation marks, user, role, type, level or filetype, a colon and a value, such as "level:s0:c100,c200"`,
	"allow_cap_add":        `a list of capability names in capitals and without the CAP_ prefix, such as ["NET_BIND_SERVICE"]`,
	"pids_limit":           "a whole number above zero, such as 256",
	"memory_cap":           `a whole number of at least 6Mi with a binary suffix, Ki, Mi, Gi or Ti, in quotation marks, such as "8Gi"`,
	"cpu_cap":              `a number of cores of at least 0.01 in quotation marks, such as "4" or "0.5"`,
	"tmp_size":             `a whole number above zero with a binary suffix, Ki, Mi, Gi or Ti, in quotation marks, such as "64Mi"`,
	"log_max_bytes":        "a whole number of bytes above zero, such as 4194304",
	"log_max_lines":        "a whole number above zero, such as 50000",
	"ulimits":              "a table holding nofile and nproc",
	"ulimits.nofile":       "a table of soft and hard, such as { soft = 1024, hard = 4096 }",
	"ulimits.nofile.soft":  "a whole number above zero",
	"ulimits.nofile.hard":  "a whole number no lower than soft and no higher than 1048576",
	"ulimits.nproc":        "a table of soft and hard, such as { soft = 256, hard = 256 }",
	"ulimits.nproc.soft":   "a whole number above zero",
	"ulimits.nproc.hard":   "a whole number above zero, and no lower than soft",
	"hooks":                "a table of timeout, pre_task and post_task, which this version reads and runs none of",
	"hooks.timeout":        `a duration in quotation marks, such as "30s"`,
	"hooks.pre_task":       `a list of commands in quotation marks, such as ["nvidia-smi -L"]`,
	"hooks.post_task":      `a list of commands in quotation marks, such as ["nvidia-smi -L"]`,
}

// keysInOrder is what a refusal of an unknown key lists, in the order the reference
// table on the page gives them.
const keysInOrder = "require_userns_remap, secrets_dir, stop_grace, helper, seccomp_profile, apparmor_profile, selinux_label, allow_cap_add, pids_limit, memory_cap, cpu_cap, tmp_size, log_max_bytes, log_max_lines, [ulimits] with nofile and nproc, each a table of soft and hard, and [hooks] with timeout, pre_task and post_task"

// notTOML refuses a file the first pass could not read as a document at all.
func notTOML(path string, err error) error {
	var de *toml.DecodeError
	if errors.As(err, &de) {
		line, _ := de.Position()
		return fmt.Errorf("%s line %d is not TOML: %s", path, line, strings.TrimPrefix(de.Error(), "toml: "))
	}
	return fmt.Errorf("%s is not TOML: %w", path, err)
}

// misshapen refuses what the strict pass found: keys no setting has, or a value of the
// wrong type, each with its line.
func misshapen(path string, err error) error {
	var missing *toml.StrictMissingError
	if errors.As(err, &missing) {
		var unknown []string
		for _, e := range missing.Errors {
			line, _ := e.Position()
			unknown = append(unknown, fmt.Sprintf("%s on line %d", strings.Join(e.Key(), "."), line))
		}
		verb := "is not a setting"
		if len(unknown) > 1 {
			verb = "are not settings"
		}
		return fmt.Errorf("%s: %s %s this runner reads.%s A key it does not read is refused rather than ignored, because a misspelled setting would otherwise leave its default in force without a word. The settings are %s", path, andList(unknown), verb, belowAHeader(missing), keysInOrder)
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		line, _ := de.Position()
		key := strings.Join(de.Key(), ".")
		if written, ok := fileKeys[key]; ok {
			return fmt.Errorf("%s line %d: %s is %s", path, line, key, written)
		}
		return fmt.Errorf("%s line %d: %s", path, line, strings.TrimPrefix(de.Error(), "toml: "))
	}
	return fmt.Errorf("%s: %w", path, err)
}

// belowAHeader explains the one refusal of an unknown key that is not a misspelling: a
// setting of the runner written after a table's header, which TOML reads as a key of that
// table. No setting shares its name with a key of a table, so a name that matches a
// setting once its table is taken off is one that was meant to be above the table.
func belowAHeader(missing *toml.StrictMissingError) string {
	var settings, tables []string
	for _, e := range missing.Errors {
		key := e.Key()
		if len(key) < 2 {
			continue
		}
		setting := key[len(key)-1]
		if _, ok := fileKeys[setting]; !ok {
			continue
		}
		settings = append(settings, setting)
		if table := "[" + strings.Join(key[:len(key)-1], ".") + "]"; !slices.Contains(tables, table) {
			tables = append(tables, table)
		}
	}
	if len(settings) == 0 {
		return ""
	}
	verb := "is a setting"
	if len(settings) > 1 {
		verb = "are settings"
	}
	return fmt.Sprintf(" %s %s of the runner written below the header of %s, and TOML puts every key after a table's header inside that table, so a setting of the runner goes above the first table of the file.", andList(settings), verb, andList(tables))
}

// exactKeys holds every key of the document to its spelling.
//
// The strict pass has already refused a key that matches no setting at all, so a key that
// fails here matches one in everything but case, and the refusal can name the spelling it
// was meant to have. Keys are visited in order so that the same file is always refused
// with the same sentence.
func exactKeys(path string, raw map[string]any, table string) error {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		dotted := key
		if table != "" {
			dotted = table + "." + key
		}
		if _, ok := fileKeys[dotted]; !ok {
			return fmt.Errorf("%s: %s is spelled %s. A key is read exactly as the documentation writes it, and one spelled otherwise is refused rather than guessed at", path, dotted, spelling(dotted))
		}
		if inner, ok := raw[key].(map[string]any); ok {
			if err := exactKeys(path, inner, dotted); err != nil {
				return err
			}
		}
	}
	return nil
}

// spelling is the setting a key matches in everything but case.
func spelling(dotted string) string {
	for key := range fileKeys {
		if strings.EqualFold(key, dotted) {
			return key
		}
	}
	return "otherwise"
}

// andList joins a list the way a sentence does.
func andList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// apply lays what the file says over the defaults, holding each value to what its setting
// allows. The type of every value was settled by the strict pass; what is checked here is
// what a type cannot say, such as a path being absolute or a limit being above zero.
func (f runnerFile) apply(path string, p *Policy) error {
	refuse := func(key string, value any) error {
		return fmt.Errorf("%s is %s in %s: it is %s", key, quoted(value), path, fileKeys[key])
	}

	if f.RequireUsernsRemap != nil {
		p.RequireUsernsRemap = RemapRequired
		if !*f.RequireUsernsRemap {
			p.RequireUsernsRemap = RemapLifted
		}
	}

	// A relative path would be read against wherever the runner happened to be started,
	// which is a fact about a shell and not about the host.
	for _, s := range []struct {
		key   string
		value *string
		into  *string
	}{
		{"secrets_dir", f.SecretsDir, &p.SecretsDir},
		{"helper", f.Helper, &p.Helper},
	} {
		if s.value == nil {
			continue
		}
		if !filepath.IsAbs(*s.value) {
			return refuse(s.key, *s.value)
		}
		*s.into = filepath.Clean(*s.value)
	}

	if f.StopGrace != nil {
		// The daemon counts the grace in whole seconds, and a fraction would be
		// rounded on the way, so the file is held to what is actually sent.
		grace, err := time.ParseDuration(*f.StopGrace)
		if err != nil || grace <= 0 || grace%time.Second != 0 {
			return refuse("stop_grace", *f.StopGrace)
		}
		p.StopGrace = grace
	}

	if f.SeccompProfile != nil {
		profile, err := seccompProfile(path, *f.SeccompProfile)
		if err != nil {
			return err
		}
		p.Seccomp = profile
	}

	if f.AppArmorProfile != nil {
		v := *f.AppArmorProfile
		if v == "" || strings.ContainsAny(v, " \t\n") {
			return refuse("apparmor_profile", v)
		}
		// The key names what a container is confined by, and it has no value for
		// nothing: the spelling that says so outright is refused. What a profile the host
		// loaded allows is written on the host, where the runner reads its name and not
		// its rules, and is answered for by whoever loaded it. A host without AppArmor is
		// said out loud when the daemon is opened; a host with it keeps it.
		if v == "unconfined" {
			return fmt.Errorf("apparmor_profile is %q in %s, which confines nothing: the key names the AppArmor profile every container is confined by, and has no value for none", v, path)
		}
		p.AppArmor = v
	}

	if f.SELinuxLabel != nil {
		v := *f.SELinuxLabel
		kind, value, ok := strings.Cut(v, ":")
		if !ok || value == "" || !slices.Contains([]string{"user", "role", "type", "level", "filetype"}, kind) {
			// disable is the one option this leaves out on purpose, for the reason
			// apparmor_profile refuses unconfined.
			return refuse("selinux_label", v)
		}
		// The same reason refuses the types the shipped policies leave unconfined, which
		// are how a container is given no confinement by name: spc_t is what the container
		// policy calls a super-privileged container. A type the host's own policy defines
		// is the host's to answer for, as an AppArmor profile is.
		if kind == "type" && slices.Contains(unconfinedTypes, value) {
			return fmt.Errorf("selinux_label is %q in %s, which confines nothing: %s is a type the SELinux policy leaves unconfined, and the key names the label every container is confined by, with no value for none", v, path, value)
		}
		p.SELinuxLabel = v
	}

	if f.AllowCapAdd != nil {
		allowed := make([]string, 0, len(*f.AllowCapAdd))
		for _, name := range *f.AllowCapAdd {
			if err := capability(path, name); err != nil {
				return err
			}
			allowed = append(allowed, name)
		}
		p.AllowCapAdd = allowed
	}

	if f.PidsLimit != nil {
		if *f.PidsLimit <= 0 {
			return refuse("pids_limit", *f.PidsLimit)
		}
		p.PidsLimit = *f.PidsLimit
		// nproc says the pid ceiling's number unless the file says otherwise, for the
		// reason DefaultPolicy gives: a process the pids cgroup will not let exist
		// should not be one the ulimit would have allowed.
		p.Ulimits.NProc = Ulimit{Soft: *f.PidsLimit, Hard: *f.PidsLimit}
	}

	// A cap is also what a step naming no resources is given, so one the daemon would
	// refuse fails every such step, on the platform's account, one task at a time. The
	// floors are the daemon's own: it refuses a memory limit under 6Mi at the create, and a
	// CPU quota under a hundredth of a core fails as the container starts. Whether the
	// host has as many cores as cpu_cap names is a fact about the daemon and not the file,
	// and New holds it there.
	for _, s := range []struct {
		key   string
		value *string
		into  *int64
		least int64
	}{
		{"memory_cap", f.MemoryCap, &p.MemoryCap, minMemoryCap},
		{"tmp_size", f.TmpSize, &p.TmpSize, 1},
	} {
		if s.value == nil {
			continue
		}
		n, err := binarySize(*s.value)
		if err != nil || !sizeGrammar.MatchString(*s.value) || n < s.least {
			return refuse(s.key, *s.value)
		}
		*s.into = n
	}

	if f.CPUCap != nil {
		cores, err := strconv.ParseFloat(*f.CPUCap, 64)
		// The grammar is resources.cpu's, and the ceiling is what NanoCpus can count.
		if err != nil || !coresGrammar.MatchString(*f.CPUCap) || cores < minCPUCap || cores > math.MaxInt64/1e9 {
			return refuse("cpu_cap", *f.CPUCap)
		}
		p.CPUCap = cores
	}

	if f.LogMaxBytes != nil {
		if *f.LogMaxBytes <= 0 {
			return refuse("log_max_bytes", *f.LogMaxBytes)
		}
		p.LogMaxBytes = *f.LogMaxBytes
	}
	if f.LogMaxLines != nil {
		if *f.LogMaxLines <= 0 || *f.LogMaxLines > math.MaxInt32 {
			return refuse("log_max_lines", *f.LogMaxLines)
		}
		p.LogMaxLines = int(*f.LogMaxLines)
	}

	if f.Ulimits != nil {
		for _, u := range []struct {
			key   string
			value *fileUlimit
			into  *Ulimit
		}{
			{"ulimits.nofile", f.Ulimits.NoFile, &p.Ulimits.NoFile},
			{"ulimits.nproc", f.Ulimits.NProc, &p.Ulimits.NProc},
		} {
			if u.value == nil {
				continue
			}
			// Both halves, always. A soft limit alone would sit under a hard limit the
			// file never mentioned, and the pair is what the daemon applies.
			if u.value.Soft == nil || u.value.Hard == nil {
				return fmt.Errorf("%s in %s writes %s, and it is %s: both halves, because the daemon applies them as one pair", u.key, path, halves(u.value), fileKeys[u.key])
			}
			soft, hard := *u.value.Soft, *u.value.Hard
			if soft <= 0 {
				return refuse(u.key+".soft", soft)
			}
			if hard < soft || (u.key == "ulimits.nofile" && hard > maxNoFile) {
				return refuse(u.key+".hard", hard)
			}
			*u.into = Ulimit{Soft: soft, Hard: hard}
		}
	}
	return nil
}

// The least of the caps and the most of nofile, each the daemon's or the kernel's figure
// rather than one chosen here.
const (
	// minMemoryCap is the daemon's least memory limit: "Minimum memory limit allowed is
	// 6MB", counted in mebibytes.
	minMemoryCap = 6 << 20

	// minCPUCap is the least the daemon takes for NanoCpus, "the range of CPUs is from
	// 0.01", and the kernel's least CPU quota, a millisecond in every tenth of a second.
	minCPUCap = 0.01

	// maxNoFile is the highest hard nofile the file takes. The kernel refuses a hard
	// limit above fs.nr_open, which the Engine API does not report, and the daemon hears
	// of it only as each container starts, so the ceiling is the kernel's default for
	// fs.nr_open: the one figure every host takes unless somebody lowered it.
	maxNoFile = 1 << 20
)

// quoted writes a refused value the way the file writes it: a string in quotation marks
// and a number without.
func quoted(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

// halves says which half of a ulimit the file wrote.
func halves(u *fileUlimit) string {
	switch {
	case u.Soft != nil:
		return "soft alone"
	case u.Hard != nil:
		return "hard alone"
	}
	return "neither soft nor hard"
}

// sizeGrammar and coresGrammar are resources.memory and resources.cpu as the brick and
// workflow schemas write them, so that a host's ceiling is spelled the way a step spells
// what it asks for, and one reader could compare the two without converting.
var (
	sizeGrammar  = regexp.MustCompile(`^[1-9][0-9]*(?:Ki|Mi|Gi|Ti)$`)
	coresGrammar = regexp.MustCompile(`^(?:[0-9]*[1-9][0-9]*(?:\.[0-9]+)?|[0-9]+\.[0-9]*[1-9][0-9]*)$`)
)

// seccompProfile reads the profile seccomp_profile names, and keeps it as the Engine API
// takes it.
//
// It is read here, once, rather than at every task, so that a profile that is missing, is
// not one, or names an action seccomp does not have refuses the start, and not every task
// after it: the daemon takes any action at the create, and the runtime refuses one it does
// not know when the container starts. What else a profile says is the daemon's to read.
// What is kept is the JSON compacted, as the docker command sends it: the whitespace of a
// hand-formatted profile is most of its bytes, and it would travel in the body of every
// create.
//
// A profile that lets every call through is refused as well. The key names what filters a
// container, and a profile filtering nothing would lift the seccomp floor under the name
// of meeting it, which is the one thing no line of this file does.
func seccompProfile(path, file string) (string, error) {
	if !filepath.IsAbs(file) {
		return "", fmt.Errorf("seccomp_profile is %q in %s: it is %s", file, path, fileKeys["seccomp_profile"])
	}
	body, err := os.ReadFile(file)
	if err != nil {
		// Said and not wrapped. LoadPolicy answers fs.ErrNotExist for a runner.toml that
		// is not there, which its caller takes as no file and answers with DefaultPolicy,
		// and a profile nobody deployed would otherwise read as that: every other setting
		// of the file dropped, and no refusal.
		reason := err
		var pe *fs.PathError
		if errors.As(err, &pe) {
			reason = pe.Err
		}
		return "", fmt.Errorf("seccomp_profile in %s names %s, which could not be read: %v", path, file, reason)
	}
	// A profile is a JSON object with a defaultAction, as the daemon's own is.
	var rules seccompRules
	var compact bytes.Buffer
	if json.Unmarshal(body, &rules) != nil || rules.DefaultAction == "" || json.Compact(&compact, body) != nil {
		return "", fmt.Errorf("seccomp_profile in %s names %s, which is not a seccomp profile: a profile is a JSON object with a defaultAction, as the daemon's own is", path, file)
	}
	for _, action := range rules.actions() {
		if !slices.Contains(seccompActions, action) {
			return "", fmt.Errorf("seccomp_profile in %s names %s, whose action %q is not one seccomp has: the actions are %s, and the runtime refuses any other as every container starts", path, file, action, strings.Join(seccompActions, ", "))
		}
	}
	if rules.filtersNothing() {
		return "", fmt.Errorf("seccomp_profile in %s names %s, which lets every system call through: the key names the profile a container is filtered by, and one that filters nothing would lift the seccomp floor, which no setting of a runner lifts", path, file)
	}
	return compact.String(), nil
}

// seccompActions are the actions a seccomp profile may name, as the runtime reads them.
var seccompActions = []string{
	"SCMP_ACT_KILL", "SCMP_ACT_KILL_PROCESS", "SCMP_ACT_KILL_THREAD", "SCMP_ACT_TRAP",
	"SCMP_ACT_ERRNO", "SCMP_ACT_TRACE", "SCMP_ACT_ALLOW", "SCMP_ACT_LOG", "SCMP_ACT_NOTIFY",
}

// seccompRules is what of a profile decides what it does: the action taken for a call no
// rule names, and the action of each rule.
type seccompRules struct {
	DefaultAction string `json:"defaultAction"`
	Syscalls      []struct {
		Action string `json:"action"`
	} `json:"syscalls"`
}

// actions is every action the profile names, its default first.
func (r seccompRules) actions() []string {
	out := []string{r.DefaultAction}
	for _, rule := range r.Syscalls {
		out = append(out, rule.Action)
	}
	return out
}

// filtersNothing says whether every action of the profile lets the call through, which is
// a profile that confines nothing however many rules it writes. SCMP_ACT_LOG lets a call
// through and writes it down, so it filters as little as SCMP_ACT_ALLOW does.
func (r seccompRules) filtersNothing() bool {
	for _, action := range r.actions() {
		if action != "SCMP_ACT_ALLOW" && action != "SCMP_ACT_LOG" {
			return false
		}
	}
	return true
}

// profileFiltersNothing says whether a profile as a Policy holds it lets every call
// through. A profile that does not decode is not one: the daemon refuses it as every
// container starts, which is a failure and never a container with the table open.
func profileFiltersNothing(profile string) bool {
	var r seccompRules
	if json.Unmarshal([]byte(profile), &r) != nil || r.DefaultAction == "" {
		return false
	}
	return r.filtersNothing()
}

// unconfinedTypes are the SELinux types the targeted and container policies ship as
// unconfined domains and that a container is given by name.
var unconfinedTypes = []string{"spc_t", "unconfined_t", "container_runtime_t"}

// capabilities are the Linux capabilities of capabilities(7), as the daemon's CapAdd takes
// them: in capitals, without the CAP_ prefix.
var capabilities = []string{
	"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL", "SETGID",
	"SETUID", "SETPCAP", "LINUX_IMMUTABLE", "NET_BIND_SERVICE", "NET_BROADCAST",
	"NET_ADMIN", "NET_RAW", "IPC_LOCK", "IPC_OWNER", "SYS_MODULE", "SYS_RAWIO",
	"SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT", "SYS_ADMIN", "SYS_BOOT", "SYS_NICE",
	"SYS_RESOURCE", "SYS_TIME", "SYS_TTY_CONFIG", "MKNOD", "LEASE", "AUDIT_WRITE",
	"AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG", "WAKE_ALARM",
	"BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF", "CHECKPOINT_RESTORE",
}

// capability holds one name of allow_cap_add to the list.
//
// ALL is refused although CapDrop spells it: allowing every capability at once is most of
// what Privileged is, and this product has no spelling for that. A name the daemon would
// have taken in another case, or with the CAP_ prefix, is refused with its spelling
// rather than folded into it, because one setting has one spelling in the file.
func capability(path, name string) error {
	if slices.Contains(capabilities, name) {
		return nil
	}
	if strings.EqualFold(name, "ALL") {
		return fmt.Errorf("allow_cap_add names %s in %s: a runner allows capabilities one at a time, and every capability at once is most of what Privileged is, which this product does not offer", name, path)
	}
	if spelled := strings.TrimPrefix(strings.ToUpper(name), "CAP_"); slices.Contains(capabilities, spelled) {
		return fmt.Errorf("allow_cap_add names %q in %s: a capability is written as the daemon's CapAdd takes it, in capitals and without the CAP_ prefix, as %s", name, path, spelled)
	}
	return fmt.Errorf("allow_cap_add names %q in %s, which is not a Linux capability: the names are those of capabilities(7), in capitals and without the CAP_ prefix", name, path)
}
