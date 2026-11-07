// Package qbittorrent is the Go port of the upstream QBitTorrentIntegration
// (packages/integrations/src/download-client/qbittorrent/qbittorrent-integration.ts),
// which drives qBittorrent through the @ctrl/qbittorrent library (version
// 9.14.0, pnpm catalog of the root package.json). The Go client reproduces the
// library's exact request semantics:
//
//   - auth (qbittorrentSession.js login/authHeaders/ensureAuthenticated):
//     with the "apiKey" secret EVERY request carries
//     "Authorization: Bearer <key>" and no login happens; otherwise
//     POST {base}/api/v2/auth/login with
//     Content-Type: application/x-www-form-urlencoded and body
//     username=..&password=.. (redirect: manual, no retries) stores the SID
//     cookie from the first Set-Cookie pair (expiry from Expires/Max-Age,
//     default 1h), and every request sends
//     "Cookie: SID=<sid>" PLUS "Authorization: Basic ..." when
//     username/password are configured (qBittorrent 5.2+ compatibility).
//     One re-login is attempted when a request answers 401/403.
//   - version detection (checkVersion): GET /api/v2/app/version (TEXT
//     response); the "v" prefix and any "-suffix" are stripped; v5+ switches
//     the pause/resume endpoints to stop/start and rewrites the torrents/info
//     "filter" values paused<->stopped, resumed<->running. Upstream calls
//     getAppVersion() on EVERY client construction
//     (qbittorrent-integration.ts:216-217), so every operation here refreshes
//     the version state when unknown.
//   - per-request timeout 5s (@ctrl defaults, qbittorrentSession.js:10).
//
// Widget mapping pinned from qbittorrent-integration.ts:33-173 (selection
// paging: at most 100 pages of at most 100 torrents, dedupe by hash, qBittorrent
// 4 wraps out-of-range offsets back to the first page) and :220-252 (state
// mapping).
package qbittorrent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "qBittorrent" — capital B).
const Kind = "qBittorrent"

// DefaultTimeout mirrors the @ctrl/qbittorrent per-request timeout
// (qbittorrentSession.js defaults.timeout = 5000).
const DefaultTimeout = 5 * time.Second

// apiPath is config.path of the library (qbittorrentSession.js:9).
const apiPath = "/api/v2"

// Definition returns the metadata of the qBittorrent kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "qBittorrent"}
}

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values. Upstream prefers
// apiKey when present and username/password otherwise
// (qbittorrent-integration.ts:201-208).
type Secrets struct {
	APIKey   string
	Username string
	Password string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map. A missing
// secret only errors when NEITHER auth mode is complete (upstream falls back
// between the two modes).
func SecretsFromMap(m map[string]string) (Secrets, error) {
	s := Secrets{APIKey: m["apiKey"], Username: m["username"], Password: m["password"]}
	if s.APIKey == "" && (m["username"] == "" || m["password"] == "") {
		if _, ok := m["apiKey"]; !ok {
			// Neither secret kind present at all: report the first upstream
			// candidate (getSecretValue("username")) like the fallback chain
			// would.
			if _, ok := m["username"]; !ok {
				return s, &mediaorganizer.MissingSecretError{Kind: "username"}
			}
			return s, &mediaorganizer.MissingSecretError{Kind: "password"}
		}
	}
	return s, nil
}

// Integration is one qBittorrent instance. All methods are safe for
// concurrent use (the session state is guarded lazily per call; concurrent
// calls may each log in — the cookie survives).
type Integration struct {
	input   integrations.Input
	secrets Secrets
	urls    mediaorganizer.URLBuilder
	http    mediaorganizer.HTTPClient
	// Now overrides the clock used for the completion-time math of the items
	// (dayjs().valueOf() upstream). nil means time.Now; test seam.
	Now func() time.Time

	// auth/version state, mirroring QBittorrentSession.state. Guarded by mu:
	// the widget router reuses one client instance across concurrent calls.
	mu        sync.Mutex
	useAPIKey bool
	sid       string
	cookieEx  time.Time
	version   string
	isV5      bool
	hasV5     bool
}

// New builds one qBittorrent client.
func New(input integrations.Input, secrets Secrets) *Integration {
	return &Integration{
		input:     input,
		secrets:   secrets,
		urls:      mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
		useAPIKey: secrets.APIKey != "",
		Now:       nil,
	}
}

// NewFromRef builds the client from a widget-router IntegrationRef.
func NewFromRef(ref downloadclient.IntegrationRef) (*Integration, error) {
	secrets, err := SecretsFromMap(ref.Secrets)
	if err != nil {
		return nil, err
	}
	return New(ref.Input(), secrets), nil
}

func (i *Integration) nowFn() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// Check implements integrations.Integration: the login flow
// (testingAsync, qbittorrent-integration.ts:23-31). A rejected login is the
// upstream UnauthorizedResult(401).
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	ok, res, err := i.login(ctx)
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.MsgRequest}, nil
	}
	if !ok {
		return integrations.CheckResult{
			StatusCode: res,
			Error:      mediaorganizer.MsgResponse,
		}, nil
	}
	return integrations.CheckResult{StatusCode: http.StatusOK}, nil
}

