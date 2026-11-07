// Package invite is the Go port of the invite tRPC router
// (packages/api/src/router/invite.ts): single-use registration invites
// created by administrators and consumed by user.register.
//
// Every procedure calls throwIfCredentialsDisabled
// (packages/api/src/router/invite/checks.ts) first; the fork ships the
// credentials provider only (server/internal/auth), so the check is
// statically satisfied — documented in ADMIN.md.
package invite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Options configures the invite router. The zero value is valid.
type Options struct{}

type service struct {
	db *db.DB
}

// Register registers the invite router under its upstream paths. Call once
// at startup (Register panics on duplicate paths).
func Register(r *trpc.Router, database *db.DB, _ Options) {
	s := &service{db: database}
	// invite.getAll (invite.ts:14-57) (query).
	r.Register("invite.getAll", s.getAll)
	// invite.createInvite (invite.ts:58-99) (mutation).
	r.Register("invite.createInvite", admin.Writable(s.createInvite))
	// invite.deleteInvite (invite.ts:100-138) (mutation).
	r.Register("invite.deleteInvite", admin.Writable(s.deleteInvite))
}

// getAll is invite.getAll: permissionRequiredProcedure "admin", invites
// ordered by expiration date with the creator name and WITHOUT the token
// (columns.token: false).
func (s *service) getAll(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	invites, err := s.db.ListInvitesWithCreator(ctx)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(invites))
	for _, inv := range invites {
		creator := map[string]any{"id": inv.CreatorID}
		if inv.CreatorName == "" {
			creator["name"] = nil
		} else {
			creator["name"] = inv.CreatorName
		}
		out = append(out, map[string]any{
			"id": inv.ID,
			// expirationDate is a drizzle timestamp column (JS Date
			// upstream); homarr-go emits the ISO string (trpc/superjson.go).
			"expirationDate": admin.ISOString(inv.ExpirationDate),
			"creator":        creator,
		})
	}
	return out, nil
}

// createInvite is invite.createInvite: mints id (cuid2-like) + token
// (randomBytes(20) hex) and stores the parsed expiration date
// (invite.ts:83-99). The output {id, token} is the only chance to see the
// token.
func (s *service) createInvite(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	rawDate, terr := d.Str("expirationDate")
	if terr != nil {
		return nil, terr
	}
	expiration, terr := parseISODate(rawDate)
	if terr != nil {
		return nil, terr
	}

	id := admin.CreateID()
	token, err := randomToken()
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if err := s.db.CreateInvite(ctx, db.Invite{
		ID:             id,
		Token:          token,
		ExpirationDate: expiration,
		CreatorID:      c.Session.UserID,
	}); err != nil {
		return nil, admin.InternalError(err)
	}
	return map[string]any{"id": id, "token": token}, nil
}

// deleteInvite is invite.deleteInvite: NOT_FOUND "Invite not found" when
// the id is unknown, else delete (invite.ts:124-138).
func (s *service) deleteInvite(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := d.Str("id")
	if terr != nil {
		return nil, terr
	}
	if _, err := s.db.GetInviteByID(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "Invite not found")
		}
		return nil, admin.InternalError(err)
	}
	if _, err := s.db.DeleteInvite(ctx, id); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// parseISODate validates the z.iso.datetime({offset: true}) input of
// createInvite: an ISO 8601 timestamp that carries a UTC offset (Z or
// ±hh:mm). RFC3339 accepts both, with or without fractional seconds
// (precision: -1).
func parseISODate(value string) (time.Time, *trpc.Error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, admin.BadRequestf(`input key "expirationDate" has an invalid value`)
	}
	return t, nil
}

// randomToken is randomBytes(20).toString("hex") (invite.ts:86): a 40
// character hex token stored in plain text (matching the register lookup by
// equality).
func randomToken() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("invite: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
