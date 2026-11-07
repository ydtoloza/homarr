// Package deluge is the Go port of the upstream DelugeIntegration
// (packages/integrations/src/download-client/deluge/deluge-integration.ts),
// which drives Deluge through the @ctrl/deluge library (version 8.0.0). The
// Go client reproduces the library's exact request semantics:
//
//   - every call is POST {base}/json with the JSON body
//     {"method": <method>, "params": [...], "id": <msgId++>} and the session
//     "Cookie" header (deluge.js request()),
//   - auth: auth.login with params [password] returns a Set-Cookie whose first
//     cookie becomes the session header; every authenticated request first
//     validates the session (cookie present, not expiring within 5s, and
//     auth.check_session true) and reconnects the daemon when needed
//     (web.connected -> web.get_hosts -> web.connect with the first host),
//   - torrents are listed with web.update_ui [[fields, filter]] where fields
//     is the fixed library list plus "label" plus the caller's extra fields
//     (homarr adds "completed_time"),
//   - per-request timeout 5s (@ctrl defaults, deluge.js:11).
//
// Widget mapping pinned from deluge-integration.ts:62-141 (rates floored,
// progress/100, ratioItems only with a selection) and :186-202 (state
// mapping).
package deluge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "deluge").
const Kind = "deluge"

// DefaultTimeout mirrors the @ctrl/deluge per-request timeout
// (deluge.js defaults.timeout = 5000).
const DefaultTimeout = 5 * time.Second

// jsonPath is config.path of the library (deluge.js:10).
const jsonPath = "/json"

// maxParallelMutations bounds the per-torrent pause/resume fan-out upstream
// runs with an unbounded Promise.all (deluge-integration.ts:143-166).
const maxParallelMutations = 10

// baseFields is the field list of listTorrents (deluge.js:345-371).
var baseFields = []string{
	"distributed_copies", "download_payload_rate", "eta", "is_auto_managed",
	"max_download_speed", "max_upload_speed", "name", "num_peers", "num_seeds",
	"progress", "queue", "ratio", "save_path", "seeds_peers_ratio", "state",
	"time_added", "total_done", "total_peers", "total_seeds", "total_uploaded",
	"total_wanted", "tracker_host", "upload_payload_rate",
	// if they don't have the label plugin it shouldn't fail (deluge.js:368-369)
	"label",
}

