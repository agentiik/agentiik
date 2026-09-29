package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/repo"
	"github.com/agentiik/agentiik/version"
)

// A push: POST git-receive-pack.
//
// "Push is registration": a push moves refs, and every commit it leaves a branch or a tag pointing
// at becomes a version, once the pre-receive hook has judged it. The hook is the one validation
// agk validate makes, version.Check, over the commit's tree, reaching the repository's pins and
// manifests, the namespace's secret declarations and the pusher's secret:use. It judges every tip
// before any ref moves, so "an invalid workflow never reaches the branch", and a push is accepted
// or refused whole, as git's atomic push is, which every push here is.
//
// In order: the commands are read, and the pack after them is spooled and unpacked, a thin one
// against what the repository holds; every object a new tip reaches is held, in the pack or in the
// repository; each command is held to what its pusher holds, workflow:write for an ordinary branch,
// grant:manage for the protected default branch, a push that is no fast-forward, a tag moved and
// any deletion; each new tip is judged; then the files of each new version are stored as the
// objects a runner fetches, the pack is written, and one transaction moves the refs, makes the pack
// live, records each version and each move. A refusal is told on git's error stream, naming the
// file, the location and the rule, and recorded in the audit log.

// receiveMaxCommands is how many refs one push may move: 1,000, which a person pushing a workflow's
// repository never comes near, and which bounds the commands read before any is checked.
const receiveMaxCommands = 1000

// command is one ref a push moves.
type command struct {
	ref      string
	old, new repo.ID

	// commit is what new peels to, where new is a tag, and new itself for a branch.
	commit repo.ID
	// forced is set where the move is no fast-forward: a branch moved to a commit its old one is
	// not behind, a tag moved, or a ref deleted.
	forced bool
}

func (c command) creates() bool { return c.old.IsZero() && !c.new.IsZero() }
func (c command) deletes() bool { return c.new.IsZero() }
func (c command) branch() bool  { return strings.HasPrefix(c.ref, "refs/heads/") }

// pushed is what a push asked for: its commands and the capabilities it said it reads.
type pushed struct {
	commands                         []command
	reportStatus, quiet, sideband64k bool
}

// readCommands reads a push's commands, up to the flush that ends them.
func readCommands(pkts *repo.PktReader) (pushed, error) {
	var p pushed
	for {
		kind, data, err := pkts.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return p, nil
			}
			return pushed{}, err
		}
		if kind == repo.PktFlush {
			return p, nil
		}
		if kind != repo.PktData {
			return pushed{}, errors.New("a push's commands are packets of data ended by a flush")
		}
		line, caps, first := strings.Cut(strings.TrimSuffix(string(data), "\n"), "\x00")
		if first && len(p.commands) == 0 {
			for _, c := range strings.Fields(caps) {
				switch c {
				case "report-status":
					p.reportStatus = true
				case "quiet":
					p.quiet = true
				case "side-band-64k":
					p.sideband64k = true
				case "push-cert", "report-status-v2", "push-options":
					return pushed{}, fmt.Errorf("a push asked for %s, which this installation does not offer", c)
				}
			}
		}
		if strings.HasPrefix(line, "shallow ") {
			return pushed{}, errors.New("this installation does not take a push from a shallow clone: its commits could name parents nobody sent")
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return pushed{}, fmt.Errorf("a push's command is an old object, a new one and a ref, and this one is %.128q", line)
		}
		old, err1 := repo.ParseID(fields[0])
		new, err2 := repo.ParseID(fields[1])
		if err := errors.Join(err1, err2); err != nil {
			return pushed{}, fmt.Errorf("a push's command %.128q: %w", line, err)
		}
		if old.IsZero() && new.IsZero() {
			return pushed{}, fmt.Errorf("a push's command %.128q neither creates, moves nor deletes %s", line, fields[2])
		}
		if len(p.commands) == receiveMaxCommands {
			return pushed{}, fmt.Errorf("a push moves at most %d refs", receiveMaxCommands)
		}
		for _, c := range p.commands {
			if c.ref == fields[2] {
				return pushed{}, fmt.Errorf("a push moves %s twice", fields[2])
			}
		}
		p.commands = append(p.commands, command{ref: fields[2], old: old, new: new})
	}
}

