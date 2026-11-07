// Package sonarr is the Go port of the upstream SonarrIntegration
// (packages/integrations/src/media-organizer/sonarr/sonarr-integration.ts),
// restricted to the media-organizer surface the widget consumes:
// getMissingAsync (GET /api/v3/wanted/missing) and getMediaQueueAsync
// (GET /api/v3/queue), plus the framework Check (testingAsync: GET /api with
// the X-Api-Key header, sonarr-integration.ts:281-290).
//
// Request semantics pinned from sonarr-integration.ts:
//   - auth: header "X-Api-Key: <apiKey secret>" on every call (apiKeyAuth;
//     integrationDefs sonarr.secretKinds = [["apiKey"]]),
//   - missing query (:192-199): page=1, pageSize, sortKey=airDateUtc,
//     sortDirection=ascending, includeSeries=true,
//   - queue query (:231-237): page=1, pageSize, includeSeries=true,
//     includeEpisode=true,
//   - links use externalUrl(/series/<titleSlug>) with fallbacks
//     /wanted/missing (missing) and /activity/queue (queue) when the record
//     carries no series.titleSlug.
package sonarr

import (
	"context"
	"net/http"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "sonarr").
const Kind = "sonarr"

// Definition returns the metadata of the sonarr kind.
func Definition() integrations.Definition { return integrations.Definition{Kind: Kind, Name: "Sonarr"} }

