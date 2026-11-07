// Package updatechecker is the Go port of the updateChecker tRPC router
// (packages/api/src/router/update-checker.ts) and its backing request handler
// (packages/request-handler/src/update-checker.ts): the release feed of
// homarr-labs/homarr that powers the "update available" indicator of the
// manage tools page.
//
// Upstream pipes the router through the GitHub API (Octokit
// rest.repos.listReleases) and layers three Redis structures on top: a fresh
// cache (TTL 24h), a stale cache (no TTL) and a lock (TTL 2min) that dedups
// concurrent checks across the next.js and websocket processes. The port
// reproduces the observable semantics in one process:
//
//   - fresh cache TTL 24h (updateCheckTtlSeconds): within the window the
//     cached entry answers without touching GitHub;
//   - single-flight dedup: concurrent misses behind ONE fetch (the
//     process-local equivalent of the Redis lock + the
//     waitForConcurrentUpdateCheckAsync poll loop; cross-process lockups
//     cannot occur in the single binary, see ARCHITECTURE.md section 2);
//   - stale-if-error: a failing fetch serves the stale entry (or an empty
//     result) AND caches that result as fresh for 24h, "suppressing retries
//     for 24 hours" (update-checker.ts:117-127) — the same window a
//     successful check uses;
//   - the router itself swallows handler errors into an empty list ("An
//     empty list hides the indicator without violating TanStack Query's data
//     contract", update-checker.ts:15-17).
//
// Documented deviations: the cache is per-process in-memory (upstream also
// keeps it in Redis so the two processes share it; homarr-go is one process);
// semver comparison is a local implementation of semver 2.0.0 precedence
// (upstream uses semver-parser's compareSemVer — same ordering for the
// release tags GitHub serves, including prerelease ordering); the
// env.NO_EXTERNAL_CONNECTION test branch of upstream is not ported (same
// decision as the bangs proxy).
package updatechecker

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

// DefaultReleasesURL is the endpoint Octokit hits with
// octokit.rest.repos.listReleases({owner: "homarr-labs", repo: "homarr"})
// (update-checker.ts:158-162).
const DefaultReleasesURL = "https://api.github.com/repos/homarr-labs/homarr/releases"

// defaultVersion is the version of the package.json the upstream handler
// imports (update-checker.ts:19); Options.Version overrides it (same default
// as the info router).
const defaultVersion = "2.11.1"

const (
	// updateCheckTtlSeconds (update-checker.ts:15): fresh window of a check.
	updateCheckTTL = 24 * time.Hour
	// fetchTimeout bounds the GitHub call (Octokit has no timeout upstream;
	// the bangs proxy port set the 10s withTimeoutAsync default — same seam
	// here so a hung GitHub cannot pin the worker).
	fetchTimeout = 10 * time.Second
	// maxReleasesBytes bounds the releases page read (the real payload is
	// ~40 KB for 30 releases).
	maxReleasesBytes = 8 << 20
)

// update is the Update interface of update-checker.ts:130-136:
// {name, contentHtml?, url, tagName, isPrerelease}. contentHtml is omitted
// when the release carries no body_html (zod optional + superjson drop the
// undefined member).
type update struct {
	Name         *string `json:"name"`
	ContentHTML  *string `json:"contentHtml,omitempty"`
	URL          string  `json:"url"`
	TagName      string  `json:"tagName"`
	IsPrerelease bool    `json:"isPrerelease"`
}

// cacheEntry is UpdateCheckCacheEntry (update-checker.ts:17-21): the cached
// payload plus the attempt bookkeeping the stale-if-error path needs.
type cacheEntry struct {
	availableUpdates []update
	attemptedAt      time.Time
	checkedAt        *time.Time
}

// Options configures the router. The zero value talks to the real GitHub
// endpoint with the default version.
type Options struct {
	// Version is the current instance version (default "2.11.1").
	Version string
	// ReleasesURL overrides the GitHub releases endpoint (test seam).
	ReleasesURL string
	// CacheTTL overrides the 24h fresh window (test seam; <= 0 = default).
	CacheTTL time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
	// Client overrides the HTTP client (tests).
	Client *http.Client
}

type service struct {
	opts    Options
	version string
	db      *db.DB

	mu       sync.Mutex
	fresh    *cacheEntry // valid until expires
	expires  time.Time
	stale    *cacheEntry
	inflight *inflightCall
}

type inflightCall struct {
	done  chan struct{}
	entry *cacheEntry
}

