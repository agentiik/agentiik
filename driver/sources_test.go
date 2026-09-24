package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// logsByTask keeps the log of each task apart, as a runner holding two tasks at once does,
// so that a test reads what one task wrote without what the other did.
type logsByTask struct {
	mu   sync.Mutex
	logs map[agk.TaskID]*strings.Builder
}

func (l *logsByTask) OpenLog(_ context.Context, task agk.TaskID) (io.WriteCloser, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logs == nil {
		l.logs = map[agk.TaskID]*strings.Builder{}
	}
	b := &strings.Builder{}
	l.logs[task] = b
	return nopCloser{b}, nil
}

func (l *logsByTask) of(task agk.TaskID) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.logs[task]; ok {
		return b.String()
	}
	return ""
}

// countedSecrets is a task's own secret source, which says how often it was asked.
type countedSecrets struct {
	values secretSource

	mu    sync.Mutex
	asked int
}

func (c *countedSecrets) Value(ctx context.Context, name string) ([]byte, error) {
	c.mu.Lock()
	c.asked++
	c.mu.Unlock()
	return c.values.Value(ctx, name)
}

func (c *countedSecrets) times() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

// refuseConfig takes the daemon's own store, tree and secret source away, so that a task
// that was answered from them instead of from its sources fails rather than passing on
// values that happened to be right.
func refuseConfig(r *runner) {
	r.cfg.Store = func(namespace string) (*artifact.Store, error) {
		return nil, fmt.Errorf("the daemon's own store was asked for namespace %s, and the task carried its own", namespace)
	}
	r.cfg.Repo = func(context.Context, string, string, string) (string, error) {
		return "", errors.New("the daemon's own tree was asked for, and the task carried its own")
	}
	r.cfg.Secrets = secretSource{}
}

// publishedIn reads the envelope a task's terminal event names for one port back out of
// one namespace's objects, which is how a test tells which store the port was written to.
func publishedIn(t *testing.T, r *runner, task agk.TaskID, port agk.Port, objects artifact.Objects, namespace string) (agk.Envelope, error) {
	t.Helper()
	r.observed.mu.Lock()
	defer r.observed.mu.Unlock()
	for _, e := range r.observed.es {
		if e.Task != task || e.State != agk.TaskSucceeded {
			continue
		}
		for _, p := range e.Outputs {
			if p.Port == port {
				return artifact.GetEnvelope(t.Context(), objects, namespace, strings.TrimPrefix(p.Digest, "sha256:"), agk.DefaultLimits())
			}
		}
	}
	t.Fatalf("task %s was never reported succeeded with a port %s", task, port)
	return agk.Envelope{}, nil
}

