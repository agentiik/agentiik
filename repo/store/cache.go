package store

import (
	"container/list"
	"sync"

	"github.com/agentiik/agentiik/repo"
)

// idxCache keeps the indexes read, by key, up to a number of bytes, letting go of the least recently
// read first. An index never changes under its key, which names its pack's checksum, so one kept is
// never stale: a pack superseded by a repack is simply not asked for again, and is let go of in its
// turn.
type idxCache struct {
	mu      sync.Mutex
	limit   int
	used    int
	recent  *list.List // of *cachedIdx, the most recently read first
	entries map[string]*list.Element
}

type cachedIdx struct {
	key  string
	idx  *repo.Idx
	size int
}

func newIdxCache(limit int) *idxCache {
	return &idxCache{limit: limit, recent: list.New(), entries: map[string]*list.Element{}}
}

func (c *idxCache) get(key string) (*repo.Idx, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.recent.MoveToFront(e)
	return e.Value.(*cachedIdx).idx, true
}

// add keeps idx, size bytes read, unless it is larger than the whole cache.
func (c *idxCache) add(key string, idx *repo.Idx, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.limit {
		return
	}
	if e, ok := c.entries[key]; ok {
		// Read twice at once by two lookups: the same bytes, kept once.
		c.recent.MoveToFront(e)
		return
	}
	for c.used+size > c.limit {
		oldest := c.recent.Back()
		gone := c.recent.Remove(oldest).(*cachedIdx)
		delete(c.entries, gone.key)
		c.used -= gone.size
	}
	c.entries[key] = c.recent.PushFront(&cachedIdx{key: key, idx: idx, size: size})
	c.used += size
}
