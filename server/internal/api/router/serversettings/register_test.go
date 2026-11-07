package serversettings_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/serversettings"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness (style of board/harness_test.go).
func newTestAPI(t *testing.T) (*db.DB, *httptest.Server, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	serversettings.Register(r, d, serversettings.Options{})

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

func TestServerSettingsReadShapes(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)

	// getCulture: public; the seeded defaults with no row present.
	status, body := call(t, srv, "", "/api/trpc/serverSettings.getCulture", "")
	if status != http.StatusOK {
		t.Fatalf("getCulture status = %d (%s)", status, body)
	}
	if got, want := string(unwrapData(t, body)), `{"defaultLocale":"en"}`; got != want {
		t.Fatalf("getCulture = %s, want %s", got, want)
	}

	// getBranding: public; parseBrandingSettings defaults (full shape).
	status, body = call(t, srv, "", "/api/trpc/serverSettings.getBranding", "")
	if status != http.StatusOK {
		t.Fatalf("getBranding status = %d (%s)", status, body)
	}
	var branding map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &branding); err != nil {
		t.Fatalf("decode getBranding: %v (%s)", err, body)
	}
	for _, key := range []string{"appName", "primaryColor", "secondaryColor", "authBranding", "defaultRadius", "customCss"} {
		if _, ok := branding[key]; !ok {
			t.Fatalf("getBranding missing key %q in %v", key, branding)
		}
	}
	if branding["appName"] != "Homarr" || branding["signInBackgroundOverlay"] != 0.55 {
		t.Fatalf("getBranding defaults mismatch: %v", branding)
	}

	// getBoardSettings: admin gate.
	status, body = call(t, srv, "", "/api/trpc/serverSettings.getBoardSettings", "")
	if status != http.StatusUnauthorized {
		code, _ := unwrapError(t, body)
		t.Fatalf("getBoardSettings anonymous = %d %s", status, code)
	}
	status, body = call(t, srv, "alice", "/api/trpc/serverSettings.getBoardSettings", "")
	if status != http.StatusForbidden {
		code, _ := unwrapError(t, body)
		t.Fatalf("getBoardSettings non-admin = %d %s", status, code)
	}
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.getBoardSettings", "")
	if status != http.StatusOK {
		t.Fatalf("getBoardSettings status = %d (%s)", status, body)
	}
	wantBoard := `{"enableStatusByDefault":true,"forceDisableStatus":false,"homeBoardId":null,"mobileHomeBoardId":null}`
	if got := string(unwrapData(t, body)); got != wantBoard {
		t.Fatalf("getBoardSettings = %s, want %s", got, wantBoard)
	}

	// getAll: admin; every default key is present.
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.getAll", "")
	if status != http.StatusOK {
		t.Fatalf("getAll status = %d (%s)", status, body)
	}
	var all map[string]map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &all); err != nil {
		t.Fatalf("decode getAll: %v (%s)", err, body)
	}
	for _, key := range []string{"analytics", "crawlingAndIndexing", "board", "user", "appearance", "branding", "culture", "search"} {
		if _, ok := all[key]; !ok {
			t.Fatalf("getAll missing key %q", key)
		}
	}
}

