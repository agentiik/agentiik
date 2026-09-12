package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"

	"github.com/agentiik/agentiik/agk"
)

// attach copies a file under /agk/out/files/ and adds the files[] entry that references
// it.
//
//	agk attach <file> --port <port> [--item <id>|--all] [--name <name>] [--media-type <type>]
//
// The entry carries all five members, with the uri written agk://run/$AGK_RUN_ID/$AGK_STEP/
// <port>/<name>: that is exactly the address the runner will upload the bytes under, and it
// verifies the digest written here against the bytes it reads. So this states a fact and the
// runner is still the one that checks it. Nothing is uploaded from in here, and there is
// nothing to upload to: the store is unreachable from a container by design.
//
// media_type is the member a brick writing its own envelope is most likely to leave out,
// and it is what a consumer reads to know what the bytes are rather than the ending of the
// name. So it is written whatever happens: --media-type where the script knows, the type
// the name's extension is registered for otherwise, and the type that says no more than
// bytes where the name says nothing.
func attach(e env, args []string) error {
	first, rest := leading(args)
	set := flags("attach")
	named := set.String("port", "", "the output port whose item the file is attached to")
	item := set.String("item", "", "the item to attach to, implied when the port carries one")
	all := set.Bool("all", false, "attach to every item of the port")
	name := set.String("name", "", "the name the artifact is addressed by, the file's own by default")
	mediaType := set.String("media-type", "", "the media type of the bytes, guessed from the name by default")
	if err := set.Parse(rest); err != nil {
		return err
	}
	const usage = "agk attach <file> --port <port> [--item <id>|--all] [--name <name>] [--media-type <type>]"
	source, err := argument(first, set, usage)
	if err != nil {
		return err
	}
	if source == "" {
		return fmt.Errorf("no file named: %s", usage)
	}
	if *named == "" {
		return fmt.Errorf("no port named: an artifact is addressed on the port it travels on, so %s", usage)
	}
	if *item != "" && *all {
		return errors.New("--item names one item and --all names every item, so one or the other")
	}

	port := agk.Port(*named)
	if err := port.Validate(); err != nil {
		return err
	}
	t, err := readTask(e.Getenv)
	if err != nil {
		return err
	}
	if err := t.declares(port); err != nil {
		return err
	}

	path := portPath(e, port)
	envelope, existing, err := readPort(e, port)
	if err != nil {
		return err
	}
	if !existing {
		return fmt.Errorf("%s is not there yet: agk emit writes the envelope of a port and this adds a file to an item of it, so the emit comes first", path)
	}
	if err := agrees(envelope, t, port, path); err != nil {
		return err
	}

	targets, err := which(envelope, *item, *all)
	if err != nil {
		return err
	}

	file, err := attachment(e, source, *name, *mediaType, agk.URI{Run: t.Run, Step: t.Step, Port: port})
	if err != nil {
		return err
	}

	for _, i := range targets {
		envelope.Items[i].Files = with(envelope.Items[i].Files, file)
	}
	return writePort(e, port, envelope)
}

// which says the items the file is attached to.
//
// It refuses rather than guesses. A port carrying several items and no --item is the one
// case where a guess would be invisible: the envelope would be valid, the run would
// succeed, and one item of several would be carrying the report.
func which(envelope agk.Envelope, id string, all bool) ([]int, error) {
	switch {
	case id != "":
		for i, item := range envelope.Items {
			if item.ID == id {
				return []int{i}, nil
			}
		}
		return nil, fmt.Errorf("no item %s on this port, which carries %s", id, count(len(envelope.Items)))
	case all:
		if len(envelope.Items) == 0 {
			return nil, errors.New("--all attaches to every item and this port carries none: agk emit writes the items first")
		}
		items := make([]int, len(envelope.Items))
		for i := range items {
			items[i] = i
		}
		return items, nil
	case len(envelope.Items) == 1:
		return []int{0}, nil
	case len(envelope.Items) == 0:
		return nil, errors.New("this port carries no item to attach to: agk emit writes the items first")
	default:
		return nil, fmt.Errorf("this port carries %s, so the item is named: --item <id>, or --all for every one of them", count(len(envelope.Items)))
	}
}