// Definition returns the metadata of the deluge kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "Deluge"}
}

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values (deluge uses the
// password only, integrationDefs deluge secretKinds [["password"]]).
type Secrets struct {
	Password string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map.
func SecretsFromMap(m map[string]string) (Secrets, error) {
	if _, ok := m["password"]; !ok {
		return Secrets{}, &mediaorganizer.MissingSecretError{Kind: "password"}
	}
	return Secrets{Password: m["password"]}, nil
}

// Integration is one Deluge instance. All methods are safe for concurrent use.
type Integration struct {
	input   integrations.Input
	secrets Secrets
	urls    mediaorganizer.URLBuilder
	http    mediaorganizer.HTTPClient
	// Now overrides the clock used for the completion-time math of the items
	// (dayjs().unix() upstream). nil means time.Now; test seam.
	Now func() time.Time

	mu       sync.Mutex
	cookie   string
	cookieEx time.Time
	msgID    int64
}

// New builds one Deluge client.
func New(input integrations.Input, secrets Secrets) *Integration {
	return &Integration{
		input:   input,
		secrets: secrets,
		urls:    mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
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

// Check implements integrations.Integration: the auth.login flow
// (testingAsync, deluge-integration.ts:49-60). A rejected login is the
// upstream UnauthorizedResult(401).
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	ok, err := i.login(ctx)
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.AsError(err).Error()}, nil
	}
	if !ok {
		return integrations.CheckResult{
			StatusCode: http.StatusUnauthorized,
			Error:      mediaorganizer.MsgResponse,
		}, nil
	}
	return integrations.CheckResult{StatusCode: http.StatusOK}, nil
}

// rpc performs one JSON-RPC call: auth preamble (when needsAuth) + the
// request. The body error member is thrown like deluge.js:557-559. out, when
// non-nil, decodes the "result" member.
func (i *Integration) rpc(ctx context.Context, method string, params []any, needsAuth bool, out any) error {
	if needsAuth {
		if err := i.validateAuth(ctx); err != nil {
			return err
		}
	}
	body, err := json.Marshal(rpcRequest{
		Method: method,
		Params: params,
		ID:     i.nextMsgID(),
	})
	if err != nil {
		return mediaorganizer.NewUnknownError(err)
	}
	u, err := i.urls.Build(jsonPath, nil)
	if err != nil {
		return mediaorganizer.NewRequestError(err)
	}
	cfg := i.http
	cfg.Timeout = DefaultTimeout
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return mediaorganizer.NewRequestError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie := i.currentCookie(); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	res, err := cfg.Std().Do(req)
	if err != nil {
		return mediaorganizer.NewRequestError(err)
	}
	defer res.Body.Close()
	var payload struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcErrorBody   `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return mediaorganizer.NewParseError(err)
	}
	if payload.Error != nil {
		// deluge.js throws a plain Error(err.message) for a body-level RPC
		// error (deluge.js:557-559); the upstream decorator has no handler
		// for it, so the classification is UNKNOWN.
		msg := payload.Error.Message
		if msg == "" {
			msg = "deluge rpc error"
		}
		return mediaorganizer.NewUnknownError(fmt.Errorf("%s", msg))
	}
	if out != nil && len(payload.Result) > 0 {
		if err := json.Unmarshal(payload.Result, out); err != nil {
			return mediaorganizer.NewParseError(err)
		}
	}
	return nil
}

type rpcRequest struct {
	Method string `json:"method"`
	Params []any  `json:"params"`
	ID     int64  `json:"id"`
}

type rpcErrorBody struct {
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// validateAuth mirrors _validateAuth + connected/connect (deluge.js:560-570,
// 84-121, 60-75): session check, then daemon connect when needed.
func (i *Integration) validateAuth(ctx context.Context) error {
	if !i.sessionValid() {
		ok, err := i.login(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return mediaorganizer.NewResponseError(fmt.Errorf("auth failed, incorrect password"))
		}
	} else {
		// checkSession pings the daemon (deluge.js:96-121).
		var valid bool
		if err := i.rpc(ctx, "auth.check_session", nil, false, &valid); err == nil && valid {
			// valid session; fall through to the connect check
		} else {
			i.resetSession()
			ok, err := i.login(ctx)
			if err != nil {
				return err
			}
			if !ok {
				return mediaorganizer.NewResponseError(fmt.Errorf("auth failed, incorrect password"))
			}
		}
	}
	// autoConnect: web.connected -> web.get_hosts -> web.connect (deluge.js:
	// 72-75 and connect at :60-71). The RPC "result" member IS the value
	// (a boolean / an array), so it decodes directly.
	var connected bool
	if err := i.rpc(ctx, "web.connected", nil, false, &connected); err == nil && connected {
		return nil
	}
	var hosts [][]any
	if err := i.rpc(ctx, "web.get_hosts", nil, false, &hosts); err != nil {
		return err
	}
	if len(hosts) == 0 || len(hosts[0]) == 0 {
		return mediaorganizer.NewResponseError(fmt.Errorf("no hosts found"))
	}
	host := hosts[0][0]
	return i.rpc(ctx, "web.connect", []any{host}, false, nil)
}

// sessionValid mirrors checkSession's cheap preconditions (deluge.js:95-105):
// a cookie that does not expire within 5 seconds.
func (i *Integration) sessionValid() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.cookie != "" && i.nowFn().Before(i.cookieEx.Add(-5*time.Second))
}

func (i *Integration) currentCookie() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.cookie
}

func (i *Integration) nextMsgID() int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.msgID >= 4096 {
		i.msgID = 0
	}
	i.msgID++
	return i.msgID - 1
}

func (i *Integration) resetSession() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cookie = ""
	i.cookieEx = time.Time{}
}

// login performs auth.login without a session cookie and captures the
// Set-Cookie (deluge.js login at :122-134 and _setAuthCookie at :581-599 —
// the FIRST cookie of the header becomes the session).
func (i *Integration) login(ctx context.Context) (bool, error) {
	body, err := json.Marshal(rpcRequest{Method: "auth.login", Params: []any{i.secrets.Password}, ID: i.nextMsgID()})
	if err != nil {
		return false, mediaorganizer.NewUnknownError(err)
	}
	u, err := i.urls.Build(jsonPath, nil)
	if err != nil {
		return false, mediaorganizer.NewRequestError(err)
	}
	cfg := i.http
	cfg.Timeout = DefaultTimeout
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return false, mediaorganizer.NewRequestError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := cfg.Std().Do(req)
	if err != nil {
		return false, mediaorganizer.NewRequestError(err)
	}
	defer res.Body.Close()
	var payload struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcErrorBody   `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return false, mediaorganizer.NewParseError(err)
	}
	if payload.Error != nil {
		msg := payload.Error.Message
		if msg == "" {
			msg = "deluge rpc error"
		}
		return false, mediaorganizer.NewResponseError(fmt.Errorf("%s", msg))
	}
	// !res.ok || !set-cookie -> Auth failed (deluge.js:125-128).
	cookies := res.Header.Values("Set-Cookie")
	if res.StatusCode < 200 || res.StatusCode >= 300 || len(cookies) == 0 {
		return false, nil
	}
	cookie, expiry := parseSessionCookie(cookies[0])
	if cookie == "" {
		return false, nil
	}
	i.mu.Lock()
	i.cookie = cookie
	i.cookieEx = expiry
	i.mu.Unlock()
	return true, nil
}

