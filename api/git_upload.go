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
// Stateless, as git speaks it over HTTP: each request carries the objects the client wants, which
// have to be what the repository advertises, and the commits it has, and says done once it has
// said enough. Without multi_ack, which is not advertised, the answer to a request that is not done
// is one line, ACK and the first commit it has that the repository holds too, or NAK, and git stops
// asking once it is told of one; the answer to a request that is done is the same line and then the
// pack, on the side band where the client asked for one.
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
	sideband, sideband64k, includeTag, noProgress bool
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

	// Only what the advertisement named may be asked for: a ref's tip, or the commit a tag peels
	// to. Anything else is an object the caller may have learnt of elsewhere, from another
	// repository or another namespace, and is answered as git answers it.
	offered := map[repo.ID]bool{}
	for _, ref := range repository.Refs {
		for _, id := range []string{ref.Commit, ref.Tag} {
			if parsed, err := repo.ParseID(id); err == nil {
				offered[parsed] = true
			}
		}
	}
	for _, want := range req.wants {
		if !offered[want] {
			writePkts(w, fmt.Sprintf("ERR upload-pack: not our ref %s\n", want))
			return
		}
	}
	if len(req.wants) == 0 {
		// A client that wants nothing says so with a flush alone, and is answered with nothing.
		return
	}

	var common []repo.ID
	for _, have := range req.haves {
		held, err := objects.Has(r.Context(), have)
		if err != nil {
			s.report(err)
			writePkts(w, "ERR upload-pack: the repository could not be read\n")
			return
		}
		if held {
			common = append(common, have)
		}
	}
	ack := "NAK\n"
	if len(common) > 0 {
		ack = "ACK " + common[0].String() + "\n"
	}
	if !req.done {
		writePkts(w, ack)
		return
	}

	sent, err := packing(r.Context(), objects, repository, req, common)
	if err != nil {
		s.report(fmt.Errorf("api: the pack of %s/%s: %w", over.Namespace, over.Workflow, err))
		writePkts(w, "ERR upload-pack: the repository could not be read\n")
		return
	}
	writePkts(w, ack)
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

// markTree marks a tree and everything under it as what the client has.
func (w *walk) markTree(id repo.ID) error {
	if w.skip[id] {
		return nil
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
			if err := w.markTree(e.ID); err != nil {
				return err
			}
		case repo.ModeSubmodule:
		default:
			w.skip[e.ID] = true
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
func (w *walk) tree(id repo.ID) error {
	if !w.add(id) {
		return nil
	}
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
			if err := w.tree(e.ID); err != nil {
				return err
			}
		case repo.ModeSubmodule:
		default:
			w.add(e.ID)
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
