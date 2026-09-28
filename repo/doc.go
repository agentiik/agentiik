// Package repo is what a workflow repository is made of, as git makes it: objects and their IDs,
// packs and their indexes, and the pkt-line framing git's protocols are written in. It holds no
// server, no database and no bus, and it imports none of db, api and bus: the smart HTTP routes,
// the refs in PostgreSQL and the packs in the object store are built on it elsewhere.
//
// It is named repo rather than git because it is the repository and not a client of anything:
// it never runs the git binary, and a package named git would read as a wrapper of it.
//
// # Why the standard library
//
// The documentation decides that Agentiik serves its repositories itself, packs in the object
// store and refs in the database, which no git server library is built around; and that git is
// served by an implementation on the standard library, as the passkey ceremonies are, with no
// dependency. What a library would bring is mostly the part this package is: reading what anyone
// with workflow:write can push, which is the part to read whole and to fuzz. compress/zlib and
// crypto/sha1 are the format's own algorithms. It is tested against the git binary rather than
// against a reading of the format: packs git writes, thin ones included, are read here, and the
// packs and indexes written here are read by git index-pack, git verify-pack and git fsck --strict.
//
// # What is read, and how strictly
//
// Commits, trees and tags are held to git fsck --strict, and a refusal names git's own check,
// treeNotSorted or hasDotgit, so that it reads the same here as on the pusher's clone. That covers
// what a checkout would trip over: an entry named .git on any filesystem, its NTFS and HFS+
// spellings included, a .gitmodules that is not a file, entries out of order or twice, and modes
// other than git's five. Two spellings git's fsck lets by are refused as well, since git never
// writes them and each is a second spelling of one object: upper-case hexadecimal in an ID, and a
// date with spaces before it. So are the short names NTFS may give .git past GIT~1, and a
// .gitmodules that is a directory or a submodule, which git fsck finds only once a clone reads it.
//
// A pack is read in two passes. The first reads every entry's header and zlib stream, inflating
// each to the size it gives and checking its checksum, hashes every object sent whole, and checks
// the pack's trailer. The second resolves every delta, OFS_DELTA and REF_DELTA, depth first from
// the object its chain is made against, against an entry of the pack or, in a thin pack, against
// an object the repository already holds. What it writes is a pack of whole objects, which depends
// on no other and whose entries can be copied into a fetch as they are stored, with nothing
// inflated or compressed again: an entry sent whole keeps its zlib stream, and a resolved delta is
// compressed once.
//
// # What is not supported
//
//   - SHA-1 alone. A repository in SHA-256 is refused, as a workflow's commit is 7 to 40
//     hexadecimal digits wherever the documentation names one.
//   - Pack version 2 and index version 2, which are what git writes. Pack version 3, index
//     version 1, multi-pack indexes, bitmaps and reverse indexes are not read.
//   - Writing deltas. A fetch copies whole objects; making deltas for it is for later.
//   - The content of .gitmodules and .gitattributes, which git fsck parses to vet the submodules
//     and attributes a checkout would act on. Their names and modes are checked; their text is
//     not, since the version check refuses submodules outright.
//   - The protocols themselves. upload-pack and receive-pack, their capabilities, negotiation and
//     reports, are the smart HTTP routes' to speak, over this package's packets and packs.
//
// # Limits
//
// A pushed pack is bounded before any of it is believed: MaxPackBytes and MaxPackObjects, one
// object by artifact_max_bytes, a chain of deltas by MaxDeltaDepth, and what has to be held whole
// by MaxHeldBytes, with DeltaBaseCacheBytes of bases kept for the deltas made against them. Each
// constant says why it is what it is. A packet is at most MaxPktLen bytes, which is the protocol's
// own bound rather than this package's.
package repo
