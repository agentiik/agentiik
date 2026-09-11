package agk_test

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

func TestTheDefaultsAreTheDocumentedValues(t *testing.T) {
	l := agk.DefaultLimits()
	cases := []struct {
		rule      string
		got, want int64
	}{
		{agk.RuleInlineMaxBytes, l.InlineMaxBytes, 262144},
		{agk.RuleEnvelopeMaxBytes, l.EnvelopeMaxBytes, 4194304},
		{agk.RuleMaxItems, int64(l.MaxItems), 100000},
		{agk.RuleArtifactMaxBytes, l.ArtifactMaxBytes, 5368709120},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s is %d, and the documentation says %d", c.rule, c.got, c.want)
		}
	}
}

// TestTheSizeRulesAndTheirTwoOutcomes is the rule that matters most about the four
// limits: two of these refusals discard a document and one fails the step that emitted
// it, and telling them apart is what decides whether a run retries.
func TestTheSizeRulesAndTheirTwoOutcomes(t *testing.T) {
	heavy := strings.Repeat("x", 300000)

	items := func(n int) []agk.Item {
		out := make([]agk.Item, n)
		for i := range out {
			out[i] = agk.Item{ID: "i", Data: map[string]any{}, Files: []agk.File{}}
		}
		return out
	}
	envelope := func(items []agk.Item) agk.Envelope {
		return agk.Envelope{
			Meta:  agk.Meta{RunID: "01JMZ8W4K2R7Q0E3N5T9", Step: "normalize", Port: "ok", Attempt: 1, Count: len(items), ProducedAt: at},
			Items: items,
		}
	}

	cases := []struct {
		name     string
		envelope agk.Envelope
		limits   agk.Limits
		rule     string
		outcome  agk.Outcome
		sentinel error
		names    string
	}{
		{
			name:     "a value left inline above inline_max_bytes",
			envelope: envelope([]agk.Item{{ID: "i1", Data: map[string]any{"report": heavy}, Files: []agk.File{}}}),
			limits:   agk.DefaultLimits(),
			rule:     agk.RuleInlineMaxBytes,
			outcome:  agk.Reject,
			sentinel: agk.ErrEnvelopeRejected,
			names:    "items[0].data.report",
		},
		{
			name:     "a serialised envelope above envelope_max_bytes",
			envelope: envelope(items(40)),
			limits:   agk.Limits{EnvelopeMaxBytes: 200},
			rule:     agk.RuleEnvelopeMaxBytes,
			outcome:  agk.Fail,
			sentinel: agk.ErrStepFailed,
			names:    "the serialised envelope",
		},
		{
			name:     "more items than max_items",
			envelope: envelope(items(3)),
			limits:   agk.Limits{MaxItems: 2},
			rule:     agk.RuleMaxItems,
			outcome:  agk.Fail,
			sentinel: agk.ErrStepFailed,
			names:    "3 items, above the 2",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.envelope.Validate(c.limits)
			if err == nil {
				t.Fatal("accepted")
			}
			var r *agk.Refusal
			if !errors.As(err, &r) {
				t.Fatalf("the refusal is not one: %v", err)
			}
			if r.Rule != c.rule {
				t.Errorf("refused by %s, want %s", r.Rule, c.rule)
			}
			if r.Outcome != c.outcome {
				t.Errorf("the outcome is %v, want %v", r.Outcome, c.outcome)
			}
			if !errors.Is(err, c.sentinel) {
				t.Errorf("the refusal does not unwrap to %v: %v", c.sentinel, err)
			}
			if r.Step != "normalize" || r.Port != "ok" {
				t.Errorf("the refusal names step %q and port %q", r.Step, r.Port)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("the refusal does not say %q: %v", c.names, err)
			}
			if !strings.Contains(err.Error(), c.rule) {
				t.Errorf("the refusal does not name the rule: %v", err)
			}
		})
	}
}

// TestTheInlineRuleNamesTheValueToSpill: a step told which value is too heavy knows
// what to write as an artifact.
func TestTheInlineRuleNamesTheValueToSpill(t *testing.T) {
	e := agk.Envelope{
		Meta: agk.Meta{RunID: "r", Step: "render", Port: "out", Attempt: 1, Count: 1, ProducedAt: at},
		Items: []agk.Item{{
			ID: "i1",
			Data: map[string]any{
				"order": map[string]any{
					"id":    "C-1042",
					"scans": []any{"small", strings.Repeat("x", 300000)},
				},
			},
			Files: []agk.File{},
		}},
	}

	err := e.Validate(agk.DefaultLimits())
	if err == nil {
		t.Fatal("accepted a value above inline_max_bytes")
	}
	if !strings.Contains(err.Error(), "items[0].data.order.scans[1]") {
		t.Fatalf("the refusal does not name the value to spill: %v", err)
	}
	if !strings.Contains(err.Error(), "referenced in files[]") {
		t.Fatalf("the refusal does not say what to do about it: %v", err)
	}
}

