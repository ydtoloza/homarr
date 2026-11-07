// Package transmission is the Go port of the upstream TransmissionIntegration
// (packages/integrations/src/download-client/transmission/transmission-integration.ts),
// which drives Transmission through the @ctrl/transmission library (version
// 7.4.0). The Go client reproduces the library's exact request semantics:
//
//   - every call is POST {base}/transmission/rpc with the JSON body
//     {"method": <rpc-method>, "arguments": {...}} and Content-Type
//     application/json (transmission.js request()),
//   - auth: "Authorization: Basic ..." when username/password are configured;
//     the X-Transmission-Session-Id header carries the session token, which is
//     obtained lazily: any request answering 409 stores the
//     x-transmission-session-id RESPONSE header and retries ONCE; when the
//     session is unknown, session-get runs first (it is the only method the
//     library sends without a prior getSession),
//   - per-request timeout 5s (@ctrl defaults, transmission.js:13),
//   - torrent-get is sent with an explicit field list (transmission.js:222+);
//     the Go client requests exactly the fields the widget mapping consumes.
//
// Widget mapping pinned from transmission-integration.ts:31-129 (rates and
// paused over the FULL torrent list, selection via selectDownloadTorrents,
// ratioItems only when a selection is applied) and :140-156 (status numbers to
// widget states).
package transmission

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "transmission").
const Kind = "transmission"

// DefaultTimeout mirrors the @ctrl/transmission per-request timeout
// (transmission.js defaults.timeout = 5000).
const DefaultTimeout = 5 * time.Second

// rpcPath is config.path of the library (transmission.js:8).
const rpcPath = "/transmission/rpc"

// Definition returns the metadata of the transmission kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "Transmission"}
}

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values (upstream
// transmission uses username/password only, integrationDefs secretKinds
// [["username","password"]]).
type Secrets struct {
	Username string
	Password string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map.
func SecretsFromMap(m map[string]string) (Secrets, error) {
	s := Secrets{Username: m["username"], Password: m["password"]}
	if m["username"] == "" {
		return s, &mediaorganizer.MissingSecretError{Kind: "username"}
	}
	if m["password"] == "" {
		return s, &mediaorganizer.MissingSecretError{Kind: "password"}
	}
	return s, nil
}

// Integration is one Transmission instance. All methods are safe for
// concurrent use.
type Integration struct {
	input   integrations.Input
	secrets Secrets
	urls    mediaorganizer.URLBuilder
	http    mediaorganizer.HTTPClient
	// Now overrides the clock used for the completion-time math of the items
	// (dayjs().valueOf() upstream). nil means time.Now; test seam.
	Now func() time.Time

	mu        sync.Mutex
	sessionID string
}

// sessionLocked returns the current session id ("" when unknown).
func (i *Integration) sessionLocked() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.sessionID
}

func (i *Integration) setSessionID(sid string) {
	i.mu.Lock()
	i.sessionID = sid
	i.mu.Unlock()
}

func (i *Integration) nowFn() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// New builds one Transmission client.
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

// Check implements integrations.Integration: getSession
// (testingAsync, transmission-integration.ts:23-29 — session-get succeeds for
// any reachable daemon; 409 session handshake is retried internally).
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	_, err := i.rpcCall(ctx, "session-get", map[string]any{}, nil)
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.AsError(err).Error()}, nil
	}
	return integrations.CheckResult{StatusCode: http.StatusOK}, nil
}

// rpc performs one JSON-RPC call with the 409 session-id handshake of the
// library (transmission.js request(): retry ONCE after capturing
// x-transmission-session-id). out, when non-nil, decodes response.arguments.
func (i *Integration) rpc(ctx context.Context, method string, args map[string]any, out any) error {
	_, err := i.rpcCall(ctx, method, args, out)
	return err
}

