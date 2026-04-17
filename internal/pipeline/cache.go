package pipeline

import (
	"sync"
	"time"
)

// maxCacheEntries is the maximum number of entries in each processing cache
// (image descriptions, PDF text). When the cache is full, all entries are
// evicted on the next store. This is a simple strategy that avoids the
// complexity of LRU tracking while bounding memory growth.
const maxCacheEntries = 1024

// cacheEntry holds a cached value plus an optional expiry. A zero expiresAt
// means the entry is permanent (does not expire).
type cacheEntry struct {
	value     string
	expiresAt time.Time
}

// boundedCache is a thread-safe string→string cache with a hard size limit
// and optional per-entry TTL. When the limit is reached, the cache is cleared
// before inserting the new entry. This prevents unbounded memory growth from
// long-running proxy processes.
//
// Entries stored via Store are permanent; entries stored via StoreWithTTL
// expire after the given duration. Expired entries are removed lazily on
// the next Load for that key.
type boundedCache struct {
	mu    sync.RWMutex
	items map[string]cacheEntry
}

func newBoundedCache() *boundedCache {
	return &boundedCache{items: make(map[string]cacheEntry)}
}

// Load returns the cached value for key. If the entry has expired, it is
// removed and treated as a miss.
func (c *boundedCache) Load(key string) (string, bool) {
	c.mu.RLock()
	entry, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		// Re-check under write lock in case another goroutine refreshed it.
		if e2, ok2 := c.items[key]; ok2 && !e2.expiresAt.IsZero() && time.Now().After(e2.expiresAt) {
			delete(c.items, key)
		}
		c.mu.Unlock()
		return "", false
	}
	return entry.value, true
}

// Store inserts a permanent entry (never expires).
func (c *boundedCache) Store(key, value string) {
	c.mu.Lock()
	if len(c.items) >= maxCacheEntries {
		c.items = make(map[string]cacheEntry)
	}
	c.items[key] = cacheEntry{value: value}
	c.mu.Unlock()
}

// StoreWithTTL inserts an entry that expires after ttl. Intended for caching
// transient failures so that repeated failed lookups don't hammer upstream
// services, while still allowing eventual retry.
func (c *boundedCache) StoreWithTTL(key, value string, ttl time.Duration) {
	c.mu.Lock()
	if len(c.items) >= maxCacheEntries {
		c.items = make(map[string]cacheEntry)
	}
	c.items[key] = cacheEntry{value: value, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
}

func (c *boundedCache) Reset() {
	c.mu.Lock()
	c.items = make(map[string]cacheEntry)
	c.mu.Unlock()
}
