package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// renewed is a runner credential the fake API mints, written the way token.New writes one.
const renewed = "agkrunner_Rn3wEdQkZ3v0bq8LrT2mN5pW7sD1fG4hJ6kA9cE0uI3o"

// aKeyFile writes a host key as join writes it, and answers its private half.
func aKeyFile(t *testing.T, path string) ed25519.PrivateKey {
	t.Helper()
	k, err := newHostKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, k.private, 0o600); err != nil {
		t.Fatal(err)
	}
	return readKey(t, path)
}

func TestTheKeyIsReadAsJoinWroteItAndOneThatIsGoneIsANewRunner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.key")
	want := aKeyFile(t, path)
	got, err := LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Error("the key read is not the key written")
	}

	// Gone, or not a key: nothing the agent can do brings it back, and the host joins again.
	for name, text := range map[string]string{"not there": "", "not a key": "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", "not PEM": "garbage"} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if text != "" {
			if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := LoadKey(p)
		if !errors.Is(err, ErrKeyGone) || !strings.Contains(err.Error(), "join --replace") || !strings.Contains(err.Error(), p) {
			t.Errorf("a key %s was answered %v", name, err)
		}
	}

	// Readable by others is a host to set right, and not a new runner.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(path); err == nil || errors.Is(err, ErrKeyGone) || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a key of mode 0640 was answered %v", err)
	}
}

// heldText is /var/lib/agentiik/credential as the agent writes it.
func heldText(runner, credential string, at, by time.Time) string {
	b, _ := json.Marshal(heldFile{Runner: runner, Credential: credential,
		RotatedAt: at.UTC().Format(time.RFC3339Nano), RotateBy: by.UTC().Format(time.RFC3339Nano)})
	return string(b) + "\n"
}

func TestServePrefersTheCredentialItRenewedToAndRefusesAnotherRunners(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	c := Config{Runner: "runner-dmz-02", Credential: credential}

	// None renewed yet: the one join wrote, whose window the agent was never told.
	held, err := ReadHeld(path, c)
	if err != nil {
		t.Fatal(err)
	}
	if held.Credential != credential || !held.RotateBy.IsZero() || !held.renewAt().IsZero() {
		t.Errorf("with nothing renewed, serve holds %+v, want runner.env's credential and no window", held)
	}

	at := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
	by := at.Add(30 * 24 * time.Hour)
	if err := os.WriteFile(path, []byte(heldText("runner-dmz-02", renewed, at, by)), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err = ReadHeld(path, c)
	if err != nil {
		t.Fatal(err)
	}
	if held.Credential != renewed || !held.RotatedAt.Equal(at) || !held.RotateBy.Equal(by) {
		t.Errorf("serve holds %+v, want the renewed credential and its window", held)
	}
	if got, want := held.renewAt(), at.Add(20*24*time.Hour); !got.Equal(want) {
		t.Errorf("a thirty-day window is renewed at %s, want %s, two thirds of the way", got, want)
	}

	for name, fix := range map[string]func(){
		"another runner's": func() {
			os.WriteFile(path, []byte(heldText("runner-lan-01", renewed, at, by)), 0o600)
		},
		"readable by others": func() {
			os.WriteFile(path, []byte(heldText("runner-dmz-02", renewed, at, by)), 0o600)
			os.Chmod(path, 0o644)
		},
		"not a credential": func() {
			os.Remove(path)
			os.WriteFile(path, []byte(heldText("runner-dmz-02", "agkjoin_Xy9QkZ3v0bq8LrT2mN5pW7sD1fG4hJ6kA9cE0uI3oY2", at, by)), 0o600)
		},
		"a window that ends before it starts": func() {
			os.WriteFile(path, []byte(heldText("runner-dmz-02", renewed, by, at)), 0o600)
		},
	} {
		fix()
		if held, err := ReadHeld(path, c); err == nil || strings.Contains(err.Error(), renewed) {
			t.Errorf("a renewed credential file %s was answered %+v, %v", name, held, err)
		}
	}
}

// rotations is an API that answers the rotation route as answer says, and keeps every request.
type rotations struct {
	srv *httptest.Server

	mu       sync.Mutex
	bearers  []string
	requests []rotationRequest
}

func aRotatingAPI(t *testing.T, answer func(n int, w http.ResponseWriter)) *rotations {
	t.Helper()
	r := &rotations{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != rotatePath {
			http.NotFound(w, req)
			return
		}
		var body rotationRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.bearers = append(r.bearers, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		r.requests = append(r.requests, body)
		n := len(r.requests)
		r.mu.Unlock()
		answer(n, w)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rotations) sent() ([]string, []rotationRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bearers...), append([]rotationRequest(nil), r.requests...)
}

// renews answers a rotation with a new credential accepted until by.
func renews(by time.Time) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"credential": renewed, "rotate_by": by.UTC().Format(time.RFC3339Nano)})
	}
}

