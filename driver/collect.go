package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"mime"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/graph"
)

// filesDir and portsDir are the two directories under /agk/out, on the host side of the
// mount. brick names them inside the container as OutFilesDir and OutPortsDir; what is
// joined here is the working directory the driver prepared.
const (
	filesDir = "files"
	portsDir = "ports"
)

// collection is what the collection of one container is given.
//
// The Task is the whole of the per-task argument, exactly as the evaluator handed it
// out, and nothing here is looked up: Task.Outputs is already the declared argument
// brick.Collect takes. What is beside it is what only this side knows, which is where
// the container wrote, when the port is being published, what it said on standard
// output, what it exited with, and the values to mask.
type collection struct {
	Task graph.Task

	// Dir is the host side of /agk/out.
	Dir string

	// ProducedAt is the moment the ports are published, which is the one member of
	// the metadata the container does not decide: a port is published when the
	// emitting step ends, and that is a moment only the runner is in a position to
	// know.
	ProducedAt time.Time

	Limits agk.Limits

	// Stdout is what the container wrote on standard output, as capture gives it
	// back.
	Stdout []byte

	// Code is what the container exited with. The shorthand is a rule about a script
	// that wrote nothing and exited 0, so the code is part of the question.
	Code int

	// Mask is the last gate before the store. A script's captured standard output
	// becomes the payload of an item, so a secret echoed there would be stored in the
	// clear, and the rule that covers the log covers this too.
	Mask *masker
}

// meta is what the runner knows about the batch and the container does not decide.
//
// The port is left out, because the port is the name of the file the envelope was found
// in, which is the reading brick.Collect takes of its own argument.
func (c collection) meta() agk.Meta {
	return agk.Meta{
		RunID:      c.Task.Run,
		Step:       c.Task.Step,
		Attempt:    c.Task.Attempt,
		ProducedAt: c.ProducedAt,
	}
}

// collected is what one container produced, read back off its working directory.
type collected struct {
	// Outputs is one envelope per declared port, which is what brick.Collect
	// returns, empty envelopes included.
	Outputs map[agk.Port]agk.Envelope

	// Artifacts are the artifacts this task put in the store, which is what the
	// observer carries and graph.Result has nowhere to put.
	Artifacts []agk.File
}

// collect reads back what one container produced: one envelope per declared port, the
// files it left under /agk/out/files/ uploaded and referenced, the standard output
// shorthand where it applies, and every value above inline_max_bytes spilled.
//
// brick.Collect reads the ports, because collecting envelopes is the brick contract and
// not the driver's, and what is here is the half brick deliberately leaves out: an
// artifact is uploaded and referenced by whatever holds the store, and the standard
// output shorthand belongs here too, since only the caller that started the container
// captured the stream.
//
// Nothing is written until every envelope has been held to inline_max_bytes,
// envelope_max_bytes and max_items, which the size rules ask of a runner before its first
// upload. The rules are measured on the document that will be published, whose files[]
// entries are the ones the store will answer, so the collection is assembled against a
// store that answers each write and keeps it back, and the writes are made only once every
// port has passed. A refused envelope leaves the store as it found it: an artifact written
// for an envelope that is never published is bytes nothing addresses.
//
// An error charged to nobody is the brick's, and conclude charges it so. A write the store
// then refuses, or a store that cannot be reached, is a *Fault charged to the platform,
// since the brick did what it was asked.
func collect(ctx context.Context, s *artifact.Store, c collection) (collected, error) {
	// Both are this side's, and a brick charged for either would fail for a runner that
	// was not given what it needed.
	if s == nil {
		return collected{}, fault(c.Task.Step, nil, ChargePlatform, "no artifact store: what a container leaves under %s is uploaded to one", brick.OutFilesDir)
	}
	if c.Dir == "" {
		return collected{}, fault(c.Task.Step, nil, ChargePlatform, "no working directory to collect the outputs from")
	}

	m := c.meta()
	out, err := brick.Collect(c.Dir, c.Task.Outputs, m, c.Limits)
	if err != nil {
		return collected{}, err
	}

	// Here and not lower down, because everything below this line is on its way to the
	// store: a value over inline_max_bytes is spilled to an artifact, so a secret masked
	// after the spill would reach the store as bytes, and an envelope is published once it
	// has been validated. This is the last moment at which what a brick wrote is still
	// only what it wrote. The shorthand is masked where it is assembled, a few lines
	// below, and every other payload is masked here.
	for port, e := range out {
		e.Items = maskItems(c.Mask, e.Items)
		out[port] = e
	}

	w := &withheld{store: s}
	files := filepath.Join(c.Dir, filesDir)
	// One description per port and name, because two items of one envelope attaching
	// the same file is ordinary, a fan-in of a shared document being the usual case, and
	// reading the bytes twice would be the price of it.
	uploaded := make(map[string]agk.File)
	// Sorted, so that a container that broke the contract on two ports at once is
	// refused by the same one every time it is collected, which is the reading
	// brick.Collect already takes of its own directory.
	for _, port := range slices.Sorted(maps.Keys(out)) {
		e := out[port]
		if err := c.attach(ctx, w, files, &e, uploaded); err != nil {
			return collected{}, err
		}
		out[port] = e
	}

	// The shorthand itself is stated in script.go, beside the keywords that produce
	// it. What is asked here is the same question it asks, and only so that the files
	// a script left are uploaded when there is an item to attach them to: a file
	// nothing references is bytes no envelope addresses, and putting those in the
	// store would fill it with artifacts nothing can ask for.
	if _, declared := out[shorthandPort]; declared && isScript(c.Task) && c.Code == 0 && wroteNoItem(out) {
		loose, err := c.publish(ctx, w, files)
		if err != nil {
			return collected{}, err
		}
		out = stdoutShorthand(c.Task, c.Code, out, c.Mask.mask(c.Stdout), loose)
	}

	for _, port := range slices.Sorted(maps.Keys(out)) {
		// Spill is the other half of the size threshold: a script's captured
		// standard output and a field a brick left inline are both values this side
		// assembled, and the engine never publishes an envelope its own rule would
		// reject.
		e, err := brick.Spill(ctx, w, out[port], c.Limits)
		if err != nil {
			return collected{}, err
		}
		// Validated after the spill and not before it, because what travels is what
		// this side assembled rather than what the container wrote: the file entries
		// were rewritten to the artifacts that will hold them, and the size rules are
		// measured on the document that will be published.
		if err := e.Validate(c.Limits); err != nil {
			return collected{}, err
		}
		out[port] = e
	}

	if err := w.commit(ctx, c.Task.Step); err != nil {
		return collected{}, err
	}
	return collected{Outputs: out, Artifacts: produced(out, m)}, nil
}

