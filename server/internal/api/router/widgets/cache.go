package widgets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// resultCache is the in-process port of the request-handler cache
// (packages/request-handler/src/lib/request-handler.ts): a TTL map with
// single-flight dedup per key. Upstream pins:
//
//   - media-organizer TTL 60_000ms (media-organizer.ts:33),
//   - downloads TTL 10_000ms (downloads.ts:18),
//   - no stale-if-error (fallbackToStaleOnError unset): a failed request is
//     NOT cached, the next caller retries,
//   - concurrent requests for the same key share one upstream call
//     (inflight map; upstream caps MAX_INFLIGHT_REQUESTS at 100, our
//     single-flight map is naturally bounded by the caller fan-out).
//
// The cache key is the integration id plus a hash of the request input — the
// upstream key hashes the integration (including a secret fingerprint) and
// the options (hashSharedCacheInput); the id plus input is equivalent for a
// single process where secrets change only through mutations, which
// invalidate explicitly.
type resultCache struct {
	ttl time.Duration

	mu       sync.Mutex
	entries  map[string]cacheEntry
	inflight map[string]*inflightCall
}

type cacheEntry struct {
	data      any
	timestamp time.Time
}

type inflightCall struct {
	done chan struct{}
	data any
	err  error
}

func newResultCache(ttlMS int64) *resultCache {
	return &resultCache{
		ttl:      time.Duration(ttlMS) * time.Millisecond,
		entries:  make(map[string]cacheEntry),
		inflight: make(map[string]*inflightCall),
	}
}

// getOrLoad returns the cached entry when fresh, joins an in-flight load, or
// runs load exactly once per key. A load error is never cached.
func (c *resultCache) getOrLoad(key string, load func() (any, error)) (any, time.Time, error) {
	if c == nil {
		data, err := load()
		return data, time.Now(), err
	}
	cacheKey := key
	now := time.Now()

	c.mu.Lock()
	if entry, ok := c.entries[cacheKey]; ok && now.Sub(entry.timestamp) < c.ttl {
		c.mu.Unlock()
		return entry.data, entry.timestamp, nil
	}
	if call, ok := c.inflight[cacheKey]; ok {
		c.mu.Unlock()
		<-call.done
		return call.data, now, call.err
	}
	call := &inflightCall{done: make(chan struct{})}
	c.inflight[cacheKey] = call
	c.mu.Unlock()

	call.data, call.err = load()
	close(call.done)

	c.mu.Lock()
	delete(c.inflight, cacheKey)
	if call.err == nil {
		c.entries[cacheKey] = cacheEntry{data: call.data, timestamp: now}
	}
	c.mu.Unlock()
	return call.data, now, call.err
}

// invalidate drops the cached entries of the given integration ids
// (downloadClientRequestHandler.invalidateCacheAsync after mutations,
// packages/api/src/router/widgets/downloads.ts:41-47). Keys are
// "<integrationID>\x00<inputHash>".
func (c *resultCache) invalidate(integrationIDs []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		for _, id := range integrationIDs {
			if strings.HasPrefix(key, id+"\x00") {
				delete(c.entries, key)
			}
		}
	}
}

// inputHash hashes the JSON-encoded input of a request so distinct cache
// inputs (pageSize/limit/selection) do not collide.
func inputHash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}
