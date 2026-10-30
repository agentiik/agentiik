package api_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// Pinning a tag the installation resolves at its registry: POST .../images with resolve, which
// workflow_pin is. A registry of the test's own answers on the loopback over TLS, as example.com,
// the one name its certificate holds, and the resolver is told to reach that address alone of every
// special-use one; every other name a reference gives resolves as the lookup below says.

// The index and the image manifest the test's registry serves under a tag: an index to a client
// saying it takes one, as a daemon does, and the amd64 manifest to one that does not, so that a pin
// of the manifest's digest is a pin a runner of another architecture could not pull.
const (
	servedIndex    = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size":1024,"platform":{"architecture":"amd64","os":"linux"}}]}`
	servedManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","size":512},"layers":[]}`
)

// servedDigest is the digest a registry serves body under.
func servedDigest(body string) string { return "sha256:" + digestOf([]byte(body)) }

// registryRequest is one request the test's registry was made: its method, its path and query, and the
// credential it carried.
type registryRequest struct {
	method, target, credential string
}

// registry is the test's registry. Under library/python, Docker Hub's python, it challenges for a
// token its token service issues to nobody in particular; under acme/agk-invoice it serves anybody
// and answers a HEAD with no digest, which a GET then reads off the bytes; under private/thing it
// asks for credentials; under library/flip it challenges for a token at flip.example.com, closing
// the connection, so that the token is asked of whatever that name resolves to next; and anything
// else it does not hold. Its certificate holds example.com and every name under it.
type registry struct {
	*httptest.Server
	mu     sync.Mutex
	asked  []registryRequest
	index  string
	failed bool
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	reg := &registry{index: servedIndex}
	reg.Server = httptest.NewTLSServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.Close)
	return reg
}

func (reg *registry) host() string { return strings.TrimPrefix(reg.URL, "https://127.0.0.1") }

func (reg *registry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	reg.asked = append(reg.asked, registryRequest{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization")})
	index, failed := reg.index, reg.failed
	reg.mu.Unlock()
	port := reg.host()
	serveOne := func(body, mediaType string, digest bool) {
		w.Header().Set("Content-Type", mediaType)
		if digest {
			w.Header().Set("Docker-Content-Digest", servedDigest(body))
		}
		if r.Method == http.MethodGet {
			w.Write([]byte(body))
		}
	}
	served := func(digest bool) {
		if strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			serveOne(index, "application/vnd.oci.image.index.v1+json", digest)
			return
		}
		serveOne(servedManifest, "application/vnd.oci.image.manifest.v1+json", digest)
	}
	switch {
	case failed:
		w.WriteHeader(http.StatusServiceUnavailable)
	case r.URL.Path == "/token":
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("service") != "registry.example" || !strings.HasPrefix(r.URL.Query().Get("scope"), "repository:library/") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "anonymous"})
	case r.URL.Path == "/v2/library/python/manifests/3.13-alpine":
		if r.Header.Get("Authorization") != "Bearer anonymous" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="https://example.com%s/token",service="registry.example",scope="repository:library/python:pull"`, port))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		served(true)
	case r.URL.Path == "/v2/library/flip/manifests/1":
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="https://flip.example.com%s/token",service="registry.example"`, port))
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusUnauthorized)
	case r.URL.Path == "/v2/acme/agk-invoice/manifests/1.4.0":
		served(false)
	case r.URL.Path == "/v2/private/thing/manifests/1":
		w.Header().Set("WWW-Authenticate", `Basic realm="registry.example"`)
		w.WriteHeader(http.StatusUnauthorized)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// requests are what the registry was asked, alone, and forgets them.
func (reg *registry) requests() []registryRequest {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	out := reg.asked
	reg.asked = nil
	return out
}

// pinningAt is an installation where alice writes finance and bob reads it, holding monthly-invoicing,
// whose image tags its registry is asked about.
type pinningAt struct {
	t        *testing.T
	rt       *api.Router
	pool     *db.Pool
	registry *registry
}

