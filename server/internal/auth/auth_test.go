package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// newTestAuth opens a migrated test database and an Auth service with a
// controllable clock.
func newTestAuth(t *testing.T) (*Auth, *db.DB, *time.Time) {
	t.Helper()
	d := db.OpenTest(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := New(d, Options{Secret: "test-secret", Now: func() time.Time { return now }})
	return a, d, &now
}

// seedUser inserts a credentials user with a bcrypt password and optionally
// grants the admin permission through a group.
func seedUser(t *testing.T, d *db.DB, id, name, password string, admin bool) {
	t.Helper()
	ctx := context.Background()
	hash := ""
	if password != "" {
		var err error
		hash, err = HashPassword(password)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
	}
	if err := d.CreateUser(ctx, db.User{ID: id, Name: name, Email: name + "@example.com", Password: hash, Provider: "credentials", ColorScheme: "dark"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if admin {
		if _, err := d.ExecContext(ctx, `INSERT INTO "group" (id, name, owner_id, position) VALUES (?, ?, NULL, 0)`, "g-admin", "admins"); err != nil {
			t.Fatalf("insert group: %v", err)
		}
		if _, err := d.ExecContext(ctx, `INSERT INTO "groupPermission" (group_id, permission) VALUES ('g-admin', 'admin')`); err != nil {
			t.Fatalf("insert groupPermission: %v", err)
		}
		if _, err := d.ExecContext(ctx, `INSERT INTO "groupMember" (group_id, user_id) VALUES ('g-admin', ?)`, id); err != nil {
			t.Fatalf("insert groupMember: %v", err)
		}
	}
}

func TestSessionCreateValidateExpire(t *testing.T) {
	a, d, now := newTestAuth(t)
	seedUser(t, d, "u1", "admin", "", false) // session.user_id references "user"
	ctx := context.Background()

	s, err := a.CreateSession(ctx, "u1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(s.Token) != 96 {
		t.Errorf("session token length = %d, want 96 (48 random bytes hex)", len(s.Token))
	}
	if !s.Expires.Equal(now.Add(DefaultSessionTTL)) {
		t.Errorf("session expires = %v, want now+30d", s.Expires)
	}

	got, err := a.ValidateSession(ctx, s.Token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if got.Token != s.Token || got.UserID != "u1" {
		t.Errorf("ValidateSession mismatch: %+v", got)
	}

	// Time passes beyond the expiry: the session is invalid.
	*now = now.Add(DefaultSessionTTL + time.Second)
	if _, err := a.ValidateSession(ctx, s.Token); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("ValidateSession(expired) = %v, want ErrNotFound", err)
	}

	// Unknown tokens are ErrNotFound too.
	if _, err := a.ValidateSession(ctx, "no-such-token"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("ValidateSession(unknown) = %v, want ErrNotFound", err)
	}
}

func TestSessionDelete(t *testing.T) {
	a, d, _ := newTestAuth(t)
	seedUser(t, d, "u1", "admin", "", false)
	ctx := context.Background()

	s, err := a.CreateSession(ctx, "u1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := a.DeleteSession(ctx, s.Token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := a.ValidateSession(ctx, s.Token); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("ValidateSession after delete = %v, want ErrNotFound", err)
	}
	// The row itself is gone.
	var n int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM "session"`).Scan(&n); err != nil || n != 0 {
		t.Errorf("session rows after delete = (%d, %v), want (0, nil)", n, err)
	}
}

// probeHandler captures the ctxauth session the middleware stored, and
// answers 200 so anonymous pass-through is observable.
type probeHandler struct {
	session *ctxauth.Session
	called  bool
}

func (p *probeHandler) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	p.called = true
	p.session = ctxauth.FromContext(r.Context())
}

func runMiddleware(a *Auth, cookie *http.Cookie, apiKey string) (*probeHandler, *httptest.ResponseRecorder) {
	h := &probeHandler{}
	req := httptest.NewRequest(http.MethodGet, "/api/trpc/whatever", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if apiKey != "" {
		req.Header.Set("ApiKey", apiKey)
	}
	rec := httptest.NewRecorder()
	a.Middleware()(h).ServeHTTP(rec, req)
	return h, rec
}

func TestMiddlewareSessionCookie(t *testing.T) {
	a, d, _ := newTestAuth(t)
	seedUser(t, d, "u1", "admin", "", true)
	ctx := context.Background()

	s, err := a.CreateSession(ctx, "u1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	cookie := &http.Cookie{Name: a.sessionCookieName(), Value: s.Token}
	h, rec := runMiddleware(a, cookie, "")
	if rec.Code != http.StatusOK || !h.called {
		t.Fatalf("middleware status = %d, called=%v; want 200, true", rec.Code, h.called)
	}
	if h.session == nil || h.session.UserID != "u1" || !h.session.IsAdministrator {
		t.Errorf("session = %+v, want u1 administrator", h.session)
	}
}

func TestMiddlewareAnonymous(t *testing.T) {
	a, _, _ := newTestAuth(t)

	h, rec := runMiddleware(a, nil, "")
	if rec.Code != http.StatusOK || !h.called {
		t.Fatalf("middleware status = %d, called=%v; want 200, true", rec.Code, h.called)
	}
	if h.session != nil {
		t.Errorf("anonymous request got session %+v, want nil", h.session)
	}
}

func TestMiddlewareExpiredSession(t *testing.T) {
	a, d, now := newTestAuth(t)
	seedUser(t, d, "u1", "admin", "", false)
	ctx := context.Background()

	s, err := a.CreateSession(ctx, "u1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	*now = now.Add(DefaultSessionTTL + time.Minute)

	h, rec := runMiddleware(a, &http.Cookie{Name: a.sessionCookieName(), Value: s.Token}, "")
	if rec.Code != http.StatusOK || !h.called {
		t.Fatalf("middleware status = %d, called=%v; want 200, true", rec.Code, h.called)
	}
	if h.session != nil {
		t.Errorf("expired session got %+v, want nil", h.session)
	}
}

func TestMiddlewareAPIKey(t *testing.T) {
	a, d, _ := newTestAuth(t)
	seedUser(t, d, "u1", "admin", "", true)
	ctx := context.Background()

	hash, err := HashPassword("tokensecret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := d.CreateAPIKey(ctx, db.APIKey{ID: "abcd1234", Hash: hash, ReadOnly: true, UserID: "u1"}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	t.Run("valid", func(t *testing.T) {
		h, rec := runMiddleware(a, nil, "abcd1234.tokensecret")
		if rec.Code != http.StatusOK || !h.called {
			t.Fatalf("middleware status = %d, called=%v; want 200, true", rec.Code, h.called)
		}
		if h.session == nil || h.session.UserID != "u1" || !h.session.IsAdministrator {
			t.Errorf("session = %+v, want u1 administrator", h.session)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		h, rec := runMiddleware(a, nil, "abcd1234.wrongsecret")
		if rec.Code != http.StatusUnauthorized || h.called {
			t.Fatalf("status = %d, called=%v; want 401, false", rec.Code, h.called)
		}
		if got := rec.Body.String(); got != `{"error":"UNAUTHORIZED"}` {
			t.Errorf("body = %q, want the upstream plain 401 shape", got)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		_, rec := runMiddleware(a, nil, "ffffffff.tokensecret")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("malformed header", func(t *testing.T) {
		for _, bad := range []string{"nodot", "a.b.c", ".tokensecret", "abcd1234."} {
			_, rec := runMiddleware(a, nil, bad)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("header %q: status = %d, want 401", bad, rec.Code)
			}
		}
	})

	// API key wins over the session cookie, and a broken key 401s even when a
	// valid session cookie is present (upstream route.ts:41-46: the ApiKey
	// branch short-circuits with 401 before the session fallback).
	t.Run("key beats session cookie", func(t *testing.T) {
		s, err := a.CreateSession(ctx, "u1")
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		cookie := &http.Cookie{Name: a.sessionCookieName(), Value: s.Token}
		h, _ := runMiddleware(a, cookie, "abcd1234.tokensecret")
		if h.session == nil || h.session.UserID != "u1" {
			t.Errorf("session = %+v, want u1", h.session)
		}
		_, rec := runMiddleware(a, cookie, "abcd1234.wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("invalid key with valid cookie: status = %d, want 401", rec.Code)
		}
	})
}
