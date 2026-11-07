package user_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/user"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/auth"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// testClaimToken backs the user.initUser claim flow.
const testClaimToken = "user-claim-token"

func newTestAPI(t *testing.T) (*db.DB, *httptest.Server, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	user.Register(r, d, user.Options{
		OnboardingClaimTokenFromContext: func(ctx context.Context) string {
			token, _ := ctx.Value(claimKey{}).(string)
			return token
		},
	})

	sessions := map[string]*ctxauth.Session{}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		if token := req.Header.Get("X-Test-Claim"); token != "" {
			ctx = context.WithValue(ctx, claimKey{}, token)
		}
		if name := req.Header.Get("X-Test-User"); name != "" {
			ctx = ctxauth.WithSession(ctx, sessions[name])
		}
		r.Handler().ServeHTTP(w, req.WithContext(ctx))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return d, srv, sessions
}

type claimKey struct{}

func seedUser(t *testing.T, d *db.DB, id, name string, admin bool) *ctxauth.Session {
	t.Helper()
	return seedUserWithPassword(t, d, id, name, "", admin)
}

func seedUserWithPassword(t *testing.T, d *db.DB, id, name, password string, admin bool) *ctxauth.Session {
	t.Helper()
	hash := ""
	if password != "" {
		var err error
		hash, err = auth.HashPassword(password)
		if err != nil {
			t.Fatalf("hash password: %v", err)
		}
	}
	if err := d.CreateUser(context.Background(), db.User{ID: id, Name: name, Password: hash, Provider: "credentials"}); err != nil {
		t.Fatalf("seed user %s: %v", name, err)
	}
	return &ctxauth.Session{ID: "session-" + id, UserID: id, Name: name, IsAdministrator: admin}
}

