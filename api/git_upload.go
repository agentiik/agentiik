package api

import (
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

// A fetch: POST git-upload-pack.
//
// Stateless, as git speaks it over HTTP: each request carries the objects the client wants, each a
// ref's tip or what one reaches, and the commits it has, and says done once it has said enough.
// With multi_ack_detailed, which git asks for wherever it is offered, every have the repository
// holds is acknowledged as common, ACK and the commit and common, which is what makes the client
// say it again in each request after, since the server keeps nothing between two; a request that is
// not done ends with NAK, and one that is done with ACK and the last common commit, or NAK where
// there is none, and then the pack, on the side band where the client asked for one. Without it,
// the one acknowledgement stops a client's negotiation at the first common commit, and its request
// after that says done without the haves that found it, so that a fetch of a history longer than
// one round of haves, sixteen commits, was answered NAK and the whole history, which the client
// read as the pack and failed on. A client that does not ask for it is answered as git answers one:
// ACK and the first common commit alone.
//
// The pack is every object reachable from what is wanted and not from the common commits, the
// trees and blobs of the common commits themselves left out too, as git leaves them out: every
// object is written whole, copied from the pack that stores it with nothing inflated or compressed
// again.

// fetchRequest is what a fetch asks for.
type fetchRequest struct {
	wants []repo.ID
	haves []repo.ID
	done  bool

	// What the client said it reads, from the first want line.
	sideband, sideband64k, includeTag, noProgress, multiAck bool
}

// readFetch reads a fetch's request, refusing what the advertisement did not offer.
func readFetch(body io.Reader) (fetchRequest, error) {
	var req fetchRequest
	pkts := repo.NewPktReader(body)
	for {
		kind, data, err := pkts.Next()
		if errors.Is(err, io.EOF) {
			return req, nil
		}
		if err != nil {
			return fetchRequest{}, err
		}
		if kind != repo.PktData {
			continue
		}
		line := strings.TrimSuffix(string(data), "\n")
		word, rest, _ := strings.Cut(line, " ")
		switch word {
		case "want":
			id, caps, _ := strings.Cut(rest, " ")
			want, err := repo.ParseID(id)
			if err != nil {
				return fetchRequest{}, fmt.Errorf("a want names %.64q, which is no object: %w", id, err)
			}
			if len(req.wants) == 0 {
				for _, c := range strings.Fields(caps) {
					switch c {
					case "side-band":
						req.sideband = true
					case "side-band-64k":
						req.sideband64k = true
					case "include-tag":
						req.includeTag = true
					case "no-progress":
						req.noProgress = true
					case "multi_ack_detailed":
						req.multiAck = true
					}
				}
			}
			req.wants = append(req.wants, want)
		case "have":
			have, err := repo.ParseID(rest)
			if err != nil {
				return fetchRequest{}, fmt.Errorf("a have names %.64q, which is no object: %w", rest, err)
			}
			req.haves = append(req.haves, have)
		case "done":
			req.done = true
		case "shallow", "deepen", "deepen-since", "deepen-not":
			return fetchRequest{}, errors.New("this installation does not serve a shallow clone: clone the whole history, which a workflow's repository is small enough for")
		case "filter":
			return fetchRequest{}, errors.New("this installation does not serve a partial clone: clone every object, which a workflow's repository is small enough for")
		default:
			return fetchRequest{}, fmt.Errorf("a fetch said %.64q, which is not a line of the protocol this installation speaks", line)
		}
	}
}

func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	if !gitRequest(w, r, uploadPack) {
		return
	}
	if s.packs == nil {
		gitFail(w, http.StatusServiceUnavailable, noPacks)
		return
	}
	body, err := gitBody(r, gitRequestMaxBytes)
	if err != nil {
		gitFail(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := readFetch(body)
	if errors.Is(err, errTooLarge) {
		gitFail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a fetch says what it wants and has in at most %d bytes", gitRequestMaxBytes))
		return
	}
	if err != nil {
		gitFail(w, http.StatusBadRequest, err.Error())
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
	objects, err := s.objectsOf(repository)
	if err != nil {
		s.report(err)
		gitFail(w, http.StatusInternalServerError, "the repository could not be read")
		return
	}
	defer objects.Close()

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	// Only what the refs reach may be asked for: a ref's tip, the commit a tag peels to, or,
	// since the refs a client was shown may have moved before it asks, as git's own upload-pack
	// allows over HTTP, a commit or a tag behind them. Anything else is an object the caller may
	// have learnt of elsewhere, from another repository or another namespace, and is answered as
	// git answers it.
	offered := map[repo.ID]bool{}
	for _, ref := range repository.Refs {
		for _, id := range []string{ref.Commit, ref.Tag} {
			if parsed, err := repo.ParseID(id); err == nil {
				offered[parsed] = true
			}
		}
	}
	behind := map[repo.ID]bool{}
	for _, want := range req.wants {
		if !offered[want] {
			behind[want] = true
		}
	}
	if len(behind) > 0 {
		if err := reached(r.Context(), objects, repository, behind); err != nil {
			var not notOurs
			if errors.As(err, &not) {
				writePkts(w, fmt.Sprintf("ERR upload-pack: not our ref %s\n", not.id))
				return
			}
			s.report(fmt.Errorf("api: the refs of %s/%s: %w", over.Namespace, over.Workflow, err))
			writePkts(w, "ERR upload-pack: the repository could not be read\n")
			return
		}
	}
	if len(req.wants) == 0 {
		// A client that wants nothing says so with a flush alone, and is answered with nothing.
		return
	}

	var common []repo.ID
	acks := &pktLines{w: w}
	for _, have := range req.haves {
		held, err := objects.Has(r.Context(), have)
		if err != nil {
			s.report(err)
			writePkts(w, "ERR upload-pack: the repository could not be read\n")
			return
		}
		if !held {
			continue
		}
		common = append(common, have)
		switch {
		case req.multiAck:
			acks.add("ACK " + have.String() + " common\n")
		case len(common) == 1:
			acks.add("ACK " + have.String() + "\n")
		}
	}
	if !req.done {
		if req.multiAck || len(common) == 0 {
			acks.add("NAK\n")
		}
		acks.flush()
		return
	}
	switch {
	case len(common) == 0:
		acks.add("NAK\n")
	case req.multiAck:
		acks.add("ACK " + common[len(common)-1].String() + "\n")
	}

	sent, err := packing(r.Context(), objects, repository, req, common)
	if err != nil {
		s.report(fmt.Errorf("api: the pack of %s/%s: %w", over.Namespace, over.Workflow, err))
		writePkts(w, "ERR upload-pack: the repository could not be read\n")
		return
	}
	acks.flush()
	out := io.Writer(w)
	switch {
	case req.sideband64k:
		out = repo.NewSidebandWriter(w, repo.BandData, repo.SidebandMaxPkt)
	case req.sideband:
		out = repo.NewSidebandWriter(w, repo.BandData, repo.SidebandSmallMaxPkt)
	}
	if err := writePack(r.Context(), out, objects, sent); err != nil {
		// Headers and part of the pack are gone: the error goes on the side band, which git
		// shows, where there is one, and the pack is cut short, which git refuses, where there
		// is none.
		s.report(fmt.Errorf("api: the pack of %s/%s: %w", over.Namespace, over.Workflow, err))
		if req.sideband || req.sideband64k {
			errs := repo.NewSidebandWriter(w, repo.BandError, repo.SidebandSmallMaxPkt)
			io.WriteString(errs, "the repository could not be read\n")
		}
		return
	}
	if req.sideband || req.sideband64k {
		repo.WriteFlush(w)
	}
}

// pktLines writes lines as packets, some tens of kilobytes at a time: a fetch's haves are
// acknowledged one line each, and there may be a million of them.
type pktLines struct {
	w io.Writer
	b []byte
}

func (p *pktLines) add(line string) {
	p.b, _ = repo.AppendPkt(p.b, []byte(line))
	if len(p.b) >= 64<<10 {
		p.flush()
	}
}

func (p *pktLines) flush() {
	if len(p.b) > 0 {
		p.w.Write(p.b)
		p.b = p.b[:0]
	}
}

// notOurs is a want no ref reaches.
type notOurs struct{ id repo.ID }

func (n notOurs) Error() string { return "not our ref " + n.id.String() }

// reached answers notOurs for the first of wanted that no ref of the repository reaches, walking
// back from every ref, through tags and parents, until each is found.
func reached(ctx context.Context, objects repo.Lookup, repository db.Repository, wanted map[repo.ID]bool) error {
	left := len(wanted)
	seen := map[repo.ID]bool{}
	var queue []repo.ID
	for _, ref := range repository.Refs {
		for _, id := range []string{ref.Commit, ref.Tag} {
			if parsed, err := repo.ParseID(id); err == nil {
				queue = append(queue, parsed)
			}
		}
	}
	for len(queue) > 0 && left > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if wanted[id] {
			left--
		}
		t, data, err := repo.ReadObject(ctx, objects, id, repo.MaxParsedBytes)
		if err != nil {
			return err
		}
		switch t {
		case repo.TypeCommit:
			c, err := repo.ParseCommit(data)
			if err != nil {
				return err
			}
			queue = append(queue, c.Parents...)
		case repo.TypeTag:
			tag, err := repo.ParseTag(data)
			if err != nil {
				return err
			}
			queue = append(queue, tag.Object)
		}
	}
	for id := range wanted {
		if !seen[id] {
			return notOurs{id}
		}
	}
	return nil
}

// writePkts writes lines as packets, as one write.
func writePkts(w io.Writer, lines ...string) error {
	var b []byte
	for _, line := range lines {
		var err error
		if b, err = repo.AppendPkt(b, []byte(line)); err != nil {
			return err
		}
	}
	_, err := w.Write(b)
	return err
}

// packing answers the objects a fetch is sent, in the order they are written.
func packing(ctx context.Context, objects repo.Lookup, repository db.Repository, req fetchRequest, common []repo.ID) ([]repo.ID, error) {
	w := &walk{ctx: ctx, objects: objects, seen: map[repo.ID]bool{}, skip: map[repo.ID]bool{}}

	// What the client has: every commit behind a common one, and the trees and blobs of the
	// common commits themselves.
	for _, id := range common {
		t, err := w.typeOf(id)
		if err != nil {
			return nil, err
		}
		if t != repo.TypeCommit {
			w.skip[id] = true
			continue
		}
		c, err := w.commit(id)
		if err != nil {
			return nil, err
		}
		if err := w.markTree(c.Tree); err != nil {
			return nil, err
		}
		if err := w.markHistory(id); err != nil {
			return nil, err
		}
	}

	// What it wants: tags as themselves and peeled, then every commit behind them that it does
	// not have, each with its tree.
	var tips []repo.ID
	for _, id := range req.wants {
		peeled, err := w.peel(id)
		if err != nil {
			return nil, err
		}
		tips = append(tips, peeled)
	}
	if err := w.history(tips); err != nil {
		return nil, err
	}
	if req.includeTag {
		// An annotated tag pointing at what is sent is sent too, where the client asked: it is
		// how a fetch of a branch learns the tags on its commits.
		for _, ref := range repository.Refs {
			if ref.Tag == "" {
				continue
			}
			tag, err1 := repo.ParseID(ref.Tag)
			commit, err2 := repo.ParseID(ref.Commit)
			if err1 != nil || err2 != nil {
				continue
			}
			if w.seen[commit] || w.skip[commit] {
				if _, err := w.peel(tag); err != nil {
					return nil, err
				}
			}
		}
	}
	return w.order, nil
}

// walk collects the objects of a fetch.
type walk struct {
	ctx     context.Context
	objects repo.Lookup

	// seen are the objects to send, in order, and skip those the client has.
	seen  map[repo.ID]bool
	order []repo.ID
	skip  map[repo.ID]bool
}

func (w *walk) add(id repo.ID) bool {
	if w.seen[id] || w.skip[id] {
		return false
	}
	w.seen[id] = true
	w.order = append(w.order, id)
	return true
}

func (w *walk) typeOf(id repo.ID) (repo.Type, error) {
	o, err := w.objects.OpenObject(w.ctx, id)
	if err != nil {
		return 0, err
	}
	defer o.Close()
	return o.Type(), nil
}

func (w *walk) read(id repo.ID, want repo.Type) ([]byte, error) {
	t, data, err := repo.ReadObject(w.ctx, w.objects, id, repo.MaxParsedBytes)
	if err != nil {
		return nil, err
	}
	if t != want {
		return nil, fmt.Errorf("%s is a %s where a %s was expected", id, t, want)
	}
	return data, nil
}

func (w *walk) commit(id repo.ID) (*repo.Commit, error) {
	data, err := w.read(id, repo.TypeCommit)
	if err != nil {
		return nil, err
	}
	return repo.ParseCommit(data)
}

// peel adds a tag and whatever it names, down to a commit, and answers that commit.
func (w *walk) peel(id repo.ID) (repo.ID, error) {
	for range 64 {
		t, err := w.typeOf(id)
		if err != nil {
			return repo.ID{}, err
		}
		if t != repo.TypeTag {
			return id, nil
		}
		w.add(id)
		data, err := w.read(id, repo.TypeTag)
		if err != nil {
			return repo.ID{}, err
		}
		tag, err := repo.ParseTag(data)
		if err != nil {
			return repo.ID{}, err
		}
		id = tag.Object
	}
	return repo.ID{}, fmt.Errorf("a tag names a tag sixty-four times over")
}

// markHistory marks every commit behind id as one the client has.
func (w *walk) markHistory(id repo.ID) error {
	queue := []repo.ID{id}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if w.skip[id] {
			continue
		}
		w.skip[id] = true
		c, err := w.commit(id)
		if err != nil {
			return err
		}
		queue = append(queue, c.Parents...)
	}
	return nil
}

// markTree marks a tree and everything under it as what the client has. Walked with a stack of its
// own rather than by recursion, as tree is, so that no chain of trees is deep enough to exhaust one.
func (w *walk) markTree(id repo.ID) error {
	stack := []repo.ID{id}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if w.skip[id] {
			continue
		}
		w.skip[id] = true
		data, err := w.read(id, repo.TypeTree)
		if err != nil {
			return err
		}
		entries, err := repo.ParseTree(data)
		if err != nil {
			return err
		}
		for _, e := range entries {
			switch e.Mode {
			case repo.ModeTree:
				stack = append(stack, e.ID)
			case repo.ModeSubmodule:
			default:
				w.skip[e.ID] = true
			}
		}
	}
	return nil
}

