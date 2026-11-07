package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
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

// --- claim issuance (claimOnboardingAsync, onboarding-claim.ts:78-117) -------

// OnboardingClaimTTL is onboardingClaimTtlMs (onboarding-claim.ts:9): claims
// live for four hours.
const OnboardingClaimTTL = 4 * time.Hour

// Claim statuses, the ClaimOnboardingResult union (onboarding-claim.ts:16-19).
const (
	ClaimStatusIssued    = "issued"
	ClaimStatusActive    = "active"
	ClaimStatusLocked    = "locked"
	ClaimStatusFinished  = "finished"
	ClaimStatusForbidden = "forbidden"
)

// ClaimOnboardingOptions carries the request-time knobs of
// claimOnboardingAsync (onboarding-claim.ts:80): the token the caller already
// holds (from the claim cookie), the administrator override, and the test
// seams Now/TTL (upstream options.now / options.ttlMs; zero values mean
// time.Now() and the four-hour default).
type ClaimOnboardingOptions struct {
	CurrentToken string
	Force        bool
	Now          time.Time
	TTL          time.Duration
}

// ClaimOnboardingResult is ClaimOnboardingResult (onboarding-claim.ts:16-19):
// issued/active carry the token and expiry; locked carries the expiry;
// finished/forbidden carry nothing.
type ClaimOnboardingResult struct {
	Status    string
	Token     string
	ExpiresAt int64 // epoch milliseconds
}

// ClaimOnboarding is claimOnboardingAsync (onboarding-claim.ts:78-117), the
// issuance side of the first-run wizard claim consumed by the REST endpoint
// POST /api/onboarding/claim (onboard.ClaimHandler):
//
//   - the wizard being finished wins over everything (status "finished");
//   - once any user exists, minting requires the administrator override
//     (status "forbidden");
//   - the caller's own unexpired claim is "active" (idempotent re-claim);
//   - someone else's unexpired claim locks minting (status "locked");
//   - otherwise the previous row is deleted only while it still holds the
//     value that was read (optimistic lock, db.DeleteServerSettingIfValue)
//     and a fresh token (32 random bytes, base64url) is stored as
//     {"hash": sha256hex(token), "expiresAt": epochMs} for the TTL. If the
//     insert loses a race (the PK is already taken again), the winner row is
//     re-read: an unexpired winner answers "locked", any other insert error
//     propagates.
func ClaimOnboarding(ctx context.Context, database *db.DB, opts ClaimOnboardingOptions) (ClaimOnboardingResult, error) {
	// onboarding?.step === "finish" checks the RAW step (no normalization).
	row, err := database.GetOnboardingRow(ctx)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return ClaimOnboardingResult{}, fmt.Errorf("admin: read onboarding row: %w", err)
	}
	if err == nil && row.Step == OnboardingStepFinish {
		return ClaimOnboardingResult{Status: ClaimStatusFinished}, nil
	}

	hasUser, err := database.HasAnyUser(ctx)
	if err != nil {
		return ClaimOnboardingResult{}, fmt.Errorf("admin: check users for claim: %w", err)
	}
	if hasUser && !opts.Force {
		return ClaimOnboardingResult{Status: ClaimStatusForbidden}, nil
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = OnboardingClaimTTL
	}

	setting, err := database.GetServerSetting(ctx, OnboardingClaimSettingKey)
	currentClaim := (*storedClaim)(nil)
	currentRowExists := false
	switch {
	case errors.Is(err, db.ErrNotFound):
		// no current row
	case err != nil:
		return ClaimOnboardingResult{}, fmt.Errorf("admin: read onboarding claim: %w", err)
	default:
		currentRowExists = true
		currentClaim = parseStoredClaim(setting.Value)
	}

	if currentClaim != nil && opts.CurrentToken != "" && tokenMatches(*currentClaim, opts.CurrentToken, now) {
		return ClaimOnboardingResult{Status: ClaimStatusActive, Token: opts.CurrentToken, ExpiresAt: currentClaim.ExpiresAt}, nil
	}
	if currentClaim != nil && currentClaim.ExpiresAt > now.UnixMilli() && !opts.Force {
		return ClaimOnboardingResult{Status: ClaimStatusLocked, ExpiresAt: currentClaim.ExpiresAt}, nil
	}

	if currentRowExists {
		// Delete only the exact row that was read (onboarding-claim.ts:97-101).
		if err := database.DeleteServerSettingIfValue(ctx, OnboardingClaimSettingKey, setting.Value); err != nil {
			return ClaimOnboardingResult{}, fmt.Errorf("admin: rotate onboarding claim: %w", err)
		}
	}

	tokenBytes := make([]byte, 32) // randomBytes(32).toString("base64url")
	if _, err := rand.Read(tokenBytes); err != nil {
		return ClaimOnboardingResult{}, fmt.Errorf("admin: generate onboarding claim token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	expiresAt := now.Add(ttl)
	value, err := json.Marshal(storedClaim{Hash: hashToken(token), ExpiresAt: expiresAt.UnixMilli()})
	if err != nil {
		return ClaimOnboardingResult{}, fmt.Errorf("admin: encode onboarding claim: %w", err)
	}
	if err := database.InsertServerSetting(ctx, OnboardingClaimSettingKey, string(value)); err != nil {
		// The insert only fails while another minter won the PK; re-read the
		// winner (onboarding-claim.ts:108-115).
		winner, winnerErr := database.GetServerSetting(ctx, OnboardingClaimSettingKey)
		if winnerErr == nil {
			if winnerClaim := parseStoredClaim(winner.Value); winnerClaim != nil && winnerClaim.ExpiresAt > now.UnixMilli() {
				return ClaimOnboardingResult{Status: ClaimStatusLocked, ExpiresAt: winnerClaim.ExpiresAt}, nil
			}
		}
		return ClaimOnboardingResult{}, fmt.Errorf("admin: store onboarding claim: %w", err)
	}
	return ClaimOnboardingResult{Status: ClaimStatusIssued, Token: token, ExpiresAt: expiresAt.UnixMilli()}, nil
}

// OnboardingClaimTokenFromCookieHeader is
// getOnboardingClaimTokenFromCookieHeader (onboarding-claim.ts:44-60): scan
// the raw Cookie header for the claim cookie and percent-decode its value.
// A missing header, a value-less match, a malformed escape or a similar
// name ("...-copy") all yield "" (upstream: undefined).
func OnboardingClaimTokenFromCookieHeader(cookieHeader string) string {
	if cookieHeader == "" {
		return ""
	}
	for _, part := range strings.Split(cookieHeader, ";") {
		separator := strings.IndexByte(part, '=')
		if separator < 0 {
			continue
		}
		if strings.TrimSpace(part[:separator]) != OnboardingClaimCookieName {
			continue
		}
		value := strings.TrimSpace(part[separator+1:])
		if value == "" {
			return ""
		}
		// decodeURIComponent: percent-decoding without "+"-as-space.
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return ""
		}
		return decoded
	}
	return ""
}