// init self-registers the kind into the default integrations registry
// (database/sql driver pattern). The registry factory builds a client without
// secrets — integrations.Input carries none yet (INTEGRATIONS.md §5.7) — so
// Check reports the upstream missing-secret error until the M4 secrets layer
// populates integrations.Input with decrypted secrets.
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values sonarr consumes.
type Secrets struct {
	// APIKey is the secret of kind "apiKey" (required by every call).
	APIKey string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map (the shape
// the widget router's resolver produces).
func SecretsFromMap(m map[string]string) (Secrets, error) {
	apiKey, err := mediaorganizer.SecretValue(m, "apiKey")
	return Secrets{APIKey: apiKey}, err
}

// Integration is one Sonarr instance. All methods are safe for concurrent use.
type Integration struct {
	input  integrations.Input
	apiKey string
	urls   mediaorganizer.URLBuilder
	http   mediaorganizer.HTTPClient
}

// New builds one Sonarr client. An empty apiKey is reported at call time as
// the upstream missing-secret error (getSecretValue,
// base/integration.ts:58-64).
func New(input integrations.Input, secrets Secrets) *Integration {
	return &Integration{
		input:  input,
		apiKey: secrets.APIKey,
		urls:   mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
	}
}

// NewFromRef builds the client from a widget-router IntegrationRef, resolving
// the "apiKey" secret with the upstream missing-secret error when absent.
func NewFromRef(ref mediaorganizer.IntegrationRef) (*Integration, error) {
	secrets, err := SecretsFromMap(ref.Secrets)
	if err != nil {
		return nil, err
	}
	return New(ref.Input(), secrets), nil
}

// Check implements integrations.Integration: GET {url}/api with the
// X-Api-Key header (testingAsync, sonarr-integration.ts:281-290). Upstream
// answers {success:true} on 2xx and StatusResult otherwise; the CheckResult
// union carries statusCode on definitive responses and error on transport
// failures, mirroring the upstream error taxonomy.
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

// GetMissing ports getMissingAsync (sonarr-integration.ts:192-229): one page
// of the wanted/missing list, series included.
func (i *Integration) GetMissing(ctx context.Context, pageSize int) (mediaorganizer.MissingResult, error) {
	header, err := i.apiHeader()
	if err != nil {
		return mediaorganizer.MissingResult{}, err
	}
	url, err := i.urls.Build("/api/v3/wanted/missing", map[string]string{
		"page":          "1",
		"pageSize":      mediaorganizer.FormatNumber(float64(pageSize)),
		"sortKey":       "airDateUtc",
		"sortDirection": "ascending",
		"includeSeries": "true",
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
			ID:            r.ID,
			Title:         r.Title,
			Type:          mediaorganizer.TypeEpisode,
			SeasonNumber:  mediaorganizer.Ptr(r.SeasonNumber),
			EpisodeNumber: mediaorganizer.Ptr(r.EpisodeNumber),
			ImageURL:      posterURL(imagesOf(r.Series)),
		}
		if r.Series != nil {
			if r.Series.Year != nil {
				item.Year = mediaorganizer.Ptr(*r.Series.Year)
			}
			item.SeriesTitle = mediaorganizer.Ptr(r.Series.Title)
		}
		item.Link = i.link(seriesPath(r.Series), "/wanted/missing")
		items = append(items, item)
	}
	return mediaorganizer.MissingResult{Items: items, TotalCount: data.TotalRecords}, nil
}

// GetMediaQueue ports getMediaQueueAsync (sonarr-integration.ts:231-272).
func (i *Integration) GetMediaQueue(ctx context.Context, pageSize int) (mediaorganizer.QueueResult, error) {
	header, err := i.apiHeader()
	if err != nil {
		return mediaorganizer.QueueResult{}, err
	}
	url, err := i.urls.Build("/api/v3/queue", map[string]string{
		"page":           "1",
		"pageSize":       mediaorganizer.FormatNumber(float64(pageSize)),
		"includeSeries":  "true",
		"includeEpisode": "true",
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
			Type:            mediaorganizer.TypeEpisode,
			Status:          r.Status,
			TimeLeft:        r.Timeleft,
			PercentComplete: mediaorganizer.PercentComplete(r.Size, r.Sizeleft),
			ImageURL:        posterURL(imagesOf(r.Series)),
		}
		if r.Episode != nil {
			item.Title = r.Episode.Title
			item.SeasonNumber = mediaorganizer.Ptr(r.Episode.SeasonNumber)
			item.EpisodeNumber = mediaorganizer.Ptr(r.Episode.EpisodeNumber)
		}
		if r.Series != nil {
			item.SeriesTitle = mediaorganizer.Ptr(r.Series.Title)
		}
		item.Link = i.link(seriesPath(r.Series), "/activity/queue")
		items = append(items, item)
	}
	return mediaorganizer.QueueResult{Items: items, TotalCount: data.TotalRecords}, nil
}

// link is externalUrl(path) with the upstream fallback path when the record
// provides none (empty path).
func (i *Integration) link(path, fallback string) string {
	if path == "" {
		path = fallback
	}
	link, err := i.urls.BuildExternal(path, nil)
	if err != nil {
		// A malformed base URL cannot build a link; the raw concat keeps
		// something meaningful instead of dropping the item.
		base := i.urls.ExternalURL
		if base == "" {
			base = i.urls.URL
		}
		return base + path
	}
	return link
}

func seriesPath(s *seriesRef) string {
	if s == nil || s.TitleSlug == "" {
		return ""
	}
	return "/series/" + s.TitleSlug
}

func imagesOf(s *seriesRef) []imageRef {
	if s == nil {
		return nil
	}
	return s.Images
}

func posterURL(images []imageRef) *string {
	for _, img := range images {
		if img.CoverType == "poster" {
			return mediaorganizer.Ptr(img.RemoteURL)
		}
	}
	return nil
}

// --- upstream response shapes (sonarr-integration.ts:293-340) ---

type imageRef struct {
	CoverType string `json:"coverType"`
	RemoteURL string `json:"remoteUrl"`
}

type seriesRef struct {
	Title     string     `json:"title"`
	Year      *int64     `json:"year"`
	TitleSlug string     `json:"titleSlug"`
	Images    []imageRef `json:"images"`
}

// missingRecord mirrors sonarrMissingEpisodeSchema (:293-312): series is
// optional (absent or null upstream both decode to nil).
type missingRecord struct {
	ID            int64      `json:"id"`
	Title         string     `json:"title"`
	SeasonNumber  int64      `json:"seasonNumber"`
	EpisodeNumber int64      `json:"episodeNumber"`
	Series        *seriesRef `json:"series"`
}

type queueEpisode struct {
	Title         string `json:"title"`
	SeasonNumber  int64  `json:"seasonNumber"`
	EpisodeNumber int64  `json:"episodeNumber"`
}

// queueRecord mirrors sonarrQueueItemSchema (:314-340); timeleft is nullish,
// size/sizeleft optional (nil -> the ?? 0 of percentComplete).
type queueRecord struct {
	ID       int64         `json:"id"`
	Title    string        `json:"title"`
	Status   string        `json:"status"`
	Timeleft *string       `json:"timeleft"`
	Size     *float64      `json:"size"`
	Sizeleft *float64      `json:"sizeleft"`
	Series   *seriesRef    `json:"series"`
	Episode  *queueEpisode `json:"episode"`
}

// Compile-time interface checks.
var (
	_ mediaorganizer.Client    = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