// refusal is why a push is refused: said to every ref in the report, and told whole on git's error
// stream, the rule and where it was written where the hook refused it.
type pushRefusal struct {
	short  string
	told   []string
	rule   *graph.Refusal
	commit repo.ID
}

func (r *pushRefusal) Error() string { return r.short }

func (s *Server) receivePack(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	ctx := r.Context()
	held, err := HoldsOn(r)(ctx, WorkflowWrite)
	if err != nil {
		gitFail(w, http.StatusInternalServerError, "the push could not be authorised")
		return
	}
	if !held {
		notPushing(w, over)
		return
	}
	if s.packs == nil || s.objects == nil {
		gitFail(w, http.StatusServiceUnavailable, noPacks)
		return
	}
	body, err := gitBody(r, repo.MaxPackBytes+receiveMaxCommands*(repo.MaxPktLen))
	if err != nil {
		gitFail(w, http.StatusBadRequest, err.Error())
		return
	}
	pkts := repo.NewPktReader(body)
	p, err := readCommands(pkts)
	if errors.Is(err, errTooLarge) {
		gitFail(w, http.StatusRequestEntityTooLarge, "a push's commands are larger than this installation reads")
		return
	}
	if err != nil {
		gitFail(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	rep := &report{w: w, sideband: p.sideband64k, status: p.reportStatus}
	if len(p.commands) == 0 {
		// A push with nothing to move, which git sends when every ref is already where it
		// is pushed to.
		rep.done("unpack ok")
		return
	}

	repository, err := s.repositoryOf(ctx, over)
	if errors.Is(err, db.ErrNoWorkflow) {
		gitFail(w, http.StatusNotFound, notFound)
		return
	}
	if err != nil {
		s.report(err)
		gitFail(w, http.StatusInternalServerError, "the repository could not be read")
		return
	}
	objects, err := s.objectsOf(repository)
	if err != nil {
		s.report(err)
		gitFail(w, http.StatusInternalServerError, "the repository could not be read")
		return
	}
	defer objects.Close()

	received, err := s.receive(ctx, body, objects, p)
	if err != nil {
		var refused *pushRefusal
		if errors.As(err, &refused) {
			s.refuse(ctx, rep, who, over, p, refused, "unpack "+oneLine(refused.short))
			return
		}
		s.report(fmt.Errorf("api: a push to %s/%s: %w", over.Namespace, over.Workflow, err))
		rep.fail(p, "unpack the push could not be read", "the installation could not read the push")
		return
	}
	defer received.close()

	checked, err := s.judge(ctx, r, who, over, repository, received, p)
	if err != nil {
		var refused *pushRefusal
		if errors.As(err, &refused) {
			s.refuse(ctx, rep, who, over, p, refused, "unpack ok")
			return
		}
		s.report(fmt.Errorf("api: a push to %s/%s: %w", over.Namespace, over.Workflow, err))
		rep.fail(p, "unpack ok", "the installation could not judge the push")
		return
	}
	if err := s.accept(ctx, who, over, received, p, checked); err != nil {
		var refused *pushRefusal
		if errors.As(err, &refused) {
			s.refuse(ctx, rep, who, over, p, refused, "unpack ok")
			return
		}
		s.report(fmt.Errorf("api: a push to %s/%s: %w", over.Namespace, over.Workflow, err))
		rep.fail(p, "unpack ok", "the installation could not record the push")
		return
	}
	lines := []string{"unpack ok"}
	for _, c := range p.commands {
		lines = append(lines, "ok "+c.ref)
	}
	rep.done(lines...)
}

// oneLine is text as one line of a report, which a newline would end early.
func oneLine(s string) string { return strings.ReplaceAll(s, "\n", " ") }

// report answers a push: its status, on the side band where the client asked for one, and what a
// person should read, on the side band's second band, which git prints after "remote:".
type report struct {
	w                http.ResponseWriter
	sideband, status bool
}

// tell writes lines a person reads, where there is a side band to write them on.
func (rp *report) tell(lines ...string) {
	if !rp.sideband {
		return
	}
	band := repo.NewSidebandWriter(rp.w, repo.BandProgress, repo.SidebandMaxPkt)
	for _, line := range lines {
		io.WriteString(band, line+"\n")
	}
}

// done writes the status lines, and ends the answer.
func (rp *report) done(lines ...string) {
	if rp.status {
		var b []byte
		for _, line := range lines {
			b, _ = repo.AppendPkt(b, []byte(line+"\n"))
		}
		b = append(b, "0000"...)
		if rp.sideband {
			repo.NewSidebandWriter(rp.w, repo.BandData, repo.SidebandMaxPkt).Write(b)
		} else {
			rp.w.Write(b)
		}
	}
	if rp.sideband {
		repo.WriteFlush(rp.w)
	}
}

// fail answers every ref of a push as refused, for why, after the unpack line.
func (rp *report) fail(p pushed, unpack, why string) {
	lines := []string{unpack}
	for _, c := range p.commands {
		lines = append(lines, "ng "+c.ref+" "+oneLine(why))
	}
	rp.tell(why)
	rp.done(lines...)
}

// refuse answers a push refused, and records it: what it asked to move and why.
func (s *Server) refuse(ctx context.Context, rep *report, who Principal, over Target, p pushed, why *pushRefusal, unpack string) {
	detail := map[string]any{"reason": why.short}
	var refs []any
	for _, c := range p.commands {
		refs = append(refs, map[string]any{"ref": c.ref, "old": idOrEmpty(c.old), "new": idOrEmpty(c.new)})
	}
	detail["refs"] = refs
	if !why.commit.IsZero() {
		detail["commit"] = why.commit.String()
	}
	if why.rule != nil {
		detail["rule"] = string(why.rule.Rule)
		if why.rule.At.File != "" {
			detail["file"] = why.rule.At.File
			detail["line"] = why.rule.At.Line
			detail["column"] = why.rule.At.Column
		}
	}
	// In a transaction of its own, since the push's was never committed: an act refused is still
	// an act somebody asked for, and the log is where it is kept.
	err := s.pool.In(context.WithoutCancel(ctx), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		return ns.Audit(ctx, audit.Record{Actor: string(who), Action: audit.PushRefuse, Target: over.Workflow, Result: audit.Done, Detail: detail})
	})
	if err != nil {
		s.report(fmt.Errorf("api: a refused push to %s/%s could not be recorded: %w", over.Namespace, over.Workflow, err))
	}
	told := why.told
	if len(told) == 0 {
		told = []string{why.short}
	}
	rep.tell(told...)
	lines := []string{unpack}
	for _, c := range p.commands {
		lines = append(lines, "ng "+c.ref+" "+oneLine(why.short))
	}
	rep.done(lines...)
}

func idOrEmpty(id repo.ID) string {
	if id.IsZero() {
		return ""
	}
	return id.String()
}

// received is a push's pack, unpacked into a pack of whole objects on disk, with what it holds and
// the repository it reaches.
type received struct {
	file     *os.File
	size     int64
	unpacked *repo.Unpacked
	pack     *repo.Pack

	// objects is the pack and the repository together, which a thin pack's commits reach.
	objects repo.Lookup
	dir     string
}

func (rc *received) close() {
	if rc.file != nil {
		rc.file.Close()
	}
	if rc.dir != "" {
		os.RemoveAll(rc.dir)
	}
}

// both finds an object in a pack received, then in the repository.
type both struct {
	pack       *repo.Pack
	repository repo.Lookup
}

func (b both) OpenObject(ctx context.Context, id repo.ID) (repo.ObjectReader, error) {
	if b.pack != nil && b.pack.Has(id) {
		return b.pack.OpenObject(ctx, id)
	}
	return b.repository.OpenObject(ctx, id)
}

// receive spools the pack after the commands, unpacks it against the repository, and checks that
// every object each new tip reaches is held.
func (s *Server) receive(ctx context.Context, body io.Reader, repository repo.Lookup, p pushed) (*received, error) {
	dir, err := os.MkdirTemp("", "agentiik-push-")
	if err != nil {
		return nil, err
	}
	rc := &received{dir: dir, objects: both{repository: repository}}
	spool, err := os.Create(path.Join(dir, "sent.pack"))
	if err != nil {
		rc.close()
		return nil, err
	}
	defer spool.Close()
	sent, err := io.Copy(spool, body)
	if errors.Is(err, errTooLarge) {
		rc.close()
		return nil, &pushRefusal{short: fmt.Sprintf("the pack is larger than the %d bytes a push may send", repo.MaxPackBytes)}
	}
	if err != nil {
		rc.close()
		return nil, err
	}
	if sent > 0 {
		out, err := os.Create(path.Join(dir, "objects.pack"))
		if err != nil {
			rc.close()
			return nil, err
		}
		rc.file = out
		u, err := repo.Unpack(ctx, spool, sent, out, repo.UnpackOptions{Bases: repository})
		if err != nil {
			rc.close()
			return nil, &pushRefusal{short: err.Error()}
		}
		size, err := out.Seek(0, io.SeekEnd)
		if err != nil {
			rc.close()
			return nil, err
		}
		var idx bytes.Buffer
		if err := repo.WriteIdx(&idx, u.Objects, u.Checksum); err != nil {
			rc.close()
			return nil, err
		}
		parsed, err := repo.ParseIdx(idx.Bytes())
		if err != nil {
			rc.close()
			return nil, err
		}
		if rc.pack, err = repo.OpenPack(out, size, parsed); err != nil {
			rc.close()
			return nil, err
		}
		rc.size, rc.unpacked = size, u
		rc.objects = both{pack: rc.pack, repository: repository}
	}

	// "Every object reachable from each new tip is in the pack or in the repository", whose
	// objects are whole by the same check at every push before, so the walk stops at the first
	// object the repository holds rather than walking its history again.
	var tips []repo.ID
	for _, c := range p.commands {
		if !c.deletes() {
			tips = append(tips, c.new)
		}
	}
	if err := connected(ctx, rc, repository, tips); err != nil {
		rc.close()
		return nil, err
	}
	return rc, nil
}

// connected refuses a push whose new tips reach an object neither its pack nor the repository
// holds, which a clone of the repository would find missing.
func connected(ctx context.Context, rc *received, repository repo.Lookup, tips []repo.ID) error {
	seen := map[repo.ID]bool{}
	queue := slices.Clone(tips)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if rc.pack == nil || !rc.pack.Has(id) {
			o, err := repository.OpenObject(ctx, id)
			if errors.Is(err, repo.ErrMissing) {
				return &pushRefusal{short: fmt.Sprintf("missing necessary objects: %s is named and neither sent nor held", id)}
			}
			if err != nil {
				return err
			}
			o.Close()
			continue
		}
		o, err := rc.pack.OpenObject(ctx, id)
		if err != nil {
			return err
		}
		t := o.Type()
		o.Close()
		switch t {
		case repo.TypeBlob:
		case repo.TypeCommit:
			_, data, err := repo.ReadObject(ctx, rc.pack, id, repo.MaxParsedBytes)
			if err != nil {
				return err
			}
			c, err := repo.ParseCommit(data)
			if err != nil {
				return err
			}
			queue = append(queue, c.Tree)
			queue = append(queue, c.Parents...)
		case repo.TypeTree:
			_, data, err := repo.ReadObject(ctx, rc.pack, id, repo.MaxParsedBytes)
			if err != nil {
				return err
			}
			entries, err := repo.ParseTree(data)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if e.Mode != repo.ModeSubmodule {
					queue = append(queue, e.ID)
				}
			}
		case repo.TypeTag:
			_, data, err := repo.ReadObject(ctx, rc.pack, id, repo.MaxParsedBytes)
			if err != nil {
				return err
			}
			tag, err := repo.ParseTag(data)
			if err != nil {
				return err
			}
			queue = append(queue, tag.Object)
		}
	}
	return nil
}