// refuses answers a rotation with status.
func refuses(status int) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": "no"})
	}
}

// aRotator is a rotator of runner-dmz-02 holding the credential join wrote, against api, keeping
// what it renews to under a directory of its own.
func aRotator(t *testing.T, url string, key ed25519.PrivateKey) *Rotator {
	t.Helper()
	c, err := NewClient(url, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRotator(c, "runner-dmz-02", key, filepath.Join(t.TempDir(), "credential"), Held{Credential: credential})
	r.Log = func(s string) { t.Log(s) }
	return r
}

// A rotation carries the credential held and a signature by the host's key over the runner and the
// time exactly as sent. What it answers is on the disk before any call carries it, and from then on
// every call does.
func TestARotationIsSignedKeptAndOnlyThenCarried(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	by := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Microsecond)
	api := aRotatingAPI(t, renews(by))
	r := aRotator(t, api.srv.URL, key)
	if err := r.Rotate(t.Context()); err != nil {
		t.Fatal(err)
	}

	bearers, requests := api.sent()
	if len(requests) != 1 || bearers[0] != credential {
		t.Fatalf("the rotation was sent %d times, the first with %q", len(requests), bearers)
	}
	sent := requests[0]
	at, err := time.Parse(time.RFC3339Nano, sent.At)
	if sent.Runner != "runner-dmz-02" || err != nil || time.Since(at).Abs() > time.Minute {
		t.Errorf("the rotation speaks for %q at %q", sent.Runner, sent.At)
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(sent.Signature)
	if err != nil || !ed25519.Verify(public, []byte("agentiik runner rotation\nrunner-dmz-02\n"+sent.At), signature) {
		t.Errorf("the signature %q is not the host key's over the runner and at as sent", sent.Signature)
	}

	if got := r.Client.Credential(); got != renewed {
		t.Errorf("the client carries %s once the credential was renewed", got)
	}
	held, err := ReadHeld(r.Path, Config{Runner: "runner-dmz-02", Credential: credential})
	if err != nil {
		t.Fatal(err)
	}
	if kept := r.Held(); held.Credential != renewed || !held.RotateBy.Equal(by) || kept.Credential != renewed || !kept.RotatedAt.Equal(held.RotatedAt) || !kept.RotateBy.Equal(by) {
		t.Errorf("the credential kept reads %+v, and the rotator holds %+v", held, r.Held())
	}
	info, err := os.Stat(r.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the credential is kept with mode %v", info.Mode())
	}
	if left, _ := os.ReadDir(filepath.Dir(r.Path)); len(left) != 1 {
		t.Errorf("renewing left %v beside the credential", left)
	}

	// Renewed again, with what it now holds and a later time.
	if err := r.Rotate(t.Context()); err != nil {
		t.Fatal(err)
	}
	bearers, requests = api.sent()
	if bearers[1] != renewed || requests[1].At <= requests[0].At {
		t.Errorf("the second rotation carried %s at %s, after one at %s", bearers[1], requests[1].At, requests[0].At)
	}
}

// A rotation whose answer never arrived is asked again with the credential still held, which the
// API goes on taking until the new one is used, and a later time, since the same request again is
// a copy the API refuses.
func TestARotationWhoseAnswerIsLostIsAskedAgainWithTheOldCredentialAndALaterTime(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	by := time.Now().Add(time.Hour)
	api := aRotatingAPI(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		renews(by)(n, w)
	})
	r := aRotator(t, api.srv.URL, key)
	if err := r.Rotate(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a rotation whose answer was lost answered %v", err)
	}
	if r.Client.Credential() != credential {
		t.Error("a rotation whose answer was lost changed the credential carried")
	}
	if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
		t.Errorf("a rotation whose answer was lost kept something: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Dir(r.Path)); len(left) != 0 {
		t.Errorf("a rotation whose answer was lost left %v", left)
	}

	if err := r.Rotate(t.Context()); err != nil {
		t.Fatal(err)
	}
	bearers, requests := api.sent()
	if bearers[1] != credential || requests[1].At <= requests[0].At {
		t.Errorf("asked again with %s at %s, after one at %s", bearers[1], requests[1].At, requests[0].At)
	}
}

