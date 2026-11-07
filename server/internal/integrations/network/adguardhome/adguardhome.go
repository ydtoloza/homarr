// Package adguardhome is the Go port of the upstream AdGuardHomeIntegration
// (packages/integrations/src/adguard-home/adguard-home-integration.ts):
// getCurrentSessionsAsync has no equivalent here — the dns-hole summary is
// the only surface the dns-hole widgets consume.
//
// Request semantics pinned from adguard-home-integration.ts:
//   - auth: HTTP BASIC with the username/password secrets
//     (`Authorization: Basic <base64(user:pass)>`, :14-16, :146-150;
//     integrationDefs adGuardHome secretKinds [["username","password"]]),
//   - summary (:18-92): GET /control/stats, GET /control/status and GET
//     /control/filtering/status IN PARALLEL; a non-2xx answer throws the
//     plain Error "Failed to fetch <what> for <name> (<id>): <statusText>"
//     and a schema mismatch "Failed to parse summary for <name> (<id>): ..."
//     (both plain/unknown taxonomy; only the enable/disable messages reach
//     the wire),
//   - today's numbers (:66-83): with time_units "days" the LAST entry of
//     blocked_filtering/dns_queries counts (index [length-1] ?? 0); with
//     "hours" the arrays are SUMMED; the percentage is blocked/queries*100
//     (0 without queries); domainsBeingBlocked is the sum of the rules_count
//     of every ENABLED filter (0 when filters is null),
//   - status: protection_enabled ? "enabled" : "disabled" (:86),
//   - testing (:94-107): GET /control/status; a JSON object body is success,
//   - enable/disable (:109-144): POST /control/protection
//     {"enabled": true} / {"enabled": false, "duration": seconds*1000}
//     (Content-Type: application/json, duration defaults to 0 = permanent);
//     failures throw the plain Error "Failed to enable/disable AdGuard Home
//     for <name> (<id>): <statusText>" whose message the dnsHole mutations
//     surface verbatim.
//
// NO per-request timeout upstream (fetchWithTrustedCertificatesAsync without
// options); the deadline comes from the widget layer (60s).
package adguardhome

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
	"github.com/ydtoloza/homarr/server/internal/integrations/network"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "adGuardHome").
const Kind = "adGuardHome"

// Definition returns the metadata of the adGuardHome kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "AdGuard Home"}
}

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values adguard consumes.
type Secrets struct {
	Username string
	Password string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map.
func SecretsFromMap(m map[string]string) (Secrets, error) {
	if _, ok := m["username"]; !ok {
		return Secrets{}, &mediaorganizer.MissingSecretError{Kind: "username"}
	}
	if _, ok := m["password"]; !ok {
		return Secrets{}, &mediaorganizer.MissingSecretError{Kind: "password"}
	}
	return Secrets{Username: m["username"], Password: m["password"]}, nil
}

// Integration is one AdGuard Home instance. All methods are safe for
// concurrent use.
type Integration struct {
	input   integrations.Input
	secrets Secrets
	urls    mediaorganizer.URLBuilder
	http    mediaorganizer.HTTPClient
}

// New builds one AdGuard Home client. Missing credentials are reported at
// call time as the upstream missing-secret error (getSecretValue).
func New(input integrations.Input, secrets Secrets) *Integration {
	return &Integration{
		input:   input,
		secrets: secrets,
		urls:    mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
	}
}

// NewFromRef builds the client from a widget-router IntegrationRef.
func NewFromRef(ref network.IntegrationRef) (*Integration, error) {
	secrets, err := SecretsFromMap(ref.Secrets)
	if err != nil {
		return nil, err
	}
	return New(ref.Input(), secrets), nil
}

// authHeader builds the Basic authorization header
// (getAuthorizationHeaderValue :146-150).
func (i *Integration) authHeader() (http.Header, error) {
	if i.secrets.Username == "" {
		// getSecretValue("username") throws first upstream.
		return nil, &mediaorganizer.MissingSecretError{Kind: "username"}
	}
	if i.secrets.Password == "" {
		return nil, &mediaorganizer.MissingSecretError{Kind: "password"}
	}
	value := base64.StdEncoding.EncodeToString([]byte(i.secrets.Username + ":" + i.secrets.Password))
	return http.Header{"Authorization": []string{"Basic " + value}}, nil
}

// Check implements integrations.Integration: GET /control/status
// (testingAsync, :94-107); a JSON object body is success.
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	header, err := i.authHeader()
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	url, err := i.urls.Build("/control/status", nil)
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	res, err := i.http.Do(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.MsgRequest}, nil
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return integrations.CheckResult{StatusCode: res.StatusCode, Error: mediaorganizer.MsgParse}, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return integrations.CheckResult{
			StatusCode: res.StatusCode,
			Error:      mediaorganizer.MsgResponse,
		}, nil
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		// TestConnectionError.ParseResult(new ParseError("Expected object data"))
		return integrations.CheckResult{StatusCode: res.StatusCode, Error: mediaorganizer.MsgParse}, nil
	}
	if _, ok := body.(map[string]any); !ok {
		return integrations.CheckResult{StatusCode: res.StatusCode, Error: mediaorganizer.MsgParse}, nil
	}
	return integrations.CheckResult{StatusCode: res.StatusCode}, nil
}

