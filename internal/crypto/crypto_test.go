package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func mustCipher(t *testing.T, secret string) *Cipher {
	t.Helper()
	c, err := New(secret)
	if err != nil {
		t.Fatalf("New(%q): %v", secret, err)
	}
	return c
}

func mustReplayCipher(t *testing.T, secret string) *Cipher {
	t.Helper()
	c := mustCipher(t, secret)
	if err := c.EnableGrokBuildChatReplay(secret); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	cases := []string{
		"",
		"sk-abc123",
		"Bearer token with spaces and a newline\n",
		`{"refresh_token":"rt","access_token":"at","expires_in":3600}`,
		strings.Repeat("x", 4096), // exercises multi-block plaintext
	}
	c := mustCipher(t, "test-secret")
	for _, pt := range cases {
		enc, err := c.Encrypt(pt)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", pt, err)
		}
		got, err := c.Decrypt(enc)
		if err != nil {
			t.Fatalf("Decrypt(%q): %v", pt, err)
		}
		if got != pt {
			t.Errorf("roundtrip mismatch: got %q want %q", got, pt)
		}
	}
}

// Nonce reuse is a catastrophic GCM failure. Each Encrypt call draws fresh
// randomness, so the same plaintext must yield distinct ciphertexts.
func TestEncryptNonceUniqueness(t *testing.T) {
	c := mustCipher(t, "test-secret")
	const pt = "same-plaintext"
	seen := make(map[string]bool, 64)
	var firstLen int
	for i := 0; i < 64; i++ {
		enc, err := c.Encrypt(pt)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if seen[enc] {
			t.Fatalf("duplicate ciphertext on iteration %d - nonce not random", i)
		}
		seen[enc] = true
		if i == 0 {
			firstLen = len(enc)
		} else if len(enc) != firstLen {
			t.Fatalf("ciphertext length varies: %d vs %d", firstLen, len(enc))
		}
	}
}

func TestEncryptDoesNotLeakPlaintext(t *testing.T) {
	c := mustCipher(t, "test-secret")
	const pt = "super-secret-api-key-12345"
	enc, err := c.Encrypt(pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(enc, pt) {
		t.Fatalf("ciphertext contains plaintext: %s", enc)
	}
}

func TestGrokBuildChatMACIsDomainSeparated(t *testing.T) {
	const secret = "instance-secret"
	payload := []byte("replay-payload")
	c := mustReplayCipher(t, secret)
	got, ok := c.GrokBuildChatMAC(payload)
	if !ok || len(got) != sha256.Size {
		t.Fatalf("mac = %d ok=%v", len(got), ok)
	}
	if !c.VerifyGrokBuildChatMAC(payload, got) {
		t.Fatal("verify rejected a fresh mac")
	}
	flipped := append([]byte(nil), got...)
	flipped[0] ^= 0x01
	if c.VerifyGrokBuildChatMAC(payload, flipped) || c.VerifyGrokBuildChatMAC([]byte("other"), got) {
		t.Fatal("verify accepted a tampered mac")
	}
	if !mustReplayCipher(t, secret).VerifyGrokBuildChatMAC(payload, got) {
		t.Fatal("mac changed across New with the same secret")
	}
	if mustCipher(t, secret).VerifyGrokBuildChatMAC(payload, got) {
		t.Fatal("New alone signed replay")
	}
	if mustReplayCipher(t, secret+"-other").VerifyGrokBuildChatMAC(payload, got) {
		t.Fatal("mac verified under a different secret")
	}
	dev := mustCipher(t, "airouter-insecure-dev-secret")
	if err := dev.EnableGrokBuildChatReplay("airouter-insecure-dev-secret"); err == nil {
		t.Fatal("built-in development secret was accepted for replay")
	}
	if dev.VerifyGrokBuildChatMAC(payload, got) {
		t.Fatal("development secret verified replay")
	}
	if err := mustCipher(t, secret).EnableGrokBuildChatReplay(""); err == nil {
		t.Fatal("empty replay secret was accepted")
	}
	aesKey := sha256.Sum256([]byte(secret))
	aesMAC := hmac.New(sha256.New, aesKey[:])
	_, _ = aesMAC.Write(payload)
	if hmac.Equal(aesMAC.Sum(nil), got) {
		t.Fatal("bridge mac reused the AES key")
	}
	if (*Cipher)(nil).VerifyGrokBuildChatMAC(payload, got) {
		t.Fatal("nil cipher verified")
	}
	if err := (*Cipher)(nil).EnableGrokBuildChatReplay(secret); err == nil {
		t.Fatal("nil cipher enabled replay")
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c := mustCipher(t, "test-secret")
	enc, err := c.Encrypt("plaintext")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Flip the last byte of the ciphertext (after the nonce). GCM's auth tag
	// covers this, so Decrypt must fail rather than return corrupted plaintext.
	raw[len(raw)-1] ^= 0x01
	tampered := base64.StdEncoding.EncodeToString(raw)
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("Decrypt succeeded on tampered ciphertext; want auth failure")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	enc, err := mustCipher(t, "secret-a").Encrypt("plaintext")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := mustCipher(t, "secret-b").Decrypt(enc); err == nil {
		t.Fatal("Decrypt under wrong key succeeded; want auth failure")
	}
}

func TestDecryptMalformed(t *testing.T) {
	c := mustCipher(t, "test-secret")
	cases := map[string]string{
		"empty":      "",
		"too short":  "AQID", // 3 bytes < 12-byte GCM nonce
		"bad base64": "not!base64!!!",
		"nonce only": base64.StdEncoding.EncodeToString(make([]byte, c.aead.NonceSize())), // valid b64, no ciphertext
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Decrypt(in); err == nil {
				t.Fatalf("Decrypt(%q) succeeded; want error", in)
			}
		})
	}
}