// parseSessionCookie mirrors _setAuthCookie (deluge.js:581-599): the first
// cookie string of the (possibly comma-joined) header; expires read from the
// raw text.
func parseSessionCookie(setCookie string) (string, time.Time) {
	first := setCookie
	// splitSetCookieString: split on commas that are not part of an expires
	// date; the simple approximation of splitting on ", " before "name="
	// covers the common single-cookie case.
	if idx := strings.Index(first, ";"); idx >= 0 {
		// keep attributes for expiry parsing below
	}
	pair := first
	if idx := strings.IndexByte(pair, ';'); idx >= 0 {
		pair = pair[:idx]
	}
	name := pair
	value := ""
	if idx := strings.IndexByte(pair, '='); idx >= 0 {
		name = strings.TrimSpace(pair[:idx])
		value = strings.TrimSpace(pair[idx+1:])
	}
	if name == "" || value == "" {
		return "", time.Time{}
	}
	var expiry time.Time
	if match := strings.Index(strings.ToLower(first), "expires="); match >= 0 {
		raw := first[match+len("expires="):]
		if idx := strings.IndexByte(raw, ';'); idx >= 0 {
			raw = raw[:idx]
		}
		if t, err := http.ParseTime(strings.TrimSpace(raw)); err == nil {
			expiry = t
		}
	}
	return name + "=" + value, expiry
}

// updateUIResult is the web.update_ui result member: the torrent dict plus
// the session stats (deluge-integration.ts:69-72 reads stats from the same
// response). TorrentsRaw keeps the raw dict so the entries can be replayed in
// the JSON key order — the upstream Object.entries(torrents) iterates the
// insertion order, which a Go map would shuffle.
type updateUIResult struct {
	Stats struct {
		DownloadRate *float64 `json:"download_rate"`
		UploadRate   *float64 `json:"upload_rate"`
	} `json:"stats"`
	TorrentsRaw json.RawMessage `json:"torrents"`
}

// listTorrents is client.listTorrents (deluge.js:344-371): web.update_ui with
// the fixed field list plus the caller's extra fields.
func (i *Integration) listTorrents(ctx context.Context, additionalFields []string) ([]torrent, error) {
	result, _, err := i.listTorrentsWithStats(ctx, additionalFields)
	return result, err
}

func (i *Integration) listTorrentsWithStats(ctx context.Context, additionalFields []string) ([]torrent, *updateUIStats, error) {
	fields := append(append([]string{}, baseFields...), additionalFields...)
	var result updateUIResult
	if err := i.rpc(ctx, "web.update_ui", []any{fields, map[string]any{}}, true, &result); err != nil {
		return nil, nil, err
	}
	torrents, err := parseTorrentsDict(result.TorrentsRaw)
	if err != nil {
		return nil, nil, err
	}
	stats := &updateUIStats{DownloadRate: result.Stats.DownloadRate, UploadRate: result.Stats.UploadRate}
	return torrents, stats, nil
}

// parseTorrentsDict replays the web.update_ui torrents dict in JSON key order
// (Object.entries upstream), injecting the key as the item id
// (deluge-integration.ts:73-76).
func parseTorrentsDict(raw json.RawMessage) ([]torrent, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, mediaorganizer.NewParseError(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, mediaorganizer.NewParseError(fmt.Errorf("torrents is not an object"))
	}
	out := make([]torrent, 0)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, mediaorganizer.NewParseError(err)
		}
		key, _ := keyTok.(string)
		var t torrent
		if err := dec.Decode(&t); err != nil {
			return nil, mediaorganizer.NewParseError(err)
		}
		t.ID = key
		out = append(out, t)
	}
	return out, nil
}

// updateUIStats carries the rates of web.update_ui result.stats.
type updateUIStats struct {
	DownloadRate *float64
	UploadRate   *float64
}

