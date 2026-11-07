// Package network is the Go port of the shared dns-hole abstraction of
// upstream (packages/integrations/src/interfaces/dns-hole-summary + the
// per-app clients under packages/integrations/src/{pi-hole,adguard-home}): the
// DNS blocking summary and the enable/disable controls the dns-hole widgets
// consume.
//
// Upstream interface (dns-hole-summary-integration.ts:3-7):
//
//	interface DnsHoleSummaryIntegration {
//	  getSummaryAsync(): Promise<DnsHoleSummary>;
//	  enableAsync(): Promise<void>;
//	  disableAsync(duration?: number): Promise<void>;
//	}
//
// The Go mirror is the Client interface below; the per-app clients live in
// subpackages that self-register their kind into the integrations registry,
// mirroring the upstream layout: pihole (kind "piHole" — v5 AND v6 behind one
// version probe, pi-hole-integration-factory.ts) and adguardhome (kind
// "adGuardHome"). Technitium DNS carries the dnsHole category upstream too but
// is out of this batch (the widget factory fails that kind per integration
// until it lands, exactly like sportarr for the media-organizer widget).
//
// The widget data surfaces are the tRPC procedures widget.dnsHole.summary /
// widget.dnsHole.enable / widget.dnsHole.disable
// (packages/api/src/router/widgets/dns-hole.ts:28-97): NO websocket or bus
// event exists for this domain upstream (apps/websocket has no dns-hole
// channel; the frontend polls the summary and invalidates it after mutations),
// so this port adds no bus topics either.
//
// pi-hole versioning: the probe of createPiHoleIntegrationAsync
// (pi-hole-integration-factory.ts:8-30) selects the implementation at
// construction — GET {base}/api/info/login answers 404 on v5 (api at
// /admin/api.php) and 200 on v6 (api at /api), unreachable servers fall back
// to v6 so the error surfaces from the call itself.
package network

import (
	"context"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// DnsHoleSummary mirrors upstream DnsHoleSummary
// (dns-hole-summary-types.ts:1-7): status is OPTIONAL — pi-hole v6 maps the
// "failed"/"unknown" blocking states to undefined (dropped from the JSON by
// superjson), v5/adguard always set it.
type DnsHoleSummary struct {
	Status                    *string `json:"status,omitempty"`
	DomainsBeingBlocked       float64 `json:"domainsBeingBlocked"`
	AdsBlockedToday           float64 `json:"adsBlockedToday"`
	AdsBlockedTodayPercentage float64 `json:"adsBlockedTodayPercentage"`
	DNSQueriesToday           float64 `json:"dnsQueriesToday"`
}

// Blocking status values of DnsHoleSummary.Status (v5 status enum and the v6
// dnsBlockingGetSchema transform).
const (
	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
)

// DisableDurationSeconds is the unit of the Client.Disable duration: seconds
// everywhere (v5 `disable=<seconds>`, v6 `{"timer": <seconds>}`, adguard
// multiplies to ms for {"duration": ms}).
type DisableDurationSeconds = float64

// Client is the Go mirror of DnsHoleSummaryIntegration. Implementations must
// be safe for concurrent use (the widget procedure fans out over
// integrations). Errors are *Error values from this package so callers can
// distinguish transport failures from bad responses; the enable/disable
// failures keep the exact upstream messages because the widget mutations
// surface them on the wire.
type Client interface {
	// GetSummary is getSummaryAsync: queries, blocked and blocking state.
	GetSummary(ctx context.Context) (DnsHoleSummary, error)
	// Enable is enableAsync: re-enable DNS blocking.
	Enable(ctx context.Context) error
	// Disable is disableAsync: nil duration = permanent disable, a number =
	// temporary disable for that many seconds.
	Disable(ctx context.Context, duration *DisableDurationSeconds) error
}

// MsgResponseNotIndicated is the message of the @homarr/common/server
// ResponseError ("Response did not indicate success",
// packages/common/src/errors/http/response-error.ts:8) — NOT the integration
// taxonomy message. pi-hole v6 throws it on every non-2xx answer, so it is
// the wire message of the v6 enable/disable mutations.
const MsgResponseNotIndicated = "Response did not indicate success"

// Re-export the shared error taxonomy of the integrations domain so callers
// of every domain see one vocabulary (the upstream classes are shared too).
type (
	Error              = mediaorganizer.Error
	ErrorKind          = mediaorganizer.ErrorKind
	MissingSecretError = mediaorganizer.MissingSecretError
)

const (
	ErrKindRequest  = mediaorganizer.ErrKindRequest
	ErrKindResponse = mediaorganizer.ErrKindResponse
	ErrKindParse    = mediaorganizer.ErrKindParse
	ErrKindUnknown  = mediaorganizer.ErrKindUnknown

	MsgRequest  = mediaorganizer.MsgRequest
	MsgResponse = mediaorganizer.MsgResponse
	MsgParse    = mediaorganizer.MsgParse
	MsgUnknown  = mediaorganizer.MsgUnknown
)

// Re-exported constructors and helpers of the shared taxonomy.
var (
	NewRequestError  = mediaorganizer.NewRequestError
	NewResponseError = mediaorganizer.NewResponseError
	NewParseError    = mediaorganizer.NewParseError
	NewUnknownError  = mediaorganizer.NewUnknownError
	AsError          = mediaorganizer.AsError
	SecretValue      = mediaorganizer.SecretValue
)

// Ptr mirrors mediaorganizer.Ptr (generic alias helpers cannot be var
// re-exports).
func Ptr[T any](v T) *T { return mediaorganizer.Ptr(v) }

// IntegrationRef is one resolved integration row as the widget router hands
// it to the client factories (mirrors mediaorganizer.IntegrationRef). Secrets
// carries the DECRYPTED secret values by kind ("apiKey" for pi-hole,
// "username"/"password" for adguard).
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