// judged is a commit the hook accepted, and the version it makes.
type judged struct {
	commit  repo.ID
	parent  string
	checked *version.Checked
}

// judge holds each command to the refs as they stand and to what the pusher holds, then judges
// every commit a ref will point at that is not a version already.
func (s *Server) judge(ctx context.Context, r *http.Request, who Principal, over Target, repository db.Repository, rc *received, p pushed) ([]judged, error) {
	current := map[string]db.Ref{}
	for _, ref := range repository.Refs {
		current[ref.Name] = ref
	}
	var owner *bool
	holdsManage := func() (bool, error) {
		if owner == nil {
			held, err := HoldsOn(r)(ctx, GrantManage)
			if err != nil {
				return false, err
			}
			owner = &held
		}
		return *owner, nil
	}

	for i := range p.commands {
		c := &p.commands[i]
		if err := db.CheckRef(c.ref); err != nil {
			return nil, &pushRefusal{short: fmt.Sprintf("%s is not a ref a workflow repository holds: only branches, refs/heads/, and tags, refs/tags/, on git's rules for a ref's name", oneLine(c.ref))}
		}
		ref, has := current[c.ref]
		now := ""
		if has {
			now = ref.Target()
		}
		if now != idOrEmpty(c.old) {
			return nil, &pushRefusal{short: fmt.Sprintf("%s is no longer where the push found it: fetch, and push again", c.ref)}
		}
		if c.deletes() {
			if c.ref == "refs/heads/"+repository.DefaultBranch {
				return nil, &pushRefusal{short: fmt.Sprintf("%s is the default branch, which is not deleted: another branch is named the default first", c.ref)}
			}
			c.forced = true
		} else {
			commit, err := peel(ctx, rc.objects, c.new)
			if err != nil {
				return nil, err
			}
			if c.branch() && commit != c.new {
				return nil, &pushRefusal{short: fmt.Sprintf("%s is a branch and is pushed to a tag object: a branch names a commit", c.ref)}
			}
			if commit.IsZero() {
				return nil, &pushRefusal{short: fmt.Sprintf("%s is pushed to %s, which is no commit: every tip a push leaves is a version, and a version is a commit", c.ref, c.new)}
			}
			c.commit = commit
			switch {
			case c.creates():
			case !c.branch():
				c.forced = true
			default:
				ahead, err := descends(ctx, rc.objects, c.new, c.old)
				if err != nil {
					return nil, err
				}
				c.forced = !ahead
			}
		}
		protected := has && ref.Protected
		if c.forced || protected {
			owns, err := holdsManage()
			if err != nil {
				return nil, err
			}
			if !owns {
				why := fmt.Sprintf("%s is the protected default branch, which only whoever holds grant:manage on the workflow moves", c.ref)
				switch {
				case c.deletes():
					why = fmt.Sprintf("deleting %s takes grant:manage on the workflow, which you do not hold", c.ref)
				case c.forced && c.branch():
					why = fmt.Sprintf("%s is pushed to a commit that is not ahead of it, which takes grant:manage on the workflow, which you do not hold: a forced push rewrites what others fetched", c.ref)
				case c.forced:
					why = fmt.Sprintf("moving the tag %s takes grant:manage on the workflow, which you do not hold: a tag others fetched names one commit", c.ref)
				}
				return nil, &pushRefusal{short: why}
			}
		}
	}

	// Each commit a ref will point at is judged once, in the order the commands name them, and a
	// commit that is a version already is not judged again: "a commit already stored is judged by
	// none of the rules that arrived after it was stored".
	var out []judged
	seen := map[repo.ID]bool{}
	for _, c := range p.commands {
		if c.deletes() || seen[c.commit] {
			continue
		}
		seen[c.commit] = true
		stored, err := s.isVersion(ctx, over, c.commit.String())
		if err != nil {
			return nil, err
		}
		if stored {
			continue
		}
		j, err := s.hook(ctx, r, over, rc, c.commit)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// isVersion says whether a commit is a version of the workflow already.
func (s *Server) isVersion(ctx context.Context, over Target, commit string) (bool, error) {
	var stored bool
	err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
		_, err := ns.Version(ctx, over.Workflow, commit)
		stored = err == nil
		if errors.Is(err, db.ErrNoVersion) {
			return nil
		}
		return err
	})
	return stored, err
}