// attach describes the files one envelope references and rewrites each entry to the
// artifact that will hold it, which commit writes once every envelope has passed.
//
// A files[] entry whose name is not under /agk/out/files/ is left exactly as it is. It
// is already a reference, and an item carried through from an input keeps the URI of the
// step that produced it: rewriting that to this step's address would claim somebody
// else's artifact, and uploading it again would be fetching bytes the store already has.
//
// A name that is under files/ carrying bytes other than the ones the envelope says it
// carries is refused. The envelope is a document about bytes, and a consumer verifies the
// digest when it reads the artifact back, so a contradiction left alone would fail a
// downstream step with nothing to say where it came from.
func (c collection) attach(ctx context.Context, w *withheld, files string, e *agk.Envelope, uploaded map[string]agk.File) error {
	step, port := e.Meta.Step, e.Meta.Port
	for i, item := range e.Items {
		for j, f := range item.Files {
			where := filepath.Join(files, f.Name)
			info, err := os.Lstat(where)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("driver: step %s: port %s: item %s: %w", step, port, item.ID, err)
			}
			if !info.Mode().IsRegular() {
				// The reading brick already takes of the ports directory: what
				// is read here is read on the runner's side of the boundary, and
				// a link would have the collection read a file the container
				// could not reach itself.
				return fmt.Errorf("driver: step %s: port %s: item %s: %s/%s is not a file, and an artifact is the bytes a container wrote under its own name: %w", step, port, item.ID, brick.OutFilesDir, f.Name, agk.ErrEnvelopeRejected)
			}

			key := string(port) + "/" + f.Name
			got, ok := uploaded[key]
			if !ok {
				if got, err = w.putFile(ctx, where, agk.URI{Run: e.Meta.RunID, Step: step, Port: port, Name: f.Name}, f.MediaType); err != nil {
					return fmt.Errorf("driver: step %s: port %s: item %s: %w", step, port, item.ID, err)
				}
				uploaded[key] = got
			}
			if got.SHA256 != f.SHA256 {
				return fmt.Errorf("driver: step %s: port %s: item %s: %s/%s carries sha256 %s and the envelope says the file called %q carries %s: what an envelope names is what the container wrote, and a consumer verifies the digest when it reads it: %w", step, port, item.ID, brick.OutFilesDir, f.Name, got.SHA256, f.Name, f.SHA256, agk.ErrEnvelopeRejected)
			}
			e.Items[i].Files[j] = got
		}
	}
	return nil
}

