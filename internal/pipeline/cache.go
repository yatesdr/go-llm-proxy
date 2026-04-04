package pipeline

import "sync"

// maxCacheEntries is the maximum number of entries in each processing cache
// (image descriptions, PDF text). When the cache is full, all entries are
// evicted on the next store. This is a simple strategy that avoids the
// complexity of LRU tracking while bounding memory growth.
const maxCacheEntries = 1024

// boundedCache is a thread-safe string→string cache with a hard size limit.
// When the limit is reached, the cache is cleared before inserting the new entry.
// This prevents unbounded memory growth from long-running proxy processes.
type boundedCache struct {
	mu    sync.RWMutex
	items map[string]string
}

func newBoundedCache() *boundedCache {
	return &boundedCache{items: make(map[string]string)}
}

func (c *boundedCache) Load(key string) (string, bool) {
	c.mu.RLock()
	val, ok := c.items[key]
	c.mu.RUnlock()
	return val, ok
}

func (c *boundedCache) Store(key, value string) {
	c.mu.Lock()
	if len(c.items) >= maxCacheEntries {
		c.items = make(map[string]string)
	}
	c.items[key] = value
	c.mu.Unlock()
}

func (c *boundedCache) Reset() {
	c.mu.Lock()
	c.items = make(map[string]string)
	c.mu.Unlock()
}