// hook judges one commit as the version it would be, by the one validation agk validate makes.
func (s *Server) hook(ctx context.Context, r *http.Request, over Target, rc *received, commit repo.ID) (judged, error) {
	_, data, err := repo.ReadObject(ctx, rc.objects, commit, repo.MaxParsedBytes)
	if err != nil {
		return judged{}, err
	}
	c, err := repo.ParseCommit(data)
	if err != nil {
		return judged{}, err
	}
	tree := repo.NewTreeFS(ctx, rc.objects, c.Tree)
	checked, err := version.Check(ctx, tree, version.Checking{
		Commit: commit.String(), Committed: true, Namespace: over.Namespace, Repository: over.Workflow,
		Resolvers: s.resolvers(r, over),
	})
	var rule *graph.Refusal
	var unusable *version.SecretsNotUsable
	switch {
	case errors.As(err, &rule):
		at := rule.At.String()
		short := string(rule.Rule)
		if at != "" {
			short += " at " + at
		}
		told := []string{fmt.Sprintf("%s refused %s: %s", over.Workflow, commit, short)}
		if rule.Detail != "" {
			told = append(told, rule.Detail)
		}
		return judged{}, &pushRefusal{short: "refused: " + short, told: told, rule: rule, commit: commit}
	case errors.As(err, &unusable):
		return judged{}, &pushRefusal{short: "refused: this commit names a secret and you do not hold secret:use", told: []string{unusable.Error()}, commit: commit}
	case err != nil:
		var fault *pushFault
		if errors.As(err, &fault) {
			return judged{}, err
		}
		return judged{}, &pushRefusal{short: "refused: " + oneLine(err.Error()), told: []string{fmt.Sprintf("%s refused %s: %s", over.Workflow, commit, err)}, commit: commit}
	}
	var parent string
	if len(c.Parents) > 0 {
		parent = c.Parents[0].String()
	}
	return judged{commit: commit, parent: parent, checked: checked}, nil
}