// GetSummary ports getSummaryAsync (:18-92).
func (i *Integration) GetSummary(ctx context.Context) (network.DnsHoleSummary, error) {
	header, err := i.authHeader()
	if err != nil {
		return network.DnsHoleSummary{}, err
	}
	// Promise.all of the three GETs (:22-26).
	type fetchResult struct {
		raw    []byte
		status int
		err    error
	}
	fetch := func(path string) <-chan fetchResult {
		ch := make(chan fetchResult, 1)
		go func() {
			url, err := i.urls.Build(path, nil)
			if err != nil {
				ch <- fetchResult{err: mediaorganizer.NewRequestError(err)}
				return
			}
			res, err := i.http.Do(ctx, http.MethodGet, url, header, nil)
			if err != nil {
				ch <- fetchResult{err: mediaorganizer.NewRequestError(err)}
				return
			}
			defer res.Body.Close()
			raw, err := io.ReadAll(res.Body)
			if err != nil {
				ch <- fetchResult{err: mediaorganizer.NewParseError(err)}
				return
			}
			ch <- fetchResult{raw: raw, status: res.StatusCode}
		}()
		return ch
	}
	statsCh, statusCh, filteringCh := fetch("/control/stats"), fetch("/control/status"), fetch("/control/filtering/status")
	statsRes, statusRes, filteringRes := <-statsCh, <-statusCh, <-filteringCh

	for _, fr := range []struct {
		result fetchResult
		what   string
	}{{statsRes, "stats"}, {statusRes, "status"}, {filteringRes, "filtering status"}} {
		if fr.result.err != nil {
			return network.DnsHoleSummary{}, mediaorganizer.AsError(fr.result.err)
		}
		if fr.result.status < 200 || fr.result.status >= 300 {
			// plain Error upstream (query path: the widget falls back on any
			// failure, the message only matters for logging).
			return network.DnsHoleSummary{}, fmt.Errorf(
				"Failed to fetch %s for %s (%s): %s", fr.what, i.input.Name, i.input.ID,
				http.StatusText(fr.result.status))
		}
	}

	var stats statsResponse
	var status statusResponse
	var filtering filteringStatus
	if err := json.Unmarshal(statsRes.raw, &stats); err != nil {
		return network.DnsHoleSummary{}, network.NewUnknownError(fmt.Errorf(
			"Failed to parse summary for %s (%s)", i.input.Name, i.input.ID))
	}
	if err := json.Unmarshal(statusRes.raw, &status); err != nil {
		return network.DnsHoleSummary{}, network.NewUnknownError(fmt.Errorf(
			"Failed to parse summary for %s (%s)", i.input.Name, i.input.ID))
	}
	if err := json.Unmarshal(filteringRes.raw, &filtering); err != nil {
		return network.DnsHoleSummary{}, network.NewUnknownError(fmt.Errorf(
			"Failed to parse summary for %s (%s)", i.input.Name, i.input.ID))
	}

	// blockedQueriesToday/queriesToday (:66-73): the last entry with
	// time_units "days" (index length-1 ?? 0), the SUM with "hours".
	blockedQueriesToday := todayValue(stats.TimeUnits, stats.BlockedFiltering)
	queriesToday := todayValue(stats.TimeUnits, stats.DNSQueries)
	countFilteredDomains := 0.0
	for _, filter := range filtering.Filters {
		if filter.Enabled {
			countFilteredDomains += filter.RulesCount
		}
	}
	adsBlockedTodayPercentage := 0.0
	if queriesToday > 0 {
		adsBlockedTodayPercentage = (blockedQueriesToday / queriesToday) * 100
	}

	summary := network.DnsHoleSummary{
		AdsBlockedToday:           blockedQueriesToday,
		AdsBlockedTodayPercentage: adsBlockedTodayPercentage,
		DomainsBeingBlocked:       countFilteredDomains,
		DNSQueriesToday:           queriesToday,
	}
	if status.ProtectionEnabled {
		summary.Status = network.Ptr(network.StatusEnabled)
	} else {
		summary.Status = network.Ptr(network.StatusDisabled)
	}
	return summary, nil
}

