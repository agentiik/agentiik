package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ImagePull pulls a reference and reads every message of the progress stream.
//
// Reading every message is the point rather than an implementation detail. The daemon
// commits to a 200 before it knows whether the pull will work, so a pull that dies
// arrives as an error object part way through a stream that has already reported half
// its layers as complete. A caller that only checked the status would take a failed pull
// for a pull that worked, and the container created afterwards would fail for a reason
// nobody could read.
//
// auth is the X-Registry-Auth header value, base64url of the registry credentials as
// JSON, and empty for an anonymous pull. Composing it is the caller's, because which
// credentials a namespace may use is a permission question and not a wire question.
//
// onProgress may be nil. Where it is not, it is called for every message, in order, on
// this goroutine.
func (c *Client) ImagePull(ctx context.Context, ref, auth string, onProgress func(Progress)) error {
	name, tag := splitReference(ref)
	q := url.Values{}
	q.Set("fromImage", name)
	if tag != "" {
		q.Set("tag", tag)
	}

	req, err := c.request(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	if auth != "" {
		req.Header.Set("X-Registry-Auth", auth)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(c.socket, http.MethodPost, "/images/create", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("pulling %s: %w", ref, refusal(resp))
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var p Progress
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("pulling %s: reading the progress stream: %w", ref, err)
		}
		if onProgress != nil {
			onProgress(p)
		}
		if message := p.failure(); message != "" {
			// The stream is abandoned here rather than drained: the pull has
			// already failed, and the daemon writes nothing after the error
			// that a caller could act on.
			return fmt.Errorf("pulling %s: %s", ref, message)
		}
	}
}

// IsPullDenied says a pull failed because the registry would not serve the image to
// whoever asked, which for a pull with no credentials is a registry that wants some.
//
// The status alone cannot say so. The daemon passes a registry's refusal on in these shapes,
// the first four of which Docker 29.8 answered for an anonymous pull of a private or missing
// repository, and the last three the containerd store's 403 and those registries' own
// refusals of a pull with no credentials:
//
//	Docker Hub         404  pull access denied for <repository>, repository does not exist or may require 'docker login'
//	ghcr.io            500  error from registry: denied
//	GitLab             403  error from registry: access forbidden
//	quay.io            500  ... unexpected status from HEAD request to https://quay.io/v2/...: 401 Unauthorized
//	a 403 elsewhere    500  ... unexpected status from HEAD request to https://...: 403 Forbidden
//	ECR                500  ... no basic auth credentials
//	Artifact Registry  500  ... denied: Unauthenticated request ...
//
// and a refusal that comes after the 200 arrives in the progress stream, with no status at
// all. So a 401 is one, and so is a refusal whose message says a registry denied access. A
// 403 alone is not: the daemon answers one of its own where an authorization plugin, or a
// proxy in front of its socket, refuses the pull, and that is not a registry wanting
// credentials. Docker Hub's sentence also covers a repository that does not exist, since a
// registry will not say to somebody with no credentials whether a private one does.
//
// Every phrase read holds a space or ends in a colon, which a repository's name cannot, so
// the name the message repeats is never read as the registry's words: an image called
// unauthorized-api whose layer was cut short is not a registry refusing anybody.
func IsPullDenied(err error) bool {
	if err == nil {
		return false
	}
	if status(err) == 401 {
		return true
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unexpected status from") &&
		(strings.Contains(message, "401 unauthorized") || strings.Contains(message, "403 forbidden")) {
		return true
	}
	for _, said := range []string{
		"pull access denied",
		"error from registry: denied",
		"access forbidden",
		"unauthorized:",
		"authentication required",
		"no basic auth credentials",
	} {
		if strings.Contains(message, said) {
			return true
		}
	}
	// A registry's DENIED error reads "denied: " and the registry's own words, at the
	// start of the message or after the daemon's "...: ". Anywhere else it is a sentence
	// that happens to end a clause with the word, "permission denied: " among them.
	for rest := message; ; {
		i := strings.Index(rest, "denied: ")
		if i < 0 {
			return false
		}
		if before := rest[:i]; before == "" || strings.HasSuffix(before, ": ") || strings.HasSuffix(before, "\n") {
			return true
		}
		rest = rest[i+len("denied: "):]
	}
}

// failure is what a progress message says went wrong, or nothing where it reports
// progress. The daemon writes the same trouble twice, once as a string and once as an
// object, and either one alone is enough to know the pull died.
func (p Progress) failure() string {
	if p.ErrorDetail != nil && p.ErrorDetail.Message != "" {
		return p.ErrorDetail.Message
	}
	return p.Error
}

// ImageInspect is the image as the daemon holds it, which is how a reference is turned
// into the digest it resolved to and how the account an image declares is read.
func (c *Client) ImageInspect(ctx context.Context, ref string) (Image, error) {
	var img Image
	if err := c.call(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &img); err != nil {
		return Image{}, err
	}
	return img, nil
}

// DistributionInspect asks the registry behind ref, through the daemon, what it serves
// under ref: the descriptor of that manifest, or the registry's refusal as the daemon
// passes it on, 404 for a manifest it does not hold and 401 or 403 for one it will not
// show to whoever asked.
//
// It is how an image built on a machine and never pushed is told apart from one a runner
// can pull, which nothing the daemon holds says: see Image.RegistryDigests.
//
// auth is the X-Registry-Auth header value, as for ImagePull, and empty asks with no
// credentials at all.
func (c *Client) DistributionInspect(ctx context.Context, ref, auth string) (Distribution, error) {
	p := "/distribution/" + ref + "/json"
	req, err := c.request(ctx, http.MethodGet, p, nil, nil)
	if err != nil {
		return Distribution{}, err
	}
	if auth != "" {
		req.Header.Set("X-Registry-Auth", auth)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Distribution{}, transportError(c.socket, http.MethodGet, p, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Distribution{}, fmt.Errorf("asking the registry for %s: %w", ref, refusal(resp))
	}
	var d Distribution
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return Distribution{}, fmt.Errorf("reading what the registry serves under %s: %w", ref, err)
	}
	return d, nil
}

// sha256Digest is a digest as an image is pinned by one: the algorithm the wire's
// imageRef names, and the whole of its hexadecimal.
var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// repositoryOf is the repository a reference names, as the reference writes it: less its
// digest, then less its tag, where only a colon after the last slash begins a tag.
func repositoryOf(ref string) string {
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}
	if colon := strings.LastIndexByte(ref, ':'); colon > strings.LastIndexByte(ref, '/') {
		ref = ref[:colon]
	}
	return ref
}

// canonical is a repository in the one spelling of it a registry has, so that two
// spellings docker pull takes for one repository compare equal: alpine, library/alpine,
// docker.io/library/alpine and index.docker.io/library/alpine are the one repository.
//
// The rule is the reference grammar's own. A first component is a registry where it
// holds a dot, a colon or a capital letter, which no repository path may hold, or is
// localhost; anything else is a path on Docker Hub, where a repository of one component
// is under library/.
func canonical(repository string) string {
	domain, path, found := strings.Cut(repository, "/")
	if !found || (!strings.ContainsAny(domain, ".:") && domain != "localhost" && strings.ToLower(domain) == domain) {
		domain, path = "docker.io", repository
	}
	if domain == "index.docker.io" {
		domain = "docker.io"
	}
	if domain == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return domain + "/" + path
}

// splitReference takes a reference apart the way POST /images/create wants it, as a name
// and a tag, where a digest travels in the tag position.
//
// A digest is separated by @ and a tag by :, and only the last colon of the last path
// segment is a tag: a registry written with a port, registry.example:5000/brick, carries
// a colon that is not one.
func splitReference(ref string) (name, tag string) {
	if name, digest, ok := strings.Cut(ref, "@"); ok {
		return name, digest
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}