// login performs POST /api/v2/auth/login and captures the SID cookie. It
// returns (false, status, nil) when the server answered without a session
// cookie (upstream login throws 'Cookie not found. Auth Failed.'; homarr maps
// that to the 401 testing result).
func (i *Integration) login(ctx context.Context) (bool, int, error) {
	form := url.Values{}
	form.Set("username", i.secrets.Username)
	form.Set("password", i.secrets.Password)
	u, err := i.urls.Build(apiPath+"/auth/login", nil)
	if err != nil {
		return false, 0, err
	}
	cfg := i.http
	cfg.Timeout = DefaultTimeout
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// redirect: manual (qbittorrentSession.js login) — do not follow.
	client := *cfg.Std()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	res, err := client.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	cookies := res.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return false, res.StatusCode, nil
	}
	_, value, ok := parseAuthCookie(cookies[0])
	if !ok {
		return false, res.StatusCode, nil
	}
	i.mu.Lock()
	i.sid = value
	i.cookieEx = cookieExpiry(cookies[0], i.nowFn().Add(time.Hour))
	i.mu.Unlock()
	return true, res.StatusCode, nil
}

// parseAuthCookie mirrors getAuthCookieName + cookieParse
// (requestUtils.js:10-13): the first pair of the first Set-Cookie header.
func parseAuthCookie(setCookie string) (name, value string, ok bool) {
	pair := setCookie
	if idx := strings.IndexByte(pair, ';'); idx >= 0 {
		pair = pair[:idx]
	}
	idx := strings.IndexByte(pair, '=')
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(pair[:idx]), strings.TrimSpace(pair[idx+1:]), true
}

// cookieExpiry parses Expires/Max-Age of the Set-Cookie header with the
// library's fallbacks (login in qbittorrentSession.js): Expires > Max-Age >
// now + 1h.
func cookieExpiry(setCookie string, fallback time.Time) time.Time {
	for _, attr := range strings.Split(setCookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(attr), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.ToLower(kv[0]) {
		case "expires":
			if t, err := http.ParseTime(kv[1]); err == nil {
				return t
			}
		case "max-age":
			if secs, err := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64); err == nil {
				return time.Now().Add(time.Duration(secs) * time.Second)
			}
		}
	}
	return fallback
}

// authHeaders mirrors authHeaders (qbittorrentSession.js:125-146).
func (i *Integration) authHeaders() http.Header {
	if i.useAPIKey {
		return http.Header{"Authorization": []string{"Bearer " + i.secrets.APIKey}}
	}
	i.mu.Lock()
	sid := i.sid
	i.mu.Unlock()
	headers := http.Header{"Cookie": []string{"SID=" + sid}}
	if i.secrets.Username != "" || i.secrets.Password != "" {
		headers.Set("Authorization", "Basic "+basicAuth(i.secrets.Username, i.secrets.Password))
	}
	return headers
}

func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

