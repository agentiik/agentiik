package driver

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// ErrRootUser is the non-root rule, refused here before a container is created rather
// than only where manifests are read.
//
// "The container user is non-root, required by the manifest and checked at publication",
// and spec.runtime.user is one of the five keys a manifest must declare precisely
// because "a manifest that leaves the account to the image gives publication nothing to
// check: root, 0 and 0:0 are refused. The group half is not read, so nonroot:0 is
// accepted."
var ErrRootUser = errors.New("the container user is non-root, required by the manifest and checked at publication: root, 0 and 0:0 are refused, because a read-only root filesystem and dropped capabilities are worth little to a process running as uid 0")

// ErrNotPushed is an image no registry serves under the digest this machine holds it by,
// which Pin refuses. A pull with no credentials is this release's: a namespace's own
// registry credentials, and the pulls they open, arrive with v0.8.0.
var ErrNotPushed = errors.New("a server run names every image by the digest its registry serves it under, and a runner pulls it from there with no credentials")

// ErrImagePullFailed is a pull that died. It is charged to the platform and never to the
// brick: the image was not reached, so nothing in it can have failed.
var ErrImagePullFailed = errors.New("the image could not be pulled, which is the runner's failure and not the brick's")

// labelManifestRead marks the throwaway container a manifest is read through.
//
// It is deliberately not the task label. Adoption looks for a container carrying
// dev.agentiik.task and takes the one it finds as the task's own, and a reader container
// wearing that label would be adopted as the container that never ran.
const labelManifestRead = "dev.agentiik.manifest"

// resolved is what a step's image turned out to be: the digest it resolved to, the
// account it runs as, and its manifest where it carries one.
//
// Manifest is nil for an image that carries no /agk/brick.yaml, which is not an error: a
// script step runs in a base image, and "running a script instead of a brick" is a
// keyword of the language rather than an accident.
type resolved struct {
	Ref      string
	Digest   string
	User     string
	Manifest *brick.Manifest

	// PullMillis is how long the pull took, which is the one part of the usage
	// block that is not measured inside the container.
	PullMillis int64
}

// manifests is the cache: read on first pull, kept by digest.
//
// The key is the image's own digest rather than the reference, because the manifest is a
// property of the image: two references resolving to one image are one manifest, and a
// tag that moved is a different digest and therefore a different entry. An image known
// to carry no manifest is remembered as well, so that a base image used by every script
// step of a run is opened once and not once per task.
type manifests struct {
	mu     sync.Mutex
	byID   map[string]brick.Manifest
	absent map[string]bool
}

// newManifests is the cache one Docker holds for as long as it lives.
func newManifests() *manifests {
	return &manifests{byID: map[string]brick.Manifest{}, absent: map[string]bool{}}
}

// lookup answers with what is cached for a digest: the manifest, whether the image is
// known to carry none, and whether anything is known at all.
func (c *manifests) lookup(digest string) (m brick.Manifest, none, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.absent[digest] {
		return brick.Manifest{}, true, true
	}
	m, known = c.byID[digest]
	return m, false, known
}

// keep records what an image turned out to carry.
func (c *manifests) keep(digest string, m *brick.Manifest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m == nil {
		c.absent[digest] = true
		return
	}
	c.byID[digest] = *m
}