// resolvers are what the hook reaches beyond a commit's tree: the repository's pins and manifests,
// the namespace's secret declarations and the pusher's secret:use. A workflow include is refused
// until workflow includes arrive: a resolver left out is a check that refuses.
func (s *Server) resolvers(r *http.Request, over Target) version.Resolvers {
	return version.Resolvers{
		Pin: func(ctx context.Context, reference string, _ agk.Step) (string, error) {
			var image string
			err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
				var err error
				image, err = ns.Pin(ctx, over.Workflow, reference)
				return err
			})
			if errors.Is(err, db.ErrNoPin) {
				return "", version.ErrNotHeld
			}
			if err != nil {
				return "", &pushFault{"the repository's pins could not be read", err}
			}
			return image, nil
		},
		Manifest: func(ctx context.Context, image string, _ agk.Step) ([]byte, error) {
			var manifest []byte
			err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
				var err error
				manifest, err = ns.Manifest(ctx, over.Workflow, image)
				return err
			})
			if errors.Is(err, db.ErrNoManifest) {
				return nil, version.ErrNotHeld
			}
			if err != nil {
				return nil, &pushFault{"the repository's manifests could not be read", err}
			}
			return manifest, nil
		},
		Secrets: func(ctx context.Context) ([]string, error) {
			var declared []db.Declaration
			err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
				var err error
				declared, err = ns.Declarations(ctx)
				return err
			})
			if err != nil {
				return nil, &pushFault{"the namespace's secret declarations could not be read", err}
			}
			names := make([]string, 0, len(declared))
			for _, d := range declared {
				names = append(names, d.Name)
			}
			return names, nil
		},
		SecretUse: func(ctx context.Context, _ []string) (bool, error) {
			held, err := HoldsOn(r)(ctx, SecretUse)
			if err != nil {
				return false, &pushFault{"the push could not be authorised", err}
			}
			return held, nil
		},
	}
}

