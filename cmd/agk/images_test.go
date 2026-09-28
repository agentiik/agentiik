package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/dockertest"
	versions "github.com/agentiik/agentiik/version"
)

// What agk push and agk validate read of the images a workflow names, recorded on the installation
// with PUT /api/v1/{ns}/images, which a git push is judged against since it reaches no registry.

// invoiceDocument is invoiceManifest as the installation is sent it, the JSON document a manifest
// is kept as.
func invoiceDocument(t *testing.T) string {
	t.Helper()
	m, err := brick.ParseManifest([]byte(invoiceManifest))
	if err != nil {
		t.Fatal(err)
	}
	return string(m.Document())
}

// Each tag the workflow names is recorded at the digest it was pushed with, a script step's base
// image included, and the manifest of the brick by the digest it was read out of, before the
// version is pushed; the push says so.
func TestAPushRecordsWhatItReadOfTheImagesBeforeTheVersion(t *testing.T) {
	dir := taggedRepository(t)
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21":                    {Digest: alpineDigest},
	})
	code, out, errs, got, _, images := pushingAll(t, dir, http.StatusOK)
	if code != exitSucceeded || got == nil {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if images == nil {
		t.Fatal("nothing was recorded of the images")
	}
	if want := got.Images; !maps.Equal(images.Pins, want) || len(want) != 2 {
		t.Errorf("the tags were recorded as %v, and pushed as %v", images.Pins, want)
	}
	pinned := "ghcr.io/acme/agk-invoice@" + invoiceDigest
	if len(images.Manifests) != 1 || string(images.Manifests[pinned]) != invoiceDocument(t) {
		t.Errorf("the manifests were recorded for %v, want the one of %s", slicesOf(images.Manifests), pinned)
	}
	if !strings.Contains(out, "2 tags pinned and 1 brick manifest recorded in finance") {
		t.Errorf("the push does not say what it recorded: %s", out)
	}
}

// A workflow naming no tag and running no brick has nothing to record, and nothing is sent.
func TestAPushWithNothingToRecordRecordsNothing(t *testing.T) {
	dir := repository(t)
	code, out, errs, got, _, images := pushingAll(t, dir, http.StatusOK)
	if code != exitSucceeded || got == nil {
		t.Fatalf("push answered %d: %s%s", code, out, errs)
	}
	if images != nil {
		t.Errorf("a workflow of one script step by digest recorded %+v", images)
	}
}

// Images the installation did not record refuse nothing today, since the version carries its own
// digests and manifests: the version is pushed, and the push says a git push naming them will be
// refused, and why they were not recorded.
func TestAPushWhoseImagesWereNotRecordedStillPushesTheVersion(t *testing.T) {
	dir := taggedRepository(t)
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21":                    {Digest: alpineDigest},
	})
	var pushed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/images") {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"no such thing, or not yours"}`))
			return
		}
		pushed = true
		json.NewEncoder(w).Encode(api.Pushed{Images: map[string]string{}})
	}))
	t.Cleanup(server.Close)
	code, out, errs := pushAgainst(dir, server.URL)
	if code != exitSucceeded || !pushed {
		t.Fatalf("push answered %d, the version pushed: %t: %s%s", code, pushed, out, errs)
	}
	for _, want := range []string{"not recorded in finance", "a git push naming them is refused", "workflow:write"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the push does not say %q: %s", want, errs)
		}
	}
	if strings.Contains(out, "recorded in finance") {
		t.Errorf("the push says it recorded what was refused: %s", out)
	}
}

// installationRecording is an installation that takes records of images, as the one agk validate
// is configured for, and keeps each by the path it was sent to.
type installationRecording struct {
	*httptest.Server
	mu   sync.Mutex
	sent map[string]api.Images
}

func anInstallationRecording(t *testing.T) *installationRecording {
	t.Helper()
	in := &installationRecording{sent: map[string]api.Images{}}
	in.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer the-token" || r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/images") {
			t.Errorf("the installation was asked %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var images api.Images
		json.NewDecoder(r.Body).Decode(&images)
		in.mu.Lock()
		in.sent[r.URL.Path] = images
		in.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(in.Close)
	return in
}

// validatingAt runs agk validate in dir, configured for the installation at url where url is not
// empty and for none where it is.
func validatingAt(t *testing.T, dir, url string, args ...string) (int, string, string) {
	t.Helper()
	out, errs := &strings.Builder{}, &strings.Builder{}
	e := Env{
		Out: out, Err: errs, Dir: dir,
		Now: func() time.Time { return time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC) },
		Getenv: func(k string) string {
			switch {
			case url == "":
			case k == tokenVariable:
				return "the-token"
			case k == serverVariable:
				return url
			}
			return ""
		},
	}
	code := validate(t.Context(), e, args)
	return code, out.String(), errs.String()
}

// aWorkingTree is a directory holding the workflow, as agk validate reads one.
func aWorkingTree(t *testing.T, workflow string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, entryPoint), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Where an installation is configured, agk validate records what it had the daemon read: each
// brick's tag at the digest its registry serves, and the manifest read out of that digest, in the
// namespace the workflow names. A script step's base image is pulled by nothing here, and is not
// recorded.
func TestValidateRecordsWhatItReadOfTheBricksWhereAnInstallationIsConfigured(t *testing.T) {
	in := anInstallationRecording(t)
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21":                    {Digest: alpineDigest},
	})
	code, out, errs := validatingAt(t, aWorkingTree(t, taggedWorkflow), in.URL)
	if code != exitSucceeded {
		t.Fatalf("validate answered %d: %s%s", code, out, errs)
	}
	got, held := in.sent["/api/v1/finance/images"]
	if !held {
		t.Fatalf("nothing was recorded in finance: %s%s", out, errs)
	}
	pinned := "ghcr.io/acme/agk-invoice@" + invoiceDigest
	if !maps.Equal(got.Pins, map[string]string{"ghcr.io/acme/agk-invoice:1.4.0": pinned}) {
		t.Errorf("the tags were recorded as %v", got.Pins)
	}
	if len(got.Manifests) != 1 || string(got.Manifests[pinned]) != invoiceDocument(t) {
		t.Errorf("the manifests were recorded for %v", slicesOf(got.Manifests))
	}
	if want := "1 tag pinned and 1 brick manifest recorded in finance on " + in.URL + ", which its git pushes are judged against"; !strings.Contains(out, want) {
		t.Errorf("validate does not say %q: %s", want, out)
	}
}

