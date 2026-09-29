package api

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/repo"
	"github.com/agentiik/agentiik/repo/store"
)

// Git's smart HTTP: "GET, POST /{ns}/{name}.git/*. Clone and fetch need workflow:read, push needs
// workflow:write, and secret:use in the namespace where a commit names a secret; a pre-receive hook
// validates the entry point before any ref moves."
//
// Three routes, the ones the smart protocol asks for: the advertisement of a repository's refs,
// GET info/refs?service=git-upload-pack or git-receive-pack; a fetch, POST git-upload-pack; and a
// push, POST git-receive-pack. Nothing else under a repository is served: the dumb protocol reads
// objects as loose files and packs by name, which a repository kept as packs in the object store and
// refs in the database does not have, and a client falls back to it only where the smart one is not
// answered.
//
// The protocol spoken is version 0, whatever a client asks for in Git-Protocol: git reads an
// advertisement of version 0 whichever it asked for, and a push is version 0 in every version of the
// protocol there is. Version 2 is an optimisation of how a fetch lists refs, for a later release.
//
// Every refusal a person should read is text, which git shows them after "remote:" or in its error,
// where the API's are JSON, which git would show as it is.

// gitAgent is what the installation calls itself to a git client, which git prints nowhere and
// records in its trace.
const gitAgent = "agentiik"

// The services the advertisement is asked for.
const (
	uploadPack  = "git-upload-pack"
	receivePack = "git-receive-pack"
)

// gitRequestMaxBytes bounds what a fetch sends, its wants and haves, once gzip has been undone:
// 64 MiB, a million lines of fifty bytes, more haves than any history git would send before
// giving up on finding what the two ends share. A push is bounded by the pack it carries instead.
const gitRequestMaxBytes = 64 << 20

// registerGit registers the three routes on rt.
func (s *Server) registerGit(rt *Router) error {
	for _, r := range []struct {
		method, pattern string
		guard           Guard
		handler         Handler
	}{
		// An advertisement for a push takes workflow:write besides, which only the service asked
		// for says, so the handler asks it.
		{"GET", "/{namespace}/{repository}/info/refs",
			OnRepository{Permission: WorkflowRead, Asks: []Permission{WorkflowWrite}}, s.advertise},
		{"POST", "/{namespace}/{repository}/" + uploadPack,
			OnRepository{Permission: WorkflowRead}, s.uploadPack},
		// A push takes workflow:write, grant:manage for what only an owner may do to a ref, and
		// secret:use where a commit it carries names a secret; which of them depends on the
		// commands, which only the handler reads.
		{"POST", "/{namespace}/{repository}/" + receivePack,
			OnRepository{Permission: WorkflowRead, Asks: []Permission{WorkflowWrite, GrantManage, SecretUse}}, s.receivePack},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return err
		}
	}
	return nil
}

// gitFail answers a refusal as the text git shows a person.
func gitFail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	io.WriteString(w, message+"\n")
}

// gitRequest refuses a POST whose body is not of the type git sends to service, as git's own
// http-backend does, with 415: neither type is one a page of another site may send without asking
// first, so a browser holding a token it was given for this host, as Basic's password, cannot be
// made to send a fetch or a push by a page it visits.
func gitRequest(w http.ResponseWriter, r *http.Request, service string) bool {
	want := "application/x-" + service + "-request"
	if r.Header.Get("Content-Type") == want {
		return true
	}
	gitFail(w, http.StatusUnsupportedMediaType, fmt.Sprintf("a POST to %s carries %s, which git sends, and this one carries %.64q", service, want, r.Header.Get("Content-Type")))
	return false
}

// notPushing is the refusal of a caller who may read a repository and not push to it: a 403 rather
// than the 404 of one they cannot read, since they can, and told why.
func notPushing(w http.ResponseWriter, over Target) {
	gitFail(w, http.StatusForbidden, fmt.Sprintf("you may read %s/%s and not push to it: a push takes workflow:write on the workflow", over.Namespace, over.Workflow))
}

// advertise answers GET info/refs: the repository's refs and what the service asked for can do.
func (s *Server) advertise(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	service := r.URL.Query().Get("service")
	switch service {
	case uploadPack:
	case receivePack:
		held, err := HoldsOn(r)(r.Context(), WorkflowWrite)
		if err != nil {
			gitFail(w, http.StatusInternalServerError, "the push could not be authorised")
			return
		}
		if !held {
			notPushing(w, over)
			return
		}
	default:
		gitFail(w, http.StatusForbidden, "this installation serves git's smart protocol alone, and a client that asks for no service is asking for the dumb one: its repositories are packs in the object store and refs in the database, and hold no loose file to read")
		return
	}
	if s.packs == nil {
		gitFail(w, http.StatusServiceUnavailable, noPacks)
		return
	}
	repository, err := s.repositoryOf(r.Context(), over)
	if errors.Is(err, db.ErrNoWorkflow) {
		gitFail(w, http.StatusNotFound, notFound)
		return
	}
	if err != nil {
		s.report(err)
		gitFail(w, http.StatusInternalServerError, "the repository could not be read")
		return
	}

	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	var b []byte
	b, _ = repo.AppendPkt(b, []byte("# service="+service+"\n"))
	b = append(b, "0000"...)
	for i, line := range advertised(repository, service) {
		if b, err = repo.AppendPkt(b, []byte(line)); err != nil {
			// A ref past what a packet holds, which db.MaxRefBytes keeps from being stored;
			// answered as the installation's failure, since no push could have made it.
			s.report(fmt.Errorf("api: ref %d of %s/%s does not fit a packet: %w", i, over.Namespace, over.Workflow, err))
			gitFail(w, http.StatusInternalServerError, "the repository could not be read")
			return
		}
	}
	b = append(b, "0000"...)
	w.Write(b)
}

