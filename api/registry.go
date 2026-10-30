package api

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/tlsfloor"
)

// Resolving an image tag at its registry, for a writer who asks: resolve, on
// POST /api/v1/{ns}/workflows/{name}/images, which workflow_pin is.
//
// "agk push resolves each tag where the image is, on the pusher's own Docker daemon", and a client of
// MCP has no daemon: it drafts a workflow naming python:3.13-alpine and is refused image-not-pinned
// with nowhere to find a digest. So the installation asks the registry the tag names for the digest
// it serves under it now, as a pull by that tag would, and records it as agk push records what it
// resolved. The hook still reaches no registry: a push is judged against what the repository holds,
// and this is one more way, explicit, of a writer recording what it holds.
//
// The registry is reached on a writer's word, at a host the reference names, so the request is held
// to what keeps a reference from steering the installation at its own network: HTTPS alone; no
// credential, since a runner pulls with none and a digest a pull with none is not served is one no
// run could start from; no proxy; every address a host resolves to checked before any is dialled,
// and the address dialled checked again as it is dialled, so that a name answering a public address
// to one lookup and a private one to the next reaches nothing inside; and bounds on the time it
// takes and the bytes it reads.

// resolveMost is how many tags one request resolves: a workflow names a handful of images, sixteen
// is more than a draft names, and each is a few requests to a registry the installation makes on
// the caller's word.
const resolveMost = 16

// The bounds of a resolution: each request to a registry dials within registryDialWait and is
// answered its headers within registryHeaderWait, and all the tags of one request are resolved
// within resolveWait, which is as long as a client of MCP is made to wait. A token is a few
// kibibytes and a manifest at most the 4 MiB the distribution specification has a registry accept.
const (
	registryDialWait   = 5 * time.Second
	registryHeaderWait = 10 * time.Second
	resolveWait        = 20 * time.Second
	tokenMost          = 64 << 10
	registryManifest   = 4 << 20
	registryRedirects  = 3
)

// resolvingMost is how many tags one process resolves at once, whoever asks: each is a connection
// to a registry the installation opens for somebody, and a writer asking as fast as they can has it
// open this many at a time rather than as many as they send. A tag past it waits for a place.
const resolvingMost = 16

// dockerHub is the host Docker Hub's registry answers at, for a reference naming no registry, or
// docker.io, as docker pull reads one.
const dockerHub = "registry-1.docker.io"

