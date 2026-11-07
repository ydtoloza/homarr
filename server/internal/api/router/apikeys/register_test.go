package apikeys_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/apikeys"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/auth"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness (style of board/harness_test.go): a real trpc.Router over
// httptest with header-driven session injection.
func newTestAPI(t *testing.T) (*db.DB, *httptest.Server, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	apikeys.Register(r, d, apikeys.Options{})

	sessions := map[string]*ctxauth.Session{}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			session, ok := sessions[name]
			if !ok {
				t.Errorf("unknown test user %q", name)
			}
			req = req.WithContext(ctxauth.WithSession(req.Context(), session))
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return d, srv, sessions
}

func seedUser(t *testing.T, d *db.DB, id, name string, admin bool) *ctxauth.Session {
	t.Helper()
	if err := d.CreateUser(context.Background(), db.User{ID: id, Name: name, Provider: "credentials"}); err != nil {
		t.Fatalf("seed user %s: %v", name, err)
	}
	return &ctxauth.Session{ID: "session-" + id, UserID: id, Name: name, IsAdministrator: admin}
}

// call performs one tRPC round trip (POST with a superjson envelope).
func call(t *testing.T, srv *httptest.Server, user, path, payload string) (int, string) {
	t.Helper()
	body := ""
	if payload == "" {
		body = "null"
	} else {
		body = `{"json":` + payload + `}`
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return res.StatusCode, string(raw)
}

func unwrapData(t *testing.T, body string) json.RawMessage {
	t.Helper()
	var envelope struct {
		Result struct {
			Data struct {
				JSON json.RawMessage `json:"json"`
			} `json:"data"`
		} `json:"result"`
		Error *struct {
			JSON struct {
				Data struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", body, err)
	}
	if envelope.Error != nil {
		t.Fatalf("expected success, got error %+v (body %s)", envelope.Error, body)
	}
	return envelope.Result.Data.JSON
}

func unwrapErrorCode(t *testing.T, body string) string {
	t.Helper()
	var envelope struct {
		Error *struct {
			JSON struct {
				Data struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Error == nil {
		t.Fatalf("expected error envelope, got %s", body)
	}
	return envelope.Error.JSON.Data.Code
}

// TestAPIKeyLifecycle covers the full required roundtrip: create returns
// the plaintext id.token once, the stored hash verifies the token (bcrypt),
// getAll lists the key with its user and delete removes it.
func TestAPIKeyLifecycle(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)

	// Guards: anonymous -> UNAUTHORIZED, non-admin -> FORBIDDEN.
	status, body := call(t, srv, "", "/api/trpc/apiKeys.create", "")
	if status != http.StatusUnauthorized || unwrapErrorCode(t, body) != "UNAUTHORIZED" {
		t.Fatalf("create anonymous = %d %s", status, body)
	}
	status, body = call(t, srv, "alice", "/api/trpc/apiKeys.create", "")
	if status != http.StatusForbidden || unwrapErrorCode(t, body) != "FORBIDDEN" {
		t.Fatalf("create non-admin = %d %s", status, body)
	}

	// Create (admin): the plaintext key comes back exactly once.
	status, body = call(t, srv, "root", "/api/trpc/apiKeys.create", "")
	if status != http.StatusOK {
		t.Fatalf("create status = %d (%s)", status, body)
	}
	var created struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &created); err != nil {
		t.Fatalf("decode create output: %v (%s)", err, body)
	}
	// Format "<id>.<token>": 8 hex chars, dot, 48 hex chars.
	keyPattern := regexp.MustCompile(`^[0-9a-f]{8}\.[0-9a-f]{48}$`)
	if !keyPattern.MatchString(created.APIKey) {
		t.Fatalf("apiKey format mismatch: %q", created.APIKey)
	}
	id, token, found := strings.Cut(created.APIKey, ".")
	if !found {
		t.Fatalf("apiKey has no dot separator: %q", created.APIKey)
	}

	// The stored hash verifies the token (bcrypt cost 10, AUTH.md §6).
	stored, err := d.GetAPIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatalf("stored key lookup: %v", err)
	}
	if stored.UserID != "user-root" {
		t.Fatalf("stored key owner = %q, want user-root", stored.UserID)
	}
	if !auth.ComparePassword(token, stored.Hash) {
		t.Fatalf("bcrypt verify of token against stored hash failed")
	}
	if auth.ComparePassword("wrong-token", stored.Hash) {
		t.Fatalf("wrong token unexpectedly verifies")
	}

	// getAll (admin): the key with its user projection.
	status, body = call(t, srv, "root", "/api/trpc/apiKeys.getAll", "")
	if status != http.StatusOK {
		t.Fatalf("getAll status = %d (%s)", status, body)
	}
	var list []struct {
		ID       string `json:"id"`
		ReadOnly bool   `json:"readOnly"`
		User     struct {
			ID   string          `json:"id"`
			Name json.RawMessage `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &list); err != nil {
		t.Fatalf("decode getAll output: %v (%s)", err, body)
	}
	if len(list) != 1 || list[0].ID != id || list[0].User.ID != "user-root" {
		t.Fatalf("getAll output mismatch: %+v", list)
	}

	// delete (admin): the key disappears.
	status, body = call(t, srv, "root", "/api/trpc/apiKeys.delete",
		fmt.Sprintf(`{"apiKeyId":%q}`, id))
	if status != http.StatusOK {
		t.Fatalf("delete status = %d (%s)", status, body)
	}
	if _, err := d.GetAPIKeyByID(context.Background(), id); err != db.ErrNotFound {
		t.Fatalf("key still present after delete: %v", err)
	}

	// readOnly flag round trip.
	status, body = call(t, srv, "root", "/api/trpc/apiKeys.create", `{"readOnly":true}`)
	if status != http.StatusOK {
		t.Fatalf("create readOnly status = %d (%s)", status, body)
	}
	if err := json.Unmarshal(unwrapData(t, body), &created); err != nil {
		t.Fatalf("decode create output: %v (%s)", err, body)
	}
	roID, _, _ := strings.Cut(created.APIKey, ".")
	stored, err = d.GetAPIKeyByID(context.Background(), roID)
	if err != nil {
		t.Fatalf("stored readOnly key lookup: %v", err)
	}
	if !stored.ReadOnly {
		t.Fatalf("readOnly flag not persisted")
	}
}
