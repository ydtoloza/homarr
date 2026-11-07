// Package pihole is the Go port of the upstream pi-hole integrations
// (packages/integrations/src/pi-hole): ONE kind "piHole" behind TWO server
// versions, selected at construction by the probe of
// pi-hole-integration-factory.ts:8-30 — GET {base}/api/info/login answers 404
// on pi-hole 5 (api at /admin/api.php) and 200 on pi-hole 6 (api at /api);
// an unreachable server falls back to v6 so the error surfaces from the call.
//
// Request semantics pinned from pi-hole-integration-v5.ts and
// pi-hole-integration-v6.ts:
//
//	v5 (pi-hole-integration-v5.ts):
//	  - auth: the webserverpassword travels as the "auth" QUERY parameter on
//	    every call (getHttpAuthenticationAsync :15-18),
//	  - summary (:20-36): GET /admin/api.php?summaryRaw=&auth=<key> mapped as
//	    status/ads_blocked_today/ads_percentage_today/
//	    domains_being_blocked/dns_queries_today,
//	  - testing (:38-53): GET /admin/api.php?status&auth=<key>; a 2xx body
//	    that is NOT an object (v5 answers an empty ARRAY for a wrong key) is
//	    the 401 test result,
//	  - enable/disable (:55-74): GET /admin/api.php?enable / ?disable[=seconds]
//	    (duration 0/undefined disables WITHOUT a value), failures throw the
//	    plain Error "Failed to enable/disable PiHole for <name> (<id>): <status>"
//	    whose message the dnsHole mutations surface verbatim.
//
//	v6 (pi-hole-integration-v6.ts):
//	  - auth: password login sessions — POST /api/auth {"password": <apiKey>}
//	    (User-Agent: Homarr, :169-198) answers {session: {valid, sid, message}};
//	    an INVALID session throws the common ResponseError as a 401. Sessions
//	    are reused through the session store (base/session-store.ts, a Redis
//	    get/set channel upstream, in-process here keyed by the integration
//	    instance) and re-created when a request answers 401 (withAuthAsync
//	    :142-163) — reusing instead of re-logging avoids exhausting
//	    webserver.api.max_sessions (:30-33),
//	  - every authenticated call carries the custom "sid" header
//	    (undefined/absent without a session),
//	  - summary (:48-96): GET /api/stats/summary (queries.total/blocked/
//	    percent_blocked + gravity.domains_being_blocked) THEN GET
//	    /api/dns/blocking (blocking state; "failed"/"unknown" drop the status
//	    member via the schema transform, pi-hole-schemas-v6.ts:11-18),
//	  - testing (:98-103): login, DELETE /api/auth (clear the server session),
//	    clear the store,
//	  - enable/disable (:105-135): POST /api/dns/blocking
//	    {"blocking": true} / {"blocking": false[,"timer": seconds]} — the
//	    timer member is DROPPED when undefined (JSON.stringify), failures
//	    throw the common ResponseError "Response did not indicate success".
//
// NO per-request timeout upstream (fetchWithTrustedCertificatesAsync without
// options); the deadline comes from the widget layer (60s).
package pihole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
	"github.com/ydtoloza/homarr/server/internal/integrations/network"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "piHole" — capital H).
const Kind = "piHole"

// authSessionHeader is the custom session header of v6 (lowercase "sid").
const authSessionHeader = "sid"

