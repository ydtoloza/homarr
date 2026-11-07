// Package sabnzbd is the Go port of the upstream SabnzbdIntegration
// (packages/integrations/src/download-client/sabnzbd/sabnzbd-integration.ts),
// which talks plain GET requests (no client library upstream):
//
//   - auth: the api key travels as the QUERY parameter "apikey"
//     (integrationDefs sabNzbd httpAuth {type: "apiKeyQuery", name: "apikey"}),
//   - every call is GET {base}/api?output=json&mode=<mode>&apikey=<key>&...
//     (sabNzbApiCallWithCustomFetchAsync, :145-173); a non-2xx answer is the
//     upstream ResponseError ("Response did not indicate success"),
//   - modes used: queue (limit), history (limit), history with
//     archive=1&start&limit (archived history paging), pause, resume,
//     queue with name=pause/resume&value=<nzo_id>, and the delete of
//     queue/history rows with name=delete&archive&value&del_files,
//   - NO per-request timeout upstream (fetchWithTrustedCertificatesAsync
//     without options.timeout); the deadline comes from the widget layer
//     (DEFAULT_REQUEST_TIMEOUT_MS = 60s).
//
// Widget mapping pinned from sabnzbd-integration.ts:27-111 (queue kbpersec is
// KiB/s -> floor(*1024), mb is MiB -> ceil(*1024*1024), timeleft "d:hh:mm:ss"
// reversed, history appended after queue then sliced to the limit) and
// sabnzbd-history.ts (archived history paging).
package sabnzbd

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "sabNzbd" — capital N, lowercase
// zbd).
const Kind = "sabNzbd"

// ArchivedHistoryPageSize / MaxArchivedHistoryPages mirror
// sabnzbd-history.ts:4-5 (pages of 100, at most 100 pages).
const (
	ArchivedHistoryPageSize = 100
	MaxArchivedHistoryPages = 100
)

// secondsPerDay mirrors SECONDS_PER_DAY (sabnzbd-history.ts:3).
const secondsPerDay = 24 * 60 * 60

// DefaultHistoryWindowDays is the procedure input default
// (packages/api/src/router/widgets/downloads.ts:89).
const DefaultHistoryWindowDays = 7

