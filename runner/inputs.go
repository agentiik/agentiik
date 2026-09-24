package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
)

// fetchInputs reads the envelope of every input port through the URL the redemption named for it,
// and holds each one to what the task message said of it.
//
// The digest is checked by artifact.GetEnvelope, which hashes what arrived and refuses bytes that
// are not the bytes the digest names, and reads no further than one byte past
// envelope_max_bytes. What is left to this side is what only the message knows: how many items
// the controller published on the port, which TaskOf holds the envelope to, and that every
// artifact the envelope names has a URL to be fetched through. The artifacts themselves are read
// by the driver as it lays the inputs down, through the same objects, and each is held to its own
// digest there, before the container is created.
func fetchInputs(ctx context.Context, objects artifact.Objects, m bus.TaskMessage, r Redemption, l agk.Limits) (map[agk.Port]agk.Envelope, error) {
	if len(m.Inputs) == 0 {
		return nil, nil
	}
	granted := make(map[string]map[string]bool, len(r.Inputs))
	for _, in := range r.Inputs {
		digests := make(map[string]bool, len(in.Artifacts))
		for _, a := range in.Artifacts {
			digests[a.SHA256] = true
		}
		granted[in.Port] = digests
	}

	out := make(map[agk.Port]agk.Envelope, len(m.Inputs))
	for _, in := range m.Inputs {
		hex, ok := hexOf(in.Digest)
		if !ok {
			return nil, fmt.Errorf("runner: task %s names %q on port %s, and an envelope is named sha256: and sixty-four lowercase hexadecimal characters", m.IdempotencyKey, in.Digest, in.Port)
		}
		e, err := artifact.GetEnvelope(ctx, objects, m.Namespace, hex, l)
		if errors.Is(err, artifact.ErrNotAnEnvelope) {
			return nil, fmt.Errorf("%w: task %s, input port %s: %w", ErrNotAsNamed, m.IdempotencyKey, in.Port, err)
		}
		if err != nil {
			return nil, fmt.Errorf("runner: task %s, input port %s: the envelope could not be fetched: %w", m.IdempotencyKey, in.Port, err)
		}
		for _, item := range e.Items {
			for _, f := range item.Files {
				if f.SHA256 != "" && !granted[in.Port][f.SHA256] {
					return nil, fmt.Errorf("%w: task %s, input port %s: the envelope names %s, and the redemption gives no URL for it", ErrAnswerUnusable, m.IdempotencyKey, in.Port, f.URI)
				}
			}
		}
		out[agk.Port(in.Port)] = e
	}
	return out, nil
}
