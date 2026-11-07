// Package bangs is the Go port of the bangs tRPC router
// (packages/api/src/router/bangs/bangs-router.ts) and its backing request
// handler (packages/request-handler/src/duckduckgo-bangs.ts): a proxy of the
// DuckDuckGo bang catalog (https://duckduckgo.com/bang.js, ~13k entries)
// that the spotlight search bar uses for "!bang" completion.
//
// Upstream pipes the router through createRequestHandler
// (packages/request-handler/lib/request-handler.ts), which adds an in-memory
// cache. The port reproduces the semantics that matter for this handler:
//
//   - cache TTL 10s — the upstream bangs handler sets no cacheTtlMs, so
//     createRequestHandler applies DEFAULT_TTL_MS = 10_000
//     (request-handler.ts:31);
//   - no stale-if-error fallback (fallbackToStaleOnError is unset): after the
//     entry expires an upstream failure surfaces to the caller and nothing
//     stale is served;
//   - inflight dedup: concurrent misses behind one fetch, like upstream's
//     inflight map;
//   - fetch with header `accept: application/json,text/plain,*/*` and a 10s
//     timeout (withTimeoutAsync default, packages/core/src/infrastructure/
//     http/timeout.ts:6; the request-handler's own 60s deadline never binds);
//   - a non-2xx response fails with the upstream ResponseError message
//     "Response did not indicate success"
//     (packages/common/src/errors/http/response-error.ts:13), which tRPC
//     surfaces as INTERNAL_SERVER_ERROR with that exact message.
//
// Documented deviations: the cache is per-process in-memory only (upstream
// also layers an optional Redis-backed shared cache on the same interface;
// homarr-go runs single-process, see ARCHITECTURE.md §2); results are sorted
// and prefix-searched with byte-wise string order instead of JS
// localeCompare (sort and search use the same comparator, so the prefix
// grouping the binary search relies on is preserved; only the ORDER of
// results around symbol/case boundaries can differ); a schema-invalid
// upstream payload fails with a descriptive message instead of the zod issue
// dump (both are INTERNAL_SERVER_ERROR); env.NO_EXTERNAL_CONNECTION (an
// upstream test/offline env returning []) is not ported.
package bangs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// DefaultCatalogURL is DUCKDUCKGO_BANGS_URL
// (packages/request-handler/src/duckduckgo-bangs.ts:11): the endpoint the
// zero Options proxies.
const DefaultCatalogURL = "https://duckduckgo.com/bang.js"

// defaultCacheTTL is DEFAULT_TTL_MS (request-handler.ts:31): the upstream
// bangs handler configures no cacheTtlMs, so the 10s default applies.
const defaultCacheTTL = 10 * time.Second

// fetchTimeout is the withTimeoutAsync default (timeout.ts:6).
const fetchTimeout = 10 * time.Second

// maxCatalogBytes bounds the decoded catalog read; the real payload is
// ~200 KB, so this only guards against a hostile replacement upstream.
const maxCatalogBytes = 32 << 20

// bang is duckDuckGoBangSchema (duckduckgo-bangs.ts:23-31): the short-key
// bang.js entry — `t` bang token, `s` display name, `u` URL template
// (containing {{{s}}}), and the optional `d` domain, `c` category, `sc`
// subcategory and `r` rank. Optional fields are pointers so an explicit
// empty string survives (omitempty only drops absent = nil keys), matching
// the zod parse + superjson output where undefined members are dropped.
type bang struct {
	T  string   `json:"t"`
	S  string   `json:"s"`
	D  *string  `json:"d,omitempty"`
	U  string   `json:"u"`
	C  *string  `json:"c,omitempty"`
	SC *string  `json:"sc,omitempty"`
	R  *float64 `json:"r,omitempty"`
}

// responseErrorText is the upstream ResponseError message
// (response-error.ts:13); tRPC's getMessageFromUnknownError puts the error
// message on the wire verbatim, so the port fails with the same string.
const responseErrorText = "Response did not indicate success"

// Options configures the bangs router. The zero value talks to the real
// DuckDuckGo endpoint; tests inject a fake upstream through CatalogURL and
// shrink CacheTTL.
type Options struct {
	// CatalogURL overrides https://duckduckgo.com/bang.js (test seam for the
	// upstream endpoint).
	CatalogURL string
	// CacheTTL overrides the 10s default TTL (test seam; <= 0 means default).
	CacheTTL time.Duration
}

type service struct {
	catalog *bangCatalog
}

// Register registers the bangs router. Call once at startup. The database is
// unused (the catalog lives on duckduckgo.com; there is no bang table).
func Register(r *trpc.Router, database *db.DB, opts Options) {
	_ = database
	url := opts.CatalogURL
	if url == "" {
		url = DefaultCatalogURL
	}
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	s := &service{catalog: newBangCatalog(url, ttl, &http.Client{})}

	// bangs.search (bangs-router.ts:7-16): publicProcedure
	// ({query, limit 1-50 default 20}), a pure read over the cached catalog.
	r.Register("bangs.search", s.search)
}

// search is bangs.search → searchDuckDuckGoBangsAsync
// (packages/api/src/services/duckduckgo-bangs.ts:35-55): normalize the query
// token, binary-search the sorted catalog for its prefix range and return up
// to `limit` consecutive matches.
func (s *service) search(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	query, terr := d.Str("query")
	if terr != nil {
		return nil, terr
	}
	limit := int64(20) // z.number().int().min(1).max(50).default(20)
	if raw, ok, terr := d.OptInt64("limit", 1, 50); terr != nil {
		return nil, terr
	} else if ok {
		limit = raw
	}

	catalog, err := s.catalog.get(ctx)
	if err != nil {
		return nil, trpc.Internal(err)
	}
	return searchBangs(catalog, query, limit), nil
}