// Definition returns the metadata of the piHole kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "Pi-hole"}
}

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(context.Background(), input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values pi-hole consumes
// (integrationDefs piHole secretKinds [["apiKey"], []] — the password is
// OPTIONAL, an unauthenticated pi-hole works without any secret).
type Secrets struct {
	// APIKey is the secret of kind "apiKey": the webserver password (v6
	// login password / v5 auth parameter).
	APIKey string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map. The apiKey
// is optional ([] is a valid secret set upstream).
func SecretsFromMap(m map[string]string) (Secrets, error) {
	return Secrets{APIKey: m["apiKey"]}, nil
}

// Integration is one Pi-hole instance of a detected version. All methods are
// safe for concurrent use; the v6 session travels under the mutex.
type Integration struct {
	input   integrations.Input
	secrets Secrets
	urls    mediaorganizer.URLBuilder
	http    mediaorganizer.HTTPClient

	// version is 5 or 6 (the /api/info/login probe result).
	version int

	mu        sync.Mutex
	storedSID *string // v6 session store ({ sid: string | null })
}

// New probes the server version (createPiHoleIntegrationAsync) and builds the
// matching client. The probe never fails construction upstream: transport
// errors fall back to v6 (:24-29).
func New(ctx context.Context, input integrations.Input, secrets Secrets) *Integration {
	i := &Integration{
		input:   input,
		secrets: secrets,
		urls:    mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
		version: 6,
	}
	if i.probeV5(ctx) {
		i.version = 5
	}
	return i
}

// NewFromRef builds the client from a widget-router IntegrationRef.
func NewFromRef(ctx context.Context, ref network.IntegrationRef) (*Integration, error) {
	secrets, err := SecretsFromMap(ref.Secrets)
	if err != nil {
		return nil, err
	}
	return New(ctx, ref.Input(), secrets), nil
}

// probeV5 mirrors the factory detection: /api/info/login answers 404 on v5.
func (i *Integration) probeV5(ctx context.Context) bool {
	url, err := i.urls.Build("/api/info/login", nil)
	if err != nil {
		return false
	}
	res, err := i.http.Do(ctx, http.MethodGet, url, nil, nil)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode == http.StatusNotFound
}

// Version reports the detected server version (5 or 6).
func (i *Integration) Version() int { return i.version }

// Check implements integrations.Integration (testingAsync of both versions):
// v6 logs in, deletes the server session and clears the store; v5 answers
// /admin/api.php?status and treats a non-object body (the empty array for a
// wrong key) as the 401 result.
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	if i.version == 6 {
		return i.checkV6(ctx)
	}
	return i.checkV5(ctx)
}

// checkV6 ports PiHoleIntegrationV6.testingAsync (:98-103).
func (i *Integration) checkV6(ctx context.Context) (integrations.CheckResult, error) {
	sid, err := i.getSessionV6(ctx)
	if err != nil {
		return integrations.CheckResult{Error: classifyMessage(err)}, nil
	}
	if err := i.clearSessionV6(ctx, sid); err != nil {
		return integrations.CheckResult{Error: classifyMessage(err)}, nil
	}
	i.mu.Lock()
	i.storedSID = nil
	i.mu.Unlock()
	return integrations.CheckResult{StatusCode: http.StatusOK}, nil
}

// checkV5 ports PiHoleIntegrationV5.testingAsync (:38-53).
func (i *Integration) checkV5(ctx context.Context) (integrations.CheckResult, error) {
	raw, status, err := i.getV5(ctx, "/admin/api.php", map[string]string{"status": ""})
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.MsgRequest}, nil
	}
	if status < 200 || status >= 300 {
		return integrations.CheckResult{StatusCode: status, Error: mediaorganizer.MsgResponse}, nil
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return integrations.CheckResult{StatusCode: status, Error: mediaorganizer.MsgParse}, nil
	}
	// Pi-hole v5 returned an empty array if the API key is wrong (:47-50).
	if _, ok := body.(map[string]any); !ok {
		return integrations.CheckResult{StatusCode: http.StatusUnauthorized, Error: mediaorganizer.MsgResponse}, nil
	}
	return integrations.CheckResult{StatusCode: status}, nil
}

// GetSummary ports getSummaryAsync of both versions.
func (i *Integration) GetSummary(ctx context.Context) (network.DnsHoleSummary, error) {
	if i.version == 6 {
		return i.getSummaryV6(ctx)
	}
	return i.getSummaryV5(ctx)
}

