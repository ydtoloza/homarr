// Package v1 is the implementation of the ping integration, the Go port of
// upstream's sendPingRequestAsync (packages/ping/src/index.ts): one HTTP
// request against a URL that reports whether the target is online and how
// long the response took.
//
// Exact upstream request semantics (sendPingRequestAsync):
//   - method GET (fetch default),
//   - redirects followed (fetch default),
//   - TLS certificates NOT verified ("Ping should always work, even with
//     untrusted certificates", rejectUnauthorized: false),
//   - hard timeout of 10s (withTimeoutAsync default,
//     packages/core/src/infrastructure/http/timeout.ts),
//   - duration measured from before the request until the response headers
//     arrive; the body is never read (response.body.cancel()).
//
// A transport failure is a definitive answer, not an error: upstream returns
// {error: message} (extractErrorMessage, packages/common/src/error.ts) and
// never throws, so Check mirrors that as CheckResult{Error: msg}, nil.
package v1

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
)

// DefaultTimeout mirrors upstream's withTimeoutAsync default (10000 ms).
const DefaultTimeout = 10 * time.Second

// client is shared across checks: it is stateless apart from connection
// pooling. TLS verification is disabled to match the upstream dispatcher
// (rejectUnauthorized: false) — a ping must succeed against self-signed
// homelab endpoints.
var client = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // upstream pings with rejectUnauthorized:false
		// Same knobs as http.DefaultTransport, without sharing its global state.
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// Integration pings one URL configured in Input.URL.
type Integration struct {
	// URL is the ping target (app.pingUrl ?? app.href, the resolution of
	// widget.app.ping, packages/api/src/router/widgets/app.ts:30).
	URL string
	// Timeout caps each check. Defaults to DefaultTimeout (upstream hardcodes
	// 10s; the field exists so tests can shorten it).
	Timeout time.Duration
}

// New builds the integration for the framework factory
// (internal/integrations/ping).
func New(input integrations.Input) *Integration {
	return &Integration{URL: input.URL, Timeout: DefaultTimeout}
}

// Check performs one ping and returns the upstream result shape: on success
// {statusCode, durationMs}, on transport failure {error}. It never returns a
// non-nil error — mirroring sendPingRequestAsync, which converts every
// failure into the error member.
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	timeout := i.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, i.URL, nil)
	if err != nil {
		// Bad URL/scheme: a definitive "cannot reach this" answer, same as
		// fetch failing on the input (upstream: {error: message}).
		return integrations.CheckResult{Error: err.Error()}, nil
	}

	start := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	durationMs := float64(time.Since(start).Microseconds()) / 1000

	// Upstream cancels the body without reading it
	// (void response.body?.cancel()); durationMs covers headers only. Closing
	// without draining mirrors that (the connection is not reused, fine for a
	// once-a-minute check).
	_ = res.Body.Close()

	return integrations.CheckResult{StatusCode: res.StatusCode, DurationMs: durationMs}, nil
}