// GetJobsAndStatus ports getClientJobsAndStatusAsync
// (deluge-integration.ts:62-141).
func (i *Integration) GetJobsAndStatus(ctx context.Context, input downloadclient.JobsInput) (downloadclient.JobsAndStatus, error) {
	// Currently there is no way to limit the number of returned torrents
	// (:68-69); homarr requests the extra "completed_time" field (:72).
	torrents, stats, err := i.listTorrentsWithStats(ctx, []string{"completed_time"})
	if err != nil {
		return downloadclient.JobsAndStatus{}, err
	}

	paused := true
	for _, t := range torrents {
		if torrentState(t.State) != downloadclient.StatePaused {
			paused = false
			break
		}
	}
	// Math.floor on both rates (:80-83).
	var down, up float64
	if stats.DownloadRate != nil {
		down = floor(*stats.DownloadRate)
	}
	if stats.UploadRate != nil {
		up = floor(*stats.UploadRate)
	}
	status := downloadclient.Status{
		Paused: paused,
		Rates: downloadclient.Rates{
			Down: down,
			Up:   mediaorganizer.Ptr(up),
		},
		Types: []string{downloadclient.TypeTorrent},
	}
	nowSecs := i.nowFn().Unix()
	selected := downloadclient.SelectTorrents(torrents, input, delugeFields(nowSecs))
	items := make([]downloadclient.Item, 0, len(selected))
	for _, t := range selected {
		items = append(items, itemOf(t, nowSecs))
	}
	result := downloadclient.JobsAndStatus{Status: status, Items: items}
	if input.Selection != nil {
		limit := int(input.Limit)
		if limit < 0 {
			limit = 0
		}
		if limit > len(torrents) {
			limit = len(torrents)
		}
		ratioItems := make([]downloadclient.RatioItem, 0, limit)
		for _, t := range torrents[:limit] {
			ratioItems = append(ratioItems, downloadclient.RatioItem{
				Sent:     mediaorganizer.Ptr(orZero(t.TotalUploaded)),
				Size:     orZero(t.TotalWanted),
				Progress: orZero(t.Progress) / 100,
				Category: labelValue(t.Label),
			})
		}
		result.RatioItems = ratioItems
	}
	return result, nil
}

// delugeFields builds the SelectTorrents extractors of
// deluge-integration.ts:87-110.
func delugeFields(nowSecs int64) downloadclient.Fields[torrent] {
	return downloadclient.Fields[torrent]{
		Category: func(t torrent) any { return labelValue(t.Label) },
		Progress: func(t torrent) float64 { return orZero(t.Progress) / 100 },
		UpSpeed:  func(t torrent) *float64 { return t.UploadPayloadRate },
		Sort: map[string]func(torrent) any{
			"name":      func(t torrent) any { return t.Name },
			"progress":  func(t torrent) any { return orZero(t.Progress) / 100 },
			"size":      func(t torrent) any { return orZero(t.TotalWanted) },
			"added":     func(t torrent) any { return orZero(t.TimeAdded) * 1000 },
			"index":     func(t torrent) any { return orZero(t.Queue) },
			"upSpeed":   func(t torrent) any { return orZero(t.UploadPayloadRate) },
			"downSpeed": func(t torrent) any { return downSpeedOf(t) },
			"sent":      func(t torrent) any { return orZero(t.TotalUploaded) },
			"received":  func(t torrent) any { return floor(orZero(t.TotalWanted) * (orZero(t.Progress) / 100)) },
			"ratio":     func(t torrent) any { return ratioOf(t) },
			"state":     func(t torrent) any { return torrentState(t.State) },
			"time":      func(t torrent) any { return timeOf(t, nowSecs) },
		},
	}
}

// itemOf maps one torrent to the widget item
// (deluge-integration.ts:111-131).
func itemOf(t torrent, nowSecs int64) downloadclient.Item {
	item := downloadclient.Item{
		Type:     downloadclient.TypeTorrent,
		ID:       t.ID,
		Index:    int64(orZero(t.Queue)),
		Name:     t.Name,
		Size:     orZero(t.TotalWanted),
		Sent:     mediaorganizer.Ptr(orZero(t.TotalUploaded)),
		UpSpeed:  t.UploadPayloadRate,
		Time:     timeOf(t, nowSecs),
		Added:    mediaorganizer.Ptr(orZero(t.TimeAdded) * 1000),
		State:    torrentState(t.State),
		Progress: orZero(t.Progress) / 100,
		Category: labelValue(t.Label),
	}
	if orZero(t.Progress) != 100 {
		item.DownSpeed = t.DownloadPayloadRate
	}
	return item
}

