package db

import (
	"context"
	"errors"
	"testing"
)

func TestAPIKeyRoundtrip(t *testing.T) {
	d := OpenTest(t)
	ctx := context.Background()

	if err := d.CreateUser(ctx, User{ID: "user-1", Name: "admin", Provider: "credentials"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	key := APIKey{ID: "abcd1234", Hash: "$2b$10$fake", ReadOnly: true, UserID: "user-1"}
	if err := d.CreateAPIKey(ctx, key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, err := d.GetAPIKeyByID(ctx, "abcd1234")
	if err != nil {
		t.Fatalf("GetAPIKeyByID: %v", err)
	}
	if got.ID != key.ID || got.Hash != key.Hash || !got.ReadOnly || got.UserID != key.UserID {
		t.Errorf("GetAPIKeyByID mismatch: %+v", got)
	}

	if _, err := d.GetAPIKeyByID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetAPIKeyByID(missing) = %v, want ErrNotFound", err)
	}
}

func TestUserPermissionsAndGroups(t *testing.T) {
	d := OpenTest(t)
	ctx := context.Background()

	for _, u := range []User{{ID: "u1", Name: "a"}, {ID: "u2", Name: "b"}} {
		if err := d.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser(%s): %v", u.ID, err)
		}
	}
	for _, g := range []string{"everyone", "admins"} {
		if _, err := d.ExecContext(ctx, `INSERT INTO "group" (id, name, owner_id, position) VALUES (?, ?, NULL, 0)`, "g-"+g, g); err != nil {
			t.Fatalf("insert group %s: %v", g, err)
		}
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO "groupPermission" (group_id, permission) VALUES ('g-admins', 'admin')`); err != nil {
		t.Fatalf("insert groupPermission: %v", err)
	}

	if added, err := d.AddUserToGroup(ctx, "u1", "g-admins"); err != nil || !added {
		t.Errorf("AddUserToGroup first time = (%v, %v), want (true, nil)", added, err)
	}
	if added, err := d.AddUserToGroup(ctx, "u1", "g-admins"); err != nil || added {
		t.Errorf("AddUserToGroup second time = (%v, %v), want (false, nil)", added, err)
	}

	has, err := d.UserHasPermission(ctx, "u1", "admin")
	if err != nil || !has {
		t.Errorf("UserHasPermission(u1, admin) = (%v, %v), want (true, nil)", has, err)
	}
	if has, err := d.UserHasPermission(ctx, "u2", "admin"); err != nil || has {
		t.Errorf("UserHasPermission(u2, admin) = (%v, %v), want (false, nil)", has, err)
	}

	perms, err := d.UserPermissions(ctx, "u1")
	if err != nil || len(perms) != 1 || perms[0] != "admin" {
		t.Errorf("UserPermissions(u1) = (%v, %v), want ([admin], nil)", perms, err)
	}

	id, err := d.GroupIDByName(ctx, "everyone")
	if err != nil || id != "g-everyone" {
		t.Errorf("GroupIDByName(everyone) = (%q, %v), want (g-everyone, nil)", id, err)
	}
	if _, err := d.GroupIDByName(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GroupIDByName(nope) = %v, want ErrNotFound", err)
	}
}

func TestUpdateUserPassword(t *testing.T) {
	d := OpenTest(t)
	ctx := context.Background()

	if err := d.CreateUser(ctx, User{ID: "u1", Name: "a", Password: "old-hash"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := d.UpdateUserPassword(ctx, "u1", "new-hash"); err != nil {
		t.Fatalf("UpdateUserPassword: %v", err)
	}
	got, err := d.GetUserByID(ctx, "u1")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.Password != "new-hash" {
		t.Errorf("Password = %q, want new-hash", got.Password)
	}
}