// todayValue ports the time_units switch (:66-73).
func todayValue(timeUnits string, values []float64) float64 {
	if timeUnits == "days" {
		if len(values) == 0 {
			return 0
		}
		return values[len(values)-1]
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum
}

// Enable ports enableAsync (:109-125).
func (i *Integration) Enable(ctx context.Context) error {
	return i.postProtection(ctx, []byte(`{"enabled":true}`), "enable")
}

// Disable ports disableAsync (:127-144): duration is seconds, defaults to 0
// (permanent) and travels as MILLISECONDS.
func (i *Integration) Disable(ctx context.Context, duration *network.DisableDurationSeconds) error {
	seconds := 0.0
	if duration != nil {
		seconds = *duration
	}
	body := fmt.Sprintf(`{"enabled":false,"duration":%s}`, formatNumber(seconds*1000))
	return i.postProtection(ctx, []byte(body), "disable")
}

func (i *Integration) postProtection(ctx context.Context, body []byte, action string) error {
	header, err := i.authHeader()
	if err != nil {
		return err
	}
	header.Set("Content-Type", "application/json")
	url, err := i.urls.Build("/control/protection", nil)
	if err != nil {
		return mediaorganizer.NewRequestError(err)
	}
	res, err := i.http.Do(ctx, http.MethodPost, url, header, body)
	if err != nil {
		return mediaorganizer.NewRequestError(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// plain Error upstream — the message travels through the mutation.
		return fmt.Errorf("Failed to %s AdGuard Home for %s (%s): %s",
			action, i.input.Name, i.input.ID,
			http.StatusText(res.StatusCode))
	}
	return nil
}

// formatNumber renders a JS number (JSON.stringify semantics).
func formatNumber(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// --- upstream response shapes (adguard-home-types.ts) ---

// statsResponse mirrors statsResponseSchema — only the members the summary
// consumes are decoded (time_units, dns_queries, blocked_filtering).
type statsResponse struct {
	TimeUnits        string    `json:"time_units"`
	DNSQueries       []float64 `json:"dns_queries"`
	BlockedFiltering []float64 `json:"blocked_filtering"`
}

// statusResponse mirrors statusResponseSchema — protection_enabled only.
type statusResponse struct {
	ProtectionEnabled bool `json:"protection_enabled"`
}

// filteringStatus mirrors filteringStatusSchema (:73-79): filters is NULLABLE
// (an absent/null array contributes no domains).
type filteringStatus struct {
	Filters []filter `json:"filters"`
}

type filter struct {
	ID         int64   `json:"id"`
	RulesCount float64 `json:"rules_count"`
	Enabled    bool    `json:"enabled"`
}

// Compile-time interface checks.
var (
	_ network.Client           = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