// history adds every commit behind the tips that the client does not have, each with its tree.
func (w *walk) history(tips []repo.ID) error {
	queue := append([]repo.ID(nil), tips...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if !w.add(id) {
			continue
		}
		c, err := w.commit(id)
		if err != nil {
			return err
		}
		if err := w.tree(c.Tree); err != nil {
			return err
		}
		queue = append(queue, c.Parents...)
	}
	return nil
}

// tree adds a tree and whatever under it the client does not have. A submodule is a commit of
// another repository, which is not sent, and no version holds one.
//
// Walked with a stack of its own rather than by recursion: the hook bounds the depth of the trees a
// ref is left at, and not of those behind them, which a push can chain a million deep, and a
// recursion that deep ends the process rather than the request.
func (w *walk) tree(id repo.ID) error {
	stack := []repo.ID{id}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !w.add(id) {
			continue
		}
		data, err := w.read(id, repo.TypeTree)
		if err != nil {
			return err
		}
		entries, err := repo.ParseTree(data)
		if err != nil {
			return err
		}
		// Its files first, then its directories in the order it lists them, which is why they
		// are stacked in reverse.
		for _, e := range entries {
			if e.Mode != repo.ModeTree && e.Mode != repo.ModeSubmodule {
				w.add(e.ID)
			}
		}
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].Mode == repo.ModeTree {
				stack = append(stack, entries[i].ID)
			}
		}
	}
	return nil
}

// writePack writes the objects as a pack, each copied from the stored pack holding it.
func writePack(ctx context.Context, out io.Writer, objects *store.Objects, ids []repo.ID) error {
	pw, err := repo.NewPackWriter(out, len(ids))
	if err != nil {
		return err
	}
	for _, id := range ids {
		from, err := objects.Find(ctx, id)
		if err != nil {
			return err
		}
		if err := pw.Copy(from, id); err != nil {
			return err
		}
	}
	_, err = pw.Close()
	return err
}