// lookups are what the names the tests' references give resolve to: example.com to the registry on
// the loopback, which the resolver is let reach, and every other one to an address it may not, or to
// a public one beside it. flip.example.com answers the registry's address once, and a private address
// to every lookup after.
func lookups() func(context.Context, string) ([]netip.Addr, error) {
	var mu sync.Mutex
	flipped := false
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		addrs := map[string][]string{
			"example.com":      {"127.0.0.1"},
			"private.example":  {"10.0.0.7"},
			"metadata.example": {"169.254.169.254"},
			"mapped.example":   {"::ffff:192.168.0.3"},
			"split.example":    {"93.184.216.34", "192.168.1.9"},
			"localhost":        {"::1"},
		}[host]
		if host == "flip.example.com" {
			mu.Lock()
			addrs = []string{"127.0.0.1"}
			if flipped {
				addrs = []string{"10.0.0.9"}
			}
			flipped = true
			mu.Unlock()
		}
		if addrs == nil {
			return nil, fmt.Errorf("no such host %s", host)
		}
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func pinning(t *testing.T) *pinningAt {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	finance := api.Target{Namespace: "finance"}
	rt, err := api.NewRouter(owningGrants{granted: granted{
		"alice": {{api.WorkflowRead, finance}, {api.WorkflowWrite, finance}},
		"bob":   {{api.WorkflowRead, finance}},
	}}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	s, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: store, Objects: artifact.Dir(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	reg := newRegistry(t)
	roots := x509.NewCertPool()
	roots.AddCert(reg.Certificate())
	api.ResolvingTags(s, "example.com"+reg.host(), roots, lookups(), netip.MustParseAddr("127.0.0.1"))
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.SaveWorkflow(ctx, "monthly-invoicing", "main")
	}); err != nil {
		t.Fatal(err)
	}
	return &pinningAt{t: t, rt: rt, pool: pool, registry: reg}
}

// resolving asks the route to resolve refs as who, and answers the status and the body.
func (p *pinningAt) resolving(who string, refs ...string) (int, map[string]any) {
	p.t.Helper()
	w, answer := call(p.t, p.rt, "POST", imagesPath, who, map[string]any{"resolve": refs})
	return w.Code, answer
}

// pinned are the repository's pins, by reference.
func (p *pinningAt) pinned() map[string]string {
	p.t.Helper()
	out := map[string]string{}
	for _, pin := range imagesOf(p.t, p.rt).Pins {
		out[pin.Reference] = pin.Image
	}
	return out
}

// A writer asks, and the installation pins each tag to the digest its registry serves under it now,
// as agk push would: Docker Hub's python under library/, the reference spelt as the step spells it,
// the index's digest where the registry tags an index, asked with no credential and a token nobody
// in particular holds, and a registry answering a HEAD with no digest read off the bytes of a GET.
// The pins are recorded as a pin a client sends is, with the same entries in the log, which carry
// workflow_pin where the tool asked.
func TestAWriterPinsTheDigestATagsRegistryServes(t *testing.T) {
	p := pinning(t)
	invoice := "example.com" + p.registry.host() + "/acme/agk-invoice:1.4.0"
	_, res, rpcErr := called(t, p.rt, "alice", "tools/call", map[string]any{"name": "workflow_pin", "arguments": map[string]any{
		"namespace": "finance", "workflow": "monthly-invoicing", "images": []string{"python:3.13-alpine", invoice},
	}})
	if rpcErr != nil || res["isError"] == true {
		t.Fatalf("workflow_pin answered %v %v", res, rpcErr)
	}
	python := "python@" + servedDigest(servedIndex)
	brick := "example.com" + p.registry.host() + "/acme/agk-invoice@" + servedDigest(servedIndex)
	for _, line := range []string{"python:3.13-alpine is pinned to " + python, invoice + " is pinned to " + brick} {
		if !strings.Contains(textOf(res), line) {
			t.Errorf("workflow_pin answered %q, which does not say %q", textOf(res), line)
		}
	}
	if got := p.pinned(); got["python:3.13-alpine"] != python || got[invoice] != brick {
		t.Errorf("the repository holds the pins %v", got)
	}

	// Asked with no credential, the token included, which the registry's challenge sent it for and
	// which it presents to the registry alone.
	var tokens, heads, gets int
	for _, a := range p.registry.requests() {
		switch {
		case strings.HasPrefix(a.target, "/token"):
			tokens++
			if a.credential != "" {
				t.Errorf("the token was asked for presenting %q", a.credential)
			}
		case a.method == http.MethodHead:
			heads++
		case a.method == http.MethodGet:
			gets++
		}
		if a.credential != "" && a.credential != "Bearer anonymous" {
			t.Errorf("%s %s presented %q", a.method, a.target, a.credential)
		}
	}
	if tokens != 1 || heads != 3 || gets != 1 {
		t.Errorf("the registry was asked for %d tokens, %d HEAD and %d GET, where two tags take one token, three HEAD and one GET", tokens, heads, gets)
	}

	// Recorded as any pin is: an image.pin per pin created, by the caller, through workflow_pin.
	var pins []string
	for _, e := range audited(t, p.pool) {
		if e.Action == audit.ImagePin {
			d := detailOf(t, e)
			pins = append(pins, fmt.Sprintf("%s %s %v %v %v %v", e.Actor, e.Target, d["reference"], d["image"], d["through"], d["tool"]))
		}
	}
	slices.Sort(pins)
	want := []string{
		"alice monthly-invoicing " + invoice + " " + brick + " mcp workflow_pin",
		"alice monthly-invoicing python:3.13-alpine " + python + " mcp workflow_pin",
	}
	if !slices.Equal(pins, want) {
		t.Errorf("the log records the pins\n%s\nwant\n%s", strings.Join(pins, "\n"), strings.Join(want, "\n"))
	}

	// The tag moved: asked again, the pin moves with it, recorded with the digest it replaced; and
	// Docker Hub's python written in full is pinned in full, as agk push spells it.
	p.registry.mu.Lock()
	p.registry.index = strings.Replace(servedIndex, "1024", "2048", 1)
	p.registry.mu.Unlock()
	if status, answer := p.resolving("alice", "python:3.13-alpine", "docker.io/library/python:3.13-alpine"); status != http.StatusOK {
		t.Fatalf("resolving again answered %d %v", status, answer)
	}
	moved := servedDigest(strings.Replace(servedIndex, "1024", "2048", 1))
	if got := p.pinned(); got["python:3.13-alpine"] != "python@"+moved || got["docker.io/library/python:3.13-alpine"] != "docker.io/library/python@"+moved {
		t.Errorf("the tag moved is pinned %v", got)
	}
	found := false
	for _, e := range audited(t, p.pool) {
		d := detailOf(t, e)
		found = found || (e.Action == audit.ImagePin && d["reference"] == "python:3.13-alpine" && d["image"] == "python@"+moved && d["was"] == python)
	}
	if !found {
		t.Error("the pin moved is not recorded with the digest it replaced")
	}
}

// Resolving is what recording a pin takes, workflow:write on the workflow, and it is asked before any
// registry is: a reader is refused as nobody's workflow, through the route and through the tool, and
// the registry hears nothing of it.
func TestOnlyAWriterHasATagResolved(t *testing.T) {
	p := pinning(t)
	if status, answer := p.resolving("bob", "python:3.13-alpine"); status != http.StatusNotFound {
		t.Errorf("a reader resolving a tag is answered %d %v", status, answer)
	}
	_, res, rpcErr := called(t, p.rt, "bob", "tools/call", map[string]any{"name": "workflow_pin", "arguments": map[string]any{
		"namespace": "finance", "workflow": "monthly-invoicing", "images": []string{"python:3.13-alpine"},
	}})
	if rpcErr == nil && res["isError"] != true {
		t.Errorf("a reader's workflow_pin answered %v", res)
	}
	if asked := p.registry.requests(); len(asked) != 0 {
		t.Errorf("a reader's resolution asked the registry %v", asked)
	}
	if got := p.pinned(); len(got) != 0 {
		t.Errorf("a reader's resolution pinned %v", got)
	}
}

// "So that the control plane cannot be steered at its own network": a reference whose registry is,
// or resolves to, an address of the loopback, a private network, the link-local range or any other
// special-use one is refused before anything is dialled, a name resolving to a public address and a
// private one beside it included, and so is a token service named at one; a name answering a public
// address and then a private one reaches nothing at the second, since every connection checks the
// address it dials. Nothing is pinned, and the log records nothing.
func TestARegistryInsideTheNetworkIsNeverAsked(t *testing.T) {
	p := pinning(t)
	port := p.registry.host()
	for ref, names := range map[string]string{
		"127.0.0.2" + port + "/team/brick:1":          "127.0.0.2",
		"[::1]" + port + "/team/brick:1":              "::1",
		"localhost" + port + "/team/brick:1":          "::1",
		"private.example/team/brick:1":                "10.0.0.7",
		"metadata.example/team/brick:1":               "169.254.169.254",
		"mapped.example/team/brick:1":                 "192.168.0.3",
		"split.example/team/brick:1":                  "192.168.1.9",
		"flip.example.com" + port + "/library/flip:1": "10.0.0.9",
		"169.254.169.254/latest/meta-data:iam":        "169.254.169.254",
	} {
		status, answer := p.resolving("alice", ref)
		said, _ := answer["error"].(string)
		if status != http.StatusUnprocessableEntity || !strings.Contains(said, names) || !strings.Contains(said, "public") {
			t.Errorf("%s is answered %d %q, where it is refused naming %s", ref, status, said, names)
		}
	}
	// The registry is reached for flip.example.com's challenge alone, never its token.
	for _, a := range p.registry.requests() {
		if strings.HasPrefix(a.target, "/token") {
			t.Errorf("a token was asked of a name that resolved to a private address: %v", a)
		}
	}
	if got := p.pinned(); len(got) != 0 {
		t.Errorf("a refused resolution pinned %v", got)
	}
	for _, e := range audited(t, p.pool) {
		if e.Action == audit.ImagePin {
			t.Errorf("a refused resolution recorded %v", e)
		}
	}
}

// What a registry answers, and what the route answers it with: a tag it does not hold, and an image
// it serves to no pull without credentials, are the reference's, 422; a registry failing is the
// registry's, 502, which workflow_pin says as a refusal the caller acts on rather than as a call the
// installation failed to answer. A reference no registry reads is refused before any is asked.
func TestARegistrysRefusalIsSaid(t *testing.T) {
	p := pinning(t)
	port := p.registry.host()
	for ref, want := range map[string]struct {
		status int
		says   string
	}{
		"example.com" + port + "/library/missing:1": {http.StatusUnprocessableEntity, "serves no image under the tag 1"},
		"example.com" + port + "/private/thing:1":   {http.StatusUnprocessableEntity, "credentials"},
		"Python:3":                    {http.StatusBadRequest, "is not an image reference a registry serves"},
		"python@" + servedDigest("x"): {http.StatusBadRequest, "needs no pin"},
		"python:not a tag":            {http.StatusBadRequest, "printable ASCII"},
		"example.com/x/" + strings.Repeat("y", 300) + ":1": {http.StatusBadRequest, "is not an image reference a registry serves"},
	} {
		status, answer := p.resolving("alice", ref)
		if said, _ := answer["error"].(string); status != want.status || !strings.Contains(said, want.says) {
			t.Errorf("%s is answered %d %q, where it is %d saying %q", ref, status, said, want.status, want.says)
		}
	}
	if status, answer := p.resolving("alice", "python:3", "python:3"); status != http.StatusBadRequest {
		t.Errorf("a tag named twice is answered %d %v", status, answer)
	}
	w, answer := call(t, p.rt, "POST", imagesPath, "alice", map[string]any{"pins": map[string]string{"python:3": "python@" + servedDigest("x")}, "resolve": []string{"python:3"}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a tag pinned and resolved by one recording is answered %d %v", w.Code, answer)
	}

	p.registry.mu.Lock()
	p.registry.failed = true
	p.registry.mu.Unlock()
	_, res, rpcErr := called(t, p.rt, "alice", "tools/call", map[string]any{"name": "workflow_pin", "arguments": map[string]any{
		"namespace": "finance", "workflow": "monthly-invoicing", "images": []string{"example.com" + port + "/acme/agk-invoice:1.4.0"},
	}})
	if rpcErr != nil || res["isError"] != true || !strings.Contains(textOf(res), "answered 503") {
		t.Errorf("a registry failing is answered %v %v", res, rpcErr)
	}
	if got := p.pinned(); len(got) != 0 {
		t.Errorf("refused resolutions pinned %v", got)
	}
}

// A draft naming a tag nobody pinned is refused through MCP as image-not-pinned, saying to call
// workflow_pin rather than to read the language, since the draft is not what is wrong; once the tool
// has pinned the tag, the same draft is valid.
func TestAnUnpinnedTagThroughMCPNamesWorkflowPin(t *testing.T) {
	p := pinning(t)
	tagged := "example.com" + p.registry.host() + "/acme/agk-invoice:1.4.0"
	pinnedTo := "example.com" + p.registry.host() + "/acme/agk-invoice@" + servedDigest(servedIndex)
	if err := p.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.RecordImages(ctx, "monthly-invoicing", "alice", time.Time{}, db.Images{Manifests: map[string][]byte{pinnedTo: []byte(brickManifest)}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	validate := func() map[string]any {
		t.Helper()
		_, res, rpcErr := called(t, p.rt, "alice", "tools/call", map[string]any{"name": "workflow_validate", "arguments": map[string]any{
			"namespace": "finance", "workflow": "monthly-invoicing", "files": map[string]any{"agentiik.yaml": strings.ReplaceAll(workflowDocument, image, tagged)},
		}})
		if rpcErr != nil {
			t.Fatalf("workflow_validate answered %v", rpcErr)
		}
		return res
	}
	res := validate()
	if text := textOf(res); res["isError"] != true || !strings.Contains(text, "image-not-pinned") && !strings.Contains(text, "holds no digest") || !strings.Contains(text, "workflow_pin") || strings.Contains(text, "read workflow_language") {
		t.Errorf("a draft naming a tag nobody pinned is answered %v", res)
	}
	if _, res, rpcErr := called(t, p.rt, "alice", "tools/call", map[string]any{"name": "workflow_pin", "arguments": map[string]any{
		"namespace": "finance", "workflow": "monthly-invoicing", "images": []string{tagged},
	}}); rpcErr != nil || res["isError"] == true {
		t.Fatalf("workflow_pin answered %v %v", res, rpcErr)
	}
	if res := validate(); res["isError"] == true {
		t.Errorf("the draft, its tag pinned, is answered %v", res)
	}
}
