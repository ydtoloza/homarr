package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUserRoundtrip(t *testing.T) {
	d := OpenTest(t)
	ctx := context.Background()

	u := User{
		ID:          "user-1",
		Name:        "admin",
		Email:       "admin@example.com",
		Password:    "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$hash",
		Provider:    "credentials",
		ColorScheme: "light",
	}
	if err := d.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	got, err := d.GetUserByID(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.ID != u.ID || got.Name != u.Name || got.Email != u.Email ||
		got.Password != u.Password || got.Provider != u.Provider || got.ColorScheme != u.ColorScheme {
		t.Errorf("GetUserByID mismatch: %+v", got)
	}

	// Defaults: no email/image/email_verified columns round-trip as empty.
	if got.Image != "" || !got.EmailVerified.IsZero() {
		t.Errorf("unexpected nullable values: image=%q emailVerified=%v", got.Image, got.EmailVerified)
	}

	// Login lookup: lowercased name + provider, exactly like upstream.
	byName, err := d.GetUserByName(ctx, "admin", "credentials")
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if byName.ID != "user-1" {
		t.Errorf("GetUserByName returned %q", byName.ID)
	}

	// Wrong provider or unknown name must be ErrNotFound.
	if _, err := d.GetUserByName(ctx, "admin", "oidc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("lookup with other provider returned %v, want ErrNotFound", err)
	}
	if _, err := d.GetUserByName(ctx, "ghost", "credentials"); !errors.Is(err, ErrNotFound) {
		t.Errorf("lookup unknown user returned %v, want ErrNotFound", err)
	}
	if _, err := d.GetUserByID(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserByID unknown returned %v, want ErrNotFound", err)
	}
}

func TestSessionRoundtrip(t *testing.T) {
	d := OpenTest(t)
	ctx := context.Background()

	if err := d.CreateUser(ctx, User{ID: "user-1", Name: "admin", Provider: "credentials", Password: "h"}); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(24 * time.Hour).Truncate(time.Millisecond)
	s := Session{Token: "tok-abc", UserID: "user-1", Expires: expires}
	if err := d.CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := d.GetSession(ctx, "tok-abc")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Session.Token != s.Token || got.Session.UserID != s.UserID {
		t.Errorf("session mismatch: %+v", got.Session)
	}
	if !got.Session.Expires.Equal(expires) {
		t.Errorf("expires = %v, want %v (millisecond precision)", got.Session.Expires, expires)
	}
	if got.User.ID != "user-1" || got.User.Name != "admin" || got.User.Password != "h" {
		t.Errorf("joined user mismatch: %+v", got.User)
	}

	if _, err := d.GetSession(ctx, "tok-missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession unknown returned %v, want ErrNotFound", err)
	}

	if err := d.DeleteSession(ctx, "tok-abc"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := d.GetSession(ctx, "tok-abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession after delete returned %v, want ErrNotFound", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	d := OpenTest(t)
	ctx := context.Background()
	if err := d.CreateUser(ctx, User{ID: "user-1", Name: "admin"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.CreateSession(ctx, Session{Token: "tok-live", UserID: "user-1", Expires: now.Add(time.Hour)}))
	must(d.CreateSession(ctx, Session{Token: "tok-dead", UserID: "user-1", Expires: now.Add(-time.Hour)}))

	n, err := d.DeleteExpiredSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted %d expired sessions, want 1", n)
	}
	if _, err := d.GetSession(ctx, "tok-live"); err != nil {
		t.Errorf("live session should survive: %v", err)
	}
	if _, err := d.GetSession(ctx, "tok-dead"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session should be gone, got %v", err)
	}
}

func TestBoardListVisibility(t *testing.T) {
	d := OpenTest(t)
	ctx := context.Background()

	users := []User{
		{ID: "u-alice", Name: "alice"},
		{ID: "u-bob", Name: "bob"},
	}
	for _, u := range users {
		if err := d.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	boards := []Board{
		{ID: "b-public", Name: "aa-public", IsPublic: true},
		{ID: "b-alice", Name: "bb-alice-private", CreatorID: "u-alice"},
		{ID: "b-hidden", Name: "cc-hidden"},
	}
	for _, b := range boards {
		if err := d.CreateBoard(ctx, b); err != nil {
			t.Fatalf("CreateBoard %s: %v", b.ID, err)
		}
	}

	// alice sees the public board and her own, not the hidden one.
	aliceBoards, err := d.ListBoardsForUser(ctx, "u-alice")
	if err != nil {
		t.Fatal(err)
	}
	wantAlice := []string{"aa-public", "bb-alice-private"}
	if len(aliceBoards) != len(wantAlice) {
		t.Fatalf("alice sees %v, want %v", boardNames(aliceBoards), wantAlice)
	}
	for i, b := range aliceBoards {
		if b.Name != wantAlice[i] {
			t.Errorf("alice board %d = %s, want %s", i, b.Name, wantAlice[i])
		}
	}

	// Direct user permission on the hidden board.
	if _, err := d.Exec(`INSERT INTO "boardUserPermission" (board_id, user_id, permission) VALUES ('b-hidden', 'u-bob', 'board-view')`); err != nil {
		t.Fatal(err)
	}
	// Group permission: bob is member of a group with a permission on b-hidden.
	if _, err := d.Exec(`INSERT INTO "group" (id, name, position) VALUES ('g-1', 'group-one', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO "groupMember" (group_id, user_id) VALUES ('g-1', 'u-bob')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO "boardGroupPermission" (board_id, group_id, permission) VALUES ('b-hidden', 'g-1', 'board-view')`); err != nil {
		t.Fatal(err)
	}

	bobBoards, err := d.ListBoardsForUser(ctx, "u-bob")
	if err != nil {
		t.Fatal(err)
	}
	// The group and direct permission both point at b-hidden; the board must
	// appear exactly once.
	var seen int
	for _, b := range bobBoards {
		if b.ID == "b-hidden" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("b-hidden appears %d times for bob, want 1 (list: %v)", seen, boardNames(bobBoards))
	}
	if len(bobBoards) != 2 {
		t.Errorf("bob sees %d boards (%v), want 2", len(bobBoards), boardNames(bobBoards))
	}

	// GetBoardByID roundtrip with NULL creator/page title.
	b, err := d.GetBoardByID(ctx, "b-hidden")
	if err != nil {
		t.Fatal(err)
	}
	if b.CreatorID != "" || b.PageTitle != "" || b.IsPublic {
		t.Errorf("b-hidden nullable fields mismatch: %+v", b)
	}
}

func TestBoardColumnsKeepUpstreamSnakeCaseNames(t *testing.T) {
	d := OpenTest(t)
	// Prove the physical column names match the upstream drizzle casing
	// (DB_CASING = snake_case): select the exact column names.
	rows, err := d.Query(`SELECT id, name, is_public, creator_id, page_title, background_image_attachment, primary_color, item_radius FROM "board" LIMIT 1`)
	if err != nil {
		t.Fatalf("snake_case column names on board: %v", err)
	}
	rows.Close()

	rows, err = d.Query(`SELECT id, name, email, email_verified, image, password, provider, home_board_id, default_search_engine_id, color_scheme, header_preferences FROM "user" LIMIT 1`)
	if err != nil {
		t.Fatalf("snake_case column names on user: %v", err)
	}
	rows.Close()

	rows, err = d.Query(`SELECT session_token, user_id, expires FROM "session" LIMIT 1`)
	if err != nil {
		t.Fatalf("snake_case column names on session: %v", err)
	}
	rows.Close()
}

func boardNames(boards []Board) []string {
	names := make([]string, len(boards))
	for i, b := range boards {
		names[i] = b.Name
	}
	return names
}
