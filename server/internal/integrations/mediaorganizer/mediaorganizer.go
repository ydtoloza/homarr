// Package mediaorganizer is the Go port of the shared media-organizer
// abstraction of upstream (packages/integrations/src/interfaces/media-organizer
// + the per-app clients under packages/integrations/src/media-organizer):
// the missing/queue data the media-organizer widget consumes from
// Sonarr-API-compatible apps.
//
// Upstream interface (media-organizer-integration.ts:3-6):
//
//	interface IMediaOrganizerIntegration {
//	  getMissingAsync(pageSize?: number): Promise<{ items: MissingMediaItem[]; totalCount: number }>;
//	  getMediaQueueAsync(pageSize?: number): Promise<{ items: QueuedMediaItem[]; totalCount: number }>;
//	}
//
// The Go mirror is the Client interface below; the per-app versioned clients
// live in subpackages that self-register their kind into the integrations
// registry (internal/integrations/integrations.go), mirroring the upstream
// layout media-organizer/<app>/<app>-integration.ts. Only the apps that
// implement IMediaOrganizerIntegration upstream are ported here: sonarr,
// radarr and bindery (lidarr/readarr/sportarr implement only the calendar
// and service-version interfaces upstream — sportarr has category
// "mediaOrganizer" but no getMissingAsync/getMediaQueueAsync methods).
package mediaorganizer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/integrations"
)

// Type discriminates the media kind of one item (media-organizer-types.ts:4).
const (
	TypeMovie   = "movie"
	TypeEpisode = "episode"
	TypeBook    = "book"
)

// MissingItem mirrors upstream MissingMediaItem (media-organizer-types.ts:1-11).
// Optional members that upstream leaves undefined (dropped from the JSON by
// superjson) are pointers with omitempty; imageUrl is explicitly null when
// absent upstream (every mapper writes `?? null`), so it stays a pointer
// without omitempty and marshals to null.
type MissingItem struct {
	ID            int64   `json:"id"`
	Title         string  `json:"title"`
	Type          string  `json:"type"`
	Year          *int64  `json:"year,omitempty"`
	SeasonNumber  *int64  `json:"seasonNumber,omitempty"`
	EpisodeNumber *int64  `json:"episodeNumber,omitempty"`
	SeriesTitle   *string `json:"seriesTitle,omitempty"`
	ImageURL      *string `json:"imageUrl"`
	Link          string  `json:"link"`
}

// QueuedItem mirrors upstream QueuedMediaItem (media-organizer-types.ts:13-26).
// percentComplete is an integer 0..100 upstream (Math.round); timeLeft is
// explicitly null when unknown.
type QueuedItem struct {
	ID              int64   `json:"id"`
	Title           string  `json:"title"`
	Type            string  `json:"type"`
	Status          string  `json:"status"`
	TimeLeft        *string `json:"timeLeft"`
	PercentComplete int64   `json:"percentComplete"`
	Year            *int64  `json:"year,omitempty"`
	SeasonNumber    *int64  `json:"seasonNumber,omitempty"`
	EpisodeNumber   *int64  `json:"episodeNumber,omitempty"`
	SeriesTitle     *string `json:"seriesTitle,omitempty"`
	ImageURL        *string `json:"imageUrl"`
	Link            string  `json:"link"`
}

// MissingResult is one getMissing page (upstream returns {items, totalCount}
// where totalCount is the server-side totalRecords, not the page length).
type MissingResult struct {
	Items      []MissingItem `json:"items"`
	TotalCount int64         `json:"totalCount"`
}

// QueueResult is one getMediaQueue page.
type QueueResult struct {
	Items      []QueuedItem `json:"items"`
	TotalCount int64        `json:"totalCount"`
}

// DefaultPageSize is the upstream default of getMissingAsync/getMediaQueueAsync
// (pageSize = 10, media-organizer-integration.ts:4-5); the widget procedure
// clamps it to 1..50 (packages/api/src/router/widgets/media-organizer.ts:19).
const DefaultPageSize = 10

