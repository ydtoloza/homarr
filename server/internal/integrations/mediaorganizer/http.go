package mediaorganizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Shared HTTP plumbing of the media-organizer clients. Upstream issues every
// request through fetchWithTrustedCertificatesAsync
// (packages/core/src/infrastructure/http/request.ts:111-137): TLS verified
// against the system store plus the trusted certificates of the certificates
// domain (not ported yet — the Go transport uses the system roots; the seam is
// Options/HTTP below), redirects followed, and NO per-request timeout unless
// the caller passes one (sonarr/radarr/bindery/sabnzbd pass none; the
// deadline comes from the request-handler layer, DEFAULT_REQUEST_TIMEOUT_MS =
// 60_000, packages/request-handler/src/lib/request-handler.ts:35).

// DefaultRequestTimeout mirrors DEFAULT_REQUEST_TIMEOUT_MS
// (packages/request-handler/src/lib/request-handler.ts:35): the deadline the
// widget layer wraps around one integration call. The clients themselves
// apply no HTTP timeout (upstream fetch has none) and honor the context.
const DefaultRequestTimeout = 60 * time.Second

// HTTPClient is the transport of one client. The zero value defers to a
// package-level client with DefaultTransport settings; tests inject their own
// (httptest or a transport stub).
type HTTPClient struct {
	// Client performs the requests. nil falls back to a shared default with
	// system-verified TLS (upstream fetchWithTrustedCertificatesAsync).
	Client *http.Client
	// Timeout caps one request when > 0. Upstream media-organizer requests
	// have no per-request timeout, so the default is 0 (context only). The
	// download-client ports use 5s (the @ctrl/* libraries hardcode
	// timeout: 5000).
	Timeout time.Duration
}

// DefaultHTTPClient is the shared, stateless transport: same knobs as
// http.DefaultTransport without sharing its global state.
var DefaultHTTPClient = &HTTPClient{
	Client: &http.Client{Transport: defaultTransport()},
}

func defaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	return t
}

func (h *HTTPClient) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return DefaultHTTPClient.Client
}

// Std returns the effective *http.Client (the configured one or the shared
// default). The download-client ports need it for requests the shared
// helpers do not cover (login flows with redirect: manual, cookie handling).
func (h *HTTPClient) Std() *http.Client { return h.client() }

// DoJSON performs one request and decodes the JSON body into out. The
// classification mirrors the upstream decorator stack (base/errors):
// transport failure -> request error, non-2xx -> response error, bad JSON ->
// parse error.
func (h *HTTPClient) DoJSON(ctx context.Context, method, url string, header http.Header, body []byte, out any) error {
	res, err := h.Do(ctx, method, url, header, body)
	if err != nil {
		return NewRequestError(err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return NewResponseError(fmt.Errorf("status %d for %s", res.StatusCode, url))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return NewParseError(err)
	}
	return nil
}

// DoRead performs one GET-shaped request and returns the raw body (used by
// clients that decode a union shape manually).
func (h *HTTPClient) DoRead(ctx context.Context, method, url string, header http.Header) ([]byte, error) {
	res, err := h.Do(ctx, method, url, header, nil)
	if err != nil {
		return nil, NewRequestError(err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, NewResponseError(fmt.Errorf("status %d for %s", res.StatusCode, url))
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, NewParseError(err)
	}
	return raw, nil
}

// Do performs one request and returns the response (caller closes the body).
func (h *HTTPClient) Do(ctx context.Context, method, url string, header http.Header, body []byte) (*http.Response, error) {
	if h.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return h.client().Do(req)
}