// manifestTypes are what a registry is asked for, and what it may answer: an index, which a
// multi-architecture image is tagged with and whose digest docker pull records, so that a runner
// of any architecture pulls the same digest, or one image's manifest, each in its OCI and its Docker
// spelling, as a daemon asks.
var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// The grammar of a reference the installation resolves, the distribution specification's: a
// registry's host, a repository's path of lowercase components, and a tag.
var (
	registryHost   = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*|\[[0-9a-fA-F:.]+\])(?::[0-9]{1,5})?$`)
	repositoryPath = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	imageTagName   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestServed   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// tagRefused is a tag the installation could not resolve, with the status the route answers it with:
// 400 for a reference it does not read, 422 for one its registry does not serve to a pull with no
// credentials or that names a registry it does not ask, and 502 for a registry it could not ask.
type tagRefused struct {
	status int
	why    string
}

func (e *tagRefused) Error() string { return e.why }

// notPublic is an address the guarded dial reaches nothing at, with the host that resolved to it.
type notPublic struct {
	host string
	addr netip.Addr
}

func (e *notPublic) Error() string {
	if e.host == e.addr.String() {
		return fmt.Sprintf("%s is no public address", e.host)
	}
	return fmt.Sprintf("%s resolves to %s, which is no public address", e.host, e.addr)
}

// tagResolver asks registries what their tags name.
type tagResolver struct {
	// hub is where Docker Hub is asked, dockerHub where empty; lookup resolves a host's addresses,
	// the system's resolver where nil; roots are the certificates trusted, the system's where nil;
	// and allowed are addresses of a special-use range the dial reaches all the same, none but a
	// test's own registry on the loopback.
	hub     string
	lookup  func(ctx context.Context, host string) ([]netip.Addr, error)
	roots   *x509.CertPool
	allowed []netip.Addr

	once   sync.Once
	client *http.Client
	places chan struct{}
}

// tagAt is a reference taken apart: the host its registry answers at, the repository's path there,
// the tag, and the repository as the reference spells it, which the pin is written in.
type tagAt struct {
	host, path, tag, repository string
}

// parseTag takes a reference to resolve apart, as docker pull reads it: a first component holding a
// dot or a colon, or localhost, is a registry, and anything else a path on Docker Hub, where a
// repository of one component is under library/. A reference with no tag names latest.
func (t *tagResolver) parseTag(ref string) (tagAt, error) {
	bad := func(why string) error {
		return &tagRefused{http.StatusBadRequest, fmt.Sprintf("%.64q %s", ref, why)}
	}
	if err := imageReference(ref); err != nil {
		return tagAt{}, &tagRefused{http.StatusBadRequest, err.Error()}
	}
	if strings.Contains(ref, "@") {
		return tagAt{}, bad("names its image by a digest, which needs no pin: a tag is resolved, since it moves, and a digest does not")
	}
	repository := agk.ImageRepository(ref)
	tag := strings.TrimPrefix(ref[len(repository):], ":")
	if tag == "" {
		tag = "latest"
	}
	if repository == "" || !imageTagName.MatchString(tag) {
		return tagAt{}, bad("is not an image named by a tag: a repository, then a colon and a tag of letters, digits, _, . and -, at most 128 characters")
	}
	host, path := "", repository
	if first, rest, found := strings.Cut(repository, "/"); found && (strings.ContainsAny(first, ".:") || first == "localhost" || strings.ToLower(first) != first) {
		host, path = first, rest
	}
	if host == "" || host == "docker.io" || host == "index.docker.io" {
		host = t.hub
		if host == "" {
			host = dockerHub
		}
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}
	}
	if !registryHost.MatchString(host) || !repositoryPath.MatchString(path) || len(path) > 255 {
		return tagAt{}, bad("is not an image reference a registry serves: a registry's host, a repository's path of lowercase letters, digits and separators, and a tag")
	}
	return tagAt{host: host, path: path, tag: tag, repository: repository}, nil
}

// resolve answers the reference a run names ref's image by: ref's repository, spelt as ref spells
// it, at the digest of the manifest its registry serves under the tag now, an index's for an image
// of several architectures. It is what agk push pins: the digest docker pull records for the tag,
// which a runner of any architecture pulls.
func (t *tagResolver) resolve(ctx context.Context, ref string) (string, error) {
	at, err := t.parseTag(ref)
	if err != nil {
		return "", err
	}
	t.once.Do(t.open)
	select {
	case t.places <- struct{}{}:
		defer func() { <-t.places }()
	case <-ctx.Done():
		return "", t.trouble(ref, ctx.Err())
	}
	manifest := (&url.URL{Scheme: "https", Host: at.host, Path: "/v2/" + at.path + "/manifests/" + at.tag}).String()

	// HEAD first, which Docker Hub counts against no pull limit and which answers the digest in
	// Docker-Content-Digest; a GET where it does not, the digest then being that of the bytes.
	token := ""
	for {
		res, err := t.ask(ctx, http.MethodHead, manifest, token)
		if err != nil {
			return "", t.trouble(ref, err)
		}
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusUnauthorized && token == "":
			if token, err = t.anonymousToken(ctx, ref, at, res.Header.Values("WWW-Authenticate")); err != nil {
				return "", err
			}
			continue
		case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden:
			return "", &tagRefused{http.StatusUnprocessableEntity, fmt.Sprintf("the registry of %s serves it to no pull with no credentials, which is how a runner pulls, so no run could start from it: a private image runs from v0.8.0, which redeems a namespace's secrets for the pull", ref)}
		case res.StatusCode == http.StatusNotFound:
			return "", &tagRefused{http.StatusUnprocessableEntity, fmt.Sprintf("the registry of %s serves no image under the tag %s", ref, at.tag)}
		case res.StatusCode != http.StatusOK:
			return "", &tagRefused{http.StatusBadGateway, fmt.Sprintf("the registry of %s answered %d when asked for the digest of the tag, and nothing was pinned", ref, res.StatusCode)}
		}
		if err := servedType(ref, res.Header.Get("Content-Type")); err != nil {
			return "", err
		}
		if digest := res.Header.Get("Docker-Content-Digest"); digestServed.MatchString(digest) {
			return at.repository + "@" + digest, nil
		}
		digest, err := t.digestOfBytes(ctx, ref, manifest, token)
		if err != nil {
			return "", err
		}
		return at.repository + "@" + digest, nil
	}
}

// digestOfBytes GETs the manifest and answers the SHA-256 of what it served, which is what its
// digest is.
func (t *tagResolver) digestOfBytes(ctx context.Context, ref, manifest, token string) (string, error) {
	res, err := t.ask(ctx, http.MethodGet, manifest, token)
	if err != nil {
		return "", t.trouble(ref, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", &tagRefused{http.StatusBadGateway, fmt.Sprintf("the registry of %s answered a HEAD of the tag and %d to a GET of it, and nothing was pinned", ref, res.StatusCode)}
	}
	if err := servedType(ref, res.Header.Get("Content-Type")); err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, registryManifest+1))
	if err != nil {
		return "", t.trouble(ref, err)
	}
	if len(body) > registryManifest {
		return "", &tagRefused{http.StatusUnprocessableEntity, fmt.Sprintf("the registry of %s serves a manifest past %d bytes under the tag, more than a registry is held to accept", ref, registryManifest)}
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if said := res.Header.Get("Docker-Content-Digest"); said != "" && said != digest {
		return "", &tagRefused{http.StatusBadGateway, fmt.Sprintf("the registry of %s says it served %s and served bytes whose digest is %s, and nothing was pinned", ref, said, digest)}
	}
	return digest, nil
}

// servedType refuses a manifest of a kind no runner pulls by digest, such as Docker's first schema,
// which a registry answers a client that does not say what it takes. A registry answering a HEAD
// with no type says nothing, and its digest is believed.
func servedType(ref, contentType string) error {
	if contentType == "" {
		return nil
	}
	served, _, err := mime.ParseMediaType(contentType)
	if err != nil || !slices.Contains(manifestTypes, served) {
		return &tagRefused{http.StatusUnprocessableEntity, fmt.Sprintf("the registry of %s serves it as %.100q, which is neither an image's manifest nor an index of them, and no runner pulls it", ref, contentType)}
	}
	return nil
}

// anonymousToken asks the token service a registry's challenge names for a token with no
// credential, as docker pull does for an image anybody may pull: a registry that challenges with
// anything but Bearer asks for credentials, which no runner holds.
func (t *tagResolver) anonymousToken(ctx context.Context, ref string, at tagAt, challenges []string) (string, error) {
	realm, service, scope := "", "", ""
	for _, c := range challenges {
		if params, ok := bearerParams(c); ok {
			realm, service, scope = params["realm"], params["service"], params["scope"]
			break
		}
	}
	credentials := &tagRefused{http.StatusUnprocessableEntity, fmt.Sprintf("the registry of %s asks for credentials, and a runner pulls with none, so no run could start from it: a private image runs from v0.8.0, which redeems a namespace's secrets for the pull", ref)}
	if realm == "" {
		return "", credentials
	}
	u, err := url.Parse(realm)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", &tagRefused{http.StatusBadGateway, fmt.Sprintf("the registry of %s names its token service %.100q, which is no https URL, and nothing was pinned", ref, realm)}
	}
	if scope == "" {
		scope = "repository:" + at.path + ":pull"
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()
	res, err := t.ask(ctx, http.MethodGet, u.String(), "")
	if err != nil {
		return "", t.trouble(ref, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return "", credentials
	}
	if res.StatusCode != http.StatusOK {
		return "", &tagRefused{http.StatusBadGateway, fmt.Sprintf("the token service of the registry of %s answered %d, and nothing was pinned", ref, res.StatusCode)}
	}
	var issued struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, tokenMost+1))
	if err != nil {
		return "", t.trouble(ref, err)
	}
	if len(body) > tokenMost || json.Unmarshal(body, &issued) != nil || (issued.Token == "" && issued.AccessToken == "") {
		return "", &tagRefused{http.StatusBadGateway, fmt.Sprintf("the token service of the registry of %s answered no token, and nothing was pinned", ref)}
	}
	if issued.Token != "" {
		return issued.Token, nil
	}
	return issued.AccessToken, nil
}

// bearerParams reads one challenge of a WWW-Authenticate header, its parameters by name, and false
// where its scheme is not Bearer.
func bearerParams(challenge string) (map[string]string, bool) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		name, after, found := strings.Cut(rest, "=")
		if !found {
			break
		}
		name, value := strings.ToLower(strings.TrimSpace(name)), ""
		after = strings.TrimSpace(after)
		if strings.HasPrefix(after, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(after) && after[i] != '"'; i++ {
				if after[i] == '\\' && i+1 < len(after) {
					i++
				}
				b.WriteByte(after[i])
			}
			value, rest = b.String(), after[min(i+1, len(after)):]
		} else {
			value, rest, _ = strings.Cut(after, ",")
			value = strings.TrimSpace(value)
		}
		params[name] = value
		rest = strings.TrimLeft(strings.TrimSpace(rest), ",")
	}
	return params, true
}

// ask makes one request of a registry or its token service, presenting token where there is one.
func (t *tagResolver) ask(ctx context.Context, method, target, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join(manifestTypes, ", "))
	req.Header.Set("User-Agent", "agentiik")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return t.client.Do(req)
}

// trouble is a request that could not be made or answered, as the route answers it: an address
// the dial refused is the caller's reference, and anything else the registry's or the network's.
func (t *tagResolver) trouble(ref string, err error) error {
	var np *notPublic
	if errors.As(err, &np) {
		return &tagRefused{http.StatusUnprocessableEntity, fmt.Sprintf("the registry of %s is not asked: %s, and the installation asks registries on the public internet alone, so that an image reference cannot steer it at its own network; an image of a registry inside it is pinned by agk push", ref, np)}
	}
	var refused *tagRefused
	if errors.As(err, &refused) {
		return refused
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &tagRefused{http.StatusBadGateway, fmt.Sprintf("the registry of %s did not answer within %s, and nothing was pinned", ref, resolveWait)}
	}
	return &tagRefused{http.StatusBadGateway, fmt.Sprintf("the registry of %s could not be asked over HTTPS, and nothing was pinned: %v", ref, err)}
}

// open builds the client every resolution asks through: the TLS floor, the roots given or the
// system's, no proxy, whose own address is one the dial would refuse and which a request the
// installation makes for itself has no reason to cross, a redirect followed only to https and at
// most registryRedirects times, and the guarded dial.
func (t *tagResolver) open() {
	tr := tlsfloor.Transport()
	tr.TLSClientConfig.RootCAs = t.roots
	tr.Proxy = nil
	tr.DialContext = t.dial
	tr.TLSHandshakeTimeout = registryDialWait
	tr.ResponseHeaderTimeout = registryHeaderWait
	tr.MaxResponseHeaderBytes = 32 << 10
	tr.IdleConnTimeout = 30 * time.Second
	t.client = &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > registryRedirects {
				return &tagRefused{http.StatusBadGateway, fmt.Sprintf("the registry redirected the request more than %d times, and nothing was pinned", registryRedirects)}
			}
			if req.URL.Scheme != "https" {
				return &tagRefused{http.StatusBadGateway, "the registry redirected the request to a URL that is not https, which is not followed, and nothing was pinned"}
			}
			return nil
		},
	}
	t.places = make(chan struct{}, resolvingMost)
}

// dial connects to a registry: every address its host resolves to is checked before any is dialled,
// a host with one address of a special-use range refused whole, and the addresses checked are those
// dialled, never resolved a second time; the dialer checks the address it connects to once more, so
// that nothing but a checked address is ever reached.
func (t *tagResolver) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		lookup := t.lookup
		if lookup == nil {
			lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		}
		if addrs, err = lookup(ctx, host); err != nil {
			return nil, err
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s resolves to no address", host)
	}
	for _, a := range addrs {
		if t.refused(a) {
			return nil, &notPublic{host: host, addr: a}
		}
	}
	d := &net.Dialer{Timeout: registryDialWait, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || t.refused(ap.Addr()) {
			return &notPublic{host: host, addr: ap.Addr()}
		}
		return nil
	}}
	var last error
	for _, a := range addrs {
		conn, err := d.DialContext(ctx, network, netip.AddrPortFrom(a, uint16(p)).String())
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// refused says whether the dial reaches nothing at an address: one of a special-use range, as a
// client's metadata document is never fetched from one, but those a test allows.
func (t *tagResolver) refused(a netip.Addr) bool {
	return specialUse(a) && !slices.Contains(t.allowed, a.Unmap())
}

// resolveAll resolves every reference at once, within resolveWait, and answers each one's pin, or
// the first refusal in the order the references were given, so that the same request is refused
// with the same sentence whichever registry answered first.
func (t *tagResolver) resolveAll(ctx context.Context, refs []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveWait)
	defer cancel()
	pins := make([]string, len(refs))
	errs := make([]error, len(refs))
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pins[i], errs[i] = t.resolve(ctx, ref)
		}()
	}
	wg.Wait()
	out := map[string]string{}
	for i, ref := range refs {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out[ref] = pins[i]
	}
	return out, nil
}