// request performs one authenticated API call with the one-shot re-login of
// requestWithAuth (qbittorrentSession.js:148-160): cookie auth retries once
// after clearing the session on 401/403; apiKey auth never retries.
func (i *Integration) request(ctx context.Context, method, path string, query url.Values, form url.Values, out any) error {
	status, err := i.requestOnce(ctx, method, path, query, form, out)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if !i.useAPIKey {
			i.mu.Lock()
			i.sid = ""
			i.mu.Unlock()
			if _, _, loginErr := i.login(ctx); loginErr == nil {
				_, err = i.requestOnce(ctx, method, path, query, form, out)
				return err
			}
		}
		return mediaorganizer.NewResponseError(fmt.Errorf("status %d for %s", status, path))
	}
	if status < 200 || status >= 300 {
		return mediaorganizer.NewResponseError(fmt.Errorf("status %d for %s", status, path))
	}
	return nil
}

// requestOnce mirrors requestOnce (qbittorrentSession.js:162-186):
// ensureAuthenticated, then the request with auth headers. A non-2xx answer
// returns (status, nil); the caller decides (request classifies it, the
// version probe only needs the status). out == nil discards the body.
func (i *Integration) requestOnce(ctx context.Context, method, path string, query url.Values, form url.Values, out any) (int, error) {
	if err := i.ensureAuthenticated(ctx, path); err != nil {
		return 0, err
	}
	return i.requestRaw(ctx, method, path, query, form, out)
}

// requestRaw issues the HTTP request without the auth preamble.
func (i *Integration) requestRaw(ctx context.Context, method, path string, query url.Values, form url.Values, out any) (int, error) {
	u, err := i.urls.Build(path, nil)
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	if query != nil {
		ru, err := url.Parse(u)
		if err != nil {
			return 0, mediaorganizer.NewRequestError(err)
		}
		q := ru.Query()
		for k, vs := range query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		ru.RawQuery = q.Encode()
		u = ru.String()
	}
	cfg := i.http
	cfg.Timeout = DefaultTimeout
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	for k, vs := range i.authHeaders() {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := cfg.Std().Do(req)
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode, nil // caller classifies
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode, nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return res.StatusCode, mediaorganizer.NewParseError(err)
	}
	return res.StatusCode, nil
}

// ensureAuthenticated mirrors ensureAuthenticated (qbittorrentSession.js:
// 106-123): apiKey mode only makes sure the version state exists (except for
// /app/version itself); cookie mode logs in when the session is missing or
// expired.
func (i *Integration) ensureAuthenticated(ctx context.Context, path string) error {
	if i.useAPIKey {
		if !i.hasV5State() && path != apiPath+"/app/version" {
			return i.checkVersion(ctx)
		}
		return nil
	}
	i.mu.Lock()
	sid, expired := i.sid, i.nowFn().After(i.cookieEx)
	i.mu.Unlock()
	if sid == "" || expired {
		ok, _, err := i.login(ctx)
		if err != nil {
			return mediaorganizer.NewRequestError(err)
		}
		if !ok {
			return mediaorganizer.NewResponseError(fmt.Errorf("login rejected"))
		}
	}
	return nil
}

// hasV5State reports whether the version probe already ran.
func (i *Integration) hasV5State() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.hasV5
}

// checkVersion mirrors checkVersion (qbittorrentSession.js:188-200): GET
// /api/v2/app/version (TEXT response, NOT JSON), strip the "v" prefix and the
// "-suffix", isVersion5OrHigher = clean == "5.0.0" || numeric-aware isGreater.
func (i *Integration) checkVersion(ctx context.Context) error {
	var text strings.Builder
	status, err := i.requestText(ctx, apiPath+"/app/version", &text)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return mediaorganizer.NewResponseError(fmt.Errorf("status %d for /app/version", status))
	}
	version := strings.TrimSpace(text.String())
	clean := strings.Split(strings.TrimPrefix(version, "v"), "-")[0]
	i.mu.Lock()
	i.version = version
	i.isV5 = clean == "5.0.0" || versionGreater(clean, "5.0.0")
	i.hasV5 = true
	i.mu.Unlock()
	return nil
}