// resolveImage resolves a step's image to a digest, pulling it where the daemon does not
// already have it, reads its manifest on the first pull and refuses one that declares a
// root user.
//
// The order is the order the rules are in. An image the daemon already holds is not
// pulled, which is what docker run itself does and what agk run --local depends on: a
// brick built on the machine and never pushed anywhere has no registry to be fetched
// from, and a pull of it is answered with "pull access denied" by a registry that has
// never heard of it. The inspect turns the reference into the digest the cache is keyed
// by; the manifest is read only when that digest is new; and the account is refused
// before anything is created, so that a manifest declaring root never becomes a
// container.
func resolveImage(ctx context.Context, cli *docker.Client, cache *manifests, t graph.Task, auth string, onProgress func(docker.Progress)) (resolved, error) {
	ref := strings.TrimSpace(t.Image)
	if ref == "" {
		return resolved{}, fault(t.Step, ErrContractBroken, ChargeBrick,
			"the step names no image, and a step carries an image or a call and never both")
	}

	image, pullMillis, err := hold(ctx, cli, t.Step, ref, auth, onProgress)
	if err != nil {
		return resolved{}, err
	}
	// A reference is a digest in production, and "a tag is a mutable pointer and has
	// no place in something that claims a commit determines what ran". A tag is not
	// refused here, because agk run --local builds images that have never been
	// pushed anywhere, but what it resolved to is recorded so that what ran is
	// knowable afterwards.
	digest := image.ID

	out := resolved{Ref: ref, Digest: digest, User: image.Config.User, PullMillis: pullMillis}

	if m, none, known := cache.lookup(digest); known {
		if none {
			return out, nil
		}
		out.Manifest = &m
		out.User = m.Spec.Runtime.User
		return out, nil
	}

	document, err := readManifest(ctx, cli, ref, digest)
	if err != nil {
		return resolved{}, fault(t.Step, ErrContractBroken, ChargeBrick,
			"reading %s out of %s: %v", brick.ManifestPath, ref, err)
	}
	if document == nil {
		// An image carrying no manifest is a base image, which is what a script
		// step runs in. The account stays the image's own.
		cache.keep(digest, nil)
		return out, nil
	}

	m, err := brick.ParseManifest(document)
	if err != nil {
		return resolved{}, fault(t.Step, ErrContractBroken, ChargeBrick,
			"%s of %s: %v", brick.ManifestPath, ref, err)
	}
	if declaresRootUser(m.Spec.Runtime.User) {
		// brick.ParseManifest already refuses the three spellings where a
		// manifest is read, so this is the second gate rather than the first. It
		// stays because the refusal that matters is the one before a container
		// is created, and because this one names the step and the image.
		return resolved{}, fault(t.Step, ErrRootUser, ChargeBrick,
			"%s of %s declares spec.runtime.user: %q", brick.ManifestPath, ref, m.Spec.Runtime.User)
	}

	cache.keep(digest, &m)
	out.Manifest = &m
	out.User = m.Spec.Runtime.User
	return out, nil
}

// hold is the image a reference names as the daemon holds it, pulled first where the
// daemon holds nothing under the reference, and how long that pull took.
func hold(ctx context.Context, cli *docker.Client, step agk.Step, ref, auth string, onProgress func(docker.Progress)) (docker.Image, int64, error) {
	image, err := cli.ImageInspect(ctx, ref)
	if err == nil {
		return image, 0, nil
	}
	if docker.IsUnreachable(err) {
		return docker.Image{}, 0, fault(step, ErrDaemonUnreachable, ChargePlatform, "inspecting %s: %v", ref, err)
	}
	if !docker.IsNotFound(err) {
		return docker.Image{}, 0, fault(step, ErrImagePullFailed, ChargePlatform,
			"%s could not be inspected: %v", ref, err)
	}

	started := time.Now()
	if err := cli.ImagePull(ctx, ref, auth, onProgress); err != nil {
		if docker.IsUnreachable(err) {
			return docker.Image{}, 0, fault(step, ErrDaemonUnreachable, ChargePlatform, "pulling %s: %v", ref, err)
		}
		return docker.Image{}, 0, fault(step, ErrImagePullFailed, ChargePlatform, "%v", err)
	}
	pullMillis := time.Since(started).Milliseconds()

	image, err = cli.ImageInspect(ctx, ref)
	if err != nil {
		if docker.IsUnreachable(err) {
			return docker.Image{}, 0, fault(step, ErrDaemonUnreachable, ChargePlatform, "inspecting %s: %v", ref, err)
		}
		return docker.Image{}, 0, fault(step, ErrImagePullFailed, ChargePlatform,
			"%s was pulled and then could not be inspected: %v", ref, err)
	}
	return image, pullMillis, nil
}

// readManifest reads /agk/brick.yaml out of an image, and answers with nothing at all
// where the image carries none.
//
// It goes through a container created from the image and never started, because that is
// the only way the Engine API offers to read a path out of an image: there is no
// endpoint that opens a file in a layer. The container is removed on every path,
// including the one where the file is not there, so that reading a manifest leaves
// nothing behind.
func readManifest(ctx context.Context, cli *docker.Client, ref, digest string) ([]byte, error) {
	created, err := cli.ContainerCreate(ctx, "", docker.Config{
		Image: ref,
		// A create is refused where neither the image nor the request names a
		// command. This container is never started, so the command exists to
		// satisfy the create and is never run.
		Cmd:    []string{brick.ManifestPath},
		Labels: map[string]string{labelManifestRead: digest},
	}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		return nil, err
	}
	defer func() {
		// The context may already be done, which is exactly when the removal
		// matters most, so it is issued on a context of its own.
		removal, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		cli.ContainerRemove(removal, created.ID, true)
	}()

	archive, err := cli.ContainerArchive(ctx, created.ID, brick.ManifestPath)
	if err != nil {
		if docker.IsNotFound(err) {
			// The image carries no manifest. That is a base image, and a base
			// image is what a script step runs in.
			return nil, nil
		}
		return nil, err
	}
	defer archive.Close()

	return oneFile(archive)
}

// oneFile reads the single regular file out of the tar an archive answers with.
func oneFile(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive carried no file")
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		return io.ReadAll(tr)
	}
}