// publish describes every file left under /agk/out/files/ for the shorthand item to carry.
//
// They are addressed on the port the shorthand publishes, because that is the port they
// will travel on: agk://run/<run>/<step>/<port>/<name> names one artifact of one port,
// and an artifact addressed on a port no item references could not be fetched by anyone.
func (c collection) publish(ctx context.Context, w *withheld, files string) ([]agk.File, error) {
	names, err := loose(files, c.Task.Step)
	if err != nil {
		return nil, err
	}
	out := make([]agk.File, 0, len(names))
	for _, name := range names {
		f, err := w.putFile(ctx, filepath.Join(files, name), agk.URI{Run: c.Task.Run, Step: c.Task.Step, Port: shorthandPort, Name: name}, typeOf(name))
		if err != nil {
			return nil, fmt.Errorf("driver: step %s: port %s: %w", c.Task.Step, shorthandPort, err)
		}
		out = append(out, f)
	}
	return out, nil
}

// loose names every file left under /agk/out/files/, in order.
//
// A missing directory is not a refusal: a container that never created it has done no
// less than one that left it empty. Anything under it that is not a file is refused
// rather than skipped, because /agk/out/files/ holds artifacts, an artifact is addressed
// by one segment of a URI, and a directory left there addresses nothing.
func loose(files string, step agk.Step) ([]string, error) {
	entries, err := os.ReadDir(files)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("driver: step %s: %w", step, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("driver: step %s: %s holds %s, where one file is one artifact addressed by its name: %w", step, brick.OutFilesDir, entry.Name(), agk.ErrEnvelopeRejected)
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// withheld is the store as the collection sees it until every envelope of the task has
// passed the size rules: each write is answered with the files[] entry the store would
// answer for it, and kept back until commit makes it.
//
// What is kept is where the bytes are and not the bytes, wherever that is possible. A file
// the container left is read again from its working directory, which outlives the
// collection, since an artifact runs to artifact_max_bytes and holding one in memory would
// be the runner's memory spent on a brick's output. A value the spill moved exists
// nowhere else, and it is small: it came out of an envelope, which envelope_max_bytes
// bounds.
type withheld struct {
	store  *artifact.Store
	writes []withheldWrite
}

// withheldWrite is one write described and not yet made: the entry the store answered, and
// either the file the container left or the bytes the spill moved.
type withheldWrite struct {
	file agk.File
	path string
	body []byte
}

// Put answers a value the spill moved, which is how brick.Spill reaches this: it takes a
// brick.Putter, and this is the one that writes nothing yet.
func (w *withheld) Put(ctx context.Context, u agk.URI, mediaType string, r io.Reader) (agk.File, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return agk.File{}, fmt.Errorf("artifact %s: %w", u, err)
	}
	f, err := w.store.Describe(ctx, u, mediaType, bytes.NewReader(body))
	if err != nil {
		return agk.File{}, err
	}
	w.writes = append(w.writes, withheldWrite{file: f, body: body})
	return f, nil
}

// putFile answers one file the container left, under the URI its port and its name give it.
func (w *withheld) putFile(ctx context.Context, where string, u agk.URI, mediaType string) (agk.File, error) {
	src, err := os.Open(where)
	if err != nil {
		return agk.File{}, err
	}
	defer src.Close()
	f, err := w.store.Describe(ctx, u, mediaType, src)
	if err != nil {
		return agk.File{}, err
	}
	w.writes = append(w.writes, withheldWrite{file: f, path: where})
	return f, nil
}

// errArtifactChanged is a file that no longer holds the bytes it was described by when it
// came to be written.
var errArtifactChanged = errors.New("an artifact is written as the bytes its envelope names, and these are not those bytes")

// commit makes the writes that were kept back, in the order they were described, once each
// envelope that names them has passed.
//
// One write per digest, because the store keeps one object per digest: the same bytes
// attached on two ports are two entries and one object, and a runner whose store cannot
// say what it already holds would otherwise send them twice.
//
// A write that does not go through is the platform's. The brick left what it was asked to
// leave, and a store that refused it, an upload policy that expired or a store that could
// not be reached says nothing about the brick.
func (w *withheld) commit(ctx context.Context, step agk.Step) error {
	written := make(map[string]bool, len(w.writes))
	for _, write := range w.writes {
		if written[write.file.SHA256] {
			continue
		}
		got, err := w.write(ctx, write)
		if err != nil {
			f := fault(step, err, ChargePlatform, "artifact %s could not be written to the store, and an envelope names an artifact only once the store holds it", write.file.URI)
			f.Port = write.file.URI.Port
			return f
		}
		if got.SHA256 != write.file.SHA256 || got.Size != write.file.Size {
			f := fault(step, errArtifactChanged, ChargePlatform, "artifact %s was described as %d bytes with sha256 %s and read back as %d bytes with sha256 %s", write.file.URI, write.file.Size, write.file.SHA256, got.Size, got.SHA256)
			f.Port = write.file.URI.Port
			return f
		}
		written[write.file.SHA256] = true
	}
	return nil
}

// write makes one write, reading the bytes from wherever they were kept.
func (w *withheld) write(ctx context.Context, write withheldWrite) (agk.File, error) {
	if write.path == "" {
		return w.store.Put(ctx, write.file.URI, write.file.MediaType, bytes.NewReader(write.body))
	}
	src, err := os.Open(write.path)
	if err != nil {
		return agk.File{}, err
	}
	defer src.Close()
	return w.store.Put(ctx, write.file.URI, write.file.MediaType, src)
}

// publishPorts writes the envelope of every port to the store, in port order, and names
// each as a result names it: by the digest the store answered and by its count.
//
// After the collection and not inside it, because what is written is the envelope the
// collection validated, and because a store that refused it is this side's trouble and not
// the brick's: the brick did what it was asked, and it is charged to the platform.
func publishPorts(ctx context.Context, s *artifact.Store, step agk.Step, out map[agk.Port]agk.Envelope) ([]EndedPort, error) {
	ports := make([]EndedPort, 0, len(out))
	for _, port := range slices.Sorted(maps.Keys(out)) {
		e := out[port]
		digest, _, err := s.PutEnvelope(ctx, e)
		if err != nil {
			f := fault(step, err, ChargePlatform, "the envelope could not be written to the store, and a result names a port only by what the store holds")
			f.Port = port
			return nil, f
		}
		ports = append(ports, EndedPort{Port: port, Digest: "sha256:" + digest, Items: e.Meta.Count})
	}
	return ports, nil
}

// typeOf guesses the media type of a file the shorthand attaches, which nothing declared.
//
// An envelope requires media_type on every file entry, and the store fills the gap with
// the type that says no more than bytes. A name ending .json or .csv says more than that
// for nothing, and a consumer left to guess is a consumer that guesses differently.
func typeOf(name string) string {
	return mime.TypeByExtension(path.Ext(name))
}

// produced is every artifact this task put in the store, read off what is published
// rather than counted as it was written, so that the fields the spill moved are in it
// too.
//
// An artifact is this task's when its URI names this run and this step. A file carried
// through from an input names the step that produced it and is somebody else's.
func produced(out map[agk.Port]agk.Envelope, m agk.Meta) []agk.File {
	var files []agk.File
	seen := make(map[string]bool)
	for _, port := range slices.Sorted(maps.Keys(out)) {
		for _, item := range out[port].Items {
			for _, f := range item.Files {
				if f.URI.Run != m.RunID || f.URI.Step != m.Step {
					continue
				}
				if uri := f.URI.String(); !seen[uri] {
					seen[uri] = true
					files = append(files, f)
				}
			}
		}
	}
	return files
}

// capture holds what a container wrote on standard output, which is the shorthand's
// payload and nothing the contract collects otherwise.
//
// It is bounded, because a container printing without stopping would otherwise be a way
// to exhaust the runner's memory rather than its own. What is kept is the beginning
// rather than the end: the shorthand is the output of a script that ran to completion,
// and where there is too much of it, what the script said first is what says why.
type capture struct {
	mu      sync.Mutex
	buf     []byte
	limit   int64
	dropped int64
	mask    *masker
}

// newCapture opens the capture of standard output.
//
// The limit the caller gives is envelope_max_bytes, since what is captured becomes one
// field of one item of one envelope: above inline_max_bytes it spills to an artifact, and
// beyond the size of the envelope itself there is nowhere for it to go. A limit of zero
// or less is a cap deliberately turned off, which is the reading agk takes of every limit
// that is not positive.
func newCapture(limit int64, m *masker) *capture {
	return &capture{limit: limit, mask: m}
}

// Write takes what was read off standard output. It never fails and never stops short:
// the reader on the other side is draining the container's socket, and a short write
// there would stall the container rather than the capture.
func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := int64(len(p))
	if c.limit > 0 {
		room = min(room, max(c.limit-int64(len(c.buf)), 0))
	}
	c.buf = append(c.buf, p[:room]...)
	c.dropped += int64(len(p)) - room
	return len(p), nil
}

// Bytes is what was captured, masked.
//
// Where the cap cut the capture short, the last bytes go with it: the cut fell wherever
// the limit landed, which may be inside a value, and what goes with the rest is
// everything that could still have been the beginning of one. A value the cut split
// therefore cannot leave half of itself in the item this becomes.
func (c *capture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buf
	if c.dropped > 0 {
		b = b[:len(b)-min(c.mask.hold(), len(b))]
	}
	return c.mask.mask(b)
}

// Truncated says whether the cap cut the capture short.
func (c *capture) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped > 0
}