// peel answers the commit an object names, through tags, or the zero ID where it names no commit.
func peel(ctx context.Context, objects repo.Lookup, id repo.ID) (repo.ID, error) {
	for range 64 {
		t, data, err := repo.ReadObject(ctx, objects, id, repo.MaxParsedBytes)
		if err != nil {
			return repo.ID{}, err
		}
		switch t {
		case repo.TypeCommit:
			return id, nil
		case repo.TypeTag:
			tag, err := repo.ParseTag(data)
			if err != nil {
				return repo.ID{}, err
			}
			id = tag.Object
		default:
			return repo.ID{}, nil
		}
	}
	return repo.ID{}, nil
}

// descendsMaxCommits bounds the walk that decides a fast-forward: a million commits, more than a
// workflow's history holds, past which a push is taken as forced and needs grant:manage.
const descendsMaxCommits = 1 << 20

// descends says whether commit is ahead of old: old is commit or behind it.
func descends(ctx context.Context, objects repo.Lookup, commit, old repo.ID) (bool, error) {
	seen := map[repo.ID]bool{}
	queue := []repo.ID{commit}
	for len(queue) > 0 && len(seen) < descendsMaxCommits {
		id := queue[0]
		queue = queue[1:]
		if id == old {
			return true, nil
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		t, data, err := repo.ReadObject(ctx, objects, id, repo.MaxParsedBytes)
		if err != nil {
			return false, err
		}
		if t != repo.TypeCommit {
			return false, nil
		}
		c, err := repo.ParseCommit(data)
		if err != nil {
			return false, err
		}
		queue = append(queue, c.Parents...)
	}
	return false, nil
}

// accept records a push the hook accepted: the files of each new version as the objects a runner
// fetches, the pack, then in one transaction the refs, the pack made live, each version and each
// move recorded.
func (s *Server) accept(ctx context.Context, who Principal, over Target, rc *received, p pushed, versions []judged) error {
	now := s.now()
	type made struct {
		v     db.Version
		files map[string]fileOf
	}
	var all []made
	for _, j := range versions {
		tree, files, err := treeOf(ctx, rc.objects, j.commit)
		if err != nil {
			return err
		}
		v := j.checked.Version
		v.Namespace, v.Workflow, v.Commit, v.Parent = over.Namespace, over.Workflow, j.commit.String(), j.parent
		v.Tree, v.Author, v.CreatedAt, v.Source = tree, string(who), now, db.SourceGit
		all = append(all, made{v: v, files: files})
	}
	// The bytes before the rows, as the tree push writes them, so that a version that exists names
	// objects that exist.
	for _, m := range all {
		if err := s.storeFiles(ctx, over.Namespace, rc.objects, m.files, false); err != nil {
			return err
		}
	}
	var pack db.Pack
	if rc.unpacked != nil && len(rc.unpacked.Objects) > 0 {
		if _, err := rc.file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		var err error
		if pack, err = s.packs.Put(ctx, s.pool, over.Namespace, over.Workflow, rc.file, rc.size, rc.unpacked); err != nil {
			return err
		}
	}

	updates := make([]db.RefUpdate, 0, len(p.commands))
	for _, c := range p.commands {
		u := db.RefUpdate{Ref: c.ref, Old: idOrEmpty(c.old), New: idOrEmpty(c.new)}
		if !c.deletes() && c.commit != c.new {
			u.Commit = c.commit.String()
		}
		updates = append(updates, u)
	}
	var saved []db.Saved
	err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.UpdateRefs(ctx, over.Workflow, string(who), now, updates); err != nil {
			return err
		}
		if pack.Name != "" {
			if err := ns.PackLive(ctx, over.Workflow, pack.Name); err != nil {
				return err
			}
		}
		saved = saved[:0]
		for _, m := range all {
			got, err := ns.SaveVersion(ctx, m.v)
			if err != nil {
				return err
			}
			saved = append(saved, got)
		}
		for _, c := range p.commands {
			detail := map[string]any{"ref": c.ref, "old": idOrEmpty(c.old), "new": idOrEmpty(c.new), "forced": c.forced}
			if err := ns.Audit(ctx, audit.Record{Actor: string(who), Action: audit.RefUpdate, Target: over.Workflow, Result: audit.Done, Detail: detail}); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, db.ErrStaleRef):
		return &pushRefusal{short: "a ref moved while this push was judged: fetch, and push again"}
	case errors.Is(err, db.ErrDefaultBranch):
		return &pushRefusal{short: "the default branch is not deleted: another branch is named the default first"}
	case errors.Is(err, db.ErrOtherTree):
		return &pushRefusal{short: "a commit this push makes a version was recorded with another tree, which one commit cannot have"}
	case err != nil:
		return err
	}
	// And again for any object a sweep had claimed while a version raised its reference onto it,
	// or whose row the version had to create, as the tree push does: see there.
	for i, m := range all {
		again := map[string]fileOf{}
		for _, digest := range saved[i].MustWriteBytes {
			again[digest] = m.files[digest]
		}
		for _, digest := range saved[i].Recorded {
			held, err := s.objects.Has(ctx, artifact.Key(over.Namespace, digest))
			if err != nil {
				return err
			}
			if !held {
				again[digest] = m.files[digest]
			}
		}
		if err := s.storeFiles(ctx, over.Namespace, rc.objects, again, true); err != nil {
			return err
		}
	}
	return nil
}