// Definition returns the metadata of the SABnzbd kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "SABnzbd"}
}

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values (sabNzbd secretKinds
// [["apiKey"]] upstream).
type Secrets struct {
	APIKey string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map.
func SecretsFromMap(m map[string]string) (Secrets, error) {
	apiKey, err := mediaorganizer.SecretValue(m, "apiKey")
	return Secrets{APIKey: apiKey}, err
}

// Integration is one SABnzbd instance. All methods are safe for concurrent use.
type Integration struct {
	input  integrations.Input
	apiKey string
	urls   mediaorganizer.URLBuilder
	http   mediaorganizer.HTTPClient
	// Now overrides the clock used for the history time math
	// (dayjs().valueOf() upstream). nil means time.Now; test seam.
	Now func() time.Time
}

func (i *Integration) nowFn() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// New builds one SABnzbd client.
func New(input integrations.Input, secrets Secrets) *Integration {
	return &Integration{
		input:  input,
		apiKey: secrets.APIKey,
		urls:   mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
		Now:    nil,
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

// apiCall ports sabNzbApiCallAsync (:142-144): GET /api with the shared
// query params plus the caller's; returns the raw JSON.
func (i *Integration) apiCall(ctx context.Context, mode string, params map[string]string) ([]byte, error) {
	query := make(map[string]string, len(params)+3)
	for k, v := range params {
		query[k] = v
	}
	query["output"] = "json"
	query["mode"] = mode
	query["apikey"] = i.apiKey
	url, err := i.urls.Build("/api", query)
	if err != nil {
		return nil, err
	}
	return i.http.DoRead(ctx, "GET", url, nil)
}

// Check implements integrations.Integration: mode=translate&value=ping, the
// cheapest api-key-guarded call (testingAsync, sabnzbd-integration.ts:21-25).
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	if i.apiKey == "" {
		return integrations.CheckResult{Error: (&mediaorganizer.MissingSecretError{Kind: "apiKey"}).Error()}, nil
	}
	_, err := i.apiCall(ctx, "translate", map[string]string{"value": "ping"})
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.AsError(err).Error()}, nil
	}
	return integrations.CheckResult{StatusCode: 200}, nil
}

// GetJobsAndStatus ports getClientJobsAndStatusAsync
// (sabnzbd-integration.ts:27-111).
func (i *Integration) GetJobsAndStatus(ctx context.Context, input downloadclient.JobsInput) (downloadclient.JobsAndStatus, error) {
	limit := mediaorganizer.FormatNumber(input.Limit)
	queueRes, err := i.queue(ctx, limit)
	if err != nil {
		return downloadclient.JobsAndStatus{}, err
	}
	historyRes, err := i.history(ctx, limit)
	if err != nil {
		return downloadclient.JobsAndStatus{}, err
	}
	historySlots := historyRes.History.Slots
	if input.IncludeArchivedHistory {
		windowDays := input.HistoryWindowDays
		if windowDays == 0 {
			windowDays = DefaultHistoryWindowDays
		}
		historySlots, err = i.historySlotsWithArchive(ctx, historySlots, windowDays)
		if err != nil {
			return downloadclient.JobsAndStatus{}, err
		}
	}

	// SABnzbd's legacy kbpersec field is kibibytes per second; normalize it
	// to bytes per second (:62-64).
	down := math.Floor(parseNonNegativeFloat(queueRes.Queue.KBPerSec) * 1024)
	status := downloadclient.Status{
		Paused: queueRes.Queue.Paused,
		Rates:  downloadclient.Rates{Down: down},
		Types:  []string{downloadclient.TypeUsenet},
	}

	nowMs := i.nowFn().UnixMilli()
	items := make([]downloadclient.Item, 0, len(queueRes.Queue.Slots)+len(historySlots))
	for _, slot := range queueRes.Queue.Slots {
		items = append(items, queueItemOf(slot, down))
	}
	for idx, slot := range historySlots {
		items = append(items, historyItemOf(slot, idx, nowMs))
	}
	// .slice(0, input.limit) after concat (:110).
	if limitInt := int(input.Limit); limitInt >= 0 && limitInt < len(items) {
		items = items[:limitInt]
	}
	return downloadclient.JobsAndStatus{Status: status, Items: items}, nil
}

// queue performs mode=queue with the given limit.
func (i *Integration) queue(ctx context.Context, limit string) (queueSchema, error) {
	raw, err := i.apiCall(ctx, "queue", map[string]string{"limit": limit})
	if err != nil {
		return queueSchema{}, err
	}
	var out queueSchema
	if err := strictUnmarshal(raw, &out); err != nil {
		return queueSchema{}, mediaorganizer.NewParseError(err)
	}
	return out, nil
}

// history performs mode=history with the given limit.
func (i *Integration) history(ctx context.Context, limit string) (historySchema, error) {
	raw, err := i.apiCall(ctx, "history", map[string]string{"limit": limit})
	if err != nil {
		return historySchema{}, err
	}
	var out historySchema
	if err := strictUnmarshal(raw, &out); err != nil {
		return historySchema{}, mediaorganizer.NewParseError(err)
	}
	return out, nil
}

// historySlotsWithArchive ports getSabnzbdHistorySlotsAsync
// (sabnzbd-history.ts:16-49): processing slots first, then the
// completed/failed rows of the window sorted by completion descending,
// deduped by nzo_id.
func (i *Integration) historySlotsWithArchive(ctx context.Context, activeSlots []historySlot, historyWindowDays int64) ([]historySlot, error) {
	cutoff := math.Floor(float64(i.nowFn().UnixMilli())/1000) - float64(historyWindowDays*secondsPerDay)
	archived := make([]historySlot, 0)
	for page := 0; page < MaxArchivedHistoryPages; page++ {
		raw, err := i.apiCall(ctx, "history", map[string]string{
			"archive": "1",
			"start":   strconv.Itoa(page * ArchivedHistoryPageSize),
			"limit":   strconv.Itoa(ArchivedHistoryPageSize),
		})
		if err != nil {
			return nil, err
		}
		var out historySchema
		if err := strictUnmarshal(raw, &out); err != nil {
			return nil, mediaorganizer.NewParseError(err)
		}
		archived = append(archived, out.History.Slots...)
		slots := out.History.Slots
		if len(slots) < ArchivedHistoryPageSize || allOlderThan(slots, cutoff) {
			break
		}
	}
	return mergeHistorySlots(activeSlots, archived, cutoff), nil
}

// mergeHistorySlots is the tail of getSabnzbdHistorySlotsAsync
// (sabnzbd-history.ts:37-48).
func mergeHistorySlots(activeSlots, archived []historySlot, cutoff float64) []historySlot {
	seen := map[string]bool{}
	out := make([]historySlot, 0, len(activeSlots)+len(archived))
	// activeSlots.filter(isProcessingSlot)
	for _, slot := range activeSlots {
		if isProcessingSlot(slot) && !seen[slot.NzoID] {
			seen[slot.NzoID] = true
			out = append(out, slot)
		}
	}
	// [...activeSlots, ...archived].filter(!processing && completed >= cutoff)
	// .toSorted((a, b) => b.completed - a.completed)
	combined := make([]historySlot, 0, len(activeSlots)+len(archived))
	for _, slot := range activeSlots {
		if !isProcessingSlot(slot) && slot.Completed >= cutoff {
			combined = append(combined, slot)
		}
	}
	for _, slot := range archived {
		if !isProcessingSlot(slot) && slot.Completed >= cutoff {
			combined = append(combined, slot)
		}
	}
	// toSorted is stable; sort by completed DESC keeping the equal order.
	for a := 1; a < len(combined); a++ {
		for b := a; b > 0 && combined[b].Completed > combined[b-1].Completed; b-- {
			combined[b], combined[b-1] = combined[b-1], combined[b]
		}
	}
	for _, slot := range combined {
		if !seen[slot.NzoID] {
			seen[slot.NzoID] = true
			out = append(out, slot)
		}
	}
	return out
}

func isProcessingSlot(slot historySlot) bool {
	return slot.Status != "Completed" && slot.Status != "Failed"
}

func allOlderThan(slots []historySlot, cutoff float64) bool {
	for _, slot := range slots {
		if slot.Completed >= cutoff {
			return false
		}
	}
	return true
}

// queueItemOf maps a queue slot (sabnzbd-integration.ts:67-92). Note there is
// NO added timestamp upstream ("Only part from all integrations that is
// missing the timestamp", :87).
func queueItemOf(slot queueSlot, downRate float64) downloadclient.Item {
	var downSpeed float64
	if slot.Index > 0 {
		downSpeed = 0
	} else {
		downSpeed = downRate
	}
	// size = Math.ceil(parseFloat(mb) * 1024 * 1024) — "Actually rounded MiB".
	size := math.Ceil(parseNonNegativeFloat(slot.MB) * 1024 * 1024)
	return downloadclient.Item{
		Type:      downloadclient.TypeUsenet,
		ID:        slot.NzoID,
		Index:     int64(slot.Index),
		Name:      slot.Filename,
		Size:      size,
		DownSpeed: mediaorganizer.Ptr(downSpeed),
		Time:      timeLeftMillis(slot.TimeLeft),
		State:     queueState(slot.Status),
		Progress:  parseNonNegativeFloat(slot.Percentage) / 100,
		Category:  strValue(slot.Cat),
	}
}

// historyItemOf maps a history slot (sabnzbd-integration.ts:94-108): index is
// the position in the merged history list, progress is always 1.
func historyItemOf(slot historySlot, index int, nowMs int64) downloadclient.Item {
	return downloadclient.Item{
		Type:     downloadclient.TypeUsenet,
		ID:       slot.NzoID,
		Index:    int64(index),
		Name:     slot.Name,
		Size:     slot.Bytes,
		Time:     float64(slot.Completed*1000) - float64(nowMs),
		Added:    mediaorganizer.Ptr(float64((slot.Completed - slot.DownloadTime - slot.PostprocTime) * 1000)),
		State:    historyState(slot.Status),
		Progress: 1,
		Category: strValue(slot.Category),
	}
}

// queueState mirrors getUsenetQueueState (sabnzbd-integration.ts:175-184).
func queueState(status string) string {
	switch status {
	case "Queued":
		return downloadclient.StateQueued
	case "Paused":
		return downloadclient.StatePaused
	default:
		return downloadclient.StateDownloading
	}
}

// historyState mirrors getUsenetHistoryState (sabnzbd-integration.ts:186-195).
func historyState(status string) string {
	switch status {
	case "Completed":
		return downloadclient.StateCompleted
	case "Failed":
		return downloadclient.StateFailed
	default:
		return downloadclient.StateProcessing
	}
}

// timeLeftMillis parses the SABnzbd "d:hh:mm:ss" timeleft (:70-78):
// split(":").reverse() -> [seconds, minutes, hours, days].
func timeLeftMillis(timeleft string) float64 {
	parts := strings.Split(timeleft, ":")
	// reverse
	for a, b := 0, len(parts)-1; a < b; a, b = a+1, b-1 {
		parts[a], parts[b] = parts[b], parts[a]
	}
	num := func(idx int) float64 {
		if idx >= len(parts) {
			return 0
		}
		return parseNonNegativeFloat(parts[idx])
	}
	return num(3)*86400000 + num(2)*3600000 + num(1)*60000 + num(0)*1000
}

// parseNonNegativeFloat is parseFloat with 0 for garbage (JS parseFloat
// yields NaN upstream; JSON cannot carry NaN, so 0 is the faithful
// representable value).
func parseNonNegativeFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

func strValue(s string) any { return s }

// PauseQueue ports pauseQueueAsync -> mode=pause (:114-116).
func (i *Integration) PauseQueue(ctx context.Context) error {
	_, err := i.apiCall(ctx, "pause", nil)
	return err
}

// ResumeQueue ports resumeQueueAsync -> mode=resume (:122-124).
func (i *Integration) ResumeQueue(ctx context.Context) error {
	_, err := i.apiCall(ctx, "resume", nil)
	return err
}

// PauseItem ports pauseItemAsync({id}) -> mode=queue name=pause value=<id>
// (:118-120).
func (i *Integration) PauseItem(ctx context.Context, item downloadclient.Item) error {
	_, err := i.apiCall(ctx, "queue", map[string]string{"name": "pause", "value": item.ID})
	return err
}

// ResumeItem ports resumeItemAsync({id}) -> mode=queue name=resume value=<id>
// (:126-128).
func (i *Integration) ResumeItem(ctx context.Context, item downloadclient.Item) error {
	_, err := i.apiCall(ctx, "queue", map[string]string{"name": "resume", "value": item.ID})
	return err
}

// DeleteItem ports deleteItemAsync({id, progress}, fromDisk)
// (sabnzbd-integration.ts:130-140): active rows delete via mode=queue,
// completed ones via mode=history; fromDisk maps to archive=0&del_files=1
// (keep the archive, remove the files).
func (i *Integration) DeleteItem(ctx context.Context, item downloadclient.Item, fromDisk bool) error {
	mode := "queue"
	if item.Progress == 1 {
		mode = "history"
	}
	archive := "1"
	delFiles := "0"
	if fromDisk {
		archive = "0"
		delFiles = "1"
	}
	_, err := i.apiCall(ctx, mode, map[string]string{
		"name":      "delete",
		"archive":   archive,
		"value":     item.ID,
		"del_files": delFiles,
	})
	return err
}

// --- upstream response shapes (sabnzbd-schema.ts) ---

// queueSchema mirrors queueSchema (sabnzbd-schema.ts:3-20).
type queueSchema struct {
	Queue struct {
		Paused   bool        `json:"paused"`
		KBPerSec string      `json:"kbpersec"`
		Slots    []queueSlot `json:"slots"`
	} `json:"queue"`
}

type queueSlot struct {
	Status     string  `json:"status"`
	Index      float64 `json:"index"`
	MB         string  `json:"mb"`
	Filename   string  `json:"filename"`
	Cat        string  `json:"cat"`
	TimeLeft   string  `json:"timeleft"`
	Percentage string  `json:"percentage"`
	NzoID      string  `json:"nzo_id"`
}

// historySchema mirrors historySchema (sabnzbd-schema.ts:22-37).
type historySchema struct {
	History struct {
		Slots []historySlot `json:"slots"`
	} `json:"history"`
}

type historySlot struct {
	Category     string  `json:"category"`
	DownloadTime float64 `json:"download_time"`
	Status       string  `json:"status"`
	Completed    float64 `json:"completed"`
	NzoID        string  `json:"nzo_id"`
	PostprocTime float64 `json:"postproc_time"`
	Name         string  `json:"name"`
	Bytes        float64 `json:"bytes"`
}

// strictUnmarshal decodes with zod's tolerance: unknown keys are IGNORED
// (z.object is not strict upstream).
func strictUnmarshal(raw []byte, out any) error {
	return json.Unmarshal(raw, out)
}

// Compile-time interface checks.
var (
	_ downloadclient.Client    = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
