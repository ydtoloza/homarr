package widgets

import (
	"strings"
	"testing"
)

// TestSecretRoundtrip pins the upstream format: "<data hex>.<iv hex>" with a
// 16-byte IV and AES-256-CBC (packages/common/src/encryption.ts:11-18).
func TestSecretRoundtrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	plain := "Uv38ByGCZUu3v5pMEnB6zQJd" // a typical *arr api key
	encrypted, err := EncryptSecret(plain, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	parts := strings.Split(encrypted, ".")
	if len(parts) != 2 {
		t.Fatalf("format = %q, want <data hex>.<iv hex>", encrypted)
	}
	if len(parts[0]) == 0 || len(parts[0])%32 != 0 { // 16-byte blocks in hex
		t.Fatalf("data part = %q", parts[0])
	}
	if got, err := DecryptSecretWithKey(encrypted, key); err != nil || got != plain {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
	// Two encryptions of the same plaintext differ (random IV).
	encrypted2, err := EncryptSecret(plain, key)
	if err != nil {
		t.Fatalf("encrypt 2: %v", err)
	}
	if encrypted2 == encrypted {
		t.Fatal("the IV must be random per encryption")
	}
}

// TestDecryptUpstreamVector pins cross-compatibility with a ciphertext
// produced by the upstream Node implementation for the all-zero key (the
// SECRET_ENCRYPTION_KEY fallback): "0".repeat(64) as hex -> 32 zero bytes.
func TestDecryptUpstreamVector(t *testing.T) {
	// node -e "const crypto=require('crypto');
	//   const key=Buffer.from('0'.repeat(64),'hex');
	//   const iv=Buffer.from('e2c2e6b7d0e85ecb3ec06a1a94a4c9c6','hex');
	//   const c=crypto.createCipheriv('aes-256-cbc',key,iv);
	//   const out=Buffer.concat([c.update('sonarr-api-key'),c.final()]);
	//   console.log(out.toString('hex')+'.'+iv.toString('hex'))"
	const vector = "84f791000d954510e55cf2d673bf8d13.e2c2e6b7d0e85ecb3ec06a1a94a4c9c6"
	got, err := DecryptSecretWithKey(vector, make([]byte, 32))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "sonarr-api-key" {
		t.Fatalf("decrypted = %q", got)
	}
}

func TestDecryptMalformed(t *testing.T) {
	for _, value := range []string{"", "no-dot", "zz.zz", "aa.zz", "."} {
		if _, err := DecryptSecretWithKey(value, make([]byte, 32)); err == nil {
			t.Fatalf("value %q must not decrypt", value)
		}
	}
}

// TestDecryptDefaultsToZeroKey pins the upstream fallback of encryption.ts:9
// ("0".repeat(64)) when SECRET_ENCRYPTION_KEY is unset in the test process.
func TestDecryptDefaultsToZeroKey(t *testing.T) {
	encrypted, err := EncryptSecret("hello", make([]byte, 32))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := DecryptSecret(encrypted)
	if err != nil || got != "hello" {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
}
