package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/ydtoloza/homarr/server/internal/db"
)

// Onboarding claim verification, ported from
// packages/api/src/onboarding-claim.ts. The claim lets an unauthenticated
// visitor run the first-run wizard: the minting endpoint (POST
// /api/onboarding/claim, apps/nextjs/src/app/api/onboarding/claim/route.ts —
// a REST route, not tRPC) stores a {"hash": sha256hex(token), "expiresAt":
// epochMs} record under the serverSettings key below and hands the token
// back as the "homarr-onboarding-claim" cookie. The tRPC procedures only
// need to VERIFY the token.

// OnboardingClaimSettingKey is the serverSettings row the claim lives in
// (onboarding-claim.ts:8).
const OnboardingClaimSettingKey = "__onboarding_claim"

// OnboardingClaimCookieName is the cookie the frontend carries
// (onboarding-claim.ts:7). The Go httpserver middleware resolves it into
// the request context through the Options.OnboardingClaimTokenFromContext
// hook of the routers with onboarding procedures.
const OnboardingClaimCookieName = "homarr-onboarding-claim"

// storedClaim is the JSON shape minted by claimOnboardingAsync
// (onboarding-claim.ts:103-105).
type storedClaim struct {
	Hash      string `json:"hash"`
	ExpiresAt int64  `json:"expiresAt"`
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// parseStoredClaim is parseStoredClaim (onboarding-claim.ts:23-32): a
// malformed record behaves like "no claim".
func parseStoredClaim(value string) *storedClaim {
	var parsed storedClaim
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return nil
	}
	if !hashPattern.MatchString(parsed.Hash) {
		return nil
	}
	return &parsed
}

// hashToken is hashToken (onboarding-claim.ts:21): sha256 hex of the token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// tokenMatches is tokenMatches (onboarding-claim.ts:34-39): the claim must
// be unexpired and the sha256 comparison constant-time.
func tokenMatches(claim storedClaim, token string, now time.Time) bool {
	if claim.ExpiresAt <= now.UnixMilli() || token == "" {
		return false
	}
	expected, err1 := hex.DecodeString(claim.Hash)
	actual := sha256.Sum256([]byte(token))
	if err1 != nil || len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare(expected, actual[:]) == 1
}

// IsOnboardingClaimValid is isOnboardingClaimValidAsync
// (onboarding-claim.ts:62-67). A missing/malformed claim row yields false.
func IsOnboardingClaimValid(ctx context.Context, database *db.DB, token string, now time.Time) (bool, error) {
	if token == "" {
		return false, nil
	}
	setting, err := database.GetServerSetting(ctx, OnboardingClaimSettingKey)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("admin: read onboarding claim: %w", err)
	}
	claim := parseStoredClaim(setting.Value)
	if claim == nil {
		return false, nil
	}
	return tokenMatches(*claim, token, now), nil
}

// OnboardingAccessAllowed is enforceOnboardingAccess
// (packages/api/src/trpc.ts:148-157): administrators pass; everyone else
// only while no user exists yet and the caller carries a valid claim token
// (isClaimOnlyOnboardingAccessAllowedAsync, onboarding-claim.ts:69-76).
func OnboardingAccessAllowed(ctx context.Context, database *db.DB, c Caller, claimToken string) (bool, error) {
	if c.IsAdmin() {
		return true, nil
	}
	hasUser, err := database.HasAnyUser(ctx)
	if err != nil {
		return false, err
	}
	if hasUser {
		return false, nil
	}
	return IsOnboardingClaimValid(ctx, database, claimToken, time.Now())
}
