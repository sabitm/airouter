package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"airouter/internal/config"
)

// grokBuildChatBridgeInfo domain-separates the Chat replay MAC from the
// AES-256-GCM key. The MAC key is not the database key and is not exported.
const grokBuildChatBridgeInfo = "airouter/grok-build-chat-bridge/v1"

// Cipher encrypts and decrypts short secrets (API keys) with AES-256-GCM. The
// key is derived from the configured secret via SHA-256, so any secret length
// is accepted. bridgeKey is an optional HMAC key for the Grok Build Chat
// replay marker. It is not derived by New. A caller enables it only from a
// private instance secret, never from the built-in development secret.
type Cipher struct {
	aead      cipher.AEAD
	bridgeKey []byte
}

func New(secret string) (*Cipher, error) {
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// EnableGrokBuildChatReplay derives the domain-separated replay MAC key from
// secret. The built-in development secret and an empty secret are rejected and
// leave the cipher unable to sign or verify. A later call with the same private
// secret reproduces the key, so a restart does not need a schema change.
func (c *Cipher) EnableGrokBuildChatReplay(secret string) error {
	if c == nil {
		return errors.New("crypto: nil cipher")
	}
	if secret == "" || config.IsInsecureDevSecret(secret) {
		return errors.New("crypto: grok build replay requires a private instance secret")
	}
	mac := hmac.New(sha256.New, []byte(grokBuildChatBridgeInfo))
	_, _ = mac.Write([]byte(secret))
	c.bridgeKey = mac.Sum(nil)
	return nil
}

// GrokBuildChatMAC returns the domain-separated HMAC-SHA256 of payload.
// The raw key is not returned. A nil cipher returns nil, false.
func (c *Cipher) GrokBuildChatMAC(payload []byte) ([]byte, bool) {
	if c == nil || len(c.bridgeKey) == 0 {
		return nil, false
	}
	mac := hmac.New(sha256.New, c.bridgeKey)
	_, _ = mac.Write(payload)
	return mac.Sum(nil), true
}

// VerifyGrokBuildChatMAC reports whether mac is the HMAC of payload.
// Comparison is constant-time. A missing key or empty mac fails closed.
func (c *Cipher) VerifyGrokBuildChatMAC(payload, mac []byte) bool {
	sum, ok := c.GrokBuildChatMAC(payload)
	if !ok || len(mac) == 0 {
		return false
	}
	return hmac.Equal(sum, mac)
}

// Encrypt returns base64(nonce || ciphertext).
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (c *Cipher) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("crypto: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: decrypt: %w", err)
	}
	return string(plain), nil
}