// --namespace names where a workflow writing none records, and a workflow writing one is held to
// it, as a push is: a namespace other than the file's is refused rather than recorded in.
func TestValidateRecordsInTheNamespaceItIsGiven(t *testing.T) {
	in := anInstallationRecording(t)
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21":                    {Digest: alpineDigest},
	})
	unnamed := strings.Replace(taggedWorkflow, "metadata: { name: monthly-invoicing, namespace: finance }", "metadata: { name: monthly-invoicing }", 1)
	code, out, errs := validatingAt(t, aWorkingTree(t, unnamed), in.URL)
	if code != exitSucceeded || !strings.Contains(out, "recorded nowhere") || len(in.sent) != 0 {
		t.Errorf("a workflow naming no namespace validated with none given answered %d, recording %v: %s%s", code, in.sent, out, errs)
	}
	code, out, errs = validatingAt(t, aWorkingTree(t, unnamed), in.URL, "--namespace", "team-ops")
	if _, held := in.sent["/api/v1/team-ops/images"]; code != exitSucceeded || !held {
		t.Errorf("--namespace team-ops answered %d, recording %v: %s%s", code, in.sent, out, errs)
	}

	code, _, errs = validatingAt(t, aWorkingTree(t, taggedWorkflow), in.URL, "--namespace", "team-ops")
	if code != exitRefused || !strings.Contains(errs, "metadata-namespace-not-repository") {
		t.Errorf("a workflow of finance validated for team-ops answered %d: %s", code, errs)
	}
	if _, held := in.sent["/api/v1/finance/images"]; held {
		t.Error("a workflow refused recorded its images")
	}
}

// With no installation configured, agk validate is what it was: nothing is sent, nothing is said
// about recording. With one configured that cannot be reached, the file is still valid and validate
// says what was not recorded; an image never pushed is valid, and recorded nowhere.
func TestValidateStaysUsableWithoutAnInstallation(t *testing.T) {
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21":                    {Digest: alpineDigest},
	})
	dir := aWorkingTree(t, taggedWorkflow)
	code, out, errs := validatingAt(t, dir, "")
	if code != exitSucceeded || strings.Contains(out+errs, "recorded") || strings.Contains(out, "resolved to") {
		t.Errorf("validate with no installation answered %d: %s%s", code, out, errs)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	code, out, errs = validatingAt(t, dir, gone.URL)
	if code != exitSucceeded || !strings.Contains(errs, "not recorded in finance") {
		t.Errorf("validate with its installation out of reach answered %d: %s%s", code, out, errs)
	}

	in := anInstallationRecording(t)
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest), Unpushed: true},
		"alpine:3.21":                    {Digest: alpineDigest},
	})
	code, out, errs = validatingAt(t, dir, in.URL)
	if code != exitSucceeded || !strings.Contains(errs, "ghcr.io/acme/agk-invoice:1.4.0 is recorded nowhere") || len(in.sent) != 0 {
		t.Errorf("validate of an image never pushed answered %d, recording %v: %s%s", code, in.sent, out, errs)
	}
}

func slicesOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// What agk push records is what a git push of the same commit is judged against: against the real
// installation, the namespace's store answers the check a hook makes of the commit's tree, each tag
// at the digest agk push resolved and the brick held to the manifest it read, with no daemon and no
// registry in reach.
func TestWhatAgkPushRecordsIsWhatAGitPushIsJudgedAgainst(t *testing.T) {
	in := anInstallation(t)
	dir := taggedRepository(t)
	sha := gitIn(t, dir, "rev-parse", "HEAD")
	aDaemon(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: invoiceDigest, Manifest: []byte(invoiceManifest)},
		"alpine:3.21":                    {Digest: alpineDigest},
	})
	if code, said := in.pushFrom(t, dir); code != exitSucceeded || strings.Contains(said, "not recorded") {
		t.Fatalf("push answered %d: %s", code, said)
	}

	t.Setenv("DOCKER_HOST", "unix:///nowhere")
	checked, err := versions.Check(t.Context(), fstest.MapFS{entryPoint: &fstest.MapFile{Data: []byte(taggedWorkflow)}}, versions.Checking{
		Commit: sha, Committed: true, Namespace: "finance", Repository: "monthly-invoicing",
		Resolvers: versions.Recorded(in.pool, "finance"),
	})
	if err != nil {
		t.Fatalf("the commit agk push pushed is refused by what it recorded: %v", err)
	}
	want := map[string]string{
		"ghcr.io/acme/agk-invoice:1.4.0": "ghcr.io/acme/agk-invoice@" + invoiceDigest,
		"alpine:3.21":                    "alpine@" + alpineDigest,
	}
	if !maps.Equal(checked.Version.Images, want) {
		t.Errorf("the check pinned %v, and agk push resolved %v", checked.Version.Images, want)
	}
	if st, _ := checked.Graph.Step("normalize"); st.Image != want["ghcr.io/acme/agk-invoice:1.4.0"] {
		t.Errorf("the brick runs %s", st.Image)
	}
}