// searchBangs is searchDuckDuckGoBangsAsync's loop: an empty token answers
// [], the lower bound finds the first candidate and consecutive prefix
// matches are collected up to the limit.
func searchBangs(catalog []bang, query string, limit int64) []bang {
	prefix := strings.ToLower(strings.TrimSpace(query))
	if prefix == "" {
		return []bang{}
	}
	matches := []bang{}
	for i := lowerBound(catalog, prefix); i < len(catalog); i++ {
		if !strings.HasPrefix(catalog[i].T, prefix) {
			break
		}
		matches = append(matches, catalog[i])
		if int64(len(matches)) >= limit {
			break
		}
	}
	return matches
}

// lowerBound is the binary search of duckduckgo-bangs.ts:17-33: the first
// index whose token is >= prefix under the SAME comparator the catalog was
// sorted with (byte-wise strings.Compare in the port, localeCompare
// upstream).
func lowerBound(catalog []bang, prefix string) int {
	lo, hi := 0, len(catalog)
	for lo < hi {
		mid := (lo + hi) >> 1
		if strings.Compare(catalog[mid].T, prefix) >= 0 {
			hi = mid
			continue
		}
		lo = mid + 1
	}
	return lo
}

// --- catalog fetch + cache ---------------------------------------------------

// bangCatalog is the in-memory equivalent of the upstream request handler
// for one fixed input ({}): a TTL cache with single-flight dedup.
type bangCatalog struct {
	url    string
	ttl    time.Duration
	client *http.Client

	mu       sync.Mutex
	bangs    []bang
	expires  time.Time
	inflight *catalogCall
}

type catalogCall struct {
	done  chan struct{}
	bangs []bang
	err   error
}

func newBangCatalog(url string, ttl time.Duration, client *http.Client) *bangCatalog {
	return &bangCatalog{url: url, ttl: ttl, client: client}
}

// get returns the catalog, refreshing it when the cached entry has expired
// (getDataAsync of request-handler.ts:260-403, reduced to the local-cache
// path). Upstream errors are not cached: the next caller retries.
func (c *bangCatalog) get(ctx context.Context) ([]bang, error) {
	c.mu.Lock()
	if c.bangs != nil && time.Now().Before(c.expires) {
		bangs := c.bangs
		c.mu.Unlock()
		return bangs, nil
	}
	if c.inflight != nil {
		call := c.inflight
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.bangs, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &catalogCall{done: make(chan struct{})}
	c.inflight = call
	c.mu.Unlock()

	bangs, err := c.fetch(ctx)

	c.mu.Lock()
	c.inflight = nil
	if err == nil {
		c.bangs = bangs
		c.expires = time.Now().Add(c.ttl)
	}
	c.mu.Unlock()
	call.bangs, call.err = bangs, err
	close(call.done)
	return bangs, err
}

// fetch is the requestAsync of duckduckgo-bangs.ts:40-72: GET with the
// accept header, 10s timeout, non-2xx → ResponseError, then the zod schema,
// token normalization, empty-token filtering and the final sort.
func (c *bangCatalog) fetch(ctx context.Context) ([]bang, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("bangs: build request: %w", err)
	}
	req.Header.Set("accept", "application/json,text/plain,*/*")
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, errors.New(responseErrorText)
	}
	raw, err := decodeCatalog(io.LimitReader(res.Body, maxCatalogBytes))
	if err != nil {
		return nil, err
	}
	normalized := make([]bang, 0, len(raw))
	for _, b := range raw {
		token := strings.ToLower(strings.TrimSpace(b.T))
		if token == "" {
			continue // the empty-token filter of duckduckgo-bangs.ts:66
		}
		b.T = token
		normalized = append(normalized, b)
	}
	// Stable sort: JS Array.prototype.sort is stable, so equal tokens keep
	// the source order on both sides.
	sort.SliceStable(normalized, func(i, j int) bool {
		return strings.Compare(normalized[i].T, normalized[j].T) < 0
	})
	return normalized, nil
}

// rawBang is the wire record before the schema check: t, s and u are
// required by duckDuckGoBangSchema (z.string(), no .optional()) — their
// presence must be distinguishable from an empty value, which only t
// normalizes away later. d/c/sc optional strings, r an optional number;
// unknown keys are dropped like the non-strict z.object. (A JSON null for an
// optional key is treated as absent; upstream zod would reject it — an
// edge case bang.js never produces.)
type rawBang struct {
	T  *string  `json:"t"`
	S  *string  `json:"s"`
	D  *string  `json:"d"`
	U  *string  `json:"u"`
	C  *string  `json:"c"`
	SC *string  `json:"sc"`
	R  *float64 `json:"r"`
}

// decodeCatalog is duckDuckGoBangsResponseSchema.parseAsync
// (duckduckgo-bangs.ts:33,58-59): an array of records where t/s/u are
// required strings — one violation fails the whole catalog (zod rejects the
// array wholesale, one zod error for the first offender).
func decodeCatalog(r io.Reader) ([]bang, error) {
	var raw []rawBang
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("bangs: invalid bang catalog JSON: %w", err)
	}
	out := make([]bang, 0, len(raw))
	for i, b := range raw {
		if b.T == nil || b.S == nil || b.U == nil {
			return nil, fmt.Errorf("bangs: invalid bang at index %d: t, s and u are required", i)
		}
		out = append(out, bang{T: *b.T, S: *b.S, D: b.D, U: *b.U, C: b.C, SC: b.SC, R: b.R})
	}
	return out, nil
}
