package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	v1 "github.com/ydtoloza/homarr/server/internal/integrations/ping/v1"
)

// shortCheck builds an integration against url with a short timeout for tests.
func shortCheck(url string) *v1.Integration {
	i := v1.New(integrations.Input{ID: "app1", Kind: "ping", URL: url})
	i.Timeout = 250 * time.Millisecond
	return i
}

// TestCheckOnline pins the success shape: any HTTP response counts as a
// definitive answer (upstream returns {statusCode, durationMs} regardless of
// the status; the frontend decides error-ness by statusCode >= 500,
// packages/widgets/src/app/ping/ping-indicator.tsx:20), the method is GET,
// and durationMs is measured up to the response headers.
func TestCheckOnline(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res, err := shortCheck(srv.URL + "/dashboard").Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/dashboard" {
		t.Fatalf("request = %s %s, want GET /dashboard", gotMethod, gotPath)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want 200", res.StatusCode)
	}
	if res.DurationMs <= 0 {
		t.Fatalf("durationMs = %v, want > 0", res.DurationMs)
	}
	if res.Error != "" {
		t.Fatalf("error = %q, want empty", res.Error)
	}
}

// TestCheckServerErrorStillAnswer pins that a 500 is a definitive result, not
// a Check error (the payload keeps statusCode so the frontend can render it).
func TestCheckServerErrorStillAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	res, err := shortCheck(srv.URL).Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusInternalServerError || res.Error != "" {
		t.Fatalf("result = %+v, want statusCode 500 without error", res)
	}
}

// TestCheckConnectionRefused pins the offline shape: a transport failure is a
// definitive {error: message} answer, never a Go error (upstream converts
// every failure with extractErrorMessage, packages/ping/src/index.ts).
func TestCheckConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // free the port so nothing listens on it

	res, err := shortCheck(srv.URL).Check(t.Context())
	if err != nil {
		t.Fatalf("Check must not fail: %v", err)
	}
	if res.Error == "" {
		t.Fatal("error member is empty, want a message")
	}
	if !strings.Contains(res.Error, "connect") && !strings.Contains(res.Error, "refused") {
		t.Logf("error message = %q (any message is acceptable, shape is the contract)", res.Error)
	}
	if res.StatusCode != 0 || res.DurationMs != 0 {
		t.Fatalf("result = %+v, want zeroed success members", res)
	}
}

// TestCheckTimeout pins the hard timeout: a slow target aborts at Timeout and
// reports an error (upstream aborts at 10s via withTimeoutAsync).
func TestCheckTimeout(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-released
	}))
	defer func() {
		close(released)
		srv.Close()
	}()

	i := shortCheck(srv.URL)
	i.Timeout = 100 * time.Millisecond
	start := time.Now()
	res, err := i.Check(t.Context())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Check must not fail: %v", err)
	}
	if res.Error == "" {
		t.Fatal("error member is empty, want a timeout message")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Check took %v, want the ~100ms timeout to abort the request", elapsed)
	}
}

// TestCheckUntrustedCertificate pins rejectUnauthorized:false: a self-signed
// certificate must not fail the ping (upstream: "Ping should always work,
// even with untrusted certificates").
func TestCheckUntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res, err := shortCheck(srv.URL).Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.Error != "" {
		t.Fatalf("result = %+v, want 200 without error (TLS verification disabled)", res)
	}
}

// TestCheckBadScheme pins that a non-HTTP URL is a definitive error answer.
func TestCheckBadScheme(t *testing.T) {
	res, err := shortCheck("vscode://profile/x").Check(t.Context())
	if err != nil {
		t.Fatalf("Check must not fail: %v", err)
	}
	if res.Error == "" || res.StatusCode != 0 {
		t.Fatalf("result = %+v, want {error: ...} only", res)
	}
}

// TestCheckParentContextCancel pins that an already-cancelled parent context
// aborts the check promptly.
func TestCheckParentContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	res, err := shortCheck(srv.URL).Check(ctx)
	if err != nil {
		t.Fatalf("Check must not fail: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancelled context did not abort the request")
	}
	if res.Error == "" {
		t.Fatal("want the cancellation surfaced as the error member")
	}
}

// TestJSONShape pins the exact wire shape of the two result branches, since
// the frontend discriminates with `"error" in pingResult`
// (packages/widgets/src/app/ping/ping-indicator.tsx:20).
func TestJSONShape(t *testing.T) {
	success, err := json.Marshal(integrations.CheckResult{StatusCode: 200, DurationMs: 12.5})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(success) != `{"statusCode":200,"durationMs":12.5}` {
		t.Fatalf("success payload = %s", success)
	}

	failure, err := json.Marshal(integrations.CheckResult{Error: "boom"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(failure) != `{"error":"boom"}` {
		t.Fatalf("failure payload = %s", failure)
	}
}
