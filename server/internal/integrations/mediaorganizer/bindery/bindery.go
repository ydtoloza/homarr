// Package bindery is the Go port of the upstream BinderyIntegration
// (packages/integrations/src/media-organizer/bindery/bindery-integration.ts).
// Bindery is a Sonarr/Radarr-style manager for ebooks and audiobooks whose
// missing/queue endpoints do NOT paginate server-side: both methods fetch the
// full response and page it client-side (upstream doc comment, :15-20).
//
// Request semantics pinned from bindery-integration.ts:
//   - auth: header "X-Api-Key: <apiKey secret>" (integrationDefs bindery
//     httpAuth {type: "apiKeyHeader", name: "X-Api-Key"}, secretKinds
//     [["apiKey"]]),
//   - missing: GET /api/v1/wanted/missing — a BARE ARRAY (no {items,total}
//     wrapper); totalCount = array length; items = slice(0, pageSize),
//   - queue: GET /api/v1/queue — an array OR {items: [...]}; rows with status
//     "imported" are excluded (historical log, :62-66); totalCount counts the
//     ACTIVE rows after filtering,
//   - links: externalUrl(/book/<id>), fallback /queue for queue records
//     without a book id.
package bindery

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "bindery").
const Kind = "bindery"

// Definition returns the metadata of the bindery kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "Bindery"}
}

// init self-registers the kind into the default integrations registry (same
// note as sonarr: no secrets in integrations.Input yet, so the registered
// factory builds an unauthenticated client whose Check reports the
// missing-secret error).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values bindery consumes.
type Secrets struct {
	// APIKey is the secret of kind "apiKey" (required by every call).
	APIKey string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map.
func SecretsFromMap(m map[string]string) (Secrets, error) {
	apiKey, err := mediaorganizer.SecretValue(m, "apiKey")
	return Secrets{APIKey: apiKey}, err
}

// Integration is one Bindery instance. All methods are safe for concurrent use.
type Integration struct {
	input  integrations.Input
	apiKey string
	urls   mediaorganizer.URLBuilder
	http   mediaorganizer.HTTPClient
}

// New builds one Bindery client.
func New(input integrations.Input, secrets Secrets) *Integration {
	return &Integration{
		input:  input,
		apiKey: secrets.APIKey,
		urls:   mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
	}
}

// NewFromRef builds the client from a widget-router IntegrationRef.
func NewFromRef(ref mediaorganizer.IntegrationRef) (*Integration, error) {
	secrets, err := SecretsFromMap(ref.Secrets)
	if err != nil {
		return nil, err
	}
	return New(ref.Input(), secrets), nil
}

// Check implements integrations.Integration: GET {url}/api/v1/system/status
// with the X-Api-Key header (testingAsync, bindery-integration.ts:101-110 —
// unlike the *arr apps, Bindery has no bare /api endpoint).
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	header, err := i.apiHeader()
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	url, err := i.urls.Build("/api/v1/system/status", nil)
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	res, err := i.http.Do(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.MsgRequest}, nil
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return integrations.CheckResult{
			StatusCode: res.StatusCode,
			Error:      mediaorganizer.MsgResponse,
		}, nil
	}
	return integrations.CheckResult{StatusCode: res.StatusCode}, nil
}

func (i *Integration) apiHeader() (http.Header, error) {
	if i.apiKey == "" {
		return nil, &mediaorganizer.MissingSecretError{Kind: "apiKey"}
	}
	return http.Header{"X-Api-Key": []string{i.apiKey}}, nil
}

// GetMissing ports getMissingAsync (bindery-integration.ts:22-50).
func (i *Integration) GetMissing(ctx context.Context, pageSize int) (mediaorganizer.MissingResult, error) {
	header, err := i.apiHeader()
	if err != nil {
		return mediaorganizer.MissingResult{}, err
	}
	url, err := i.urls.Build("/api/v1/wanted/missing", nil)
	if err != nil {
		return mediaorganizer.MissingResult{}, err
	}
	var books []missingBook
	if err := i.http.DoJSON(ctx, http.MethodGet, url, header, nil, &books); err != nil {
		return mediaorganizer.MissingResult{}, err
	}
	items := make([]mediaorganizer.MissingItem, 0, len(books))
	for _, b := range books {
		if len(items) >= pageSize {
			break
		}
		item := mediaorganizer.MissingItem{
			ID:          b.ID,
			Title:       b.Title,
			Type:        mediaorganizer.TypeBook,
			Year:        releaseYear(b.ReleaseDate),
			SeriesTitle: authorName(b.Author),
			ImageURL:    httpImageURL(b.ImageURL),
			Link:        i.link("/book/" + mediaorganizer.FormatNumber(float64(b.ID))),
		}
		items = append(items, item)
	}
	return mediaorganizer.MissingResult{Items: items, TotalCount: int64(len(books))}, nil
}