// Two tasks from two namespaces, both naming billing, in flight on one driver at the same
// moment. A source asked by name alone could give them one value between them; each is
// given the value its own redemption answered, masked in its own log, and its tree and
// store are its own too.
func TestTwoTasksThatNameOneSecretAreEachGivenTheirOwnValue(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	values := map[string]string{
		"finance": "finance-bills-with-this",
		"payroll": "payroll-bills-with-that",
	}
	other := map[string]string{"finance": "payroll", "payroll": "finance"}

	// Each container waits until the other has started, so that the two tasks are
	// held, prepared and running at once rather than one after the other.
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()

	var mu sync.Mutex
	mounted := map[string]string{}
	trees := map[string]string{}
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		arrived.Done()
		namespace := c.Labels[LabelNamespace]

		m, ok := c.Mount(SecretsDir + "/billing")
		if !ok {
			return 1, errors.New("nothing is mounted at /agk/secrets/billing")
		}
		b, err := os.ReadFile(m.Source)
		if err != nil {
			return 1, err
		}
		tree, _ := c.Mount(RepoDir)
		mu.Lock()
		mounted[namespace] = string(b)
		trees[namespace] = tree.Source
		mu.Unlock()

		select {
		case <-both:
		case <-time.After(10 * time.Second):
			return 1, errors.New("the other task never started, so the two were never in flight at once")
		}
		fmt.Fprintf(c.Stderr, "billing with %s\n", b)
		fmt.Fprintf(c.Stderr, "the other namespace bills with %s\n", values[other[namespace]])
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"namespace": namespace}))
	})
	refuseConfig(r)
	logs := &logsByTask{}
	r.cfg.Logs = logs

	type tenant struct {
		task    graph.Task
		objects artifact.Objects
		repo    string
		sources Sources
	}
	tenants := map[string]*tenant{}
	for namespace, run := range map[string]agk.RunID{"finance": "01JMZ8V1P9C4", "payroll": "01JMZ8V1P9D5"} {
		task := oneTask(ref)
		task.ID = agk.NewTaskID(run, "fetch", 1, agk.Shard{})
		task.Run = run
		task.Namespace = namespace
		task.Workflow = namespace + "/monthly-invoicing@a3f9c1e"
		task.Secrets = []graph.SecretMount{{Name: "billing"}}

		objects := artifact.Dir(t.TempDir())
		store, err := artifact.New(objects, namespace, agk.DefaultLimits())
		if err != nil {
			t.Fatalf("opening the store of %s: %s", namespace, err)
		}
		repo := t.TempDir()
		tenants[namespace] = &tenant{
			task:    task,
			objects: objects,
			repo:    repo,
			sources: Sources{Store: store, Secrets: secretSource{"billing": values[namespace]}, Repo: repo},
		}
	}

	var wg sync.WaitGroup
	results := map[string]graph.Result{}
	failures := map[string]error{}
	for namespace, tn := range tenants {
		wg.Go(func() {
			result, err := r.Run(WithSources(t.Context(), tn.sources), tn.task)
			mu.Lock()
			defer mu.Unlock()
			results[namespace], failures[namespace] = result, err
		})
	}
	wg.Wait()

	for namespace, tn := range tenants {
		if err := failures[namespace]; err != nil {
			t.Fatalf("the task of %s: %s", namespace, err)
		}
		if got := results[namespace].State; got != agk.TaskSucceeded {
			t.Fatalf("the task of %s ended %s, and its container exited 0", namespace, got)
		}

		own, theirs := values[namespace], values[other[namespace]]
		if mounted[namespace] != own {
			t.Errorf("the task of %s read %q at /agk/secrets/billing, and its redemption gave it %q", namespace, mounted[namespace], own)
		}
		if trees[namespace] != tn.repo {
			t.Errorf("the task of %s was given the tree at %q, and its runner laid it out at %q", namespace, trees[namespace], tn.repo)
		}

		log := logs.of(tn.task.ID)
		if strings.Contains(log, own) {
			t.Errorf("the log of %s carries its own value in the clear: %q", namespace, log)
		}
		if !strings.Contains(log, "billing with "+maskToken) {
			t.Errorf("the log of %s does not read %q: %q", namespace, "billing with "+maskToken, log)
		}
		// The other task's value was never this task's, so this task's masker has
		// nothing to match it against. It stays as the container printed it, which is
		// what shows the masker holds the values of its own task and of no other.
		if !strings.Contains(log, "the other namespace bills with "+theirs) {
			t.Errorf("the log of %s masked a value this task was never given: %q", namespace, log)
		}

		e, err := publishedIn(t, r, tn.task.ID, "out", tn.objects, namespace)
		if err != nil {
			t.Errorf("the port of %s is not in the store its redemption opened: %s", namespace, err)
		} else if len(e.Items) != 1 || e.Items[0].Data["namespace"] != namespace {
			t.Errorf("the store of %s holds %+v for its port", namespace, e.Items)
		}
		if _, err := publishedIn(t, r, tn.task.ID, "out", tenants[other[namespace]].objects, namespace); err == nil {
			t.Errorf("the port of %s was written to the store of %s", namespace, other[namespace])
		}
	}
}

// A redelivery that adopts a container an earlier delivery left behind masks its log with
// the values it redeemed itself. The earlier delivery's copy left with the process that
// held it, and a restarted runner holds its own redemption and no other.
func TestAnAdoptedContainerIsMaskedWithTheValuesOfTheDeliveryThatAdoptsIt(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		fmt.Fprintln(c.Stderr, "authorising with s3cr3t-value")
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": 1}))
	})
	logs := &logsByTask{}
	r.cfg.Logs = logs

	task := taskWithASecret(ref)
	exitedFirstDelivery(t, r, task)

	// The first delivery is gone and so is everything it was answered from.
	refuseConfig(r)
	objects := artifact.Dir(t.TempDir())
	store, err := artifact.New(objects, task.Namespace, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}
	secrets := &countedSecrets{values: secretSource{"bearer": "s3cr3t-value"}}

	result, err := r.Run(WithSources(t.Context(), Sources{Store: store, Secrets: secrets}), task)
	if err != nil {
		t.Fatalf("the redelivery: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the redelivery reports %s, and the container exited 0", result.State)
	}
	if secrets.times() == 0 {
		t.Error("the redelivery never asked its own sources for the value it masks with")
	}

	log := logs.of(task.ID)
	if strings.Contains(log, "s3cr3t-value") {
		t.Errorf("the log carries the secret value: %q", log)
	}
	if !strings.Contains(log, "authorising with "+maskToken) {
		t.Errorf("the log reads %q, and it is what the container wrote, masked", log)
	}
	if _, err := publishedIn(t, r, task.ID, "out", objects, task.Namespace); err != nil {
		t.Errorf("the adopted container's port is not in the store the redelivery was given: %s", err)
	}
}

