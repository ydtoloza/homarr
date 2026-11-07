package widgets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/deluge"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/qbittorrent"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/sabnzbd"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/transmission"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer/bindery"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer/radarr"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer/sonarr"
)

// Supported widget kinds (packages/definitions/src/widget-integration-map.ts):
//
//	mediaMissing: { supportedIntegrations: getIntegrationKindsByCategory("mediaOrganizer") } (:148)
//	downloads:    { supportedIntegrations: getIntegrationKindsByCategory("downloadClient") } (:141)
//
// The lists are the integrationDef entries carrying the category, in
// definition order (packages/definitions/src/integration.ts), with the demo
// "mock" kind last (its definition is the last entry of integrationDefs).
const (
	WidgetKindMediaMissing = "mediaMissing"
	WidgetKindDownloads    = "downloads"
)

// SupportedKinds returns the integration kinds a widget kind accepts.
func SupportedKinds(widgetKind string) []string {
	switch widgetKind {
	case WidgetKindMediaMissing:
		return []string{"sonarr", "sportarr", "radarr", "bindery", "mock"}
	case WidgetKindDownloads:
		return []string{"sabNzbd", "nzbGet", "deluge", "transmission", "qBittorrent", "aria2", "slskd", "mock"}
	default:
		return nil
	}
}

// Integration is one resolved integration row: the middleware output
// (createManyIntegrationMiddlewareForKinds, packages/api/src/middlewares/
// integration.ts) reduced to what the widget data flows consume. SecretValues
// holds the RAW (still encrypted) integrationSecret.value strings by kind —
// the router decrypts them with Options.SecretDecryptor before building
// clients, mirroring the middleware's decryptedSecrets step.
type Integration struct {
	ID           string
	Name         string
	Kind         string
	URL          string
	ExternalURL  string
	SecretValues map[string]string
}

// ResolveRequest is the upstream middleware contract: action "query" or
// "interact" (IntegrationAction, middlewares/integration.ts:27), the widget
// kind deciding the supported kinds, and the requested ids.
type ResolveRequest struct {
	Action         string
	WidgetKind     string
	IntegrationIDs []string
}

// ResolveIntegrations is the upstream createManyWidgetIntegrationMiddleware:
// load the rows by id restricted to the widget's supported kinds, enforce the
// action permission, and return them in the requested order. Errors must be
// *trpc.Error values (the upstream middleware throws NOT_FOUND for missing
// ids and FORBIDDEN for denied access — see notFoundMessage below for the
// exact messages).
type ResolveIntegrations func(ctx context.Context, req ResolveRequest) ([]Integration, error)

// ClientFactory builds the domain client for one resolved integration.
type (
	// MediaOrganizerFactory mirrors createIntegrationAsync for kinds with
	// IMediaOrganizerIntegration (sonarr, radarr, bindery, mock).
	MediaOrganizerFactory func(integration Integration) (mediaorganizer.Client, error)
	// DownloadClientFactory mirrors createIntegrationAsync for
	// IDownloadClientIntegration kinds (qBittorrent, transmission, deluge,
	// sabNzbd, ...).
	DownloadClientFactory func(integration Integration) (downloadclient.Client, error)
)

// SecretDecryptor decrypts one integrationSecret.value
// (decryptSecret, packages/common/src/encryption.ts:23-35).
type SecretDecryptor func(value string) (string, error)

// options is the resolved configuration (Options with defaults applied).
type options struct {
	resolveIntegrations ResolveIntegrations
	secretDecryptor     SecretDecryptor
	newMediaOrganizer   MediaOrganizerFactory
	newDownloadClient   DownloadClientFactory
	parallelism         int
	mediaOrganizerTTL   int64 // ms; negative = cache disabled
	downloadsTTL        int64 // ms; negative = cache disabled
}

// DefaultParallelism bounds the per-request fan-out over integrations.
// Upstream runs Promise.allSettled WITHOUT a limit
// (packages/api/src/settle-integrations.ts:27); the port caps it to protect a
// small SQLite-backed process (same reasoning as the ping job,
// INTEGRATIONS.md §5.4).
const DefaultParallelism = 10

