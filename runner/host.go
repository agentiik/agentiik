package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/internal/docker"
)

// MemInfoPath is where the kernel says how much memory the host has.
const MemInfoPath = "/proc/meminfo"

// Capacity is what the host has, as join sends it: "as the agent measured it on the host rather
// than as an operator typed it", memory and disk in the grammar a step writes its own memory in.
type Capacity struct {
	VCPU   int    `json:"vcpu"`
	Memory string `json:"memory"`
	Disk   string `json:"disk"`
}

// Containment is what the host can prove about how a container will be contained on it, read from
// the daemon rather than claimed.
type Containment struct {
	Runtime     string `json:"runtime"`
	UsernsRemap bool   `json:"userns_remap"`
}

// Architecture is what this host is, spelled as the arch= label spells it.
//
// It is the architecture this agent was built for, which is Go's own spelling, amd64 and arm64,
// and the one the arch= label was written in. A static agent runs on the architecture it was built
// for, so the two are the same machine.
func Architecture() string { return runtime.GOARCH }

// measure reads the host's capacity: its vCPU, the memory the kernel reports at meminfo, and the
// disk available under the work root.
func measure(meminfo, workDir string) (Capacity, error) {
	memory, err := memTotal(meminfo)
	if err != nil {
		return Capacity{}, err
	}
	disk, err := diskAvailable(workDir)
	if err != nil {
		return Capacity{}, err
	}
	m, err := sizeOf(memory)
	if err != nil {
		return Capacity{}, fmt.Errorf("runner: %s reports %d bytes of memory, which %s", meminfo, memory, err)
	}
	d, err := sizeOf(disk)
	if err != nil {
		return Capacity{}, fmt.Errorf("runner: the filesystem under %s has %d bytes available, which %s", workDir, disk, err)
	}
	// NumCPU is the processors this process may run on, which is what the host gives the
	// agent and its containers when a cgroup or an affinity mask narrows the machine.
	return Capacity{VCPU: runtime.NumCPU(), Memory: m, Disk: d}, nil
}

// memTotal reads MemTotal from a meminfo file, in bytes.
//
// The kernel writes it "MemTotal:       16318196 kB", where kB is kibibytes whatever it says, and
// has since the file existed. A file without the line, or with it written another way, is refused
// rather than read as a host with no memory.
func memTotal(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("runner: the host's memory cannot be measured, since %s cannot be read: %s", path, reasonOf(err))
	}
	defer f.Close()
	s := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for s.Scan() {
		rest, found := strings.CutPrefix(s.Text(), "MemTotal:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("runner: the host's memory cannot be measured, since the MemTotal line of %s is not a number of kB", path)
		}
		kib, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kib < 1 || kib > math.MaxInt64>>10 {
			return 0, fmt.Errorf("runner: the host's memory cannot be measured, since the MemTotal line of %s is not a number of kB", path)
		}
		return kib << 10, nil
	}
	if err := s.Err(); err != nil {
		return 0, fmt.Errorf("runner: the host's memory cannot be measured, since %s cannot be read: %s", path, reasonOf(err))
	}
	return 0, fmt.Errorf("runner: the host's memory cannot be measured, since %s has no MemTotal line", path)
}

// diskAvailable is how many bytes the filesystem under dir has for an account that is not root.
//
// Available rather than total, since the wire's disk is "how much disk the working directory may
// use": a root filesystem holding the system and its logs has far less room for tasks than its
// size. The work root may not exist yet, since serve creates it, so the filesystem measured is the
// one it will be created on, that of its nearest ancestor that exists.
func diskAvailable(dir string) (int64, error) {
	at := filepath.Clean(dir)
	for {
		_, err := os.Stat(at)
		if err == nil {
			break
		}
		parent := filepath.Dir(at)
		if !errors.Is(err, fs.ErrNotExist) || parent == at {
			return 0, fmt.Errorf("runner: the disk under %s cannot be measured: %s", dir, reasonOf(err))
		}
		at = parent
	}
	n, err := statfsAvailable(at)
	if err != nil {
		return 0, fmt.Errorf("runner: the disk under %s cannot be measured: %s", dir, reasonOf(err))
	}
	return n, nil
}

// sizeOf writes a number of bytes in the wire's grammar, a whole number above zero with a binary
// suffix: in the largest unit that holds it exactly, so that 64Gi is written 64Gi and what the
// kernel reports is written as it reports it, and in kibibytes, rounded down, where no unit does.
// Rounding down declares a host a little smaller than it is, never larger.
func sizeOf(bytes int64) (string, error) {
	if bytes < 1<<10 {
		return "", errors.New("is less than the 1Ki the wire's grammar counts from")
	}
	for _, u := range []struct {
		suffix string
		shift  uint
	}{{"Ti", 40}, {"Gi", 30}, {"Mi", 20}} {
		if bytes&(1<<u.shift-1) == 0 {
			return strconv.FormatInt(bytes>>u.shift, 10) + u.suffix, nil
		}
	}
	return strconv.FormatInt(bytes>>10, 10) + "Ki", nil
}

// readContainment asks the daemon how it contains a container: the runtime it creates one with,
// and whether it remaps container root.
//
// It is read here, at join, and refused where the daemon cannot be reached, rather than left out:
// the wire makes the block optional, but a host whose daemon does not answer is a host that can run
// nothing, and joining it would be a runner that is refused its first start.
func readContainment(ctx context.Context, socket string) (Containment, error) {
	c, err := docker.Dial(socket)
	if err != nil {
		return Containment{}, fmt.Errorf("runner: the daemon cannot be reached, and join reads from it how this host contains a container: %w", err)
	}
	defer c.Close()
	info, err := c.Info(ctx)
	if err != nil {
		return Containment{}, fmt.Errorf("runner: the daemon at %s cannot say how it contains a container: %w", c.Socket(), err)
	}
	if info.DefaultRuntime == "" {
		return Containment{}, fmt.Errorf("runner: the daemon at %s names no default runtime, and it is the runtime this host's containers run under, which join reports", c.Socket())
	}
	return Containment{Runtime: info.DefaultRuntime, UsernsRemap: info.UsernsRemapped()}, nil
}