// torrentState mirrors getTorrentState (deluge-integration.ts:186-202).
func torrentState(state string) string {
	switch state {
	case "Queued", "Checking", "Allocating", "Downloading":
		return downloadclient.StateLeeching
	case "Seeding":
		return downloadclient.StateSeeding
	case "Paused":
		return downloadclient.StatePaused
	default: // Error, Moving, ...
		return downloadclient.StateUnknown
	}
}

func downSpeedOf(t torrent) any {
	if orZero(t.Progress) == 100 {
		return nil
	}
	return orZero(t.DownloadPayloadRate)
}

func ratioOf(t torrent) any {
	if orZero(t.TotalWanted) <= 0 {
		return nil
	}
	return orZero(t.TotalUploaded) / orZero(t.TotalWanted)
}

// timeOf mirrors the time field (deluge-integration.ts:106-109): a complete
// torrent reports (completed_time - now) * 1000 (max -1), an active one the
// eta in ms (min 0).
func timeOf(t torrent, nowSecs int64) float64 {
	if orZero(t.Progress) == 100 {
		return mathMin((orZero(t.CompletedTime)-float64(nowSecs))*1000, -1)
	}
	return mathMax(orZero(t.ETA)*1000, 0)
}

// PauseQueue ports pauseQueueAsync (deluge-integration.ts:143-151): list the
// torrents, then core.pause_torrent per id (Promise.all upstream, bounded
// here).
func (i *Integration) PauseQueue(ctx context.Context) error {
	return i.forEachTorrent(ctx, "core.pause_torrent")
}

// ResumeQueue ports resumeQueueAsync (:158-166): core.resume_torrent per id.
func (i *Integration) ResumeQueue(ctx context.Context) error {
	return i.forEachTorrent(ctx, "core.resume_torrent")
}

func (i *Integration) forEachTorrent(ctx context.Context, method string) error {
	torrents, err := i.listTorrents(ctx, nil)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(torrents))
	for _, t := range torrents {
		ids = append(ids, t.ID)
	}
	sem := make(chan struct{}, maxParallelMutations)
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := i.rpc(ctx, method, []any{[]string{id}}, true, nil); err != nil {
				errOnce.Do(func() { firstErr = err })
			}
		}(id)
	}
	wg.Wait()
	return firstErr
}

// PauseItem ports pauseItemAsync({id}) -> core.pause_torrent [[id]]
// (deluge-integration.ts:153-156).
func (i *Integration) PauseItem(ctx context.Context, item downloadclient.Item) error {
	return i.rpc(ctx, "core.pause_torrent", []any{[]string{item.ID}}, true, nil)
}

// ResumeItem ports resumeItemAsync({id}) -> core.resume_torrent [[id]].
func (i *Integration) ResumeItem(ctx context.Context, item downloadclient.Item) error {
	return i.rpc(ctx, "core.resume_torrent", []any{[]string{item.ID}}, true, nil)
}

// DeleteItem ports deleteItemAsync({id}, fromDisk) -> core.remove_torrent
// [id, removeData] (deluge-integration.ts:173-176).
func (i *Integration) DeleteItem(ctx context.Context, item downloadclient.Item, fromDisk bool) error {
	return i.rpc(ctx, "core.remove_torrent", []any{item.ID, fromDisk}, true, nil)
}

// --- helpers and response shapes ---

// torrent is one entry of web.update_ui result.torrents (the dict key becomes
// ID, deluge-integration.ts:73-76).
type torrent struct {
	ID                  string   `json:"-"`
	Name                string   `json:"name"`
	State               string   `json:"state"`
	Progress            *float64 `json:"progress"`
	Queue               *float64 `json:"queue"`
	ETA                 *float64 `json:"eta"`
	TimeAdded           *float64 `json:"time_added"`
	TotalWanted         *float64 `json:"total_wanted"`
	TotalUploaded       *float64 `json:"total_uploaded"`
	DownloadPayloadRate *float64 `json:"download_payload_rate"`
	UploadPayloadRate   *float64 `json:"upload_payload_rate"`
	CompletedTime       *float64 `json:"completed_time"`
	Label               *string  `json:"label"`
}

// labelValue mirrors `category: torrent.label` (nil -> dropped).
func labelValue(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func orZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// floor is Math.floor for the rates and the received sort field.
func floor(f float64) float64 { return math.Floor(f) }

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