// agk run --local redeems nothing and gives no sources, and it runs as it always did: the
// store, the tree and the values are the ones Config answers with.
func TestWithNoSourcesTheConfigAnswersAsBefore(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var value, tree string
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		m, _ := c.Mount(SecretsDir + "/bearer")
		b, err := os.ReadFile(m.Source)
		if err != nil {
			return 1, err
		}
		value = string(b)
		repo, _ := c.Mount(RepoDir)
		tree = repo.Source
		return 0, wrote(c, "out", agk.NewItem(map[string]any{"n": 1}))
	})
	repo := t.TempDir()
	r.cfg.Repo = func(context.Context, string, string, string) (string, error) { return repo, nil }

	result, err := r.Run(t.Context(), taskWithASecret(ref))
	if err != nil {
		t.Fatalf("running with no sources: %s", err)
	}
	if result.State != agk.TaskSucceeded || len(result.Outputs["out"].Items) != 1 {
		t.Errorf("the task reports %s with %+v", result.State, result.Outputs)
	}
	if value != "s3cr3t-value" {
		t.Errorf("the container read %q, and Config's source answers %q", value, "s3cr3t-value")
	}
	if tree != repo {
		t.Errorf("the tree mounted is %q, and Config's hook answers %q", tree, repo)
	}
}

// Each source is its own override. A task that carries its own secrets and nothing else
// takes its store and its tree from Config, and its values from its sources and not from
// Config.
func TestASourceLeftOutLeavesTheConfigHookInForce(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	var value, tree string
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		m, _ := c.Mount(SecretsDir + "/bearer")
		b, err := os.ReadFile(m.Source)
		if err != nil {
			return 1, err
		}
		value = string(b)
		repo, _ := c.Mount(RepoDir)
		tree = repo.Source
		return 0, nil
	})
	repo := t.TempDir()
	r.cfg.Repo = func(context.Context, string, string, string) (string, error) { return repo, nil }

	ctx := WithSources(t.Context(), Sources{Secrets: secretSource{"bearer": "this-task-only"}})
	if _, err := r.Run(ctx, taskWithASecret(ref)); err != nil {
		t.Fatalf("running with one source given: %s", err)
	}
	if value != "this-task-only" {
		t.Errorf("the container read %q, and the task's own source answers %q", value, "this-task-only")
	}
	if tree != repo {
		t.Errorf("the tree mounted is %q, and with none given the Config hook answers %q", tree, repo)
	}
}

// A store is opened for one namespace, and one handed to a task of another is refused
// before anything is created: its outputs would be written under the other namespace's
// prefix, and an artifact never crosses that boundary.
func TestAStoreOpenedForAnotherNamespaceIsRefused(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })

	store, err := artifact.New(artifact.Dir(t.TempDir()), "payroll", agk.DefaultLimits())
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}
	_, err = r.Run(WithSources(t.Context(), Sources{Store: store}), oneTask(ref))
	if !errors.Is(err, ErrContractBroken) {
		t.Fatalf("a finance task given the payroll store answered %v", err)
	}
	if !strings.Contains(err.Error(), "payroll") || !strings.Contains(err.Error(), "finance") {
		t.Errorf("the refusal does not name both namespaces: %s", err)
	}
	if n := len(r.daemon.Created()); n != 0 {
		t.Errorf("%d containers were created for a task given another namespace's store", n)
	}
}

// Sources travel with one context. One that carries none leaves every hook to Config, and a
// second WithSources replaces the first whole, so a task never runs on part of another
// task's answer.
func TestSourcesAreReplacedWholeAndNeverMerged(t *testing.T) {
	if s := sourcesOf(t.Context()); s.Store != nil || s.Secrets != nil || s.Repo != "" {
		t.Errorf("a context nobody gave sources carries %+v", s)
	}

	first := WithSources(t.Context(), Sources{Secrets: secretSource{"billing": "first"}, Repo: "/work/first"})
	second := WithSources(first, Sources{Repo: "/work/second"})
	got := sourcesOf(second)
	if got.Repo != "/work/second" {
		t.Errorf("the tree is %q after the second sources were given", got.Repo)
	}
	if got.Secrets != nil {
		t.Errorf("the second sources gave no secrets and the first's are still there: %+v", got.Secrets)
	}
}
