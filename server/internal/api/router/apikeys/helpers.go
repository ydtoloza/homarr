package apikeys

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// randomHexID is randomBytes(4).toString("hex") (apiKeys.ts:72): 8 hex
// characters.
func randomHexID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("apikeys: generate id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// randomToken is generateSecureRandomToken(n)
// (packages/common/src/security.ts:8-10): n random bytes hex-encoded, so 24
// bytes become the 48-character token (apiKeys.ts:73).
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("apikeys: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// apiKeyOAuthShape renders one apiKeys.getOwnOAuth / apiKeys.getAll row
// (apiKeys.ts:12-19): null columns stay null and the timestamp travels as
// an ISO string (see admin.ISOStringOrNull for the superjson note).
func apiKeyOAuthShape(key db.APIKeyOAuth, user *db.APIKeyUser) any {
	out := map[string]any{
		"id":       key.ID,
		"readOnly": key.ReadOnly,
	}
	if key.OAuthClientID == "" {
		out["oauthClientId"] = nil
	} else {
		out["oauthClientId"] = key.OAuthClientID
	}
	if key.OAuthClientName == "" {
		out["oauthClientName"] = nil
	} else {
		out["oauthClientName"] = key.OAuthClientName
	}
	if key.OAuthRedirectURI == "" {
		out["oauthRedirectUri"] = nil
	} else {
		out["oauthRedirectUri"] = key.OAuthRedirectURI
	}
	out["oauthIssuedAt"] = admin.ISOStringOrNull(key.OAuthIssuedAt)
	if user != nil {
		out["user"] = userShape(*user)
	}
	return out
}

func apiKeyOAuthList(keys []db.APIKeyOAuth) []any {
	out := make([]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, apiKeyOAuthShape(key, nil))
	}
	return out
}

// userShape is the user projection of apiKeys.getAll
// (apiKeys.ts:50-59): {id, name, image, email} with nulls preserved.
func userShape(user db.APIKeyUser) map[string]any {
	return map[string]any{
		"id":    user.ID,
		"name":  nullString(user.Name),
		"image": nullString(user.Image),
		"email": nullString(user.Email),
	}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
