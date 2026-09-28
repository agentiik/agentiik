package repo

// The bounds a pack is read within. Each is a constant rather than a setting: they bound what one
// request may make the API hold, whoever pushes, and none of them is met by a repository a
// workflow is.
const (
	// MaxPackBytes is the most a pushed pack may weigh: 2 GiB, the bound GitHub sets on one push,
	// so that a repository that can be pushed there can be pushed here. A pack is written to a
	// temporary file before a byte of it is read, and this is the disk that takes.
	MaxPackBytes int64 = 2 << 30

	// MaxPackObjects is the most objects one pushed pack may hold: 1,048,576. Reading one holds a
	// little over a hundred bytes an object until the pack written is closed, and some two hundred
	// where every entry is a delta naming its base by ID, so this is 200 MiB at its worst, where a
	// workflow repository holds thousands of objects in its whole history.
	MaxPackObjects = 1 << 20

	// MaxDeltaDepth is the longest chain of deltas an object may be resolved through: 50, git's
	// own pack.depth at its default, so that no pack git makes with its defaults is refused. It
	// bounds the work one object costs, which grows with its chain.
	MaxDeltaDepth = 50

	// DeltaBaseCacheBytes is how much of the objects deltas are resolved against a pack being
	// read keeps at once: 64 MiB. Past it the oldest are let go, and resolved again from the pack
	// when another delta needs them, so that a push holds this much of them whatever it carries.
	DeltaBaseCacheBytes = 64 << 20

	// MaxHeldBytes is the most one object may weigh where it has to be held whole in memory: a
	// commit, a tree or a tag, which is parsed, and the base and the result of a delta, which a
	// delta copies from and into. 512 MiB is git's own core.bigFileThreshold at its default, the
	// size above which git sends a file whole rather than as a delta, so no push git makes with its
	// defaults meets it. A blob sent whole is never held, and is bounded by artifact_max_bytes.
	MaxHeldBytes = 512 << 20
)

// maxHeld and deltaBaseCache are the two bounds on memory as the code reads them: variables, so
// that a test can make them small enough to be met by what it builds, and a fuzz target can bound
// what an input makes it allocate.
var (
	maxHeld        int64 = MaxHeldBytes
	deltaBaseCache int64 = DeltaBaseCacheBytes
)