func TestUpdateBoardSettings(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)

	// Seed one public and one private board.
	ctx := context.Background()
	if err := d.CreateBoard(ctx, db.Board{ID: "board-public", Name: "Public", IsPublic: true}); err != nil {
		t.Fatalf("seed public board: %v", err)
	}
	if err := d.CreateBoard(ctx, db.Board{ID: "board-private", Name: "Private"}); err != nil {
		t.Fatalf("seed private board: %v", err)
	}

	// Partial update referencing a public board: the merged record comes
	// back and the row is upserted.
	status, body := call(t, srv, "root", "/api/trpc/serverSettings.updateBoardSettings",
		`{"homeBoardId":"board-public","enableStatusByDefault":false}`)
	if status != http.StatusOK {
		t.Fatalf("updateBoardSettings status = %d (%s)", status, body)
	}
	got := string(unwrapData(t, body))
	want := `{"enableStatusByDefault":false,"forceDisableStatus":false,"homeBoardId":"board-public","mobileHomeBoardId":null}`
	if got != want {
		t.Fatalf("updateBoardSettings = %s, want %s", got, want)
	}

	// A private (non-public) board id is rejected with the upstream
	// message.
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.updateBoardSettings",
		`{"mobileHomeBoardId":"board-private"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("private home board status = %d (%s)", status, body)
	}
	code, message := unwrapError(t, body)
	if code != "BAD_REQUEST" || message != "Board settings home board IDs must reference public boards: board-private" {
		t.Fatalf("private home board error = %s (%s)", code, message)
	}

	// Unknown ids land in the same bucket.
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.updateBoardSettings",
		`{"homeBoardId":"missing-board"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("missing home board status = %d (%s)", status, body)
	}

	// Wrong types reject (boardServerSettingsSchema).
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.updateBoardSettings",
		`{"forceDisableStatus":"yes"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bad type status = %d (%s)", status, body)
	}

	// Guards.
	status, body = call(t, srv, "", "/api/trpc/serverSettings.updateBoardSettings", `{}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous update = %d (%s)", status, body)
	}
	status, body = call(t, srv, "alice", "/api/trpc/serverSettings.updateBoardSettings", `{}`)
	if status != http.StatusForbidden {
		t.Fatalf("non-admin update = %d (%s)", status, body)
	}
}

func TestSaveSettingsBranches(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)

	// Generic merge (culture): the supplied keys overwrite, the rest of
	// the record survives.
	status, body := call(t, srv, "root", "/api/trpc/serverSettings.saveSettings",
		`{"settingsKey":"culture","value":{"defaultLocale":"de"}}`)
	if status != http.StatusOK {
		t.Fatalf("saveSettings culture status = %d (%s)", status, body)
	}
	status, body = call(t, srv, "", "/api/trpc/serverSettings.getCulture", "")
	if got := string(unwrapData(t, body)); got != `{"defaultLocale":"de"}` {
		t.Fatalf("culture after save = %s", got)
	}

	// Analytics: strict schema — unknown keys reject.
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.saveSettings",
		`{"settingsKey":"analytics","value":{"enableGeneral":false,"sneaky":true}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("analytics unknown key status = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.saveSettings",
		`{"settingsKey":"analytics","value":{"enableGeneral":false}}`)
	if status != http.StatusOK {
		t.Fatalf("analytics save status = %d (%s)", status, body)
	}
	setting, err := d.GetServerSetting(context.Background(), "analytics")
	if err != nil {
		t.Fatalf("read analytics row: %v", err)
	}
	if !strings.Contains(setting.Value, `"enableGeneral":false`) {
		t.Fatalf("analytics row = %s", setting.Value)
	}
	// The analytics defaults keep the untouched keys.
	if !strings.Contains(setting.Value, `"instanceId":null`) {
		t.Fatalf("analytics row lost defaults: %s", setting.Value)
	}

	// Branding: legacy keys fold into authBranding.
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.saveSettings",
		`{"settingsKey":"branding","value":{"showCustomLogoOnLogin":false,"appName":"MyDash"}}`)
	if status != http.StatusOK {
		t.Fatalf("branding save status = %d (%s)", status, body)
	}
	setting, err = d.GetServerSetting(context.Background(), "branding")
	if err != nil {
		t.Fatalf("read branding row: %v", err)
	}
	if !strings.Contains(setting.Value, `"showLogo":false`) {
		t.Fatalf("branding row missing legacy fold: %s", setting.Value)
	}
	if !strings.Contains(setting.Value, `"appName":"MyDash"`) {
		t.Fatalf("branding row missing appName: %s", setting.Value)
	}

	// Unknown settingsKey rejects (defaultServerSettingsKeys enum).
	status, body = call(t, srv, "root", "/api/trpc/serverSettings.saveSettings",
		`{"settingsKey":"nope","value":{}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown key status = %d (%s)", status, body)
	}
}
