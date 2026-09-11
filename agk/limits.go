package agk

// The four size rules, at the values the documentation records as their defaults. A
// namespace may carry its own, which is why they travel in a Limits rather than being
// read from here at the point of use.
const (
	// DefaultInlineMaxBytes is the weight above which a value stops travelling
	// inside an item and travels as an artifact referenced from files[].
	DefaultInlineMaxBytes int64 = 262144

	// DefaultEnvelopeMaxBytes is the largest serialised envelope a port carries.
	DefaultEnvelopeMaxBytes int64 = 4194304

	// DefaultMaxItems is the largest number of items one envelope carries. Beyond
	// it a step paginates across several outputs or writes a JSON Lines artifact.
	DefaultMaxItems int = 100000

	// DefaultArtifactMaxBytes is the largest single artifact. It bounds what may be
	// written to the store, and is deliberately not a bound on File.Size: an
	// envelope may name an artifact written when the limit was higher.
	DefaultArtifactMaxBytes int64 = 5368709120
)

// Limits carries the four engine settings that bound what travels on a port. They are
// applied by the runner to a serialised envelope and are not properties of its shape,
// which is why they are passed to the calls that read and check one rather than being
// written into the document.
//
// A limit that is zero or negative is not applied. That is how a caller turns one rule
// off deliberately, reading a fixture it knows to be oversized for instance; an engine
// passes DefaultLimits or the namespace's own and never a zero value.
type Limits struct {
	InlineMaxBytes   int64
	EnvelopeMaxBytes int64
	MaxItems         int
	ArtifactMaxBytes int64
}

// DefaultLimits returns the four settings at the values the documentation prints.
func DefaultLimits() Limits {
	return Limits{
		InlineMaxBytes:   DefaultInlineMaxBytes,
		EnvelopeMaxBytes: DefaultEnvelopeMaxBytes,
		MaxItems:         DefaultMaxItems,
		ArtifactMaxBytes: DefaultArtifactMaxBytes,
	}
}
