// Package apikeys is the Go port of the apiKeys tRPC router
// (packages/api/src/router/apiKeys.ts): administration of user API keys.
// Key format and hashing follow server/internal/auth/AUTH.md §6: the client
// sends "<id>.<token>" in the ApiKey header, the id is 8 hex chars
// (randomBytes(4)), the token 48 hex chars (generateSecureRandomToken(24))
// and the token is stored bcrypt-hashed (cost 10) in apiKey.api_key. The
// plaintext is returned exactly once by apiKeys.create.
package apikeys

import (
	"context"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/auth"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Options configures the apiKeys router. The zero value is valid.
type Options struct{}

type service struct {
	db *db.DB
}

// Register registers the apiKeys router under its upstream paths. Call once
// at startup (Register panics on duplicate paths).
func Register(r *trpc.Router, database *db.DB, _ Options) {
	s := &service{db: database}

	// apiKeys.getOwnOAuth (apiKeys.ts:22-27): the caller's OAuth-issued keys.
	r.Register("apiKeys.getOwnOAuth", s.getOwnOAuth)
	// apiKeys.revokeOwnOAuth (apiKeys.ts:28-41).
	r.Register("apiKeys.revokeOwnOAuth", s.revokeOwnOAuth)
	// apiKeys.getAll (apiKeys.ts:42-61): every key with its user (admin).
	r.Register("apiKeys.getAll", s.getAll)
	// apiKeys.create (apiKeys.ts:62-84): mint one key, plaintext once (admin).
	r.Register("apiKeys.create", s.create)
	// apiKeys.delete (apiKeys.ts:85-96): delete by id (admin).
	r.Register("apiKeys.delete", s.delete)
}

// getOwnOAuth is apiKeys.getOwnOAuth: protectedProcedure listing the
// caller's keys that carry an oauth_client_id.
func (s *service) getOwnOAuth(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	keys, err := s.db.ListOAuthAPIKeysForUser(ctx, c.Session.UserID)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	return apiKeyOAuthList(keys), nil
}

// revokeOwnOAuth is apiKeys.revokeOwnOAuth: deletes one of the caller's own
// OAuth keys. Upstream deletes without existence feedback (a foreign or
// non-OAuth id is a silent no-op).
func (s *service) revokeOwnOAuth(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	apiKeyID, terr := d.Str("apiKeyId")
	if terr != nil {
		return nil, terr
	}
	if _, err := s.db.DeleteOAuthAPIKeyOfUser(ctx, apiKeyID, c.Session.UserID); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// getAll is apiKeys.getAll: permissionRequiredProcedure.requiresPermission("admin").
func (s *service) getAll(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	keys, err := s.db.ListAPIKeysWithUser(ctx)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, apiKeyOAuthShape(k.APIKeyOAuth, &k.User))
	}
	return out, nil
}

// create is apiKeys.create: generates id + token, stores the bcrypt hash of
// the token and returns the plaintext "id.token" exactly once
// (apiKeys.ts:71-84). The input is z.object({readOnly: boolean.optional()})
// .optional() — null/absent input means readOnly=false.
func (s *service) create(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	readOnly := false
	if len(input) > 0 && string(input) != "null" {
		d, terr := admin.NewInputDecoder(input)
		if terr != nil {
			return nil, terr
		}
		var terr2 *trpc.Error
		readOnly, _, terr2 = d.OptBool("readOnly")
		if terr2 != nil {
			return nil, terr2
		}
	}

	id, err := randomHexID()
	if err != nil {
		return nil, admin.InternalError(err)
	}
	token, err := randomToken(24)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	hash, err := auth.HashPassword(token)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if err := s.db.CreateAPIKey(ctx, db.APIKey{ID: id, Hash: hash, ReadOnly: readOnly, UserID: c.Session.UserID}); err != nil {
		return nil, admin.InternalError(err)
	}
	return map[string]any{"apiKey": id + "." + token}, nil
}

// delete is apiKeys.delete: permissionRequiredProcedure "admin"; deleting
// an unknown id is a silent no-op like the upstream unconditional delete.
func (s *service) delete(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	apiKeyID, terr := d.Str("apiKeyId")
	if terr != nil {
		return nil, terr
	}
	if _, err := s.db.DeleteAPIKeyByID(ctx, apiKeyID); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}