// getSummaryV5 ports PiHoleIntegrationV5.getSummaryAsync (:20-36).
func (i *Integration) getSummaryV5(ctx context.Context) (network.DnsHoleSummary, error) {
	raw, status, err := i.getV5(ctx, "/admin/api.php", map[string]string{"summaryRaw": ""})
	if err != nil {
		return network.DnsHoleSummary{}, mediaorganizer.NewRequestError(err)
	}
	if status < 200 || status >= 300 {
		return network.DnsHoleSummary{}, mediaorganizer.NewResponseError(fmt.Errorf("status %d for %s", status, "/admin/api.php"))
	}
	var data struct {
		Status              string   `json:"status"`
		DomainsBeingBlocked *float64 `json:"domains_being_blocked"`
		AdsBlockedToday     *float64 `json:"ads_blocked_today"`
		DNSQueriesToday     *float64 `json:"dns_queries_today"`
		AdsPercentageToday  *float64 `json:"ads_percentage_today"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return network.DnsHoleSummary{}, mediaorganizer.NewParseError(err)
	}
	return network.DnsHoleSummary{
		Status:                    i.v5Status(data.Status),
		DomainsBeingBlocked:       derefNumber(data.DomainsBeingBlocked),
		AdsBlockedToday:           derefNumber(data.AdsBlockedToday),
		AdsBlockedTodayPercentage: derefNumber(data.AdsPercentageToday),
		DNSQueriesToday:           derefNumber(data.DNSQueriesToday),
	}, nil
}

// v5Status maps the v5 status enum ("enabled"/"disabled") — anything else
// would fail the zod enum upstream, so unknown values drop the member (the
// only representable behavior).
func (i *Integration) v5Status(status string) *string {
	if status == network.StatusEnabled || status == network.StatusDisabled {
		return network.Ptr(status)
	}
	return nil
}

// getSummaryV6 ports PiHoleIntegrationV6.getSummaryAsync (:85-96): stats
// first, blocking status second (sequential awaits upstream).
func (i *Integration) getSummaryV6(ctx context.Context) (network.DnsHoleSummary, error) {
	stats, err := i.getStatsSummaryV6(ctx)
	if err != nil {
		return network.DnsHoleSummary{}, err
	}
	blocking, err := i.getDnsBlockingStatusV6(ctx)
	if err != nil {
		return network.DnsHoleSummary{}, err
	}
	return network.DnsHoleSummary{
		Status:                    blocking,
		AdsBlockedToday:           stats.blocked,
		AdsBlockedTodayPercentage: stats.percentBlocked,
		DomainsBeingBlocked:       stats.domainsBeingBlocked,
		DNSQueriesToday:           stats.total,
	}, nil
}

// v6Stats is the mapped slice of statsSummaryGetSchema
// (pi-hole-schemas-v6.ts:20-29).
type v6Stats struct {
	total               float64
	blocked             float64
	percentBlocked      float64
	domainsBeingBlocked float64
}

func (i *Integration) getStatsSummaryV6(ctx context.Context) (v6Stats, error) {
	raw, err := i.withAuthV6(ctx, func(sid *string) (*http.Response, error) {
		return i.getV6(ctx, "/api/stats/summary", sid)
	})
	if err != nil {
		return v6Stats{}, err
	}
	var data struct {
		Queries struct {
			Total          *float64 `json:"total"`
			Blocked        *float64 `json:"blocked"`
			PercentBlocked *float64 `json:"percent_blocked"`
		} `json:"queries"`
		Gravity struct {
			DomainsBeingBlocked *float64 `json:"domains_being_blocked"`
		} `json:"gravity"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return v6Stats{}, mediaorganizer.NewParseError(err)
	}
	return v6Stats{
		total:               derefNumber(data.Queries.Total),
		blocked:             derefNumber(data.Queries.Blocked),
		percentBlocked:      derefNumber(data.Queries.PercentBlocked),
		domainsBeingBlocked: derefNumber(data.Gravity.DomainsBeingBlocked),
	}, nil
}