func TestTheInlineRuleIsAboveAndNotAt(t *testing.T) {
	l := agk.Limits{InlineMaxBytes: 10}
	value := func(s string) agk.Envelope {
		return agk.Envelope{
			Meta:  agk.Meta{RunID: "r", Step: "s", Port: "p", Attempt: 1, Count: 1, ProducedAt: at},
			Items: []agk.Item{{ID: "i1", Data: map[string]any{"v": s}, Files: []agk.File{}}},
		}
	}

	// Eight characters and the two quotes around them are the limit exactly.
	if err := value(strings.Repeat("x", 8)).Validate(l); err != nil {
		t.Fatalf("a value at the limit was refused: %v", err)
	}
	if err := value(strings.Repeat("x", 9)).Validate(l); err == nil {
		t.Fatal("a value above the limit was accepted")
	}
}

// TestEnvelopeMaxBytesIsMeasuredOnWhatEncodeWrites ties the rule to the bytes: the size
// refused is the size of the document that would have been published.
func TestEnvelopeMaxBytesIsMeasuredOnWhatEncodeWrites(t *testing.T) {
	e := sample(t)

	var b bytes.Buffer
	n, err := e.Encode(&b)
	if err != nil {
		t.Fatal(err)
	}

	if err := e.Validate(agk.Limits{EnvelopeMaxBytes: n}); err != nil {
		t.Fatalf("an envelope of exactly the limit was refused: %v", err)
	}

	err = e.Validate(agk.Limits{EnvelopeMaxBytes: n - 1})
	var r *agk.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("an envelope above the limit was accepted: %v", err)
	}
	if r.Got != n {
		t.Fatalf("the refusal says %d bytes and Encode wrote %d", r.Got, n)
	}

	// A port that carried nothing publishes an empty list, so that is the document the
	// rule is measured on. Measuring the form it would never be published in is
	// measuring something else.
	empty := agk.Empty("01JMZ8W4K2R7Q0E3N5T9", "normalize", "rejected", 1, at)
	empty.Items = nil
	b.Reset()
	if n, err = empty.Encode(&b); err != nil {
		t.Fatal(err)
	}
	if err := empty.Validate(agk.Limits{EnvelopeMaxBytes: n}); err != nil {
		t.Fatalf("an envelope carrying nothing, of exactly the limit, was refused: %v", err)
	}
	err = empty.Validate(agk.Limits{EnvelopeMaxBytes: n - 1})
	if !errors.As(err, &r) {
		t.Fatalf("an envelope carrying nothing, above the limit, was accepted: %v", err)
	}
	if r.Got != n {
		t.Fatalf("the refusal says %d bytes and Encode wrote %d", r.Got, n)
	}
}

// TestDecodeStopsReadingAtTheLimit: the limit is what keeps an oversized batch from
// being held whole before it is refused, so reading has to stop at it.
func TestDecodeStopsReadingAtTheLimit(t *testing.T) {
	endless := &counted{}

	_, err := agk.Decode(endless, agk.Limits{EnvelopeMaxBytes: 1024})
	var r *agk.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("an endless document was not refused: %v", err)
	}
	if r.Rule != agk.RuleEnvelopeMaxBytes || r.Outcome != agk.Fail {
		t.Fatalf("refused by %s, %v", r.Rule, r.Outcome)
	}
	if !errors.Is(err, agk.ErrStepFailed) {
		t.Fatalf("the refusal does not unwrap to a failed step: %v", err)
	}
	if endless.n > 1025 {
		t.Fatalf("read %d bytes of a document limited to 1024", endless.n)
	}
}

// counted hands out an endless document and says how much of it was taken.
type counted struct{ n int64 }

func (c *counted) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	c.n += int64(len(p))
	return len(p), nil
}

// TestEncodeValueIsWhatTheThresholdMeasures ties the encoding a caller can ask for to
// the rule that weighs it. A value above inline_max_bytes is written as an artifact, and
// the bytes stored are the bytes that were weighed: measuring one encoding and storing
// another would put the threshold and the artifact out of step, and the same value would
// weigh two different things on the two sides of the threshold.
func TestEncodeValueIsWhatTheThresholdMeasures(t *testing.T) {
	values := []any{
		"a report with <angles> & an ampersand",
		map[string]any{"title": "September", "lines": []any{1, 2, 3}},
		[]any{"a", "b"},
		1290.50,
		nil,
	}
	for _, v := range values {
		b, err := agk.EncodeValue(v)
		if err != nil {
			t.Fatalf("EncodeValue: %v", err)
		}
		n := int64(len(b))
		e := agk.Envelope{
			Meta:  agk.Meta{RunID: "r", Step: "render", Port: "out", Attempt: 1, Count: 1, ProducedAt: at},
			Items: []agk.Item{{ID: "i1", Data: map[string]any{"report": v}, Files: []agk.File{}}},
		}
		if err := e.Validate(agk.Limits{InlineMaxBytes: n}); err != nil {
			t.Errorf("%s weighs %d bytes and was refused at a threshold of %d: %v", b, n, n, err)
		}
		if err := e.Validate(agk.Limits{InlineMaxBytes: n - 1}); err == nil {
			t.Errorf("%s weighs %d bytes and was accepted at a threshold of %d", b, n, n-1)
		}
	}
}