// attachment copies the bytes under /agk/out/files/ and returns the entry that references
// them.
//
// It is not called store, and there is none in reach: what this writes is a file in a
// directory the runner reads, and the object store is unreachable from a container by design.
//
// The copy is what makes the reference true. An artifact is the bytes a container wrote
// under their own name inside /agk/out/files/, which is the one directory the runner reads
// them from, so a file left in /tmp and named in an envelope would be an entry pointing at
// nothing.
func attachment(e env, source, name, mediaType string, u agk.URI) (agk.File, error) {
	info, err := os.Stat(source)
	if err != nil {
		return agk.File{}, err
	}
	if !info.Mode().IsRegular() {
		return agk.File{}, fmt.Errorf("%s is not a file: an artifact is bytes with a size and a digest, and a directory or a device has neither", source)
	}

	if name == "" {
		name = filepath.Base(source)
	}
	u.Name = name
	if mediaType == "" {
		mediaType = guess(name)
	}

	if err := os.MkdirAll(e.outFilesDir(), 0o755); err != nil {
		return agk.File{}, err
	}
	target := filepath.Join(e.outFilesDir(), name)
	size, sum, err := place(e, source, target, info)
	if err != nil {
		return agk.File{}, err
	}

	file := agk.File{Name: name, URI: u, MediaType: mediaType, Size: size, SHA256: sum}
	if err := file.Validate(); err != nil {
		// The whole of the file entry rule, asked of agk rather than restated here,
		// since a name that is not one segment is a name that addresses one artifact
		// and mounts another.
		return agk.File{}, fmt.Errorf("the files[] entry for %s is refused: %w", source, err)
	}
	return file, nil
}

// place puts the bytes under their name and returns what they weigh and what they hash to.
//
// Three cases, and the second two are why this is not a copy and a hash. The bytes may
// already be under that name, because a script is free to write straight into
// /agk/out/files/ and then attach what it wrote, and copying a file onto itself truncates
// it. And a name already taken by other bytes is refused rather than overwritten: the
// address agk://run/<run>/<step>/<port>/<name> would then name two artifacts, and the one
// that would be lost is whichever the other item is still referencing.
func place(e env, source, target string, info os.FileInfo) (int64, string, error) {
	existing, err := os.Lstat(target)
	switch {
	case err == nil && os.SameFile(info, existing):
		size, sum, err := hash(source)
		return size, sum, err
	case err == nil:
		if !existing.Mode().IsRegular() {
			return 0, "", fmt.Errorf("%s is not a file, and an artifact is the bytes a container wrote under its own name", target)
		}
		size, sum, err := hash(source)
		if err != nil {
			return 0, "", err
		}
		_, had, err := hash(target)
		if err != nil {
			return 0, "", err
		}
		if had != sum {
			return 0, "", fmt.Errorf("%s already carries other bytes, sha256 %s, and these are %s: one name on one port addresses one artifact, so attaching these under that name would lose whichever item is referencing the others", target, had, sum)
		}
		// The same bytes under the same name is the ordinary case: two items
		// attaching one shared document, which costs nothing and is not a
		// contradiction.
		return size, sum, nil
	case !errors.Is(err, fs.ErrNotExist):
		return 0, "", err
	}

	var size int64
	var sum string
	err = writeFile(e, target, func(w *os.File) error {
		f, err := os.Open(source)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if size, err = io.Copy(io.MultiWriter(w, h), f); err != nil {
			return err
		}
		sum = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	return size, sum, nil
}

// hash weighs and digests one file, reading it once.
func hash(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(h.Sum(nil)), nil
}

// with adds the entry to an item's files, replacing the one of that name where there is
// one.
//
// Replacing and not appending, for the reason the name is refused for elsewhere: one name
// on one port is one artifact, and two entries of one name on one item would be the same
// artifact claimed twice. A second agk attach of the same name is a script correcting
// itself.
func with(files []agk.File, file agk.File) []agk.File {
	for i, had := range files {
		if had.Name == file.Name {
			files[i] = file
			return files
		}
	}
	return append(files, file)
}

// guess names the bytes from the ending of the name, which is the one thing there is to go
// on when the script did not say.
//
// Where that says nothing, the type that says no more than bytes is written rather than
// nothing at all: media_type is required of every entry, and a consumer left to guess is a
// consumer that guesses differently.
func guess(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// count writes an item count the way a sentence reads it.
func count(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}