// getDnsBlockingStatusV6 ports getDnsBlockingStatusAsync (:48-64) with the
// dnsBlockingGetSchema transform (:11-18): "failed"/"unknown" -> undefined
// (member dropped).
func (i *Integration) getDnsBlockingStatusV6(ctx context.Context) (*string, error) {
	raw, err := i.withAuthV6(ctx, func(sid *string) (*http.Response, error) {
		return i.getV6(ctx, "/api/dns/blocking", sid)
	})
	if err != nil {
		return nil, err
	}
	var data struct {
		Blocking string `json:"blocking"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, mediaorganizer.NewParseError(err)
	}
	switch data.Blocking {
	case network.StatusEnabled, network.StatusDisabled:
		return network.Ptr(data.Blocking), nil
	default:
		return nil, nil
	}
}

// Enable ports enableAsync of both versions.
func (i *Integration) Enable(ctx context.Context) error {
	if i.version == 6 {
		_, err := i.withAuthV6(ctx, func(sid *string) (*http.Response, error) {
			return i.postV6(ctx, "/api/dns/blocking", sid, []byte(`{"blocking":true}`))
		})
		return err
	}
	_, status, err := i.getV5(ctx, "/admin/api.php", map[string]string{"enable": ""})
	if err != nil {
		return mediaorganizer.NewRequestError(err)
	}
	if status < 200 || status >= 300 {
		// plain Error upstream — the message travels through the mutation.
		return fmt.Errorf("Failed to enable PiHole for %s (%s): %s",
			i.input.Name, i.input.ID, statusTextOf(status))
	}
	return nil
}

// Disable ports disableAsync of both versions. duration is seconds; v5
// appends `=<seconds>` only for a truthy duration (0/undefined disable
// WITHOUT a value, :65-74); v6 drops the timer member when undefined
// (:121-135); upstream errors keep their exact wire messages.
func (i *Integration) Disable(ctx context.Context, duration *network.DisableDurationSeconds) error {
	if i.version == 6 {
		body := []byte(`{"blocking":false}`)
		if duration != nil {
			body = []byte(fmt.Sprintf(`{"blocking":false,"timer":%s}`, formatNumber(*duration)))
		}
		_, err := i.withAuthV6(ctx, func(sid *string) (*http.Response, error) {
			return i.postV6(ctx, "/api/dns/blocking", sid, body)
		})
		return err
	}
	params := map[string]string{"disable": ""}
	if duration != nil && *duration != 0 {
		params["disable"] = formatNumber(*duration)
	}
	_, status, err := i.getV5(ctx, "/admin/api.php", params)
	if err != nil {
		return mediaorganizer.NewRequestError(err)
	}
	if status < 200 || status >= 300 {
		// plain Error upstream — the message travels through the mutation.
		return fmt.Errorf("Failed to disable PiHole for %s (%s): %s",
			i.input.Name, i.input.ID, statusTextOf(status))
	}
	return nil
}

// --- v6 session flow ---

// withAuthV6 ports withAuthAsync (:142-163): no apiKey -> no session; a
// stored session is reused unless the request answers 401, then a fresh
// login runs before retrying.
func (i *Integration) withAuthV6(ctx context.Context, callback func(sid *string) (*http.Response, error)) ([]byte, error) {
	if i.secrets.APIKey == "" {
		res, err := callback(nil)
		if err != nil {
			return nil, err
		}
		return readV6Body(res)
	}

	i.mu.Lock()
	stored := i.storedSID
	i.mu.Unlock()
	if stored != nil {
		res, err := callback(stored)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusUnauthorized {
			return readV6Body(res)
		}
		// Session expired, getting new session (:155-157); drain the rejected
		// response before retrying.
		_, _ = ioReadAll(res.Body)
		res.Body.Close()
	}

	sid, err := i.getSessionV6(ctx)
	if err != nil {
		return nil, err
	}
	i.mu.Lock()
	i.storedSID = network.Ptr(sid)
	i.mu.Unlock()
	res, err := callback(network.Ptr(sid))
	if err != nil {
		return nil, err
	}
	return readV6Body(res)
}

// getSessionV6 ports getSessionAsync (:169-198): POST /api/auth with the
// password (empty string without an apiKey), User-Agent Homarr.
func (i *Integration) getSessionV6(ctx context.Context) (string, error) {
	url, err := i.urls.Build("/api/auth", nil)
	if err != nil {
		return "", mediaorganizer.NewRequestError(err)
	}
	password := ""
	if i.secrets.APIKey != "" {
		password = i.secrets.APIKey
	}
	body, _ := json.Marshal(map[string]string{"password": password})
	header := http.Header{"User-Agent": []string{"Homarr"}}
	res, err := i.http.Do(ctx, http.MethodPost, url, header, body)
	if err != nil {
		return "", mediaorganizer.NewRequestError(err)
	}
	defer res.Body.Close()
	raw, err := ioReadAll(res.Body)
	if err != nil {
		return "", mediaorganizer.NewParseError(err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// ResponseError(response) — the message is pinned verbatim.
		return "", errors.New(network.MsgResponseNotIndicated)
	}
	// sessionResponseSchema (pi-hole-schemas-v6.ts:3-9): the wrapper object.
	var data struct {
		Session struct {
			Valid   bool    `json:"valid"`
			SID     *string `json:"sid"`
			Message *string `json:"message"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", mediaorganizer.NewParseError(err)
	}
	if !data.Session.Valid {
		// ResponseError({status: 401, ...}, {cause: message}) — the message is
		// pinned verbatim (:186-193).
		return "", errors.New(network.MsgResponseNotIndicated)
	}
	if data.Session.SID == nil {
		// A valid session without a sid (passwordless pi-hole).
		return "", nil
	}
	return *data.Session.SID, nil
}

