// Package store is where a workflow repository's packs are kept: in the object store, under the
// namespace that owns the workflow, as the documentation decides, "Objects are packfiles in the
// object store; refs live in PostgreSQL". It joins what package repo reads and writes to the rows
// package db keeps of it, which repo, holding no database, does not.
//
// # Keys
//
// A pack and its index are two objects of the namespace's store,
//
//	<namespace>/git/<repository>/pack-<name>.pack
//	<namespace>/git/<repository>/pack-<name>.idx
//
// where repository is the workflow's key, random and never changed, so that a rename moves no
// object, and name is the pack's checksum, as git names a pack, so that a key names one set of
// bytes for good. They sit beside sha256/, which holds the counted objects, and are nothing it
// counts: the orphan sweep walks sha256/ alone and never takes a pack, whose row in git_packs is
// what keeps it, and which the repack collects. A pack holds every object whole, as repo.Unpack
// writes it, so no pack depends on another and a fetch copies its entries as they are stored.
//
// # Writing
//
// Put records a pack receiving in git_packs, in a transaction of its own, before a byte of it is
// written, then writes the pack and its index. The push makes it live in the transaction that moves
// its refs, db.NS.PackLive beside db.NS.UpdateRefs; a push that dies before leaves a pack receiving,
// which is collected once it has been receiving past the grace. So a file under git/ always has a
// row naming it.
//
// # Reading
//
// Open reads the live packs a transaction listed, db.NS.Repository, through their indexes: any
// object by its ID, with ranged reads of the pack and never the whole of it, which the store has to
// allow, artifact.Ranged. An index is read once for the process and kept, IdxCacheBytes of them at
// most, since a pack's index is the same bytes for as long as its name is its checksum. A pack is
// opened once a lookup finds an object in it, and held open until the lookup is closed.
package store