// declaresRootUser says whether an account is one of the three spellings of root.
//
// "The group half is not read, so nonroot:0 is accepted": the rule is about the process
// the container runs as, and a group of zero grants nothing on its own.
func declaresRootUser(user string) bool {
	account, _, _ := strings.Cut(user, ":")
	switch strings.TrimSpace(account) {
	case "root", "0":
		return true
	default:
		return false
	}
}

// Manifest reads the brick manifest of one image, which is what agk validate needs to
// check a step's ports against the brick it runs and what a caller needs before it can
// call graph.Build at all.
//
// It is a method of this package because this package is the only one in the module that
// may reach a Docker daemon, and because the work is already here: resolve the reference,
// pull only what the daemon does not hold, read /agk/brick.yaml out of the image, refuse a
// manifest declaring root, and keep what was read in the same digest-keyed cache a Run
// would have filled. The manifest agk validate read is therefore the manifest the run that
// follows it uses, and neither pulls twice.
//
// The step is an argument because every error of this package names one: a *Fault naming no
// step would be a refusal a person has to locate themselves, and the caller always knows
// which step asked.
//
// There is no absent answer. graph.Images names the image of a non-script step, and such a
// step is held to its manifest, so an image with no /agk/brick.yaml is a contract break
// here rather than the base image a script step legitimately runs in.
func (d *Docker) Manifest(ctx context.Context, step agk.Step, image string) (brick.Manifest, error) {
	r, err := resolveImage(ctx, d.cli, d.cache, graph.Task{Step: step, Image: image}, "", nil)
	if err != nil {
		return brick.Manifest{}, err
	}
	if r.Manifest == nil {
		return brick.Manifest{}, fault(step, ErrContractBroken, ChargeBrick,
			"%s carries no %s: an image becomes a brick by carrying one, and a step that is not a script step is held to the ports and the parameters its manifest declares", r.Ref, brick.ManifestPath)
	}
	return *r.Manifest, nil
}

// Pin answers the reference a server run names image by: image's repository, spelt as
// image spells it, at the digest of the manifest its registry serves, which is the
// digest this daemon holds the image under there. The image is pulled first where the
// daemon holds nothing under the reference, as it is for Manifest.
//
// It is how agk push resolves a tag, because "a tag is a mutable pointer, and a commit
// must determine what ran": the version records the digest once, and every run of it
// runs the same bytes. A reference that already names a digest is recorded as it is
// written, and agk push asks nothing about it.
//
// The registry is asked whether it serves that digest, because nothing on this machine
// can say: the containerd store holds an image built here and never pushed under a
// digest of its registry exactly as it holds one it pulled, as
// docker.Image.RegistryDigests explains. It is asked with no credentials because a runner
// pulls with none, so a digest the registry will not serve to such a pull is one no run
// of the version could start from. Either way the image is refused with ErrNotPushed,
// naming it. A registry that could not be asked at all is the platform's trouble rather
// than the image's.
func (d *Docker) Pin(ctx context.Context, step agk.Step, image string) (string, error) {
	ref := strings.TrimSpace(image)
	held, _, err := hold(ctx, d.cli, step, ref, "", nil)
	if err != nil {
		return "", err
	}
	candidates := held.RegistryDigests(ref)
	if len(candidates) == 0 {
		return "", fault(step, ErrNotPushed, ChargeBrick,
			"%s is held on this machine under no digest of its registry, which is an image built here and never pushed: push it to a registry every runner can reach, then push the workflow", ref)
	}
	var refused error
	for _, pinned := range candidates {
		_, digest, _ := strings.Cut(pinned, "@")
		served, err := d.cli.DistributionInspect(ctx, pinned, "")
		switch {
		case err == nil && served.Descriptor.Digest == digest:
			return pinned, nil
		case err == nil:
			refused = fmt.Errorf("asked for %s, the registry answers %s", digest, served.Descriptor.Digest)
		case docker.IsUnreachable(err):
			return "", fault(step, ErrDaemonUnreachable, ChargePlatform, "asking the registry of %s about %s: %v", ref, pinned, err)
		case docker.IsNotFound(err), docker.IsDenied(err):
			refused = err
		default:
			return "", fault(step, nil, ChargePlatform,
				"the registry of %s could not be asked whether it serves %s, and a version is not recorded under a digest nobody could confirm: %v", ref, pinned, err)
		}
	}
	return "", fault(step, ErrNotPushed, ChargeBrick,
		"%s is held on this machine as %s, which its registry does not serve to a pull with no credentials (%v), so it was built here and never pushed, or pushed where a runner cannot pull it: push it to a registry every runner can reach, then push the workflow", ref, candidates[0], refused)
}
