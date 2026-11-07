package auth

import "golang.org/x/crypto/bcrypt"

// Upstream password hashing is bcrypt with cost 10, done by the npm `bcrypt`
// package (packages/auth/security.ts:1-15):
//
//	export const hashPasswordAsync = async (password: string) => {
//	  return bcrypt.hash(password, 10);
//	};
//
// The npm bcrypt package emits the standard modular crypt format
// ($2a$10$... or $2b$10$...) and x/crypto/bcrypt both generates ($2a$) and
// verifies ($2a$/$2b$/$2y$) that format, so hashes stored by an existing
// Homarr instance (including the $2b$ demo seed in
// packages/db/migrations/seed.ts:955) verify directly and vice versa.
// CompareHashAndPassword is constant time, like bcrypt.compare.

// bcryptCost is the upstream cost parameter (packages/auth/security.ts:4).
const bcryptCost = 10

// HashPassword hashes a password in the exact format upstream stores
// ($2a$10$..., 60 chars). Used for credential sign-up / change-password /
// API key creation (packages/api/src/router/apiKeys.ts:74 hashes the API key
// token with the same function).
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// ComparePassword reports whether the plaintext password matches the stored
// bcrypt hash (upstream comparePasswordsAsync, packages/auth/security.ts:13-15).
// An empty or malformed hash never matches.
func ComparePassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