// A credential that could not be kept is never carried: the agent would come back holding one the
// API no longer takes. So the API is not asked for one where there is nowhere to keep it.
func TestNoCredentialIsAskedForWhereItCannotBeKept(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	api := aRotatingAPI(t, renews(time.Now().Add(time.Hour)))
	r := aRotator(t, api.srv.URL, key)
	r.Path = filepath.Join(t.TempDir(), "gone", "credential")
	if err := r.Rotate(t.Context()); err == nil || !strings.Contains(err.Error(), r.Path) {
		t.Errorf("a rotation with nowhere to keep its answer answered %v", err)
	}
	if _, requests := api.sent(); len(requests) != 0 {
		t.Errorf("the API was asked %d times for a credential that could not be kept", len(requests))
	}
	if r.Client.Credential() != credential {
		t.Error("the credential carried changed")
	}
}

// Run renews at once a credential whose window it was never told, and then at two thirds of the
// window each answer gives.
func TestTheCredentialIsRenewedAtOnceAndThenAtTwoThirdsOfItsWindow(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	var window atomic.Int64
	window.Store(int64(3 * time.Second))
	api := aRotatingAPI(t, func(n int, w http.ResponseWriter) {
		renews(time.Now().Add(time.Duration(window.Load())))(n, w)
	})
	r := aRotator(t, api.srv.URL, key)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- r.Run(ctx) }()

	eventually(t, "the first renewal", func() bool { _, rs := api.sent(); return len(rs) == 1 })
	if since := time.Since(start); since > time.Second {
		t.Errorf("a credential whose window is not known was renewed after %s", since)
	}
	first := time.Now()
	eventually(t, "the second renewal", func() bool { _, rs := api.sent(); return len(rs) == 2 })
	if since := time.Since(first); since < 1500*time.Millisecond || since > 2800*time.Millisecond {
		t.Errorf("a credential of a three-second window was renewed %s after it arrived, want two seconds", since)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("run stopped with %v", err)
	}
}

// What a refused renewal ends in: a 401 is a credential that opens nothing, and a 403 a revoked
// runner, which stops renewing, or a key the API does not take, which is a new runner. Anything
// else is asked again.
func TestARefusedRenewalEndsAsItsAnswerSays(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	for _, row := range []struct {
		name    string
		status  int
		revoked bool
		is      error
	}{
		{"refused credential", http.StatusUnauthorized, false, ErrCredentialRefused},
		{"key not taken", http.StatusForbidden, false, ErrKeyGone},
		{"revoked", http.StatusForbidden, true, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			api := aRotatingAPI(t, refuses(row.status))
			r := aRotator(t, api.srv.URL, key)
			r.Revoked = func() bool { return row.revoked }
			err := r.Run(t.Context())
			if !errors.Is(err, row.is) || (row.is == nil) != (err == nil) {
				t.Errorf("run ended with %v, want %v", err, row.is)
			}
			if err != nil && !strings.Contains(err.Error(), "join --replace") {
				t.Errorf("run ended with %v, which does not say to join again", err)
			}
			if _, requests := api.sent(); len(requests) != 1 {
				t.Errorf("asked %d times", len(requests))
			}
		})
	}

	api := aRotatingAPI(t, refuses(http.StatusServiceUnavailable))
	r := aRotator(t, api.srv.URL, key)
	ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Errorf("a renewal the API did not answer ended run with %v", err)
	}
	if _, requests := api.sent(); len(requests) < 2 {
		t.Errorf("a renewal the API did not answer was asked %d times", len(requests))
	}
}