// rpcCall performs one call and returns the HTTP status (for Check).
func (i *Integration) rpcCall(ctx context.Context, method string, args map[string]any, out any) (int, error) {
	if i.sessionLocked() == "" && method != "session-get" {
		// transmission.js:292-294 — bootstrap the session first.
		if _, err := i.rpcCall(ctx, "session-get", map[string]any{}, nil); err != nil {
			return 0, err
		}
	}
	status, err := i.rpcOnce(ctx, method, args, out)
	if err != nil {
		return status, err
	}
	if status == http.StatusConflict {
		// The 409 response header carries the fresh session id; retry the
		// original request exactly once (transmission.js:327-333).
		return i.rpcOnce(ctx, method, args, out)
	}
	return status, nil
}

func (i *Integration) rpcOnce(ctx context.Context, method string, args map[string]any, out any) (int, error) {
	u, err := i.urls.Build(rpcPath, nil)
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	body, err := json.Marshal(struct {
		Method    string         `json:"method"`
		Arguments map[string]any `json:"arguments"`
	}{Method: method, Arguments: args})
	if err != nil {
		return 0, mediaorganizer.NewUnknownError(err)
	}
	cfg := i.http
	cfg.Timeout = DefaultTimeout
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sid := i.sessionLocked(); sid != "" {
		req.Header.Set("X-Transmission-Session-Id", sid)
	}
	if i.secrets.Username != "" || i.secrets.Password != "" {
		req.Header.Set("Authorization", "Basic "+basicAuth(i.secrets.Username, i.secrets.Password))
	}
	res, err := cfg.Std().Do(req)
	if err != nil {
		return 0, mediaorganizer.NewRequestError(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		if sid := res.Header.Get("x-transmission-session-id"); sid != "" {
			i.setSessionID(sid)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode, mediaorganizer.NewResponseError(fmt.Errorf("status %d for %s", res.StatusCode, method))
	}
	if out != nil {
		var payload struct {
			Result    string          `json:"result"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
			return res.StatusCode, mediaorganizer.NewParseError(err)
		}
		if payload.Result != "success" {
			return res.StatusCode, mediaorganizer.NewResponseError(fmt.Errorf("rpc result %q for %s", payload.Result, method))
		}
		if err := json.Unmarshal(payload.Arguments, out); err != nil {
			return res.StatusCode, mediaorganizer.NewParseError(err)
		}
	}
	return res.StatusCode, nil
}

// GetJobsAndStatus ports getClientJobsAndStatusAsync
// (transmission-integration.ts:31-102).
func (i *Integration) GetJobsAndStatus(ctx context.Context, input downloadclient.JobsInput) (downloadclient.JobsAndStatus, error) {
	// Currently there is no way to limit the number of returned torrents
	// (:37-38): the full torrent-get answer is fetched.
	torrents, err := i.listTorrents(ctx)
	if err != nil {
		return downloadclient.JobsAndStatus{}, err
	}
	var down, up float64
	paused := true
	for _, t := range torrents {
		down += orZero(t.RateDownload)
		up += orZero(t.RateUpload)
		if torrentState(int(orZero(f64(t.Status)))) != downloadclient.StatePaused {
			paused = false
		}
	}
	status := downloadclient.Status{
		Paused: paused,
		Rates:  downloadclient.Rates{Down: down, Up: mediaorganizer.Ptr(up)},
		Types:  []string{downloadclient.TypeTorrent},
	}
	now := i.nowFn().UnixMilli()
	selected := downloadclient.SelectTorrents(torrents, input, transmissionFields(now))
	items := make([]downloadclient.Item, 0, len(selected))
	for _, t := range selected {
		items = append(items, itemOf(t, now))
	}
	result := downloadclient.JobsAndStatus{Status: status, Items: items}
	if input.Selection != nil {
		// ratioItems = torrents.slice(0, limit) mapped (:95-100).
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
				Sent:     t.UploadedEver,
				Size:     orZero(t.TotalSize),
				Progress: orZero(t.PercentDone),
				Category: labelsValue(t.Labels),
			})
		}
		result.RatioItems = ratioItems
	}
	return result, nil
}

// transmissionFields builds the SelectTorrents extractors of
// transmission-integration.ts:47-70.
func transmissionFields(nowMs int64) downloadclient.Fields[torrent] {
	return downloadclient.Fields[torrent]{
		Category: func(t torrent) any { return labelsValue(t.Labels) },
		Progress: func(t torrent) float64 { return orZero(t.PercentDone) },
		UpSpeed:  func(t torrent) *float64 { return t.RateUpload },
		Sort: map[string]func(torrent) any{
			"name":      func(t torrent) any { return t.Name },
			"progress":  func(t torrent) any { return orZero(t.PercentDone) },
			"size":      func(t torrent) any { return orZero(t.TotalSize) },
			"added":     func(t torrent) any { return orZero(t.AddedDate) * 1000 },
			"index":     func(t torrent) any { return orZero(t.QueuePosition) },
			"upSpeed":   func(t torrent) any { return orZero(t.RateUpload) },
			"downSpeed": func(t torrent) any { return downSpeedOf(t) },
			"sent":      func(t torrent) any { return orZero(t.UploadedEver) },
			"received":  func(t torrent) any { return orZero(t.DownloadedEver) },
			"ratio":     func(t torrent) any { return ratioOf(t) },
			"state":     func(t torrent) any { return torrentState(int(orZero(f64(t.Status)))) },
			"time":      func(t torrent) any { return timeOf(t, nowMs) },
		},
	}
}

// itemOf maps one torrent to the widget item
// (transmission-integration.ts:71-92).
func itemOf(t torrent, nowMs int64) downloadclient.Item {
	item := downloadclient.Item{
		Type:     downloadclient.TypeTorrent,
		ID:       t.HashString,
		Index:    int64(orZero(t.QueuePosition)),
		Name:     t.Name,
		Size:     orZero(t.TotalSize),
		Sent:     t.UploadedEver,
		Received: t.DownloadedEver,
		UpSpeed:  t.RateUpload,
		Time:     timeOf(t, nowMs),
		Added:    mediaorganizer.Ptr(orZero(t.AddedDate) * 1000),
		State:    torrentState(int(orZero(f64(t.Status)))),
		Progress: orZero(t.PercentDone),
		Category: labelsValue(t.Labels),
	}
	if orZero(t.PercentDone) != 1 {
		item.DownSpeed = t.RateDownload
	}
	return item
}

// torrentState mirrors getTorrentState (transmission-integration.ts:140-156):
// 0 paused; 1,3 stalled; 2,4 leeching; 5,6 seeding; else unknown.
func torrentState(status int) string {
	switch status {
	case 0:
		return downloadclient.StatePaused
	case 1, 3:
		return downloadclient.StateStalled
	case 2, 4:
		return downloadclient.StateLeeching
	case 5, 6:
		return downloadclient.StateSeeding
	default:
		return downloadclient.StateUnknown
	}
}

func downSpeedOf(t torrent) any {
	if orZero(t.PercentDone) == 1 {
		return nil
	}
	return orZero(t.RateDownload)
}

func ratioOf(t torrent) any {
	if orZero(t.TotalSize) <= 0 {
		return nil
	}
	return orZero(t.UploadedEver) / orZero(t.TotalSize)
}

// timeOf mirrors the time field (transmission-integration.ts:66-69): a
// complete torrent reports the time since completion (max -1), an active one
// the eta in ms (min 0).
func timeOf(t torrent, nowMs int64) float64 {
	if orZero(t.PercentDone) == 1 {
		return mathMin(orZero(t.DoneDate)*1000-float64(nowMs), -1)
	}
	return mathMax(orZero(t.ETA)*1000, 0)
}

// listTorrents is client.listTorrents (torrent-get with the library's field
// list reduced to the mapped fields).
func (i *Integration) listTorrents(ctx context.Context) ([]torrent, error) {
	var args struct {
		Torrents []torrent `json:"torrents"`
	}
	fields := []string{
		"id", "name", "hashString", "totalSize", "percentDone", "rateDownload",
		"rateUpload", "eta", "addedDate", "doneDate", "queuePosition",
		"uploadedEver", "downloadedEver", "status", "labels",
	}
	err := i.rpc(ctx, "torrent-get", map[string]any{"fields": fields}, &args)
	if err != nil {
		return nil, err
	}
	return args.Torrents, nil
}

// PauseQueue ports pauseQueueAsync (transmission-integration.ts:104-108):
// collect every hash, then torrent-stop with the id array.
func (i *Integration) PauseQueue(ctx context.Context) error {
	torrents, err := i.listTorrents(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(torrents))
	for _, t := range torrents {
		ids = append(ids, t.HashString)
	}
	return i.rpc(ctx, "torrent-stop", map[string]any{"ids": ids}, nil)
}

// PauseItem ports pauseItemAsync({id}) -> torrent-stop with [id]
// (transmission-integration.ts:110-113). _handleNormalizedIds turns a string
// id into a one-element array.
func (i *Integration) PauseItem(ctx context.Context, item downloadclient.Item) error {
	return i.rpc(ctx, "torrent-stop", map[string]any{"ids": []string{item.ID}}, nil)
}

// ResumeQueue ports resumeQueueAsync (:115-119): every hash, torrent-start.
func (i *Integration) ResumeQueue(ctx context.Context) error {
	torrents, err := i.listTorrents(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(torrents))
	for _, t := range torrents {
		ids = append(ids, t.HashString)
	}
	return i.rpc(ctx, "torrent-start", map[string]any{"ids": ids}, nil)
}

// ResumeItem ports resumeItemAsync({id}) -> torrent-start with [id].
func (i *Integration) ResumeItem(ctx context.Context, item downloadclient.Item) error {
	return i.rpc(ctx, "torrent-start", map[string]any{"ids": []string{item.ID}}, nil)
}

// DeleteItem ports deleteItemAsync({id}, fromDisk) -> torrent-remove with
// delete-local-data (transmission-integration.ts:126-129).
func (i *Integration) DeleteItem(ctx context.Context, item downloadclient.Item, fromDisk bool) error {
	return i.rpc(ctx, "torrent-remove", map[string]any{
		"ids":               []string{item.ID},
		"delete-local-data": fromDisk,
	}, nil)
}

// --- helpers and response shapes ---

// torrent is the raw torrent-get entry the widget mapping consumes.
type torrent struct {
	ID             *float64 `json:"id"`
	Name           string   `json:"name"`
	HashString     string   `json:"hashString"`
	TotalSize      *float64 `json:"totalSize"`
	PercentDone    *float64 `json:"percentDone"`
	RateDownload   *float64 `json:"rateDownload"`
	RateUpload     *float64 `json:"rateUpload"`
	ETA            *float64 `json:"eta"`
	AddedDate      *float64 `json:"addedDate"`
	DoneDate       *float64 `json:"doneDate"`
	QueuePosition  *float64 `json:"queuePosition"`
	UploadedEver   *float64 `json:"uploadedEver"`
	DownloadedEver *float64 `json:"downloadedEver"`
	Status         *float64 `json:"status"`
	Labels         []string `json:"labels"`
}

// labelsValue mirrors `category: torrent.labels`: absent labels -> undefined
// (dropped), [] -> present as [].
func labelsValue(labels []string) any {
	if labels == nil {
		return nil
	}
	return labels
}

func orZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// f64 lifts a *float64 (the Status numeric enum decodes as number).
func f64(v *float64) *float64 { return v }

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

func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

// Compile-time interface checks.
var (
	_ downloadclient.Client    = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
