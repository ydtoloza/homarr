package appmeta

// The SSRF-guarded HTTP fetch of the metadata detection — the port of
// fetchGuardedAsync (packages/core/src/infrastructure/http/guarded-fetch.ts)
// reduced to the needs of the website metadata flow (no trusted-origin
// exception, GET only, Accept as the only header).
//
// Guarantees kept from upstream:
//   - every hop resolves the hostname and refuses addresses inside loopback
//     (127/8, ::1), link-local, unspecified, multicast, reserved and the
//     cloud metadata services (169.254/16 plus the AWS/Azure hosts outside
//     link-local); private networks stay reachable because self-hosted apps
//     run there (guarded-fetch.ts:34-51);
//   - the check happens per connection in the dialer, so every redirect hop
//     is re-checked and a DNS change between check and connect cannot
//     redirect the request (the pinned lookup of guarded-fetch.ts:117-142 —
//     the port dials only an address it just verified);
//   - up to five redirects (maximumRedirects, guarded-fetch.ts:29), manual
//     like upstream; credentials are stripped from every hop;
//   - headers do not follow a cross-origin redirect
//     (guarded-fetch.ts:216-219);
//   - no env proxy (upstream forces noProxy "*", guarded-fetch.ts:147-149).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// maximumRedirects mirrors guarded-fetch.ts:29.
const maximumRedirects = 5

// blockedV4 / blockedV6 mirror the two BlockList seed loops
// (guarded-fetch.ts:35-54).
var blockedV4 = mustParseCIDRs([]string{
	"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16",
	"100.100.100.200/32", "168.63.129.16/32",
	"224.0.0.0/4", "240.0.0.0/4",
})

var blockedV6 = mustParseCIDRs([]string{
	"::/96", "fe80::/10", "fd00:ec2::254/128", "ff00::/8",
})

func mustParseCIDRs(list []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(list))
	for _, cidr := range list {
		_, parsed, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("appmeta: bad blocked CIDR %q: %v", cidr, err))
		}
		out = append(out, parsed)
	}
	return out
}

// isBlockedAddress reports whether the IP falls inside a blocked network.
// The IPv4-compatible range (::/96) is blocked as a whole before any IPv4
// mapping applies (guarded-fetch.ts:48); IPv4-mapped IPv6 addresses fall
// under the IPv4 rules, like the Node BlockList does ("Node matches the IPv4
// rules against IPv4-mapped IPv6 addresses as well",
// guarded-fetch.ts:31-33).
func isBlockedAddress(ip net.IP) bool {
	if len(ip) == net.IPv6len && blockedV6[0].Contains(ip) {
		return true // ::/96
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range blockedV4 {
			if n.Contains(v4) {
				return true
			}
		}
		return false
	}
	for _, n := range blockedV6 {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// resolveAllowedAddresses is resolveAllowedAddressesAsync
// (guarded-fetch.ts:157-178): resolve, filter to v4/v6 and refuse the whole
// hop when any address is blocked (the upstream `addresses.find(
// isBlockedAddress)` semantics: one blocked record rejects the origin).
func resolveAllowedAddresses(ctx context.Context, host string) ([]net.IP, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%s did not resolve: %w", host, err)
	}
	out := make([]net.IP, 0, len(ips))
	for _, entry := range ips {
		if entry.IP.To4() == nil && entry.IP.To16() == nil {
			continue
		}
		out = append(out, entry.IP)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s did not resolve to an address", host)
	}
	for _, ip := range out {
		if isBlockedAddress(ip) {
			return nil, fmt.Errorf("refusing to request %s because it resolves to %s", host, ip)
		}
	}
	return out, nil
}

// newGuardedClient builds the HTTP client the detection requests run
// through. timeout bounds each hop's dial; the caller wraps the whole
// detection in its own deadline.
func newGuardedClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		// Upstream forces noProxy "*" — a proxy would resolve the target
		// itself and bypass the pinned lookup (guarded-fetch.ts:143-150).
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			allowed, err := resolveAllowedAddresses(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range allowed {
				// Pin the connection to an address that was JUST verified.
				conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
				lastErr = dialErr
			}
			if lastErr == nil {
				lastErr = errors.New("no checked address for the requested family")
			}
			return nil, lastErr
		},
	}
	client := &http.Client{Transport: transport}
	// Manual redirects: fetchGuardedAsync loops itself so every hop is
	// checked; ErrUseLastResponse hands the 3xx response back to the loop.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

// guardedHTTP carries the client the hops run through.
type guardedHTTP struct {
	client *http.Client
}

// get performs the redirect loop of fetchGuardedAsync
// (guarded-fetch.ts:192-224): at most five hops, credentials stripped,
// headers dropped when the redirect leaves the origin, per-hop timeout, the
// caller deadline on top. Returns the final response (unclosed — the caller
// owns the body) and its URL.
func (g *guardedHTTP) get(ctx context.Context, requestURL *url.URL, headers map[string]string, timeout time.Duration) (*http.Response, *url.URL, error) {
	current := *requestURL
	current.User = nil
	currentHeaders := headers
	for redirects := 0; ; redirects++ {
		hopCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(hopCtx, http.MethodGet, current.String(), nil)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		for k, v := range currentHeaders {
			req.Header.Set(k, v)
		}
		res, err := g.client.Do(req)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		location := res.Header.Get("Location")
		if res.StatusCode < 300 || res.StatusCode > 399 || location == "" {
			// Final response: the hop context must outlive the body read, so
			// its cancel fires when the caller closes the body (or at the
			// hop timeout, whichever comes first).
			res.Body = &cancelOnCloseBody{ReadCloser: res.Body, cancel: cancel}
			return res, &current, nil
		}
		// Redirect hop: drain and close, then follow.
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		_ = res.Body.Close()
		cancel()
		if redirects == maximumRedirects {
			return nil, nil, fmt.Errorf("too many redirects starting at %s", requestURL.Scheme+"://"+requestURL.Host)
		}
		target, err := current.Parse(location)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid redirect location from %s", current.Host)
		}
		target.User = nil
		if target.Host != current.Host || target.Scheme != current.Scheme {
			currentHeaders = nil // headers never follow cross-origin
		}
		current = *target
	}
}

// cancelOnCloseBody releases the hop context when the caller closes the
// final response body (the deadline of the last hop would otherwise cancel
// a body the caller is still reading).
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelOnCloseBody) Close() error {
	b.once.Do(b.cancel)
	return b.ReadCloser.Close()
}