// requestText performs a request whose body is a plain string (the version
// probe). It carries the same one-shot re-login as the JSON path
// (requestWithAuth wraps every request, qbittorrentSession.js:148-160).
func (i *Integration) requestText(ctx context.Context, path string, out *strings.Builder) (int, error) {
	status, err := i.requestTextOnce(ctx, path, out)
	if err != nil {
		return status, err
	}
	if (status == http.StatusUnauthorized || status == http.StatusForbidden) && !i.useAPIKey {
		i.sid = ""
		if _, _, loginErr := i.login(ctx); loginErr == nil {
			return i.requestTextOnce(ctx, path, out)
		}
	}
	return status, nil
}

func (i *Integration) requestTextOnce(ctx context.Context, path string, out *strings.Builder) (int, error) {
	if err := i.ensureAuthenticated(ctx, path); err != nil {
		return 0, err
	}
	u, err := i.urls.Build(path, nil)
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	cfg := i.http
	cfg.Timeout = DefaultTimeout
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	for k, vs := range i.authHeaders() {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	res, err := cfg.Std().Do(req)
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, mediaorganizer.NewParseError(err)
	}
	out.Write(raw)
	return res.StatusCode, nil
}

// versionGreater is isGreater (requestUtils.js:31-33): numeric-aware string
// comparison (localeCompare with {numeric:true}).
func versionGreater(a, b string) bool {
	return compareVersions(a, b) > 0
}