// PublicIntegrationError is PUBLIC_INTEGRATION_ERROR
// (packages/api/src/settle-integrations.ts:7): every per-integration failure
// of a query carries exactly this string.
const PublicIntegrationError = "INTEGRATION_REQUEST_FAILED"

// Options configures the widget routers. The zero value answers NOT_FOUND to
// every widget query (a nil resolver behaves as "no integrations exist").
type Options struct {
	// ResolveIntegrations is the DB-backed integration middleware (see
	// ResolveIntegrations). nil behaves as "no integrations exist".
	ResolveIntegrations ResolveIntegrations
	// SecretDecryptor decrypts an integrationSecret.value. The default is
	// the upstream cipher (secrets.go): AES-256-CBC over "<data-hex>.<iv-hex>"
	// with the SECRET_ENCRYPTION_KEY env (64 hex chars; 32 zero bytes when
	// unset, exactly like packages/common/src/encryption.ts:9).
	SecretDecryptor SecretDecryptor
	// NewMediaOrganizer builds the media-organizer client for one resolved
	// integration. The default dispatches on kind (sonarr/radarr/bindery)
	// and fails other kinds per integration — the query then reports the
	// public error for that integration, like an upstream runtime failure.
	NewMediaOrganizer MediaOrganizerFactory
	// NewDownloadClient builds the download-client client (default dispatch:
	// qBittorrent/transmission/deluge/sabNzbd).
	NewDownloadClient DownloadClientFactory
	// Parallelism bounds the integrations processed concurrently per request
	// (0 -> DefaultParallelism).
	Parallelism int
	// MediaOrganizerCacheTTLMS overrides the 60_000ms TTL of the upstream
	// media-organizer request handler (packages/request-handler/src/
	// media-organizer.ts:33); negative disables the cache. Test seam.
	MediaOrganizerCacheTTLMS int64
	// DownloadsCacheTTLMS overrides the 10_000ms TTL of the downloads request
	// handler (packages/request-handler/src/downloads.ts:18); negative
	// disables the cache. Test seam.
	DownloadsCacheTTLMS int64
}

func (o Options) withDefaults() options {
	resolved := options{
		resolveIntegrations: o.ResolveIntegrations,
		secretDecryptor:     o.SecretDecryptor,
		newMediaOrganizer:   o.NewMediaOrganizer,
		newDownloadClient:   o.NewDownloadClient,
		parallelism:         o.Parallelism,
		mediaOrganizerTTL:   o.MediaOrganizerCacheTTLMS,
		downloadsTTL:        o.DownloadsCacheTTLMS,
	}
	if resolved.parallelism <= 0 {
		resolved.parallelism = DefaultParallelism
	}
	if resolved.secretDecryptor == nil {
		resolved.secretDecryptor = DefaultSecretDecryptor
	}
	if resolved.newMediaOrganizer == nil {
		resolved.newMediaOrganizer = DefaultMediaOrganizerFactory
	}
	if resolved.newDownloadClient == nil {
		resolved.newDownloadClient = DefaultDownloadClientFactory
	}
	return resolved
}

// DefaultMediaOrganizerFactory dispatches on the integration kind, the
// equivalent of upstream's integrationCreators entries. Unknown kinds (mock,
// sportarr — which carries the mediaOrganizer category but implements no
// IMediaOrganizerIntegration) fail per integration like upstream's runtime
// "integrationInstance.getMissingAsync is not a function".
func DefaultMediaOrganizerFactory(integration Integration) (mediaorganizer.Client, error) {
	ref := mediaorganizer.IntegrationRef{
		ID:          integration.ID,
		Name:        integration.Name,
		Kind:        integration.Kind,
		URL:         integration.URL,
		ExternalURL: integration.ExternalURL,
		Secrets:     integration.SecretValues,
	}
	switch integration.Kind {
	case "sonarr":
		return sonarr.NewFromRef(ref)
	case "radarr":
		return radarr.NewFromRef(ref)
	case "bindery":
		return bindery.NewFromRef(ref)
	default:
		return nil, fmt.Errorf("media-organizer: kind %q does not implement the media-organizer interface", integration.Kind)
	}
}

