package widgets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Port of the upstream secret encryption (@homarr/encryption,
// packages/common/src/encryption.ts): integrationSecret.value columns store
// "<data hex>.<iv hex>" where data is AES-256-CBC (PKCS#7 padding, the Go
// stdlib cipher mode) of the plaintext with the key derived from the
// SECRET_ENCRYPTION_KEY environment variable.
//
// Upstream details pinned from encryption.ts:
//
//	const algorithm = "aes-256-cbc";                       // :4
//	const key = Buffer.from(env.SECRET_ENCRYPTION_KEY      // :9
//	  || "0".repeat(64), "hex");                           // 64 hex chars = 32 bytes
//	encrypt: iv = randomBytes(16); return `${encrypted.toString("hex")}.${iv.toString("hex")}`  // :11-18
//	decrypt: split "." -> data, iv; createDecipheriv(...); hex-decode both  // :23-35
//
// The env validation (packages/common/env.ts:16-28) requires exactly 64 hex
// characters upstream; the fallback of 32 zero bytes only applies when env
// validation is skipped (CI). homarr-go replicates both behaviors.

// secretEnvKey is the upstream environment variable name
// (packages/common/env.ts runtimeEnv SECRET_ENCRYPTION_KEY).
const secretEnvKey = "SECRET_ENCRYPTION_KEY"

var (
	secretKeyOnce sync.Once
	secretKey     []byte
)

// SecretKey returns the AES-256 key: SECRET_ENCRYPTION_KEY decoded from hex,
// or 32 zero bytes when the variable is unset/invalid (the upstream
// encryption.ts:9 fallback).
func SecretKey() []byte {
	secretKeyOnce.Do(func() {
		raw := os.Getenv(secretEnvKey)
		if key, err := hex.DecodeString(raw); err == nil && len(key) == 32 {
			secretKey = key
			return
		}
		secretKey = make([]byte, 32)
	})
	return secretKey
}

// DecryptSecret decrypts one integrationSecret.value with the process key
// (decryptSecret, encryption.ts:23-35).
func DecryptSecret(value string) (string, error) {
	return DecryptSecretWithKey(value, SecretKey())
}

// DecryptSecretWithKey decrypts with an explicit key
// (decryptSecretWithKey, encryption.ts:31-35) — the seam for tests and for a
// future key-rotation path.
func DecryptSecretWithKey(value string, key []byte) (string, error) {
	dataHex, ivHex, ok := strings.Cut(value, ".")
	if !ok {
		return "", fmt.Errorf("secrets: value is not in the \"<data>.<iv>\" format")
	}
	data, err := hex.DecodeString(dataHex)
	if err != nil {
		return "", fmt.Errorf("secrets: invalid data hex: %w", err)
	}
	iv, err := hex.DecodeString(ivHex)
	if err != nil {
		return "", fmt.Errorf("secrets: invalid iv hex: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return "", fmt.Errorf("secrets: ciphertext is not a multiple of the block size")
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, data)
	return string(unpadPKCS7(plain)), nil
}

// unpadPKCS7 removes the PKCS#7 padding Node's createDecipheriv strips
// automatically.
func unpadPKCS7(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return data
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return data // invalid padding; keep the raw bytes
		}
	}
	return data[:len(data)-pad]
}

// DefaultSecretDecryptor is the Options.SecretDecryptor default.
func DefaultSecretDecryptor(value string) (string, error) {
	return DecryptSecret(value)
}

// EncryptSecret encrypts a plaintext into the "<data hex>.<iv hex>" storage
// format (encryptSecret, encryption.ts:11-18) — used by tests to build
// fixture rows; the write path itself arrives with the M4 integration
// management router.
func EncryptSecret(plaintext string, key []byte) (string, error) {
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := padPKCS7([]byte(plaintext))
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return hex.EncodeToString(out) + "." + hex.EncodeToString(iv), nil
}

func padPKCS7(data []byte) []byte {
	pad := aes.BlockSize - len(data)%aes.BlockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}