// fileOf is where the bytes of a version's file are: the blob holding them.
type fileOf struct{ blob repo.ID }

// treeOf is a commit's tree as a version keeps it: every file with its path, SHA-256, size and
// mode, and where the bytes of each digest are. The hook has refused a tree holding a symbolic
// link or a submodule, so every entry is a directory or a file.
func treeOf(ctx context.Context, objects repo.Lookup, commit repo.ID) ([]db.TreeFile, map[string]fileOf, error) {
	_, data, err := repo.ReadObject(ctx, objects, commit, repo.MaxParsedBytes)
	if err != nil {
		return nil, nil, err
	}
	c, err := repo.ParseCommit(data)
	if err != nil {
		return nil, nil, err
	}
	var tree []db.TreeFile
	files := map[string]fileOf{}
	var walk func(id repo.ID, dir string) error
	walk = func(id repo.ID, dir string) error {
		_, data, err := repo.ReadObject(ctx, objects, id, repo.MaxParsedBytes)
		if err != nil {
			return err
		}
		entries, err := repo.ParseTree(data)
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := path.Join(dir, e.Name)
			switch e.Mode {
			case repo.ModeTree:
				if err := walk(e.ID, name); err != nil {
					return err
				}
			case repo.ModeFile, repo.ModeExecutable:
				digest, size, err := digestOf(ctx, objects, e.ID)
				if err != nil {
					return err
				}
				mode := "0644"
				if e.Mode == repo.ModeExecutable {
					mode = "0755"
				}
				tree = append(tree, db.TreeFile{Path: name, SHA256: digest, Size: size, Mode: mode})
				files[digest] = fileOf{blob: e.ID}
			default:
				return fmt.Errorf("%s is a %s, which a version's tree does not hold", name, e.Mode)
			}
		}
		return nil
	}
	if err := walk(c.Tree, ""); err != nil {
		return nil, nil, err
	}
	return tree, files, nil
}

// digestOf is a blob's SHA-256 and size, read as it streams.
func digestOf(ctx context.Context, objects repo.Lookup, blob repo.ID) (string, int64, error) {
	o, err := objects.OpenObject(ctx, blob)
	if err != nil {
		return "", 0, err
	}
	defer o.Close()
	h := sha256.New()
	n, err := io.Copy(h, o)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// storeFiles writes each file as the object a runner fetches, by its SHA-256, streamed from its
// blob, leaving one the store holds unless again says to write it all the same.
func (s *Server) storeFiles(ctx context.Context, namespace string, objects repo.Lookup, files map[string]fileOf, again bool) error {
	digests := make([]string, 0, len(files))
	for digest := range files {
		digests = append(digests, digest)
	}
	slices.Sort(digests)
	for _, digest := range digests {
		key := artifact.Key(namespace, digest)
		if !again {
			held, err := s.objects.Has(ctx, key)
			if err != nil {
				return err
			}
			if held {
				continue
			}
		}
		o, err := objects.OpenObject(ctx, files[digest].blob)
		if err != nil {
			return err
		}
		err = s.objects.Put(ctx, key, o)
		o.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
