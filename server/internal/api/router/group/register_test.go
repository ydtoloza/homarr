package group_test

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
	"github.com/ydtoloza/homarr/server/internal/api/router/group"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// testClaimToken backs the createInitialExternalGroup claim flow.
const testClaimToken = "group-claim-token"

func newTestAPI(t *testing.T) (*db.DB, *httptest.Server, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	group.Register(r, d, group.Options{
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
	if err := d.CreateUser(context.Background(), db.User{ID: id, Name: name, Provider: "credentials"}); err != nil {
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

func TestGroupCRUDRoundtrip(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)

	// Guards: anonymous -> UNAUTHORIZED, non-admin -> FORBIDDEN.
	status, body := call(t, srv, "", "/api/trpc/group.createGroup", `{"name":"admins"}`, "")
	if status != http.StatusUnauthorized {
		code, _ := unwrapError(t, body)
		t.Fatalf("anonymous create = %d %s", status, code)
	}
	status, body = call(t, srv, "alice", "/api/trpc/group.createGroup", `{"name":"admins"}`, "")
	if status != http.StatusForbidden {
		code, _ := unwrapError(t, body)
		t.Fatalf("non-admin create = %d %s", status, code)
	}

	// createGroup returns the raw id string; position starts at 2
	// (MaxGroupPosition of an empty table coalesces to 1).
	status, body = call(t, srv, "root", "/api/trpc/group.createGroup", `{"name":"admins"}`, "")
	if status != http.StatusOK {
		t.Fatalf("createGroup status = %d (%s)", status, body)
	}
	var groupID string
	if err := json.Unmarshal(unwrapData(t, body), &groupID); err != nil {
		t.Fatalf("decode createGroup output: %v (%s)", err, body)
	}
	if groupID == "" {
		t.Fatalf("createGroup returned an empty id (%s)", body)
	}
	var position int64
	if err := d.QueryRow(`SELECT position FROM "group" WHERE id = ?`, groupID).Scan(&position); err != nil {
		t.Fatalf("read group position: %v", err)
	}
	if position != 2 {
		t.Fatalf("group position = %d, want 2", position)
	}

	// Membership so getAll/getById render members.
	if err := d.AddGroupMember(context.Background(), groupID, "user-alice"); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// getAll: the group with the member projection.
	status, body = call(t, srv, "root", "/api/trpc/group.getAll", "", "")
	if status != http.StatusOK {
		t.Fatalf("getAll status = %d (%s)", status, body)
	}
	var groups []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &groups); err != nil {
		t.Fatalf("decode getAll: %v (%s)", err, body)
	}
	if len(groups) != 1 {
		t.Fatalf("getAll groups = %d, want 1", len(groups))
	}
	g := groups[0]
	if g["name"] != "admins" || g["position"] != float64(2) || g["ownerId"] != "user-root" {
		t.Fatalf("getAll group row = %v", g)
	}
	members, ok := g["members"].([]any)
	if !ok || len(members) != 1 {
		t.Fatalf("getAll members = %v", g["members"])
	}
	member := members[0].(map[string]any)
	if member["id"] != "user-alice" || member["name"] != "alice" {
		t.Fatalf("getAll member = %v", member)
	}

	// getById: permissions + owner projection; unknown id -> NOT_FOUND.
	status, body = call(t, srv, "root", "/api/trpc/group.getById", fmt.Sprintf(`{"id":%q}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("getById status = %d (%s)", status, body)
	}
	var detail map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &detail); err != nil {
		t.Fatalf("decode getById: %v (%s)", err, body)
	}
	if detail["owner"].(map[string]any)["id"] != "user-root" {
		t.Fatalf("getById owner = %v", detail["owner"])
	}
	status, body = call(t, srv, "root", "/api/trpc/group.getById", `{"id":"missing"}`, "")
	if status != http.StatusNotFound {
		code, message := unwrapError(t, body)
		t.Fatalf("getById unknown = %d %s (%s)", status, code, message)
	}

	// search (admin): LIKE %query%.
	status, body = call(t, srv, "root", "/api/trpc/group.search", `{"query":"dmi","limit":10}`, "")
	if status != http.StatusOK {
		t.Fatalf("search status = %d (%s)", status, body)
	}
	var found []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &found); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(found) != 1 || found[0]["id"] != groupID {
		t.Fatalf("search results = %v", found)
	}

	// getPaginated: {items, totalCount}.
	status, body = call(t, srv, "root", "/api/trpc/group.getPaginated", `{"pageSize":10,"page":1}`, "")
	if status != http.StatusOK {
		t.Fatalf("getPaginated status = %d (%s)", status, body)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		TotalCount int64            `json:"totalCount"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &page); err != nil {
		t.Fatalf("decode getPaginated: %v (%s)", err, body)
	}
	if page.TotalCount != 1 || len(page.Items) != 1 {
		t.Fatalf("getPaginated = %+v", page)
	}

	// selectable (protected): non-admin gets the plain {id, name} rows.
	status, body = call(t, srv, "alice", "/api/trpc/group.selectable", "", "")
	if status != http.StatusOK {
		t.Fatalf("selectable status = %d (%s)", status, body)
	}
	var selectable []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &selectable); err != nil {
		t.Fatalf("decode selectable: %v", err)
	}
	if len(selectable) != 1 || len(selectable[0]) != 2 {
		t.Fatalf("selectable rows = %v", selectable)
	}
	// withPermissions for an admin carries the (empty) permission list.
	status, body = call(t, srv, "root", "/api/trpc/group.selectable", `{"withPermissions":true}`, "")
	if status != http.StatusOK {
		t.Fatalf("selectable admin status = %d (%s)", status, body)
	}
	if err := json.Unmarshal(unwrapData(t, body), &selectable); err != nil {
		t.Fatalf("decode selectable admin: %v", err)
	}
	if _, ok := selectable[0]["permissions"]; !ok {
		t.Fatalf("admin selectable missing permissions: %v", selectable[0])
	}

	// savePermissions: stored deduplicated, exactly as selected.
	status, body = call(t, srv, "root", "/api/trpc/group.savePermissions",
		fmt.Sprintf(`{"groupId":%q,"permissions":["admin","admin","board-create"]}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("savePermissions status = %d (%s)", status, body)
	}
	var perms []string
	if err := json.Unmarshal(unwrapData(t, body), &perms); err == nil {
		t.Fatalf("savePermissions is void (%s)", body)
	}
	var storedPerms []string
	rows, err := d.Query(`SELECT permission FROM "groupPermission" WHERE group_id = ? ORDER BY permission`, groupID)
	if err != nil {
		t.Fatalf("read permissions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan permission: %v", err)
		}
		storedPerms = append(storedPerms, p)
	}
	if len(storedPerms) != 2 {
		t.Fatalf("stored permissions = %v, want deduplicated [admin board-create]", storedPerms)
	}

	// savePartialSettings: only the supplied keys change.
	status, body = call(t, srv, "root", "/api/trpc/group.savePartialSettings",
		fmt.Sprintf(`{"id":%q,"settings":{"homeBoardId":null}}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("savePartialSettings status = %d (%s)", status, body)
	}
	var homeBoard *string
	if err := d.QueryRow(`SELECT home_board_id FROM "group" WHERE id = ?`, groupID).Scan(&homeBoard); err != nil {
		t.Fatalf("read home board: %v", err)
	}
	if homeBoard != nil {
		t.Fatalf("home board = %v, want NULL", homeBoard)
	}

	// savePositions: index+1 per id.
	status, body = call(t, srv, "root", "/api/trpc/group.savePositions",
		fmt.Sprintf(`{"positions":[%q]}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("savePositions status = %d (%s)", status, body)
	}
	if err := d.QueryRow(`SELECT position FROM "group" WHERE id = ?`, groupID).Scan(&position); err != nil {
		t.Fatalf("read position: %v", err)
	}
	if position != 1 {
		t.Fatalf("position after savePositions = %d, want 1", position)
	}

	// transferOwnership + updateGroup (rename).
	status, body = call(t, srv, "root", "/api/trpc/group.transferOwnership",
		fmt.Sprintf(`{"groupId":%q,"userId":"user-alice"}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("transferOwnership status = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/group.updateGroup",
		fmt.Sprintf(`{"id":%q,"name":"moderators"}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("updateGroup status = %d (%s)", status, body)
	}
	var name string
	if err := d.QueryRow(`SELECT name FROM "group" WHERE id = ?`, groupID).Scan(&name); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if name != "moderators" {
		t.Fatalf("renamed group = %q", name)
	}

	// addMember / removeMember.
	status, body = call(t, srv, "root", "/api/trpc/group.addMember",
		fmt.Sprintf(`{"groupId":%q,"userId":"user-root"}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("addMember status = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/group.addMember",
		fmt.Sprintf(`{"groupId":%q,"userId":"user-missing"}`, groupID), "")
	if status != http.StatusNotFound {
		code, _ := unwrapError(t, body)
		t.Fatalf("addMember unknown user = %d %s", status, code)
	}
	status, body = call(t, srv, "root", "/api/trpc/group.removeMember",
		fmt.Sprintf(`{"groupId":%q,"userId":"user-alice"}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("removeMember status = %d (%s)", status, body)
	}

	// deleteGroup removes the row.
	status, body = call(t, srv, "root", "/api/trpc/group.deleteGroup",
		fmt.Sprintf(`{"id":%q}`, groupID), "")
	if status != http.StatusOK {
		t.Fatalf("deleteGroup status = %d (%s)", status, body)
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "group" WHERE id = ?`, groupID).Scan(&count); err != nil {
		t.Fatalf("count groups: %v", err)
	}
	if count != 0 {
		t.Fatalf("group still present after delete")
	}
}

func TestGroupReservedAndSimilarNames(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = seedUser(t, d, "user-root", "root", true)

	// groupCreateSchema rejects the reserved name outright.
	status, body := call(t, srv, "root", "/api/trpc/group.createGroup", `{"name":"everyone"}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("create everyone status = %d (%s)", status, body)
	}

	// Seed the everyone group directly (the auth layer creates it on sign
	// in; seed.ts:307-320).
	if _, err := d.Exec(`INSERT INTO "group" (id, name, position) VALUES ('group-everyone', 'everyone', 0)`); err != nil {
		t.Fatalf("seed everyone: %v", err)
	}

	// Similar names CONFLICT (LIKE match, case-insensitive).
	status, body = call(t, srv, "root", "/api/trpc/group.createGroup", `{"name":"ADMINS"}`, "")
	if status != http.StatusOK {
		t.Fatalf("create ADMINS status = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/group.createGroup", `{"name":"admins"}`, "")
	if status != http.StatusConflict {
		code, message := unwrapError(t, body)
		t.Fatalf("similar name = %d %s (%s)", status, code, message)
	}

	// The reserved group rejects mutations with FORBIDDEN.
	for _, proc := range []struct {
		path, payload string
	}{
		{"/api/trpc/group.updateGroup", `{"id":"group-everyone","name":"all"}`},
		{"/api/trpc/group.deleteGroup", `{"id":"group-everyone"}`},
		{"/api/trpc/group.transferOwnership", `{"groupId":"group-everyone","userId":"user-root"}`},
		{"/api/trpc/group.addMember", `{"groupId":"group-everyone","userId":"user-root"}`},
	} {
		status, body = call(t, srv, "root", proc.path, proc.payload, "")
		if status != http.StatusForbidden {
			code, _ := unwrapError(t, body)
			t.Fatalf("%s on everyone = %d %s", proc.path, status, code)
		}
	}

	// Validation: name > 64 chars and the empty name reject.
	status, body = call(t, srv, "root", "/api/trpc/group.createGroup",
		`{"name":"`+strings.Repeat("x", 65)+`"}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("long name = %d (%s)", status, body)
	}
	status, body = call(t, srv, "root", "/api/trpc/group.createGroup", `{"name":"  "}`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("blank name = %d (%s)", status, body)
	}
}

func TestGroupCreateInitialExternalGroup(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = &ctxauth.Session{ID: "s1", UserID: "user-root", Name: "root", IsAdministrator: true}

	// The wizard row at the "group" step plus a valid claim.
	sum := sha256Sum(t, testClaimToken)
	value := fmt.Sprintf(`{"hash":%q,"expiresAt":%d}`, sum, time.Now().Add(time.Hour).UnixMilli())
	if err := d.UpsertServerSetting(context.Background(), "__onboarding_claim", value); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO "onboarding" (id, step) VALUES ('onboarding-1', 'group')`); err != nil {
		t.Fatalf("seed onboarding: %v", err)
	}

	// Without the claim the gate rejects.
	status, body := call(t, srv, "", "/api/trpc/group.createInitialExternalGroup", `{"name":"admins"}`, "")
	if status != http.StatusForbidden || !strings.Contains(body, "not claimed") {
		t.Fatalf("no claim = %d %s", status, body)
	}

	// With the claim the group is created with the admin permission and
	// the wizard moves to "setup".
	status, body = call(t, srv, "", "/api/trpc/group.createInitialExternalGroup", `{"name":"admins"}`, testClaimToken)
	if status != http.StatusOK {
		t.Fatalf("with claim = %d (%s)", status, body)
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "groupPermission" gp JOIN "group" g ON g.id = gp.group_id
		WHERE g.name = 'admins' AND gp.permission = 'admin'`).Scan(&count); err != nil {
		t.Fatalf("read permission: %v", err)
	}
	if count != 1 {
		t.Fatalf("admin permission rows = %d, want 1", count)
	}
	var step string
	if err := d.QueryRow(`SELECT step FROM "onboarding" WHERE id = 'onboarding-1'`).Scan(&step); err != nil {
		t.Fatalf("read step: %v", err)
	}
	if step != "setup" {
		t.Fatalf("onboarding step = %q, want setup", step)
	}

	// A second creation is rejected by the requiresStep("group") gate
	// before the handler (the wizard already moved to "setup") — the
	// CONFLICT "already created" branch only fires in a gate/handler race.
	status, body = call(t, srv, "", "/api/trpc/group.createInitialExternalGroup", `{"name":"admins"}`, testClaimToken)
	if status != http.StatusForbidden || !strings.Contains(body, "Step denied") {
		t.Fatalf("second create = %d %s", status, body)
	}
}

// sha256Sum mirrors admin.hashToken for the seeded claim.
func sha256Sum(t *testing.T, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