// A call sent with a credential that was replaced while it was on its way, and refused 401 for it,
// is sent once more with the credential now held. One refused for the credential still held is
// not, since asking again gets the same answer.
func TestACallRefusedForACredentialReplacedOnItsWayIsSentAgain(t *testing.T) {
	var c *Client
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, bearer)
		mu.Unlock()
		if bearer != renewed {
			// The new credential is used by another call meanwhile, and this one is refused.
			if r.URL.Path == "/replaced" {
				c.use(renewed)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	c, _ = NewClient(srv.URL, credential, nil)

	if err := c.Do(t.Context(), http.MethodPost, "/replaced", nil, nil); err != nil {
		t.Errorf("a call refused for the credential replaced on its way answered %v", err)
	}
	if err := c.Do(t.Context(), http.MethodPost, "/held", nil, nil); err != nil {
		t.Errorf("a call with the new credential answered %v", err)
	}
	c.use(credential)
	if err := c.Do(t.Context(), http.MethodPost, "/held", nil, nil); !errors.Is(err, ErrCredentialRefused) {
		t.Errorf("a call refused for the credential held answered %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{credential, renewed, renewed, credential}; strings.Join(seen, " ") != strings.Join(want, " ") {
		t.Errorf("the calls carried %v, want %v", seen, want)
	}
}

// Against the real API and database, with a join rotation of seconds: the agent renews its
// credential before each rotate_by, its bus credential with it, since each is cut short by the
// credential it was asked with, and takes and runs a task after the credential it joined with, and
// the one it first renewed to, have stopped being accepted.
func TestTheAgentRenewsBeforeRotateByAndKeepsWorkingPastIt(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const rotation = 4 * time.Second
	in := anInstallationRotating(t, rotation, public)
	joinedUntil := time.Now().Add(rotation)
	// The task of the step render runs until it is stopped, and every other one succeeds.
	held := string(agk.NewTaskID(in.run, "render", 1, agk.Shard{}))
	c := carrier(t, func(ctr dockertest.Container) (int, error) {
		if ctr.Labels[driver.LabelTask] != held {
			return 0, nil
		}
		select {
		case <-ctr.Signalled():
			return 143, nil
		case <-time.After(30 * time.Second):
			return 0, nil
		}
	})
	client, err := NewClient(in.url, in.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "credential")
	var log strings.Builder
	var logged sync.Mutex
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, Agent{
			Config: Config{
				API: in.url, Runner: in.runner, Pool: in.poolName, Concurrency: 1,
				WorkDir: c.root, Credential: in.credential, Labels: []string{"zone=dmz"},
			},
			Driver: c.carrier.Driver.(*driver.Docker), Client: client, Endings: c.carrier.Endings,
			Log: func(s string) { logged.Lock(); log.WriteString(s + "\n"); logged.Unlock() },
			Key: key, Held: Held{Credential: in.credential}, CredentialFile: path,
			// Heartbeats and takes shorter than the credentials, which last seconds.
			every: 500 * time.Millisecond, wait: 300 * time.Millisecond,
		})
	}()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %s", err)
		}
		logged.Lock()
		t.Log(log.String())
		logged.Unlock()
	}()

	var first Held
	eventually(t, "the first renewal", func() bool {
		held, err := ReadHeld(path, Config{Runner: in.runner, Credential: in.credential})
		first = held
		return err == nil && held.Credential != in.credential
	})
	past := first.RotateBy
	if joinedUntil.After(past) {
		past = joinedUntil
	}
	time.Sleep(time.Until(past.Add(time.Second)))
	select {
	case err := <-served:
		served <- nil
		t.Fatalf("the agent stopped once its first credentials were past their rotate_by: %v", err)
	default:
	}

	// The credential it joined with opens nothing any longer, and the agent still works.
	old, err := NewClient(in.url, in.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.BusCredentials(t.Context(), in.poolName); !errors.Is(err, ErrCredentialRefused) {
		t.Errorf("the credential the runner joined with still opens the bus: %v", err)
	}
	ended := func(task bus.TaskMessage, state string) func() bool {
		return func() bool {
			stream, err := in.js.Stream(context.Background(), bus.Results)
			if err != nil {
				return false
			}
			msg, err := stream.GetLastMsgForSubject(context.Background(), bus.ResultSubject(in.runner))
			if err != nil {
				return false
			}
			var said map[string]any
			return json.Unmarshal(msg.Data, &said) == nil && said["task_id"] == task.TaskID && said["state"] == state
		}
	}
	task := in.dispatch(t, "invoice")
	eventually(t, "the task's result, past the first credentials' rotate_by", ended(task, "succeeded"))

	// The connection the agent opened first is closed by now, its credential run out, and a stop
	// is heard on the one that replaced it.
	stopped := in.dispatch(t, "render")
	eventually(t, "the second task's container starting", func() bool {
		for _, made := range c.daemon.Created() {
			if made.Labels[driver.LabelTask] == held {
				return true
			}
		}
		return false
	})
	if err := in.control.Stop(t.Context(), graph.Stop{Task: agk.TaskID(held), Reason: graph.StopCancelled}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task stopped on the bus reported cancelled", ended(stopped, "cancelled"))
	now, err := ReadHeld(path, Config{Runner: in.runner, Credential: in.credential})
	if err != nil || !now.RotateBy.After(first.RotateBy) {
		t.Errorf("the credential kept is accepted until %s, and the first renewed to until %s: %v", now.RotateBy, first.RotateBy, err)
	}
	logged.Lock()
	renewals, busRenewals := strings.Count(log.String(), "credential was renewed"), strings.Count(log.String(), "bus credential could not be renewed")
	logged.Unlock()
	if renewals < 2 || busRenewals != 0 {
		t.Errorf("the credential was renewed %d times, and a bus credential failed to renew %d times", renewals, busRenewals)
	}
	// Each bus credential lasts no longer than the runner credential it was asked with, a few
	// seconds, and the bus the test runs on does not hold a runner to its credential's expiry,
	// so the renewals are counted where they are asked for.
	if n := in.busTokens.Load(); n < 3 {
		t.Errorf("the bus credential was asked for %d times in %s of credentials lasting %s", n, time.Since(joinedUntil.Add(-rotation)).Round(time.Second), rotation)
	}
}
