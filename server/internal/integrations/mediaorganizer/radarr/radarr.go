// Package radarr is the Go port of the upstream RadarrIntegration
// (packages/integrations/src/media-organizer/radarr/radarr-integration.ts),
// restricted to the media-organizer surface the widget consumes:
// getMissingAsync (GET /api/v3/wanted/missing) and getMediaQueueAsync
// (GET /api/v3/queue), plus the framework Check (testingAsync: GET /api with
// the X-Api-Key header, radarr-integration.ts:230-239).
//
// Request semantics pinned from radarr-integration.ts:
//   - auth: header "X-Api-Key: <apiKey secret>" (integrationDefs radarr
//     secretKinds = [["apiKey"]]),
//   - missing query (:150-156): page=1, pageSize, sortKey=title,
//     sortDirection=ascending (no includeSeries — movies),
//   - queue query (:183-188): page=1, pageSize, includeMovie=true,
//   - links: externalUrl(/movie/<titleSlug>), fallback /activity/queue for
//     queue records without a movie.titleSlug.
package radarr

import (
	"context"
	"net/http"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "radarr").
const Kind = "radarr"

// Definition returns the metadata of the radarr kind.
func Definition() integrations.Definition { return integrations.Definition{Kind: Kind, Name: "Radarr"} }

// init self-registers the kind into the default integrations registry (same
// note as sonarr: no secrets in integrations.Input yet, so the registered
// factory builds an unauthenticated client whose Check reports the
// missing-secret error).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values radarr consumes.
type Secrets struct {
	// APIKey is the secret of kind "apiKey" (required by every call).
	APIKey string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map.
func SecretsFromMap(m map[string]string) (Secrets, error) {
	apiKey, err := mediaorganizer.SecretValue(m, "apiKey")
	return Secrets{APIKey: apiKey}, err
}

// Integration is one Radarr instance. All methods are safe for concurrent use.
type Integration struct {
	input  integrations.Input
	apiKey string
	urls   mediaorganizer.URLBuilder
	http   mediaorganizer.HTTPClient
}

// New builds one Radarr client.
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

// Check implements integrations.Integration: GET {url}/api with the
// X-Api-Key header (testingAsync, radarr-integration.ts:230-239).
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	header, err := i.apiHeader()
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	url, err := i.urls.Build("/api", nil)
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

// GetMissing ports getMissingAsync (radarr-integration.ts:150-181).
func (i *Integration) GetMissing(ctx context.Context, pageSize int) (mediaorganizer.MissingResult, error) {
	header, err := i.apiHeader()
	if err != nil {
		return mediaorganizer.MissingResult{}, err
	}
	url, err := i.urls.Build("/api/v3/wanted/missing", map[string]string{
		"page":          "1",
		"pageSize":      mediaorganizer.FormatNumber(float64(pageSize)),
		"sortKey":       "title",
		"sortDirection": "ascending",
	})
	if err != nil {
		return mediaorganizer.MissingResult{}, err
	}
	var data struct {
		TotalRecords int64           `json:"totalRecords"`
		Records      []missingRecord `json:"records"`
	}
	if err := i.http.DoJSON(ctx, http.MethodGet, url, header, nil, &data); err != nil {
		return mediaorganizer.MissingResult{}, err
	}
	items := make([]mediaorganizer.MissingItem, 0, len(data.Records))
	for _, r := range data.Records {
		item := mediaorganizer.MissingItem{
			ID:       r.ID,
			Title:    r.Title,
			Type:     mediaorganizer.TypeMovie,
			Year:     r.Year,
			ImageURL: posterURL(r.Images),
		}
		item.Link = i.link("/movie/" + r.TitleSlug)
		items = append(items, item)
	}
	return mediaorganizer.MissingResult{Items: items, TotalCount: data.TotalRecords}, nil
}

// GetMediaQueue ports getMediaQueueAsync (radarr-integration.ts:183-221).
func (i *Integration) GetMediaQueue(ctx context.Context, pageSize int) (mediaorganizer.QueueResult, error) {
	header, err := i.apiHeader()
	if err != nil {
		return mediaorganizer.QueueResult{}, err
	}
	url, err := i.urls.Build("/api/v3/queue", map[string]string{
		"page":         "1",
		"pageSize":     mediaorganizer.FormatNumber(float64(pageSize)),
		"includeMovie": "true",
	})
	if err != nil {
		return mediaorganizer.QueueResult{}, err
	}
	var data struct {
		TotalRecords int64         `json:"totalRecords"`
		Records      []queueRecord `json:"records"`
	}
	if err := i.http.DoJSON(ctx, http.MethodGet, url, header, nil, &data); err != nil {
		return mediaorganizer.QueueResult{}, err
	}
	items := make([]mediaorganizer.QueuedItem, 0, len(data.Records))
	for _, r := range data.Records {
		item := mediaorganizer.QueuedItem{
			ID:              r.ID,
			Title:           r.Title,
			Type:            mediaorganizer.TypeMovie,
			Status:          r.Status,
			TimeLeft:        r.Timeleft,
			PercentComplete: mediaorganizer.PercentComplete(r.Size, r.Sizeleft),
			ImageURL:        posterURL(imagesOf(r.Movie)),
		}
		if r.Movie != nil {
			item.Title = r.Movie.Title
			item.Year = r.Movie.Year
		}
		if r.Movie != nil && r.Movie.TitleSlug != "" {
			item.Link = i.link("/movie/" + r.Movie.TitleSlug)
		} else {
			item.Link = i.link("/activity/queue")
		}
		items = append(items, item)
	}
	return mediaorganizer.QueueResult{Items: items, TotalCount: data.TotalRecords}, nil
}

// link is externalUrl(path); the malformed-base-URL fallback mirrors sonarr.
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

func imagesOf(m *movieRef) []imageRef {
	if m == nil {
		return nil
	}
	return m.Images
}

func posterURL(images []imageRef) *string {
	for _, img := range images {
		if img.CoverType == "poster" {
			return mediaorganizer.Ptr(img.RemoteURL)
		}
	}
	return nil
}

// --- upstream response shapes (radarr-integration.ts:250-283) ---

type imageRef struct {
	CoverType string `json:"coverType"`
	RemoteURL string `json:"remoteUrl"`
}

type movieRef struct {
	Title     string     `json:"title"`
	Year      *int64     `json:"year"`
	TitleSlug string     `json:"titleSlug"`
	Images    []imageRef `json:"images"`
}

// missingRecord mirrors radarrMissingMovieSchema (:250-261): images required,
// year optional, no series.
type missingRecord struct {
	ID        int64      `json:"id"`
	Title     string     `json:"title"`
	Year      *int64     `json:"year"`
	TitleSlug string     `json:"titleSlug"`
	Images    []imageRef `json:"images"`
}

// queueRecord mirrors radarrQueueItemSchema (:263-283): timeleft optional
// (NOT nullish here — z.string().optional()), movie optional.
type queueRecord struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Status   string    `json:"status"`
	Timeleft *string   `json:"timeleft"`
	Size     *float64  `json:"size"`
	Sizeleft *float64  `json:"sizeleft"`
	Movie    *movieRef `json:"movie"`
}

// Compile-time interface checks.
var (
	_ mediaorganizer.Client    = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
