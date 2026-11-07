package auth

import (
	"strings"
	"testing"
)

// upstreamSeedHash is the bcrypt hash of the "demo" user as shipped in the
// upstream migration seed (packages/db/migrations/seed.ts:955). It was
// produced by the npm bcrypt package, so verifying it proves cross-
// implementation compatibility of the stored-hash format. The demo password
// is "demo" (the credentials of https://demo.homarr.dev).
const upstreamSeedHash = "$2b$10$odRXt5e95kSQV5Axmk/FeO6GVOxuRQQ8NnRcBA78Wg4V3kZxPY68u"

func TestComparePasswordUpstreamFixture(t *testing.T) {
	if !ComparePassword("demo", upstreamSeedHash) {
		t.Error("ComparePassword(demo, upstream seed hash) = false, want true")
	}
	for _, wrong := range []string{"Demo", "demodemo", "", "password"} {
		if ComparePassword(wrong, upstreamSeedHash) {
			t.Errorf("ComparePassword(%q, upstream seed hash) = true, want false", wrong)
		}
	}
}

func TestHashPasswordFormat(t *testing.T) {
	h1, err := HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// Standard modular crypt format: $2a$10$ + 22 char base64 salt +
	// 31 char base64 digest = 60 chars, exactly what npm bcrypt stores.
	if !strings.HasPrefix(h1, "$2a$10$") {
		t.Errorf("hash prefix = %q, want $2a$10$", h1[:7])
	}
	if len(h1) != 60 {
		t.Errorf("hash length = %d, want 60", len(h1))
	}
	h2, _ := HashPassword("s3cret")
	if h1 == h2 {
		t.Error("two hashes of the same password are equal; salt should differ")
	}
	if !ComparePassword("s3cret", h1) {
		t.Error("roundtrip ComparePassword(s3cret, hash) = false, want true")
	}
	for _, wrong := range []string{"", "S3cret", "s3cret ", "s3cretx"} {
		if ComparePassword(wrong, h1) {
			t.Errorf("ComparePassword(%q) = true, want false", wrong)
		}
	}
}

func TestComparePasswordMalformedHash(t *testing.T) {
	for _, hash := range []string{"", "not-a-hash", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$hash"} {
		if ComparePassword("demo", hash) {
			t.Errorf("ComparePassword with malformed hash %q = true, want false", hash)
		}
	}
}