// clearSessionV6 ports clearSessionAsync (:204-225): DELETE /api/auth with
// the sid header; failures are logged upstream, not thrown.
func (i *Integration) clearSessionV6(ctx context.Context, sid string) error {
	if sid == "" {
		return nil
	}
	url, err := i.urls.Build("/api/auth", nil)
	if err != nil {
		return nil
	}
	header := http.Header{authSessionHeader: []string{sid}}
	res, err := i.http.Do(ctx, http.MethodDelete, url, header, nil)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	_, _ = ioReadAll(res.Body)
	return nil
}

func (i *Integration) getV6(ctx context.Context, path string, sid *string) (*http.Response, error) {
	url, err := i.urls.Build(path, nil)
	if err != nil {
		return nil, mediaorganizer.NewRequestError(err)
	}
	return i.http.Do(ctx, http.MethodGet, url, sessionHeader(sid), nil)
}

func (i *Integration) postV6(ctx context.Context, path string, sid *string, body []byte) (*http.Response, error) {
	url, err := i.urls.Build(path, nil)
	if err != nil {
		return nil, mediaorganizer.NewRequestError(err)
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	if sid := sessionHeader(sid); sid != nil {
		header = sid
		header.Set("Content-Type", "application/json")
	}
	return i.http.Do(ctx, http.MethodPost, url, header, body)
}

// sessionHeader builds the "sid" header of an authenticated v6 request —
// absent without a session (sid ?? undefined, :37-45).
func sessionHeader(sid *string) http.Header {
	if sid == nil || *sid == "" {
		return nil
	}
	return http.Header{authSessionHeader: []string{*sid}}
}

// readV6Body enforces the response.ok checks of the v6 integration (every
// !response.ok throws the common ResponseError, whose message is pinned).
func readV6Body(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	raw, err := ioReadAll(res.Body)
	if err != nil {
		return nil, mediaorganizer.NewParseError(err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, responseNotIndicatedError(res.StatusCode)
	}
	return raw, nil
}

func responseNotIndicatedError(int) error {
	// ResponseError(response) — a PLAIN error carrying the pinned message
	// verbatim: the dnsHole mutations surface it on the wire (upstream throws
	// the @homarr/common/server ResponseError, not an IntegrationError).
	return errors.New(network.MsgResponseNotIndicated)
}

// classifyMessage renders an error message for the CheckResult union.
func classifyMessage(err error) string {
	return mediaorganizer.AsError(err).Error()
}

// --- v5 plumbing ---

// getV5 performs one GET /admin/api.php with the auth query parameter. The
// query values keep the upstream URL.searchParams serialization (an empty
// value serializes as "key=", base/integration.ts createUrl).
func (i *Integration) getV5(ctx context.Context, path string, params map[string]string) ([]byte, int, error) {
	query := map[string]string{"auth": i.secrets.APIKey}
	for k, v := range params {
		query[k] = v
	}
	url, err := i.urls.Build(path, query)
	if err != nil {
		return nil, 0, mediaorganizer.NewRequestError(err)
	}
	res, err := i.http.Do(ctx, http.MethodGet, url, nil, nil)
	if err != nil {
		return nil, 0, mediaorganizer.NewRequestError(err)
	}
	defer res.Body.Close()
	raw, err := ioReadAll(res.Body)
	if err != nil {
		return nil, res.StatusCode, mediaorganizer.NewParseError(err)
	}
	return raw, res.StatusCode, nil
}

// statusTextOf mirrors JS response.statusText (the standard reason phrase;
// HTTP/2 answers carry "").
func statusTextOf(status int) string {
	return http.StatusText(status)
}

func derefNumber(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// formatNumber renders a JS number (JSON.stringify semantics: integral
// values without decimals).
func formatNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ioReadAll drains a body fully (response.text()/json() upstream).
func ioReadAll(body io.Reader) ([]byte, error) { return io.ReadAll(body) }

// Compile-time interface checks.
var (
	_ network.Client           = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