// Register registers the updateChecker router. Call once at startup. The
// database is unused (the release cache lives in memory).
func Register(r *trpc.Router, database *db.DB, opts Options) {
	_ = database
	if opts.Version == "" {
		opts.Version = defaultVersion
	}
	if opts.ReleasesURL == "" {
		opts.ReleasesURL = DefaultReleasesURL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	s := &service{opts: opts, version: opts.Version, db: database}

	// updateChecker.getAvailableUpdates (update-checker.ts:9-19):
	// permissionRequiredProcedure.requiresPermission("admin"), a query.
	r.Register("updateChecker.getAvailableUpdates", s.getAvailableUpdates)
}

// getAvailableUpdates mirrors the router body: the cached check (any error →
// empty list, hiding the indicator).
func (s *service) getAvailableUpdates(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	caller, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(caller, "admin"); terr != nil {
		return nil, terr
	}

	entry := s.getCached(ctx)
	// The fetch itself never fails the procedure: getCached already reduced
	// every failure to a (possibly empty) entry, matching the router's
	// try/catch.
	return entry.availableUpdates, nil
}

// getCached is getCachedAvailableUpdatesAsync (update-checker.ts:64-133),
// reduced to the single-process shape: fresh window, single-flight, stale
// fallback.
func (s *service) getCached(ctx context.Context) *cacheEntry {
	s.mu.Lock()
	if s.fresh != nil && s.opts.Now().Before(s.expires) {
		entry := s.fresh
		s.mu.Unlock()
		return entry
	}
	if s.inflight != nil {
		call := s.inflight
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.entry
		case <-ctx.Done():
			// The caller went away; the fetch continues for the others. The
			// upstream waitForConcurrentUpdateCheckAsync answers stale on
			// timeout (update-checker.ts:52-61).
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.fresh != nil {
				return s.fresh
			}
			return &cacheEntry{availableUpdates: staleAvailable(s.stale), attemptedAt: s.opts.Now()}
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	s.inflight = call
	s.mu.Unlock()

	entry := s.check(ctx)

	s.mu.Lock()
	s.inflight = nil
	// Both success and failure are cached as fresh for the TTL window (the
	// failure path sets freshUpdateCheckChannel too, update-checker.ts:119).
	s.fresh = entry
	s.expires = s.opts.Now().Add(s.ttl())
	// The stale cache only advances on a SUCCESSFUL check
	// (staleUpdateCheckChannel.setAsync inside the try,
	// update-checker.ts:115): a failure merges the old payload into fresh
	// and leaves stale untouched.
	if entry.checkedAt != nil {
		s.stale = entry
	}
	s.mu.Unlock()
	call.entry = entry
	close(call.done)
	return entry
}

// ttl is the fresh window (24h; test seam).
func (s *service) ttl() time.Duration {
	if s.opts.CacheTTL > 0 {
		return s.opts.CacheTTL
	}
	return updateCheckTTL
}

// check runs one GitHub check (getAvailableUpdatesAsync,
// update-checker.ts:139-201). Every failure degrades to the
// stale-or-empty entry instead of erroring (the upstream catch in
// getCachedAvailableUpdatesAsync logs a warning and suppresses retries).
func (s *service) check(ctx context.Context) *cacheEntry {
	now := s.opts.Now()
	releases, err := s.fetch(ctx)
	if err != nil {
		s.mu.Lock()
		stale := s.stale
		s.mu.Unlock()
		return &cacheEntry{
			availableUpdates: staleAvailable(stale),
			attemptedAt:      now,
			checkedAt:        checkedAtOf(stale),
		}
	}
	return &cacheEntry{
		availableUpdates: filterUpdates(releases, s.version),
		attemptedAt:      now,
		checkedAt:        &now,
	}
}

func staleAvailable(stale *cacheEntry) []update {
	if stale != nil {
		return stale.availableUpdates
	}
	return []update{}
}

func checkedAtOf(stale *cacheEntry) *time.Time {
	if stale != nil {
		return stale.checkedAt
	}
	return nil
}

// githubRelease is the subset of the listReleases response the handler maps
// (update-checker.ts:168-175): name, body_html, html_url, tag_name and the
// prerelease flag.
type githubRelease struct {
	Name       *string `json:"name"`
	BodyHTML   *string `json:"body_html"`
	HTMLURL    string  `json:"html_url"`
	TagName    string  `json:"tag_name"`
	Prerelease bool    `json:"prerelease"`
}

// fetch GETs the releases page with the Octokit headers (Accept
// application/vnd.github+json; GitHub requires a User-Agent).
func (s *service) fetch(ctx context.Context) ([]update, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.ReleasesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("updatechecker: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "homarr-go")
	res, err := s.opts.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("updatechecker: GitHub responded %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxReleasesBytes))
	if err != nil {
		return nil, err
	}
	var releases []githubRelease
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, fmt.Errorf("updatechecker: invalid releases payload: %w", err)
	}
	out := make([]update, 0, len(releases))
	for _, r := range releases {
		out = append(out, update{
			Name:         r.Name,
			ContentHTML:  r.BodyHTML,
			URL:          r.HTMLURL,
			TagName:      r.TagName,
			IsPrerelease: r.Prerelease,
		})
	}
	return out, nil
}

// filterUpdates is the tail of getAvailableUpdatesAsync
// (update-checker.ts:177-199): skip non-semantic tags, drop prereleases
// unless the current version is one, keep strictly newer releases and sort
// them newest first.
func filterUpdates(releases []update, currentVersion string) []update {
	current, ok := parseSemVer(currentVersion)
	if !ok {
		// "Unable to check for updates due to non semantic current version"
		// (update-checker.ts:147-150) — the router catch turns it into [].
		return []update{}
	}
	semantic := make([]update, 0, len(releases))
	for _, r := range releases {
		if _, ok := parseSemVer(r.TagName); !ok {
			continue // skippedTags (logged upstream)
		}
		semantic = append(semantic, r)
	}
	available := make([]update, 0, len(semantic))
	for _, r := range semantic {
		// isCurrentPrerelease || !release.isPrerelease
		// (update-checker.ts:185): the GitHub prerelease flag filters, the
		// current version's tag decides whether it applies.
		if !current.hasPre && r.IsPrerelease {
			continue
		}
		tag := r.semver()
		if compareSemVer(tag, current) <= 0 {
			continue
		}
		available = append(available, r)
	}
	// Newest first (compareSemVer(b, a) descending).
	sort.SliceStable(available, func(i, j int) bool {
		return compareSemVer(available[i].semver(), available[j].semver()) > 0
	})
	return available
}

// semver is a parsed semantic version (semver-parser parseSemVer subset):
// major.minor.patch with an optional -pre identifier list. The optional
// leading v of release tags is accepted.
type semver struct {
	major, minor, patch uint64
	pre                 []string
	hasPre              bool
}

func (u update) semver() semver {
	sv, _ := parseSemVer(u.TagName)
	return sv
}

// parseSemVer parses `v?MAJOR.MINOR.PATCH(-PRE)?` (build metadata is
// accepted and ignored, per semver 2.0.0).
func parseSemVer(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var out semver
	for i, p := range parts {
		n, err := parseNumericID(p)
		if err != nil {
			return semver{}, false
		}
		switch i {
		case 0:
			out.major = n
		case 1:
			out.minor = n
		case 2:
			out.patch = n
		}
	}
	if pre != "" {
		out.hasPre = true
		out.pre = strings.Split(pre, ".")
		for _, id := range out.pre {
			if id == "" {
				return semver{}, false
			}
		}
	}
	return out, true
}

// parseNumericID parses a numeric identifier without leading zeros
// (semver 2.0.0 section 2).
func parseNumericID(s string) (uint64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, errors.New("invalid numeric identifier")
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("invalid numeric identifier")
		}
		n = n*10 + uint64(s[i]-'0')
	}
	return n, nil
}

// compareSemVer implements semver 2.0.0 precedence (semver-parser
// compareSemVer ordering for release tags): core fields numerically, then
// release > prerelease and the prerelease identifier rules (numeric <
// alphanumeric, fewer identifiers lose when all shared ones are equal).
func compareSemVer(a, b semver) int {
	if a.major != b.major {
		return cmpUint64(a.major, b.major)
	}
	if a.minor != b.minor {
		return cmpUint64(a.minor, b.minor)
	}
	if a.patch != b.patch {
		return cmpUint64(a.patch, b.patch)
	}
	switch {
	case !a.hasPre && !b.hasPre:
		return 0
	case !a.hasPre:
		return 1 // release > prerelease
	case !b.hasPre:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xerr := parseNumericID(x)
		yn, yerr := parseNumericID(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				return cmpUint64(xn, yn)
			}
		case xerr == nil:
			return -1 // numeric < alphanumeric
		case yerr == nil:
			return 1
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

func cmpUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