func call(t *testing.T, srv *httptest.Server, user, path, payload, claim string) (int, string) {
	t.Helper()
	method := http.MethodGet
	if payload != "" {
		method = http.MethodPost
	}
	var reader io.Reader
	body := ""
	if payload != "" {
		body = `{"json":` + payload + `}`
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	if claim != "" {
		req.Header.Set("X-Test-Claim", claim)
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
		t.Fatalf("expected success, got %s: %s", envelope.Error.JSON.Data.Code, body)
	}
	return envelope.Result.Data.JSON
}

func unwrapError(t *testing.T, body string) (string, string) {
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

func TestUserCreateAndList(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)

	// Guards: anonymous -> UNAUTHORIZED, non-admin -> FORBIDDEN.
	status, body := call(t, srv, "", "/api/trpc/user.create",
		`{"username":"bob","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass"}`, "")
	if status != http.StatusUnauthorized {
		code, _ := unwrapError(t, body)
		t.Fatalf("anonymous create = %d %s", status, code)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.create",
		`{"username":"bob","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass"}`, "")
	if status != http.StatusForbidden {
		code, _ := unwrapError(t, body)
		t.Fatalf("non-admin create = %d %s", status, code)
	}

	// create: void output; the user row is stored with a bcrypt hash.
	status, body = call(t, srv, "root", "/api/trpc/user.create",
		`{"username":"Bob","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass","email":"bob@example.com","groupIds":[]}`, "")
	if status != http.StatusOK {
		t.Fatalf("create status = %d (%s)", status, body)
	}
	var stored db.User
	stored, err := d.GetUserByName(context.Background(), "bob", "credentials")
	if err != nil {
		t.Fatalf("read created user: %v", err)
	}
	if !auth.ComparePassword("Str0ng!Pass", stored.Password) {
		t.Fatalf("created user password hash does not verify")
	}

	// Duplicate username (case-insensitive through the lowercase
	// transform) CONFLICTs.
	status, body = call(t, srv, "root", "/api/trpc/user.create",
		`{"username":"BOB","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass"}`, "")
	if status != http.StatusConflict {
		code, message := unwrapError(t, body)
		t.Fatalf("duplicate username = %d %s (%s)", status, code, message)
	}

	// Negative input validation: short password, mismatched confirmation,
	// short username.
	status, body = call(t, srv, "root", "/api/trpc/user.create",
		`{"username":"carol","password":"short","confirmPassword":"short"}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("short password = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/user.create",
		`{"username":"carol","password":"Str0ng!Pass","confirmPassword":"Different1!"}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("mismatched confirmation = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/user.create",
		`{"username":"ab","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass"}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("short username = %d (%s)", status, body)
	}

	// getAll (admin): the {id, name, email, emailVerified, image}
	// projection with nulls preserved.
	status, body = call(t, srv, "root", "/api/trpc/user.getAll", "", "")
	if status != http.StatusOK {
		t.Fatalf("getAll status = %d (%s)", status, body)
	}
	var users []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &users); err != nil {
		t.Fatalf("decode getAll: %v (%s)", err, body)
	}
	if len(users) != 3 {
		t.Fatalf("getAll users = %d, want 3", len(users))
	}
	var bob map[string]any
	for _, u := range users {
		if u["name"] == "bob" {
			bob = u
		}
	}
	if bob == nil || bob["email"] != "bob@example.com" || bob["emailVerified"] != nil || bob["image"] != nil {
		t.Fatalf("getAll bob = %v", bob)
	}

	// selectable (protected): every signed-in caller can list users.
	status, body = call(t, srv, "alice", "/api/trpc/user.selectable", "", "")
	if status != http.StatusOK {
		t.Fatalf("selectable status = %d (%s)", status, body)
	}
	if err := json.Unmarshal(unwrapData(t, body), &users); err != nil {
		t.Fatalf("decode selectable: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("selectable users = %d, want 3", len(users))
	}

	// search (admin): LIKE %query%.
	status, body = call(t, srv, "root", "/api/trpc/user.search", `{"query":"ali","limit":10}`, "")
	if status != http.StatusOK {
		t.Fatalf("search status = %d (%s)", status, body)
	}
	if err := json.Unmarshal(unwrapData(t, body), &users); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(users) != 1 || users[0]["name"] != "alice" {
		t.Fatalf("search results = %v", users)
	}
}

func TestUserChangePassword(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["alice"] = seedUserWithPassword(t, d, "user-alice", "alice", "OldPassword1!", false)
	sessions["root"] = seedUserWithPassword(t, d, "user-root", "root", "OldPassword1!", true)

	// Anonymous -> UNAUTHORIZED.
	status, body := call(t, srv, "", "/api/trpc/user.changePassword",
		`{"userId":"user-alice","previousPassword":"OldPassword1!","password":"NewPassword1!","confirmPassword":"NewPassword1!"}`, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous changePassword = %d (%s)", status, body)
	}

	// Wrong previous password -> FORBIDDEN "Invalid password".
	status, body = call(t, srv, "alice", "/api/trpc/user.changePassword",
		`{"userId":"user-alice","previousPassword":"WrongPassword1!","password":"NewPassword1!","confirmPassword":"NewPassword1!"}`, "")
	if status != http.StatusForbidden {
		code, message := unwrapError(t, body)
		t.Fatalf("wrong previous = %d %s (%s)", status, code, message)
	}

	// Self change: the new hash verifies, the old one does not.
	status, body = call(t, srv, "alice", "/api/trpc/user.changePassword",
		`{"userId":"user-alice","previousPassword":"OldPassword1!","password":"NewPassword1!","confirmPassword":"NewPassword1!"}`, "")
	if status != http.StatusOK {
		t.Fatalf("self change = %d (%s)", status, body)
	}
	stored, err := d.GetUserByID(context.Background(), "user-alice")
	if err != nil {
		t.Fatalf("read user: %v", err)
	}
	if !auth.ComparePassword("NewPassword1!", stored.Password) {
		t.Fatalf("new password does not verify")
	}
	if auth.ComparePassword("OldPassword1!", stored.Password) {
		t.Fatalf("old password still verifies")
	}

	// A non-admin changing someone else's password hides behind NOT_FOUND.
	status, body = call(t, srv, "alice", "/api/trpc/user.changePassword",
		`{"userId":"user-root","previousPassword":"whatever","password":"NewPassword1!","confirmPassword":"NewPassword1!"}`, "")
	if status != http.StatusNotFound {
		code, _ := unwrapError(t, body)
		t.Fatalf("other user = %d %s", status, code)
	}

	// An admin resets another user's password without the previous one.
	status, body = call(t, srv, "root", "/api/trpc/user.changePassword",
		`{"userId":"user-alice","previousPassword":"","password":"AdminSet1!","confirmPassword":"AdminSet1!"}`, "")
	if status != http.StatusOK {
		t.Fatalf("admin reset = %d (%s)", status, body)
	}
	stored, err = d.GetUserByID(context.Background(), "user-alice")
	if err != nil {
		t.Fatalf("read user: %v", err)
	}
	if !auth.ComparePassword("AdminSet1!", stored.Password) {
		t.Fatalf("admin-set password does not verify")
	}
}

func TestUserPreferences(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)

	// getPreferences: the full shape with the parsed header preferences.
	status, body := call(t, srv, "alice", "/api/trpc/user.getPreferences", "", "")
	if status != http.StatusOK {
		t.Fatalf("getPreferences status = %d (%s)", status, body)
	}
	var prefs map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &prefs); err != nil {
		t.Fatalf("decode getPreferences: %v (%s)", err, body)
	}
	if prefs["userId"] != "user-alice" || prefs["colorScheme"] != "dark" ||
		prefs["byteUnitSystem"] != "decimal" || prefs["firstDayOfWeek"] != float64(1) {
		t.Fatalf("getPreferences scalars = %v", prefs)
	}
	header, ok := prefs["headerPreferences"].(map[string]any)
	if !ok || header["version"] != float64(3) || header["visible"] != true {
		t.Fatalf("getPreferences headerPreferences = %v", prefs["headerPreferences"])
	}

	// Another user without admin hides behind NOT_FOUND.
	status, body = call(t, srv, "alice", "/api/trpc/user.getPreferences", `{"userId":"user-root"}`, "")
	if status != http.StatusNotFound {
		code, _ := unwrapError(t, body)
		t.Fatalf("other user preferences = %d %s", status, code)
	}

	// updatePreferences: merge of colorScheme only; the rest survives.
	status, body = call(t, srv, "alice", "/api/trpc/user.updatePreferences", `{"colorScheme":"light"}`, "")
	if status != http.StatusOK {
		t.Fatalf("updatePreferences status = %d (%s)", status, body)
	}
	var updated map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &updated); err != nil {
		t.Fatalf("decode updatePreferences: %v (%s)", err, body)
	}
	if updated["colorScheme"] != "light" || updated["byteUnitSystem"] != "decimal" {
		t.Fatalf("updatePreferences merge = %v", updated)
	}

	// Empty patch and unknown keys reject.
	status, body = call(t, srv, "alice", "/api/trpc/user.updatePreferences", `{}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("empty patch = %d (%s)", status, body)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.updatePreferences", `{"colorScheme":"light","nope":true}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("unknown key = %d (%s)", status, body)
	}

	// changeColorScheme (self only) and changeFirstDayOfWeek bounds.
	status, body = call(t, srv, "alice", "/api/trpc/user.changeColorScheme", `{"colorScheme":"auto"}`, "")
	if status != http.StatusOK {
		t.Fatalf("changeColorScheme = %d (%s)", status, body)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.changeColorScheme", `{"colorScheme":"blue"}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("invalid scheme = %d (%s)", status, body)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.changeFirstDayOfWeek",
		`{"firstDayOfWeek":0,"id":"user-alice"}`, "")
	if status != http.StatusOK {
		t.Fatalf("changeFirstDayOfWeek = %d (%s)", status, body)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.changeFirstDayOfWeek",
		`{"firstDayOfWeek":9,"id":"user-alice"}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("out of range day = %d (%s)", status, body)
	}

	// changeHomeBoards: private boards of other users hide behind
	// NOT_FOUND ("Board not found"), public boards pass.
	ctx := context.Background()
	if err := d.CreateBoard(ctx, db.Board{ID: "board-public", Name: "Public", IsPublic: true}); err != nil {
		t.Fatalf("seed public board: %v", err)
	}
	if err := d.CreateBoard(ctx, db.Board{ID: "board-private", Name: "Private", CreatorID: "user-root"}); err != nil {
		t.Fatalf("seed private board: %v", err)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.changeHomeBoards",
		`{"userId":"user-alice","homeBoardId":"board-public","mobileHomeBoardId":null}`, "")
	if status != http.StatusOK {
		t.Fatalf("changeHomeBoards public = %d (%s)", status, body)
	}
	var homeBoard *string
	if err := d.QueryRow(`SELECT home_board_id FROM "user" WHERE id = 'user-alice'`).Scan(&homeBoard); err != nil {
		t.Fatalf("read home board: %v", err)
	}
	if homeBoard == nil || *homeBoard != "board-public" {
		t.Fatalf("home board = %v", homeBoard)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.changeHomeBoards",
		`{"userId":"user-alice","homeBoardId":"board-private","mobileHomeBoardId":null}`, "")
	if status != http.StatusNotFound {
		code, message := unwrapError(t, body)
		t.Fatalf("private home board = %d %s (%s)", status, code, message)
	}

	// changeDefaultSearchEngine validates the engine reference.
	status, body = call(t, srv, "alice", "/api/trpc/user.changeDefaultSearchEngine",
		`{"userId":"user-alice","defaultSearchEngineId":"engine-missing","ddgBangsEnabled":true}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("missing search engine = %d (%s)", status, body)
	}
	if _, err := d.Exec(`INSERT INTO "search_engine" (id, icon_url, name, short, type) VALUES ('engine-1','http://i','G','g','generic')`); err != nil {
		t.Fatalf("seed engine: %v", err)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.changeDefaultSearchEngine",
		`{"userId":"user-alice","defaultSearchEngineId":"engine-1","ddgBangsEnabled":true}`, "")
	if status != http.StatusOK {
		t.Fatalf("valid search engine = %d (%s)", status, body)
	}

	// changeHeaderPreferences replaces the zones (v3 shape).
	status, body = call(t, srv, "alice", "/api/trpc/user.changeHeaderPreferences",
		`{"id":"user-alice","headerPreferences":{"version":3,"visible":false,"searchDisplay":"icon","zones":{"left":[{"type":"builtin","id":"logo"}],"center":[{"type":"builtin","id":"search"}],"right":[{"type":"builtin","id":"user"}]}}}`, "")
	if status != http.StatusOK {
		t.Fatalf("changeHeaderPreferences = %d (%s)", status, body)
	}
	var storedHeader string
	if err := d.QueryRow(`SELECT header_preferences FROM "user" WHERE id = 'user-alice'`).Scan(&storedHeader); err != nil {
		t.Fatalf("read header: %v", err)
	}
	if !strings.Contains(storedHeader, `"visible":false`) || !strings.Contains(storedHeader, `"searchDisplay":"icon"`) {
		t.Fatalf("stored header = %s", storedHeader)
	}
	// A header without account access rejects (zod refine).
	status, body = call(t, srv, "alice", "/api/trpc/user.changeHeaderPreferences",
		`{"id":"user-alice","headerPreferences":{"version":3,"visible":true,"searchDisplay":"input","zones":{"left":[],"center":[],"right":[{"type":"builtin","id":"logo"}]}}}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("header without user item = %d (%s)", status, body)
	}

	// Tours.
	status, body = call(t, srv, "alice", "/api/trpc/user.completeTour", `{"tour":"manage"}`, "")
	if status != http.StatusOK {
		t.Fatalf("completeTour = %d (%s)", status, body)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.getTourStatus", "", "")
	if got := string(unwrapData(t, body)); got != `{"completedBoardTour":false,"completedManageTour":true}` {
		t.Fatalf("getTourStatus = %s", got)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.resetTours", "", "")
	if status != http.StatusOK {
		t.Fatalf("resetTours = %d (%s)", status, body)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.getTourStatus", "", "")
	if got := string(unwrapData(t, body)); got != `{"completedBoardTour":false,"completedManageTour":false}` {
		t.Fatalf("getTourStatus after reset = %s", got)
	}
}

func TestUserGetByIdEditProfileDelete(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)
	sessions["bob"] = seedUser(t, d, "user-bob", "bob", false)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)

	// getById: self works, another non-admin is FORBIDDEN, admin passes,
	// unknown is NOT_FOUND.
	status, body := call(t, srv, "alice", "/api/trpc/user.getById", `{"userId":"user-alice"}`, "")
	if status != http.StatusOK {
		t.Fatalf("getById self = %d (%s)", status, body)
	}
	var profile map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &profile); err != nil {
		t.Fatalf("decode getById: %v (%s)", err, body)
	}
	if profile["provider"] != "credentials" || profile["headerPreferences"] == nil {
		t.Fatalf("getById profile = %v", profile)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.getById", `{"userId":"user-bob"}`, "")
	if status != http.StatusForbidden {
		code, _ := unwrapError(t, body)
		t.Fatalf("getById other = %d %s", status, code)
	}
	status, body = call(t, srv, "root", "/api/trpc/user.getById", `{"userId":"user-bob"}`, "")
	if status != http.StatusOK {
		t.Fatalf("getById admin = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/user.getById", `{"userId":"missing"}`, "")
	if status != http.StatusNotFound {
		code, _ := unwrapError(t, body)
		t.Fatalf("getById unknown = %d %s", status, code)
	}

	// editProfile: rename + email reset; the email change clears
	// emailVerified.
	if _, err := d.Exec(`UPDATE "user" SET email = 'old@example.com', email_verified = ? WHERE id = 'user-alice'`,
		time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed email: %v", err)
	}
	status, body = call(t, srv, "alice", "/api/trpc/user.editProfile",
		`{"id":"user-alice","name":"Alice2","email":"new@example.com"}`, "")
	if status != http.StatusOK {
		t.Fatalf("editProfile = %d (%s)", status, body)
	}
	var emailVerified *int64
	if err := d.QueryRow(`SELECT email, email_verified FROM "user" WHERE id = 'user-alice'`).Scan(
		new(*string), &emailVerified); err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if emailVerified != nil {
		t.Fatalf("emailVerified not cleared after email change")
	}

	// delete: self deletion works.
	status, body = call(t, srv, "bob", "/api/trpc/user.delete", `{"userId":"user-bob"}`, "")
	if status != http.StatusOK {
		t.Fatalf("delete self = %d (%s)", status, body)
	}
	if _, err := d.GetUserByID(context.Background(), "user-bob"); !strings.Contains(fmt.Sprint(err), "row not found") {
		t.Fatalf("user still present after delete: %v", err)
	}
}

func TestUserInitUserOnboarding(t *testing.T) {
	d, srv, sessions := newTestAPI(t)

	// The wizard row at the "user" step plus a valid claim (the fresh
	// installation flow: POST /api/onboarding/claim mints it).
	sum := sha256Sum(t, testClaimToken)
	value := fmt.Sprintf(`{"hash":%q,"expiresAt":%d}`, sum, time.Now().Add(time.Hour).UnixMilli())
	if err := d.UpsertServerSetting(context.Background(), "__onboarding_claim", value); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO "onboarding" (id, step) VALUES ('onboarding-1', 'user')`); err != nil {
		t.Fatalf("seed onboarding: %v", err)
	}

	// Without the claim (and no administrator) the gate rejects.
	status, body := call(t, srv, "", "/api/trpc/user.initUser",
		`{"username":"admin","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass"}`, "")
	if status != http.StatusForbidden || !strings.Contains(body, "not claimed") {
		t.Fatalf("no claim = %d %s", status, body)
	}

	// With the claim: the administrator, the credentials-admin group, the
	// admin permission and the membership are created and the wizard moves
	// to "setup" (no ldap/oidc providers in the fork).
	status, body = call(t, srv, "", "/api/trpc/user.initUser",
		`{"username":"Admin","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass","email":null}`, testClaimToken)
	if status != http.StatusOK {
		t.Fatalf("initUser = %d (%s)", status, body)
	}
	var adminID string
	if err := d.QueryRow(`SELECT id FROM "user" WHERE name = 'admin'`).Scan(&adminID); err != nil {
		t.Fatalf("read created admin: %v", err)
	}
	stored, err := d.GetUserByID(context.Background(), adminID)
	if err != nil {
		t.Fatalf("load admin: %v", err)
	}
	if !auth.ComparePassword("Str0ng!Pass", stored.Password) {
		t.Fatalf("admin password does not verify")
	}
	var checks int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "groupPermission" gp JOIN "group" g ON g.id = gp.group_id
		JOIN "groupMember" gm ON gm.group_id = g.id
		WHERE g.name = 'credentials-admin' AND gp.permission = 'admin' AND gm.user_id = ?`, adminID).Scan(&checks); err != nil {
		t.Fatalf("read admin setup: %v", err)
	}
	if checks != 1 {
		t.Fatalf("credentials-admin setup rows = %d, want 1", checks)
	}
	var step string
	if err := d.QueryRow(`SELECT step FROM "onboarding" WHERE id = 'onboarding-1'`).Scan(&step); err != nil {
		t.Fatalf("read step: %v", err)
	}
	if step != "setup" {
		t.Fatalf("onboarding step = %q, want setup", step)
	}

	// A second initUser is rejected: the gate no longer matches the step
	// (the CONFLICT "already created" branch fires only in a race).
	sessions["admin"] = &ctxauth.Session{ID: "s1", UserID: adminID, Name: "admin", IsAdministrator: true}
	status, body = call(t, srv, "admin", "/api/trpc/user.initUser",
		`{"username":"admin2","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass"}`, "")
	if status != http.StatusForbidden || !strings.Contains(body, "Step denied") {
		t.Fatalf("second initUser = %d %s", status, body)
	}
}

func TestUserRegisterWithInvite(t *testing.T) {
	d, srv, _ := newTestAPI(t)

	// Seed a live and an expired invite (the creator references the
	// registering admin; the row requires one).
	if err := d.CreateUser(context.Background(), db.User{ID: "user-root", Name: "root", Provider: "credentials"}); err != nil {
		t.Fatalf("seed creator: %v", err)
	}
	if err := d.CreateInvite(context.Background(), db.Invite{
		ID: "invite-live", Token: "livetoken", CreatorID: "user-root",
		ExpirationDate: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed live invite: %v", err)
	}
	if err := d.CreateInvite(context.Background(), db.Invite{
		ID: "invite-dead", Token: "deadtoken", CreatorID: "user-root",
		ExpirationDate: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed dead invite: %v", err)
	}

	// Expired invite -> FORBIDDEN "Invalid invite".
	status, body := call(t, srv, "", "/api/trpc/user.register",
		`{"username":"newbie","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass","inviteId":"invite-dead","token":"deadtoken"}`, "")
	if status != http.StatusForbidden {
		code, message := unwrapError(t, body)
		t.Fatalf("expired invite = %d %s (%s)", status, code, message)
	}

	// Wrong token -> FORBIDDEN.
	status, body = call(t, srv, "", "/api/trpc/user.register",
		`{"username":"newbie","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass","inviteId":"invite-live","token":"wrong"}`, "")
	if status != http.StatusForbidden {
		t.Fatalf("wrong token = %d (%s)", status, body)
	}

	// Valid invite: the user is created and the invite consumed.
	status, body = call(t, srv, "", "/api/trpc/user.register",
		`{"username":"newbie","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass","inviteId":"invite-live","token":"livetoken"}`, "")
	if status != http.StatusOK {
		t.Fatalf("register = %d (%s)", status, body)
	}
	if _, err := d.GetUserByName(context.Background(), "newbie", "credentials"); err != nil {
		t.Fatalf("registered user missing: %v", err)
	}
	if _, err := d.GetInviteByID(context.Background(), "invite-live"); !strings.Contains(fmt.Sprint(err), "row not found") {
		t.Fatalf("invite not consumed: %v", err)
	}

	// The consumed invite cannot be reused.
	status, body = call(t, srv, "", "/api/trpc/user.register",
		`{"username":"newbie2","password":"Str0ng!Pass","confirmPassword":"Str0ng!Pass","inviteId":"invite-live","token":"livetoken"}`, "")
	if status != http.StatusForbidden {
		t.Fatalf("reuse invite = %d (%s)", status, body)
	}
}

// sha256Sum mirrors the claim hashing of the onboarding-claim module.
func sha256Sum(t *testing.T, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