// notFound is how git is told a repository is not there, or not the caller's to read.
const notFound = "no such repository, or not yours"

// noPacks is the refusal of an installation whose object store cannot hold a repository's packs.
const noPacks = "this installation's object store cannot read a range of an object, and a repository's packs are read one entry at a time: the built-in store, AGK_OBJECTS_DIR, can"

// advertised are the lines of an advertisement, each ending in a newline, the first carrying the
// capabilities after a NUL: for a fetch HEAD first where the default branch is born, then every
// branch and tag in git's order, an annotated tag followed by the commit it peels to; for a push the
// refs alone. A repository with no ref advertises capabilities^{} under the zero ID, as git does.
func advertised(r db.Repository, service string) []string {
	var lines []string
	var head string
	for _, ref := range r.Refs {
		if ref.Name == "refs/heads/"+r.DefaultBranch && ref.Commit != "" {
			head = ref.Commit
		}
	}
	caps := receiveCapabilities
	if service == uploadPack {
		caps = uploadCapabilities
		if head != "" {
			caps += " symref=HEAD:refs/heads/" + r.DefaultBranch
			lines = append(lines, head+" HEAD")
		}
	}
	caps += " object-format=sha1 agent=" + gitAgent
	for _, ref := range r.Refs {
		if ref.Commit == "" {
			continue
		}
		lines = append(lines, ref.Target()+" "+ref.Name)
		if service == uploadPack && ref.Tag != "" {
			lines = append(lines, ref.Commit+" "+ref.Name+"^{}")
		}
	}
	if len(lines) == 0 {
		lines = append(lines, strings.Repeat("0", 40)+" capabilities^{}")
	}
	lines[0] += "\x00" + caps
	for i := range lines {
		lines[i] += "\n"
	}
	return lines
}

// The capabilities of each service, as git names them.
//
// A fetch: the pack on side band 64k, or 1k for a client that asks for that one, progress left out
// where no-progress is asked, an annotated tag sent where it points at what is sent, where
// include-tag is asked, and every common commit acknowledged, which is what lets a client that
// asks nothing of the server between two requests go on finding them (see git_upload.go).
// ofs-delta says a client may be sent deltas against an offset, which a pack of whole objects never
// holds and which is harmless to say. There is no shallow: a shallow clone is refused by git
// itself, saying the server does not support it.
//
// A push: its status reported, refs deleted, the report on side band 64k, quiet, and atomic, which
// every push is whether asked or not, since the refs of one push move all together or not at all.
const (
	uploadCapabilities  = "multi_ack_detailed side-band-64k side-band ofs-delta include-tag no-progress"
	receiveCapabilities = "report-status delete-refs side-band-64k quiet atomic ofs-delta"
)

// repositoryOf reads a workflow's repository: its default branch, its refs and its live packs.
func (s *Server) repositoryOf(ctx context.Context, over Target) (db.Repository, error) {
	var repository db.Repository
	err := s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		repository, err = ns.Repository(ctx, over.Workflow)
		return err
	})
	return repository, err
}

// gitBody is a request's body with gzip undone where git compressed it, bounded by most bytes once
// undone: git compresses the request of a fetch past a kilobyte.
func gitBody(r *http.Request, most int64) (io.Reader, error) {
	body := io.Reader(r.Body)
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip", "x-gzip":
		z, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("the request says it is gzip and is not: %w", err)
		}
		body = z
	default:
		return nil, fmt.Errorf("the request is encoded as %q, and git sends gzip or nothing", r.Header.Get("Content-Encoding"))
	}
	return &bounded{r: body, left: most}, nil
}

// bounded reads at most left bytes, and refuses the byte past them.
type bounded struct {
	r    io.Reader
	left int64
}

// errTooLarge is a body past what its route reads.
var errTooLarge = errors.New("the request is larger than this route reads")

func (b *bounded) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		if n, _ := b.r.Read(one[:]); n > 0 {
			return 0, errTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// objectsOf opens the objects of a repository's live packs, as the transaction that read it listed
// them.
func (s *Server) objectsOf(repository db.Repository) (*store.Objects, error) {
	return s.packs.Open(repository)
}
