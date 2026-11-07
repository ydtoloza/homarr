package board_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/board"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness: a real trpc.Router with the board-domain routers registered,
// served over httptest, with a header-driven session injection standing in
// for the auth middleware (ctxauth is populated by internal/auth in
// production; tests inject sessions directly, as documented in ctxauth.go).

// testUser is one seeded caller.
type testUser struct {
	id    string
	name  string
	admin bool // fork administrator flag on the session
}

const (
	aliceName = "alice"
	bobName   = "bob"
	carolName = "carol"
	rootName  = "root"
)

// newTestAPI builds the server. The returned map keys are user names.
func newTestAPI(t *testing.T) (*db.DB, *httptest.Server, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	board.Register(r, d, board.Options{})

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

// seedUser creates the user row and the session the middleware would carry.
func seedUser(t *testing.T, d *db.DB, u testUser) *ctxauth.Session {
	t.Helper()
	err := d.CreateUser(context.Background(), db.User{ID: u.id, Name: u.name, Provider: "credentials"})
	if err != nil {
		t.Fatalf("seed user %s: %v", u.name, err)
	}
	return &ctxauth.Session{ID: "session-" + u.id, UserID: u.id, Name: u.name, IsAdministrator: u.admin}
}

// seedGroup inserts a group row and optionally membership + permission rows.
func seedGroup(t *testing.T, d *db.DB, id, name string, memberUserIDs []string, permissions []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `INSERT INTO "group" (id, name, position) VALUES (?, ?, 0)`, id, name); err != nil {
		t.Fatalf("seed group %s: %v", name, err)
	}
	for _, userID := range memberUserIDs {
		if _, err := d.ExecContext(ctx, `INSERT INTO "groupMember" (group_id, user_id) VALUES (?, ?)`, id, userID); err != nil {
			t.Fatalf("seed group member: %v", err)
		}
	}
	for _, permission := range permissions {
		if _, err := d.ExecContext(ctx, `INSERT INTO "groupPermission" (group_id, permission) VALUES (?, ?)`, id, permission); err != nil {
			t.Fatalf("seed group permission: %v", err)
		}
	}
}

// grantBoardCreate puts the user into a group carrying board-create, like an
// administrator would configure upstream (no default permissions are seeded:
// seed.ts:307-320 creates the everyone group without permission rows).
func grantBoardCreate(t *testing.T, d *db.DB, userID string) {
	t.Helper()
	seedGroup(t, d, "group-creator-"+userID, "creators-"+userID, []string{userID}, []string{"board-create"})
}

func seedBoardUserPermission(t *testing.T, d *db.DB, boardID, userID, permission string) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO "boardUserPermission" (board_id, user_id, permission) VALUES (?, ?, ?)`,
		boardID, userID, permission); err != nil {
		t.Fatalf("seed board user permission: %v", err)
	}
}

func seedBoardGroupPermission(t *testing.T, d *db.DB, boardID, groupID, permission string) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO "boardGroupPermission" (board_id, group_id, permission) VALUES (?, ?, ?)`,
		boardID, groupID, permission); err != nil {
		t.Fatalf("seed board group permission: %v", err)
	}
}

// call performs one tRPC round trip: method GET (queryString) or POST (body).
func call(t *testing.T, srv *httptest.Server, user string, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return res.StatusCode, string(raw)
}

// unwrapData peels {"result":{"data":{"json": X}}} into X (httpLink shape).
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
				Code    int    `json:"code"`
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
		t.Fatalf("expected success, got error %+v (body %s)", envelope.Error, body)
	}
	return envelope.Result.Data.JSON
}

// unwrapError peels the error envelope of a failed single call.
func unwrapError(t *testing.T, body string) (code, message string) {
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

// expectError runs one call and asserts its tRPC error code.
func expectError(t *testing.T, srv *httptest.Server, user string, method, path, body, wantCode string) string {
	t.Helper()
	_, resBody := call(t, srv, user, method, path, body)
	gotCode, message := unwrapError(t, resBody)
	if gotCode != wantCode {
		t.Fatalf("%s %s: got error %s (%s), want %s", method, path, gotCode, message, wantCode)
	}
	return message
}

// encodedInput builds the ?input= query value of a GET call.
func encodedInput(payload string) string {
	return "?input=" + superjsonEnvelope(payload)
}

// superjsonEnvelope wraps a payload string into the superjson envelope
// {"json": <payload>}, URL-unencoded (http.NewRequest escapes it).
func superjsonEnvelope(payload string) string {
	if payload == "" {
		payload = "null"
	}
	return `{"json":` + payload + `}`
}

// mustCreateBoard creates a board through the API as the given user and
// returns (boardID, baseLayoutID).
func mustCreateBoard(t *testing.T, srv *httptest.Server, user, name string, columnCount int, isPublic bool) (string, string) {
	t.Helper()
	input := fmt.Sprintf(`{"name":%q,"columnCount":%d,"isPublic":%v}`, name, columnCount, isPublic)
	status, body := call(t, srv, user, http.MethodPost, "/api/trpc/board.createBoard", superjsonEnvelope(input))
	if status != http.StatusOK {
		t.Fatalf("createBoard %s: status %d body %s", name, status, body)
	}
	var out struct {
		BoardID  string `json:"boardId"`
		Name     string `json:"name"`
		LayoutID string `json:"layoutId"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode createBoard output: %v (%s)", err, body)
	}
	if out.BoardID == "" || out.LayoutID == "" || out.Name != name {
		t.Fatalf("createBoard output mismatch: %+v", out)
	}
	return out.BoardID, out.LayoutID
}
