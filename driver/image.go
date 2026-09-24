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

// ErrImageNotByDigest is a task whose image is a tag, refused under Policy.RequireDigest
// before anything is asked of the host.
var ErrImageNotByDigest = errors.New("a server runs only an image named by digest, name@sha256, which agk push records in place of every tag so that every run of a version runs the same bytes")

// pullWithoutCredentials is what a pull a registry refused says of the credentials it
// was not given. Pulling in v0.2.0 takes "none: registries every runner can reach. A 401
// fails the task on the platform's account and names v0.8.0", whose namespace secrets are
// redeemed for the pull.
const pullWithoutCredentials = "the registry refused a pull with no credentials, and a runner pulls with none until v0.8.0, when a namespace declares its private registry's credentials as namespace secrets redeemed for the pull: until then, push the image where every runner can pull it anonymously"

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
		// How long a pull that was cut short ran is kept all the same, since a
		// deadline that passed during it is an ending whose usage says so.
		return resolved{Ref: ref, PullMillis: pullMillis}, err
	}
	// A reference is a digest on a server, and "a tag is a mutable pointer and has no
	// place in something that claims a commit determines what ran". A tag is not
	// refused here but in Run, under Policy.RequireDigest, because agk run --local
	// builds images that have never been pushed anywhere, and Manifest and Pin read
	// the tags agk validate and agk push are given. What it resolved to is recorded so
	// that what ran is knowable afterwards.
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
		if ctx.Err() != nil {
			// Cut short, by a deadline or by the caller, which says nothing of the
			// image: the brick is not charged for what the clock did.
			return out, fault(t.Step, nil, ChargePlatform,
				"reading %s out of %s was cut short: %v", brick.ManifestPath, ref, err)
		}
		return out, fault(t.Step, ErrContractBroken, ChargeBrick,
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
		return out, fault(t.Step, ErrContractBroken, ChargeBrick,
			"%s of %s: %v", brick.ManifestPath, ref, err)
	}
	if declaresRootUser(m.Spec.Runtime.User) {
		// brick.ParseManifest already refuses the three spellings where a
		// manifest is read, so this is the second gate rather than the first. It
		// stays because the refusal that matters is the one before a container
		// is created, and because this one names the step and the image.
		return out, fault(t.Step, ErrRootUser, ChargeBrick,
			"%s of %s declares spec.runtime.user: %q", brick.ManifestPath, ref, m.Spec.Runtime.User)
	}

	cache.keep(digest, &m)
	out.Manifest = &m
	out.User = m.Spec.Runtime.User
	return out, nil
}

// resolve is resolveImage for one task of Run, with the pull bounded by the task's
// deadline where bounded says so and the task carries one.
//
// A task that carries only a timeout has no deadline yet: its deadline runs from the
// dispatch, which is the create, after the pull. One that carries a deadline was given it
// by whoever dispatched it, the controller or the evaluator, and a pull still running at
// that moment is work nobody can use any more: the container it would start is stopped as
// it starts, and the grant it would redeem its inputs with has expired with it. A pull
// the deadline cut short answers *pastDeadline, and so does a deadline that had already
// passed when the pull would have begun.
//
// Only a failure charged to the platform is read that way, since only the platform's side
// of the resolution waits on the daemon and can be cut short by the clock. A manifest the
// brick got wrong is the brick's refusal whenever it is read, and a deadline that fired a
// moment before it was does not make it a timeout for the step's retry to run again.
func (d *Docker) resolve(ctx context.Context, t graph.Task, bounded bool) (resolved, error) {
	if !bounded || t.Deadline.IsZero() {
		return resolveImage(ctx, d.cli, d.cache, t, "", nil)
	}
	pulling, cancel := context.WithDeadline(ctx, t.Deadline)
	defer cancel()
	image, err := resolveImage(pulling, d.cli, d.cache, t, "", nil)
	if charge, _ := Charged(err); err != nil && charge == ChargePlatform && ctx.Err() == nil && errors.Is(pulling.Err(), context.DeadlineExceeded) {
		return resolved{}, &pastDeadline{deadline: t.Deadline, ref: image.Ref, pulled: image.PullMillis, err: err}
	}
	return image, err
}

// hold is the image a reference names as the daemon holds it, pulled first where the
// daemon holds nothing under the reference, and how long that pull took, which is
// answered for a pull that failed as well.
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
	err = cli.ImagePull(ctx, ref, auth, onProgress)
	// A pull that happened is never reported as taking no time, since 0 is what the
	// usage block says of an image the host already held. One quicker than a
	// millisecond comes from a registry on the same machine, and is rounded up.
	pullMillis := max(time.Since(started).Milliseconds(), 1)
	switch {
	case err == nil:
	case docker.IsUnreachable(err):
		return docker.Image{}, pullMillis, fault(step, ErrDaemonUnreachable, ChargePlatform, "pulling %s: %v", ref, err)
	case docker.IsPullDenied(err):
		// A registry that wants credentials is refused on the platform's account,
		// since the brick never ran, and the refusal says where credentials will
		// come from rather than leaving somebody to look for a setting that is not
		// there.
		return docker.Image{}, pullMillis, fault(step, ErrImagePullFailed, ChargePlatform,
			"%v: %s", err, pullWithoutCredentials)
	default:
		return docker.Image{}, pullMillis, fault(step, ErrImagePullFailed, ChargePlatform, "%v", err)
	}

	image, err = cli.ImageInspect(ctx, ref)
	if err != nil {
		if docker.IsUnreachable(err) {
			return docker.Image{}, pullMillis, fault(step, ErrDaemonUnreachable, ChargePlatform, "inspecting %s: %v", ref, err)
		}
		return docker.Image{}, pullMillis, fault(step, ErrImagePullFailed, ChargePlatform,
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
	// The create is not cut short with ctx. A create the daemon carries out after the
	// caller gave up answers nobody, so the container's identifier is never learnt and
	// nothing removes it; a deadline bounding the resolution makes that ordinary rather
	// than rare. It is given the removal's own bound instead, and ctx is asked after it.
	creating, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
	defer cancel()
	created, err := cli.ContainerCreate(creating, "", docker.Config{
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