// Client is the Go mirror of IMediaOrganizerIntegration. Implementations must
// be safe for concurrent use (the widget procedure fans out over
// integrations). Errors are *Error values from this package so callers can
// distinguish transport failures from bad responses.
type Client interface {
	// GetMissing is getMissingAsync: the wanted/missing page.
	GetMissing(ctx context.Context, pageSize int) (MissingResult, error)
	// GetMediaQueue is getMediaQueueAsync: the download queue page.
	GetMediaQueue(ctx context.Context, pageSize int) (QueueResult, error)
}

// ErrorKind classifies an integration failure exactly like the upstream
// decorator stack (packages/integrations/src/base/errors): the class name
// decides how logs and the frontend render the failure.
type ErrorKind string

const (
	// ErrKindRequest is IntegrationRequestError — transport failure (DNS,
	// connection refused, timeout): "Request to integration failed".
	ErrKindRequest ErrorKind = "request"
	// ErrKindResponse is IntegrationResponseError — non-2xx status:
	// "Response from integration did not indicate success".
	ErrKindResponse ErrorKind = "response"
	// ErrKindParse is IntegrationParseError — JSON/zod mismatch:
	// "Failed to parse integration data".
	ErrKindParse ErrorKind = "parse"
	// ErrKindUnknown is IntegrationUnknownError — anything else:
	// "An unknown error occured while executing Integration method"
	// (message verbatim upstream, including the typo).
	ErrKindUnknown ErrorKind = "unknown"
)

// pinned messages of the upstream IntegrationError subclasses
// (base/errors/http/integration-request-error.ts, integration-response-error.ts,
// parse/integration-parse-error.ts, integration-unknown-error.ts). They are
// the only text that reaches the wire (mutations throw them; queries replace
// them with the public INTEGRATION_REQUEST_FAILED constant).
const (
	MsgRequest  = "Request to integration failed"
	MsgResponse = "Response from integration did not indicate success"
	MsgParse    = "Failed to parse integration data"
	MsgUnknown  = "An unknown error occured while executing Integration method"
)

// Error is the Go port of the upstream IntegrationError family: a classified
// failure with the pinned upstream message. Cause stays internal (logged by
// callers, never serialized).
type Error struct {
	Kind  ErrorKind
	Cause error
}

func (e *Error) Error() string {
	switch e.Kind {
	case ErrKindRequest:
		return MsgRequest
	case ErrKindResponse:
		return MsgResponse
	case ErrKindParse:
		return MsgParse
	default:
		return MsgUnknown
	}
}

func (e *Error) Unwrap() error { return e.Cause }

// NewRequestError / NewResponseError / NewParseError / NewUnknownError build
// the classified errors.
func NewRequestError(cause error) *Error  { return &Error{Kind: ErrKindRequest, Cause: cause} }
func NewResponseError(cause error) *Error { return &Error{Kind: ErrKindResponse, Cause: cause} }
func NewParseError(cause error) *Error    { return &Error{Kind: ErrKindParse, Cause: cause} }
func NewUnknownError(cause error) *Error  { return &Error{Kind: ErrKindUnknown, Cause: cause} }

// AsError classifies any error into the upstream taxonomy: *Error passes
// through, everything else becomes an unknown error (the decorator fallback,
// base/errors/decorator.ts handleError).
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return NewUnknownError(err)
}

// MissingSecretError is the upstream error of getSecretValue when the
// integration row has no secret of the requested kind
// (base/integration.ts:58-64: `No secret of kind ${kind} was found`). It is
// reported verbatim as the Check error.
type MissingSecretError struct {
	Kind string
}

func (e *MissingSecretError) Error() string {
	return fmt.Sprintf("No secret of kind %s was found", e.Kind)
}