func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for idx := 0; idx < len(as) || idx < len(bs); idx++ {
		var av, bv int
		if idx < len(as) {
			av, _ = strconv.Atoi(as[idx])
		}
		if idx < len(bs) {
			bv, _ = strconv.Atoi(bs[idx])
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

// listTorrents is client.listTorrents (qbittorrent.js:212-266): GET
// /torrents/info with the translated filter. category is only sent when
// non-nil (empty string means "without category" in the qBittorrent API).
func (i *Integration) listTorrents(ctx context.Context, opts listOptions) ([]torrent, error) {
	if err := i.ensureVersion(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.Hashes != "" {
		q.Set("hashes", opts.Hashes)
	}
	if opts.Filter != "" {
		q.Set("filter", i.translateFilter(opts.Filter))
	}
	if opts.Category != nil {
		q.Set("category", *opts.Category)
	}
	if opts.Offset != nil {
		q.Set("offset", strconv.FormatInt(*opts.Offset, 10))
	}
	if opts.Limit != nil {
		q.Set("limit", mediaorganizer.FormatNumber(*opts.Limit))
	}
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	if opts.Reverse != nil {
		// reverse: JSON.stringify(reverse) -> "true"/"false"
		q.Set("reverse", strconv.FormatBool(*opts.Reverse))
	}
	var torrents []torrent
	if err := i.request(ctx, http.MethodGet, apiPath+"/torrents/info", q, nil, &torrents); err != nil {
		return nil, err
	}
	return torrents, nil
}

// translateFilter mirrors the v4/v5 filter renaming (qbittorrent.js:218-234).
func (i *Integration) translateFilter(filter string) string {
	if i.isV5State() {
		switch filter {
		case "paused":
			return "stopped"
		case "resumed":
			return "running"
		}
		return filter
	}
	switch filter {
	case "stopped":
		return "paused"
	case "running":
		return "resumed"
	}
	return filter
}

// ensureVersion refreshes the version state when unknown (the upstream
// getClientAsync calls getAppVersion on every construction,
// qbittorrent-integration.ts:216-217).
func (i *Integration) ensureVersion(ctx context.Context) error {
	if i.hasV5State() {
		return nil
	}
	return i.checkVersion(ctx)
}

// isV5State snapshots the version verdict.
func (i *Integration) isV5State() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.isV5
}

type listOptions struct {
	Hashes   string
	Filter   string
	Category *string
	Offset   *int64
	Limit    *float64
	Sort     string
	Reverse  *bool
}

// torrent is the raw /torrents/info entry the widget mapping consumes.
type torrent struct {
	Hash         string   `json:"hash"`
	Name         string   `json:"name"`
	State        string   `json:"state"`
	Size         *float64 `json:"size"`
	Uploaded     *float64 `json:"uploaded"`
	Progress     *float64 `json:"progress"`
	Dlspeed      *float64 `json:"dlspeed"`
	Upspeed      *float64 `json:"upspeed"`
	ETA          *float64 `json:"eta"`
	AddedOn      *float64 `json:"added_on"`
	CompletionOn *float64 `json:"completion_on"`
	Priority     *float64 `json:"priority"`
	Category     *string  `json:"category"`
}

// GetJobsAndStatus ports getClientJobsAndStatusAsync
// (qbittorrent-integration.ts:33-173).
func (i *Integration) GetJobsAndStatus(ctx context.Context, input downloadclient.JobsInput) (downloadclient.JobsAndStatus, error) {
	torrents, err := i.getSelectedTorrents(ctx, input)
	if err != nil {
		return downloadclient.JobsAndStatus{}, err
	}

	if input.Selection == nil {
		var down, up float64
		paused := true
		for _, t := range torrents {
			down += orZero(t.Dlspeed)
			up += orZero(t.Upspeed)
			if stateOf(t.State) != downloadclient.StatePaused {
				paused = false
			}
		}
		return downloadclient.JobsAndStatus{
			Status: downloadclient.Status{
				Paused: paused,
				Rates:  downloadclient.Rates{Down: down, Up: mediaorganizer.Ptr(up)},
				Types:  []string{downloadclient.TypeTorrent},
			},
			Items: itemsOf(torrents, i.nowFn),
		}, nil
	}

	// Selection mode recomputes the status from the global transfer rates and
	// the running-torrent filter, and collects ratioItems (:72-85).
	var transferInfo struct {
		DlInfoSpeed float64 `json:"dl_info_speed"`
		UpInfoSpeed float64 `json:"up_info_speed"`
	}
	running, err := i.listTorrents(ctx, listOptions{Filter: "running", Limit: mediaorganizer.Ptr(1.0)})
	if err != nil {
		return downloadclient.JobsAndStatus{}, err
	}
	if err := i.request(ctx, http.MethodGet, apiPath+"/transfer/info", nil, nil, &transferInfo); err != nil {
		return downloadclient.JobsAndStatus{}, err
	}
	ratioItems, err := i.ratioItems(ctx, input.Limit)
	if err != nil {
		return downloadclient.JobsAndStatus{}, err
	}
	return downloadclient.JobsAndStatus{
		Status: downloadclient.Status{
			Paused: len(running) == 0,
			Rates: downloadclient.Rates{
				Down: transferInfo.DlInfoSpeed,
				Up:   mediaorganizer.Ptr(transferInfo.UpInfoSpeed),
			},
			Types: []string{downloadclient.TypeTorrent},
		},
		Items:      itemsOf(torrents, i.nowFn),
		RatioItems: ratioItems,
	}, nil
}

// getSelectedTorrents ports getSelectedTorrentsAsync
// (qbittorrent-integration.ts:113-173).
func (i *Integration) getSelectedTorrents(ctx context.Context, input downloadclient.JobsInput) ([]torrent, error) {
	limit := input.Limit
	if input.Selection == nil {
		return i.listTorrents(ctx, listOptions{Limit: mediaorganizer.Ptr(limit)})
	}
	selection := input.Selection

	// Only fields with the same meaning in the provider and widget can select
	// the remote window (:120-129).
	sortFields := map[string]string{
		"name":      "name",
		"progress":  "progress",
		"size":      "size",
		"added":     "added_on",
		"index":     "priority",
		"upSpeed":   "upspeed",
		"downSpeed": "dlspeed",
		"sent":      "uploaded",
	}
	var category *string
	if selection.FilterIsWhitelist && len(selection.CategoryFilter) == 1 {
		category = mediaorganizer.Ptr(selection.CategoryFilter[0])
	}

	// Bound every response, retained items, and total work even when nearly
	// the whole queue is hidden (:136-143).
	pageSize := limit
	if pageSize > 100 {
		pageSize = 100
	}
	if len(selection.CategoryFilter) > 0 || !selection.ShowCompletedTorrent || selection.ActiveTorrentThreshold > 0 {
		pageSize = 100
	}

	seen := map[string]bool{}
	torrents := make([]torrent, 0)
	for page := 0; page < 100 && len(torrents) < int(limit); page++ {
		previousSeen := len(seen)
		batch, err := i.listTorrents(ctx, listOptions{
			Limit:    mediaorganizer.Ptr(pageSize),
			Offset:   mediaorganizer.Ptr(int64(float64(page) * pageSize)),
			Sort:     sortFields[selection.Sort],
			Reverse:  mediaorganizer.Ptr(selection.Descending),
			Category: category,
		})
		if err != nil {
			return nil, err
		}
		for _, t := range batch {
			if seen[t.Hash] {
				continue
			}
			seen[t.Hash] = true
			if len(selection.CategoryFilter) > 0 {
				matches := contains(selection.CategoryFilter, orEmpty(t.Category))
				if selection.FilterIsWhitelist != matches {
					continue
				}
			}
			progress := orZero(t.Progress)
			if progress >= 1 {
				if !selection.ShowCompletedTorrent {
					continue
				}
				if orZero(t.Upspeed) < selection.ActiveTorrentThreshold*1024 {
					continue
				}
			}
			torrents = append(torrents, t)
			if len(torrents) >= int(limit) {
				break
			}
		}
		// qBittorrent 4 wraps out-of-range offsets back to the first page
		// (:169-170).
		if float64(len(batch)) < pageSize || len(seen) == previousSeen {
			break
		}
	}
	return torrents, nil
}

// ratioItems ports getRatioItemsAsync (qbittorrent-integration.ts:88-111):
// the original unfiltered limited population, paged in pages of 100 with the
// offset always stepping by 100.
func (i *Integration) ratioItems(ctx context.Context, limit float64) ([]downloadclient.RatioItem, error) {
	out := make([]downloadclient.RatioItem, 0)
	seen := map[string]bool{}
	for page := 0; page < 100 && float64(len(out)) < limit; page++ {
		pageSize := limit - float64(len(out))
		if pageSize > 100 {
			pageSize = 100
		}
		batch, err := i.listTorrents(ctx, listOptions{
			Limit:  mediaorganizer.Ptr(pageSize),
			Offset: mediaorganizer.Ptr(int64(page * 100)),
		})
		if err != nil {
			return nil, err
		}
		previous := len(out)
		for _, t := range batch {
			if seen[t.Hash] {
				continue
			}
			seen[t.Hash] = true
			out = append(out, downloadclient.RatioItem{
				Sent:     t.Uploaded,
				Size:     orZero(t.Size),
				Progress: orZero(t.Progress),
				Category: categoryValue(t.Category),
			})
			if float64(len(out)) >= limit {
				break
			}
		}
		if float64(len(batch)) < pageSize || len(out) == previous {
			break
		}
	}
	return out, nil
}

// itemsOf maps raw torrents to widget items (qbittorrent-integration.ts:47-69).
func itemsOf(torrents []torrent, now func() time.Time) []downloadclient.Item {
	items := make([]downloadclient.Item, 0, len(torrents))
	for _, t := range torrents {
		progress := orZero(t.Progress)
		item := downloadclient.Item{
			Type:  downloadclient.TypeTorrent,
			ID:    t.Hash,
			Index: int64(orZero(t.Priority)),
			Name:  t.Name,
			Size:  orZero(t.Size),
			Sent:  t.Uploaded,
			// downSpeed only while not complete (:56).
			State:    stateOf(t.State),
			Progress: progress,
			Category: categoryValue(t.Category),
		}
		if progress != 1 {
			item.DownSpeed = t.Dlspeed
		}
		item.UpSpeed = t.Upspeed
		switch {
		case progress == 1:
			// Math.min(completion_on * 1000 - now, -1) — negative time since
			// completion, never 0.
			completed := orZero(t.CompletionOn) * 1000
			item.Time = mathMin(completed-float64(now().UnixMilli()), -1)
		case orZero(t.ETA) == 8640000:
			// qBittorrent sends eta=8640000 for completed torrents (:58-63).
			item.Time = 0
		default:
			item.Time = mathMax(orZero(t.ETA)*1000, 0)
		}
		item.Added = jsSecondsToMillis(t.AddedOn)
		items = append(items, item)
	}
	return items
}

// stateOf mirrors getTorrentState (qbittorrent-integration.ts:220-252).
func stateOf(state string) string {
	switch state {
	case "allocating", "checkingDL", "downloading", "forcedDL", "forcedMetaDL",
		"metaDL", "queuedDL", "queuedForChecking":
		return downloadclient.StateLeeching
	case "checkingUP", "forcedUP", "queuedUP", "uploading", "stalledUP":
		return downloadclient.StateSeeding
	case "pausedDL", "pausedUP", "stoppedDL", "stoppedUP":
		return downloadclient.StatePaused
	case "stalledDL":
		return downloadclient.StateStalled
	default: // error, checkingResumeData, missingFiles, moving, unknown, ...
		return downloadclient.StateUnknown
	}
}

// PauseQueue ports pauseQueueAsync -> client.pauseTorrent("all"): POST
// /torrents/stop (v5+) or /torrents/pause with hashes=all.
func (i *Integration) PauseQueue(ctx context.Context) error {
	return i.stopTorrent(ctx, "all")
}

// PauseItem ports pauseItemAsync({id}) -> client.pauseTorrent(id).
func (i *Integration) PauseItem(ctx context.Context, item downloadclient.Item) error {
	return i.stopTorrent(ctx, item.ID)
}

// ResumeQueue ports resumeQueueAsync -> client.resumeTorrent("all").
func (i *Integration) ResumeQueue(ctx context.Context) error {
	return i.startTorrent(ctx, "all")
}

// ResumeItem ports resumeItemAsync({id}) -> client.resumeTorrent(id).
func (i *Integration) ResumeItem(ctx context.Context, item downloadclient.Item) error {
	return i.startTorrent(ctx, item.ID)
}

// DeleteItem ports deleteItemAsync({id}, fromDisk)
// -> client.removeTorrent(id, fromDisk): POST /torrents/delete.
func (i *Integration) DeleteItem(ctx context.Context, item downloadclient.Item, fromDisk bool) error {
	return i.removeTorrent(ctx, item.ID, fromDisk)
}

// stopTorrent is client.stopTorrent (qbittorrent.js:646-654).
func (i *Integration) stopTorrent(ctx context.Context, hashes string) error {
	if err := i.ensureVersion(ctx); err != nil {
		return err
	}
	endpoint := "/torrents/pause"
	if i.isV5State() {
		endpoint = "/torrents/stop"
	}
	form := url.Values{"hashes": []string{hashes}}
	return i.request(ctx, http.MethodPost, apiPath+endpoint, nil, form, nil)
}

// startTorrent is client.startTorrent (qbittorrent.js:663-670).
func (i *Integration) startTorrent(ctx context.Context, hashes string) error {
	if err := i.ensureVersion(ctx); err != nil {
		return err
	}
	endpoint := "/torrents/resume"
	if i.isV5State() {
		endpoint = "/torrents/start"
	}
	form := url.Values{"hashes": []string{hashes}}
	return i.request(ctx, http.MethodPost, apiPath+endpoint, nil, form, nil)
}

// removeTorrent is client.removeTorrent (qbittorrent.js:681-687).
func (i *Integration) removeTorrent(ctx context.Context, hashes string, deleteFiles bool) error {
	if err := i.ensureVersion(ctx); err != nil {
		return err
	}
	form := url.Values{
		"hashes":      []string{hashes},
		"deleteFiles": []string{strconv.FormatBool(deleteFiles)},
	}
	return i.request(ctx, http.MethodPost, apiPath+"/torrents/delete", nil, form, nil)
}

// --- small helpers ---

func orZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func orEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// categoryValue mirrors `category: torrent.category`: absent in the raw
// response -> undefined (dropped), "" -> present as "".
func categoryValue(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// jsSecondsToMillis mirrors added_on * 1000 (nil -> 0 like JS null coercion).
func jsSecondsToMillis(secs *float64) *float64 {
	if secs == nil {
		return mediaorganizer.Ptr(0.0)
	}
	return mediaorganizer.Ptr(*secs * 1000)
}

func mathMin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func mathMax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// Compile-time interface checks.
var (
	_ downloadclient.Client    = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
