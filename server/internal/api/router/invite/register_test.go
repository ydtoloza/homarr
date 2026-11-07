package invite_test

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
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/invite"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness (style of board/harness_test.go).
func newTestAPI(t *testing.T) (*db.DB, *httptest.Server, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	invite.Register(r, d, invite.Options{})

	sessions := map[string]*ctxauth.Session{}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			req = req.WithContext(ctxauth.WithSession(req.Context(), sessions[name]))
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

func call(t *testing.T, srv *httptest.Server, user, path, payload string) (int, string) {
	t.Helper()
	body := "null"
	if payload != "" {
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
				Message string `json:"message"`
				Data    struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", body, err)
	}
	if envelope.Error != nil {
		t.Fatalf("expected success, got %s (%s)", envelope.Error.JSON.Data.Code, body)
	}
	return envelope.Result.Data.JSON
}

func unwrapErrorCode(t *testing.T, body string) (string, string) {
	t.Helper()
	var envelope struct {
		Error *struct {
			JSON struct {
				Message string `json:"message"`
				Data    struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Error == nil {
		t.Fatalf("expected error envelope, got %s", body)
	}
	return envelope.Error.JSON.Data.Code, envelope.Error.JSON.Message
}

func TestInviteLifecycle(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)

	// Guards: anonymous -> UNAUTHORIZED, non-admin -> FORBIDDEN.
	status, body := call(t, srv, "", "/api/trpc/invite.createInvite", `{"expirationDate":"2030-01-01T00:00:00Z"}`)
	if status != http.StatusUnauthorized {
		if code, _ := unwrapErrorCode(t, body); code != "UNAUTHORIZED" {
			t.Fatalf("anonymous create = %s (%s)", body, code)
		}
	}
	status, body = call(t, srv, "alice", "/api/trpc/invite.getAll", "")
	if status != http.StatusForbidden {
		code, _ := unwrapErrorCode(t, body)
		t.Fatalf("non-admin getAll status = %d code = %s, want FORBIDDEN", status, code)
	}

	// createInvite: {id, token} with a 40-char hex token.
	status, body = call(t, srv, "root", "/api/trpc/invite.createInvite", `{"expirationDate":"2030-01-01T00:00:00Z"}`)
	if status != http.StatusOK {
		t.Fatalf("createInvite status = %d (%s)", status, body)
	}
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &created); err != nil {
		t.Fatalf("decode createInvite output: %v (%s)", err, body)
	}
	if created.ID == "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(created.Token) {
		t.Fatalf("createInvite output mismatch: %+v", created)
	}

	// getAll: the invite with its creator and WITHOUT the token, ordered
	// by expiration; expirationDate is an ISO string (superjson note).
	future := time.Now().Add(48 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	if err := d.CreateInvite(context.Background(), db.Invite{
		ID: "invite-old", Token: "older-token",
		ExpirationDate: time.Now().Add(time.Hour), CreatorID: "user-root",
	}); err != nil {
		t.Fatalf("seed second invite: %v", err)
	}
	status, body = call(t, srv, "root", "/api/trpc/invite.getAll", "")
	if status != http.StatusOK {
		t.Fatalf("getAll status = %d (%s)", status, body)
	}
	var list []struct {
		ID             string          `json:"id"`
		ExpirationDate string          `json:"expirationDate"`
		Token          json.RawMessage `json:"token"`
		Creator        struct {
			Name json.RawMessage `json:"name"`
			ID   string          `json:"id"`
		} `json:"creator"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &list); err != nil {
		t.Fatalf("decode getAll output: %v (%s)", err, body)
	}
	if len(list) != 2 {
		t.Fatalf("getAll returned %d invites, want 2 (%s)", len(list), body)
	}
	if list[0].ID != "invite-old" {
		t.Fatalf("getAll not ordered by expiration: %+v", list)
	}
	for _, inv := range list {
		if inv.Token != nil { // the key must not appear at all
			t.Fatalf("getAll leaked the token: %s", inv.Token)
		}
		if inv.Creator.ID != "user-root" || string(inv.Creator.Name) != `"root"` {
			t.Fatalf("getAll creator mismatch: %+v", inv.Creator)
		}
	}
	if !strings.HasPrefix(list[1].ExpirationDate, "2030-01-01T00:00:00") &&
		list[1].ID == created.ID {
		t.Fatalf("expirationDate not rendered as ISO string: %q (want like %q)", list[1].ExpirationDate, future)
	}

	// deleteInvite: unknown id -> NOT_FOUND "Invite not found".
	status, body = call(t, srv, "root", "/api/trpc/invite.deleteInvite", `{"id":"missing"}`)
	if status != http.StatusNotFound {
		code, message := unwrapErrorCode(t, body)
		t.Fatalf("delete unknown = %d %s (%s)", status, code, message)
	}
	status, body = call(t, srv, "root", "/api/trpc/invite.deleteInvite",
		fmt.Sprintf(`{"id":%q}`, created.ID))
	if status != http.StatusOK {
		t.Fatalf("deleteInvite status = %d (%s)", status, body)
	}
	if _, err := d.GetInviteByID(context.Background(), created.ID); err != db.ErrNotFound {
		t.Fatalf("invite still present after delete: %v", err)
	}
}

func TestInviteValidation(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)

	// z.iso.datetime requires an offset-carrying timestamp.
	status, body := call(t, srv, "root", "/api/trpc/invite.createInvite", `{"expirationDate":"not-a-date"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid date status = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/invite.createInvite", `{"expirationDate":"2030-01-01T00:00:00"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("offset-less date status = %d (%s)", status, body)
	}
}
