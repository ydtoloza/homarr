package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// CSRF protection port of @auth/core
// packages/core/src/lib/actions/callback/oauth/csrf-token.ts: a random token
// travels in the JSON response body and in a cookie whose value is
// "token|sha256hex(token + secret)". The hash proves the cookie was set by
// this server (the secret acts as salt); the body/cookie equality is the
// OWASP double-submit check. POST actions (signout and the credentials
// callback) require a verified pair, GET /csrf always mints a fresh one.

// newCSRFToken returns (token, cookieValue) where cookieValue is
// "token|hash". The token is 32 hex characters, the same shape as
// upstream's randomString(32) (16 random bytes hex-encoded).
func (a *Auth) newCSRFToken() (string, string, error) {
	token, err := randomHex(16) // 32 hex chars
	if err != nil {
		return "", "", err
	}
	return token, a.csrfCookieValue(token), nil
}

// csrfCookieValue builds "token|sha256hex(token+secret)".
func (a *Auth) csrfCookieValue(token string) string {
	return token + "|" + hex.EncodeToString(csrfHash(token, a.opts.Secret))
}

// verifyCSRF validates a csrf cookie value plus the csrfToken sent in the
// request body, mirroring upstream's createCSRFToken verification branch:
// the cookie must carry the token|hash shape with a matching hash, and the
// body token must equal the cookie token. Only POST requests reach this
// check (upstream: csrfTokenVerified = isPost && csrfToken === bodyValue).
func (a *Auth) verifyCSRF(cookieValue, bodyValue string) bool {
	token, hash, ok := strings.Cut(cookieValue, "|")
	if !ok || token == "" {
		return false
	}
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return false
	}
	if !hmac.Equal(hashBytes, csrfHash(token, a.opts.Secret)) {
		return false
	}
	return bodyValue != "" && token == bodyValue
}

// csrfHash mirrors upstream createHash(`${csrfToken}${options.secret}`):
// SHA-256 over token+secret (the secret acts as salt).
func csrfHash(token, secret string) []byte {
	h := sha256.Sum256([]byte(token + secret))
	return h[:]
}