// TestTheLargestLimitThereIsStillReadsTheDocument: reading stops one byte past the
// limit, and a caller naming the largest limit there is must not be left with a reader
// that stops before it has begun.
func TestTheLargestLimitThereIsStillReadsTheDocument(t *testing.T) {
	var b bytes.Buffer
	if _, err := sample(t).Encode(&b); err != nil {
		t.Fatal(err)
	}

	back, err := agk.Decode(bytes.NewReader(b.Bytes()), agk.Limits{EnvelopeMaxBytes: math.MaxInt64})
	if err != nil {
		t.Fatalf("an envelope was refused under the largest limit there is: %v", err)
	}
	if len(back.Items) != 1 {
		t.Fatalf("read %d items, want the one the envelope holds", len(back.Items))
	}
}

// TestALimitThatIsNotSetIsNotApplied states the one thing a zero Limits means, so that
// a caller reading a fixture it knows to be oversized can say so.
func TestALimitThatIsNotSetIsNotApplied(t *testing.T) {
	e := agk.Envelope{
		Meta:  agk.Meta{RunID: "r", Step: "s", Port: "p", Attempt: 1, Count: 1, ProducedAt: at},
		Items: []agk.Item{{ID: "i1", Data: map[string]any{"report": strings.Repeat("x", 300000)}, Files: []agk.File{}}},
	}
	if err := e.Validate(agk.Limits{}); err != nil {
		t.Fatalf("a rule that was not set was applied: %v", err)
	}
}

// TestArtifactMaxBytesIsNotABoundOnFileSize: the setting caps what may be written to
// the store, where it is applied. The documentation says in as many words that it is
// not a bound on the size an envelope records, and an artifact written under a ceiling
// that has since been lowered still has to be nameable.
func TestArtifactMaxBytesIsNotABoundOnFileSize(t *testing.T) {
	l := agk.DefaultLimits()
	e := agk.Envelope{
		Meta: agk.Meta{RunID: "r", Step: "archive", Port: "out", Attempt: 1, Count: 1, ProducedAt: at},
		Items: []agk.Item{{
			ID:   "i1",
			Data: map[string]any{},
			Files: []agk.File{{
				Name:      "backup.tar",
				URI:       agk.URI{Run: "r", Step: "archive", Port: "out", Name: "backup.tar"},
				MediaType: "application/x-tar",
				Size:      l.ArtifactMaxBytes * 2,
				SHA256:    strings.Repeat("a", 64),
			}},
		}},
	}
	if err := e.Validate(l); err != nil {
		t.Fatalf("an envelope naming a large artifact was refused: %v", err)
	}
}

// TestARefusalReadsAsALogLine covers the Refusal a store or a driver builds for the one
// rule this package does not apply itself.
func TestARefusalReadsAsALogLine(t *testing.T) {
	r := &agk.Refusal{
		Step:    "archive",
		Port:    "out",
		Rule:    agk.RuleArtifactMaxBytes,
		Outcome: agk.Fail,
		Limit:   agk.DefaultArtifactMaxBytes,
		Got:     agk.DefaultArtifactMaxBytes + 1,
	}
	want := "step archive: port out: artifact_max_bytes: 5368709121 bytes, above the 5368709120 the rule allows; step failed"
	if r.Error() != want {
		t.Fatalf("the refusal reads\n%s\nwant\n%s", r.Error(), want)
	}
	if !errors.Is(r, agk.ErrStepFailed) {
		t.Fatal("a failed step does not unwrap to one")
	}
	if errors.Is(r, agk.ErrEnvelopeRejected) {
		t.Fatal("a failed step unwraps to a rejected envelope")
	}
}

func TestTheTwoOutcomesAreNamed(t *testing.T) {
	if agk.Reject.String() != "envelope rejected" {
		t.Errorf("Reject reads %q", agk.Reject)
	}
	if agk.Fail.String() != "step failed" {
		t.Errorf("Fail reads %q", agk.Fail)
	}
	if agk.Reject == agk.Fail {
		t.Error("the two outcomes are one")
	}
}