// GetMediaQueue ports getMediaQueueAsync (bindery-integration.ts:52-93).
func (i *Integration) GetMediaQueue(ctx context.Context, pageSize int) (mediaorganizer.QueueResult, error) {
	header, err := i.apiHeader()
	if err != nil {
		return mediaorganizer.QueueResult{}, err
	}
	url, err := i.urls.Build("/api/v1/queue", nil)
	if err != nil {
		return mediaorganizer.QueueResult{}, err
	}
	raw, err := i.http.DoRead(ctx, http.MethodGet, url, header)
	if err != nil {
		return mediaorganizer.QueueResult{}, err
	}
	// The endpoint returns either a bare array or {items: [...]} (upstream
	// z.union, :58-61); decode the union manually.
	queueItems, err := decodeQueueItems(raw)
	if err != nil {
		return mediaorganizer.QueueResult{}, err
	}
	// Bindery returns its entire historical log; exclude the already
	// imported rows so the widget shows what is actually active (:62-66).
	active := make([]queueItem, 0, len(queueItems))
	for _, q := range queueItems {
		if q.Status != "imported" {
			active = append(active, q)
		}
	}
	items := make([]mediaorganizer.QueuedItem, 0, len(active))
	for _, q := range active {
		if len(items) >= pageSize {
			break
		}
		// percentComplete = Math.round(parseFloat(percentage)) (0 when
		// absent); a 100%-complete item has no meaningful time remaining,
		// so a stale timeLeft is dropped (:70-76).
		percent := parsePercent(q.Percentage)
		var timeLeft *string
		if percent < 100 {
			timeLeft = q.TimeLeft
		}
		title := q.Title
		if q.Book != nil {
			title = q.Book.Title
		}
		item := mediaorganizer.QueuedItem{
			ID:              q.ID,
			Title:           title,
			Type:            mediaorganizer.TypeBook,
			Status:          q.Status,
			TimeLeft:        timeLeft,
			PercentComplete: percent,
			SeriesTitle:     queueAuthorName(q.Book),
			ImageURL:        nil, // the queue endpoint returns no cover art
			Link:            i.link(queuePath(q.Book)),
		}
		items = append(items, item)
	}
	return mediaorganizer.QueueResult{Items: items, TotalCount: int64(len(active))}, nil
}

// decodeQueueItems accepts the upstream union: array | {items: array}.
func decodeQueueItems(raw []byte) ([]queueItem, error) {
	var arr []queueItem
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var wrapped struct {
		Items []queueItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, mediaorganizer.NewParseError(err)
	}
	return wrapped.Items, nil
}

// link is externalUrl(path); malformed-base fallback mirrors sonarr/radarr.
func (i *Integration) link(path string) string {
	link, err := i.urls.BuildExternal(path, nil)
	if err != nil {
		base := i.urls.ExternalURL
		if base == "" {
			base = i.urls.URL
		}
		return base + path
	}
	return link
}

func authorName(a *binderyAuthor) *string {
	if a == nil {
		return nil
	}
	return mediaorganizer.Ptr(a.AuthorName)
}

func queueAuthorName(b *queueBook) *string {
	if b == nil || b.AuthorName == nil {
		return nil
	}
	return b.AuthorName
}

// queuePath is `item.book?.id ? /book/<id> : /queue` (:87-89).
func queuePath(b *queueBook) string {
	if b != nil && b.ID != nil {
		return "/book/" + mediaorganizer.FormatNumber(float64(*b.ID))
	}
	return "/queue"
}

// httpImageURL passes through only absolute provider URLs (upstream :40-45):
// the missing endpoint returns direct unauthenticated image URLs while other
// endpoints return Bindery-proxied paths that would 401 in the browser.
func httpImageURL(u *string) *string {
	if u == nil || !strings.HasPrefix(*u, "http") {
		return nil
	}
	return u
}

// releaseYear mirrors `book.releaseDate ? new Date(book.releaseDate)
// .getUTCFullYear() : undefined` (:39). Only the calendar year survives, so
// a handful of accepted layouts is enough; an unparseable date omits the
// year (upstream would produce NaN, which cannot travel over JSON).
func releaseYear(date *string) *int64 {
	if date == nil || *date == "" {
		return nil
	}
	layouts := []string{time.RFC3339, "2006-01-02", "2006-01-02T15:04:05", "2006-01-02 15:04:05"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, *date); err == nil {
			return mediaorganizer.Ptr(int64(t.UTC().Year()))
		}
	}
	return nil
}

// parsePercent is Math.round(parseFloat(percentage)) with 0 when absent or
// unparseable (upstream parseFloat -> NaN would poison Math.round; JSON
// cannot carry NaN, so 0 is the faithful representable value).
func parsePercent(percentage *string) int64 {
	if percentage == nil {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(*percentage), 64)
	if err != nil {
		return 0
	}
	return mediaorganizer.Round(f)
}

// --- upstream response shapes (bindery-integration.ts:113-140) ---

type binderyAuthor struct {
	AuthorName string `json:"authorName"`
}

// missingBook mirrors binderyMissingBookSchema (:113-125): releaseDate and
// imageUrl optional AND nullable; author optional.
type missingBook struct {
	ID          int64          `json:"id"`
	Title       string         `json:"title"`
	ReleaseDate *string        `json:"releaseDate"`
	ImageURL    *string        `json:"imageUrl"`
	Author      *binderyAuthor `json:"author"`
}

type queueBook struct {
	ID         *int64  `json:"id"`
	Title      string  `json:"title"`
	AuthorName *string `json:"authorName"`
}

// queueItem mirrors binderyQueueItemSchema (:127-140): percentage a STRING,
// timeLeft optional and nullable, book optional.
type queueItem struct {
	ID         int64      `json:"id"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	Percentage *string    `json:"percentage"`
	TimeLeft   *string    `json:"timeLeft"`
	Book       *queueBook `json:"book"`
}

// Compile-time interface checks.
var (
	_ mediaorganizer.Client    = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