// SecretValue resolves one secret from a decrypted kind->value map, mirroring
// getSecretValue (base/integration.ts:58-64).
func SecretValue(secrets map[string]string, kind string) (string, error) {
	v, ok := secrets[kind]
	if !ok {
		return "", &MissingSecretError{Kind: kind}
	}
	return v, nil
}

// Priorities mirrors mediaOrganizerPriorities
// (packages/integrations/src/media-organizer/media-organizer.ts:6-17): image
// cover types ordered best-first. Exposed for the calendar/media-organizer
// clients that pick the best remote image.
var Priorities = []string{
	"cover", "poster", "banner", "disc", "logo", "fanart", "screenshot", "clearlogo", "headshot", "unknown",
}

// IntegrationRef is one resolved integration row as the widget router hands
// it to the client factories: the upstream middleware output
// (packages/api/src/middlewares/integration.ts createManyIntegrationMiddlewareForKinds)
// reduced to the fields the domain clients consume. Secrets carries the
// DECRYPTED secret values by kind ("apiKey", "username", "password").
type IntegrationRef struct {
	ID          string
	Name        string
	Kind        string
	URL         string
	ExternalURL string
	Secrets     map[string]string
}

// Input converts the ref into the framework constructor input.
func (r IntegrationRef) Input() integrations.Input {
	return integrations.Input{ID: r.ID, Kind: r.Kind, Name: r.Name, URL: r.URL, ExternalURL: r.ExternalURL}
}

// URLBuilder reproduces the URL composition of the upstream Integration base
// class (base/integration.ts:70-98): baseUrl with trailing slashes removed,
// path appended verbatim, external URLs falling back to the internal URL.
type URLBuilder struct {
	// URL is the internal base URL (integration.url).
	URL string
	// ExternalURL is integration.externalUrl ?? app.href ?? null (empty when
	// null); external URLs fall back to URL like upstream externalUrl().
	ExternalURL string
}

func removeTrailingSlash(u string) string { return strings.TrimRight(u, "/") }

// Build builds the request URL for path (which must start with "/") plus
// query parameters. Values are formatted like upstream createUrl: booleans as
// "true"/"false", numbers with strconv (integral floats without decimals).
func (b URLBuilder) Build(path string, query map[string]string) (string, error) {
	base := removeTrailingSlash(b.URL) + path
	u, err := url.Parse(base)
	if err != nil {
		return "", NewRequestError(err)
	}
	if len(query) > 0 {
		q := u.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// BuildExternal builds a web-UI link like upstream externalUrl(path): the
// external base URL when configured, the internal one otherwise
// (base/integration.ts:93-98).
func (b URLBuilder) BuildExternal(path string, query map[string]string) (string, error) {
	external := b.ExternalURL
	if external == "" {
		external = b.URL
	}
	return URLBuilder{URL: external}.Build(path, query)
}

// FormatNumber renders a JS-style number for query parameters: integral
// values without a decimal part ("2", not "2.5"->"2"; 2.5 stays "2.5").
func FormatNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Ptr is a small helper for optional item fields.
func Ptr[T any](v T) *T { return &v }

// PercentComplete ports the shared queue computation
// (sonarr-integration.ts:252-254, radarr-integration.ts:203-205):
// Math.round(((size - sizeleft) / size) * 100) with both defaulting to 0
// (`?? 0`) and a non-positive total yielding 0.
func PercentComplete(size, sizeleft *float64) int64 {
	var sizeTotal, sizeLeft float64
	if size != nil {
		sizeTotal = *size
	}
	if sizeleft != nil {
		sizeLeft = *sizeleft
	}
	if sizeTotal <= 0 {
		return 0
	}
	return Round(((sizeTotal - sizeLeft) / sizeTotal) * 100)
}

// Round is Math.round on float64 -> integer.
func Round(f float64) int64 {
	if f < 0 {
		return int64(f - 0.5)
	}
	return int64(f + 0.5)
}