// DefaultDownloadClientFactory dispatches on the integration kind.
func DefaultDownloadClientFactory(integration Integration) (downloadclient.Client, error) {
	ref := downloadclient.IntegrationRef{
		ID:          integration.ID,
		Name:        integration.Name,
		Kind:        integration.Kind,
		URL:         integration.URL,
		ExternalURL: integration.ExternalURL,
		Secrets:     integration.SecretValues,
	}
	switch integration.Kind {
	case "qBittorrent":
		return qbittorrent.NewFromRef(ref)
	case "transmission":
		return transmission.NewFromRef(ref)
	case "deluge":
		return deluge.NewFromRef(ref)
	case "sabNzbd":
		return sabnzbd.NewFromRef(ref)
	default:
		return nil, fmt.Errorf("download-client: kind %q does not implement the download-client interface", integration.Kind)
	}
}

// service carries the configured dependencies of both routers.
type service struct {
	opts options

	mediaOrganizerCache *resultCache
	downloadsCache      *resultCache
}

func newService(opts Options) *service {
	o := opts.withDefaults()
	s := &service{opts: o}
	if o.mediaOrganizerTTL >= 0 {
		s.mediaOrganizerCache = newResultCache(o.mediaOrganizerTTL)
	}
	if o.downloadsTTL >= 0 {
		s.downloadsCache = newResultCache(o.downloadsTTL)
	}
	return s
}

// protected is the protectedProcedure gate (enforceUserIsAuthed,
// packages/api/src/trpc.ts:163-172): UNAUTHORIZED for anonymous callers.
func (s *service) protected(next trpc.Procedure) trpc.Procedure {
	return func(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
		if err := admin.RequireSignedIn(admin.Caller{Session: ctxauth.FromContext(ctx)}); err != nil {
			return nil, err
		}
		return next(ctx, input)
	}
}

// resolve runs the resolver and decrypts the secrets. It mirrors the
// middleware's decryptSecret step (middlewares/integration.ts:
// decryptedSecrets: secrets.map(... value: decryptSecret(secret.value))).
func (s *service) resolve(ctx context.Context, req ResolveRequest) ([]Integration, *trpc.Error) {
	if s.opts.resolveIntegrations == nil {
		return nil, trpc.NewError(trpc.CodeNotFound, notFoundMessage(len(req.IntegrationIDs), req.IntegrationIDs, SupportedKinds(req.WidgetKind), nil))
	}
	rows, err := s.opts.resolveIntegrations(ctx, req)
	if err != nil {
		var terr *trpc.Error
		if errors.As(err, &terr) {
			return nil, terr
		}
		return nil, admin.InternalError(err)
	}
	for idx := range rows {
		for kind, value := range rows[idx].SecretValues {
			decrypted, derr := s.opts.secretDecryptor(value)
			if derr != nil {
				return nil, admin.InternalError(fmt.Errorf("decrypt secret %q of integration %s: %w", kind, rows[idx].ID, derr))
			}
			rows[idx].SecretValues[kind] = decrypted
		}
	}
	return rows, nil
}

// notFoundMessage is the exact upstream TRPCError message of the many
// middleware (middlewares/integration.ts:186-192):
//
//	"<offset> of the specified integrations not found or not of kinds <kinds>:
//	 ([<requested ids>] compared to [<resolved kind:id pairs>])"
func notFoundMessage(offset int, requested, kinds []string, resolved []Integration) string {
	return fmt.Sprintf("%d of the specified integrations not found or not of kinds %s: ([%s] compared to [%s])",
		offset, joinStrings(kinds), joinStrings(requested), joinKindIDs(resolved))
}

func joinStrings(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func joinKindIDs(rows []Integration) string {
	out := ""
	for i, row := range rows {
		if i > 0 {
			out += ","
		}
		out += row.Kind + ":" + row.ID
	}
	return out
}
