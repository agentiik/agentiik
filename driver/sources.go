package driver

import (
	"context"

	"github.com/agentiik/agentiik/artifact"
)

// Sources is what a runner knows about one task and a Config cannot hold: the store, the
// secret values and the repository tree its grant redemption answered for that task alone.
//
// A redemption is per task. It hands over "the repository tree at the commit, presigned URLs
// to the input envelopes and artifacts, the secret values and one upload policy", and every
// one of those belongs to the task whose grant was redeemed: the URLs and the policy expire
// with its deadline and are bound to its run, and the values are the ones its namespace
// declared. Config's hooks are given once per daemon and asked on every Run, concurrently, by
// namespace or by name alone, so a runner holding two redemptions at once, two tasks from two
// namespaces that both name billing, has no way through them to say which task a value is
// asked for.
//
// Sources travels on the context of one call to Run, through WithSources, rather than in the
// Task or in a wider Run. graph.Driver is the evaluator's contract and a Task "carries no
// secret value at all": either one widened would be the evaluator learning about grants,
// which is what graph/driver.go exists to prevent. A context is scoped to exactly one call,
// which is the scope of one redemption, and agk run --local, which redeems nothing, passes
// none and is answered from Config as it always was.
//
// Each field set here answers in place of the Config hook of the same name, for this task
// only, the adoption of a container an earlier delivery left behind included; a field left
// zero leaves that hook in force. A server runner therefore leaves Config.Store,
// Config.Secrets and Config.Repo nil, so that a task whose Sources leaves one out has none,
// rather than one meant for some other task.
type Sources struct {
	// Store is the store of the task's namespace, opened on its redemption: the presigned
	// URLs its inputs are read through and the upload policy its outputs are posted under.
	// It is refused where it was opened for another namespace than the task's, because an
	// artifact "never crosses a namespace boundary" and nothing else in the driver would
	// notice that one had.
	Store *artifact.Store

	// Secrets answers with the values the redemption gave this task, decoded from the
	// encoding they travelled in, since masking is a literal match against the bytes a
	// container can print.
	Secrets Secrets

	// Repo is the path of the workflow repository tree the runner laid out for this task
	// from the files its redemption names, which is bound read-only at /agk/repo.
	Repo string
}

// sourcesKey is the context key Sources travels under. It is a type of this package's own,
// so that nothing outside it can set or read the value except through WithSources.
type sourcesKey struct{}

// WithSources carries one task's sources on the context its Run is called with.
//
// A runner calls it once per task, with what that task's redemption answered, and hands the
// context to Run; a context that already carries sources has them replaced, not merged, so
// that a task never runs on part of another task's answer.
func WithSources(ctx context.Context, s Sources) context.Context {
	return context.WithValue(ctx, sourcesKey{}, s)
}

// sourcesOf is what the context carries, or the zero Sources, which leaves every hook to
// Config.
func sourcesOf(ctx context.Context) Sources {
	s, _ := ctx.Value(sourcesKey{}).(Sources)
	return s
}

// secrets is the secret source of the task Run was called for: its own where the runner gave
// it one, the daemon's otherwise.
func (d *Docker) secrets(ctx context.Context) Secrets {
	if s := sourcesOf(ctx).Secrets; s != nil {
		return s
	}
	return d.cfg.Secrets
}
