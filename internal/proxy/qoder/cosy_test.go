package qoder

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBuildCosyHeaders(t *testing.T) {
	body := []byte("hello qoder")
	h, err := BuildCosyHeaders(body, ChatURL, Creds{
		UserID: "u1", AuthToken: "dt-test", Name: "n", Email: "e@x", MachineID: "m-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h["Authorization"], "Bearer COSY.") {
		t.Fatalf("auth=%q", h["Authorization"])
	}
	sum := md5.Sum(body)
	if h["Cosy-Bodyhash"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("bodyhash=%q", h["Cosy-Bodyhash"])
	}
	if h["Cosy-Bodylength"] != "11" {
		t.Fatalf("bodylength=%q", h["Cosy-Bodylength"])
	}
	if h["Cosy-Sigpath"] != "/api/v2/service/pro/sse/agent_chat_generation" {
		t.Fatalf("sigpath=%q", h["Cosy-Sigpath"])
	}
	if h["Cosy-User"] != "u1" || h["Cosy-Machineid"] != "m-1" {
		t.Fatalf("user/machine = %q/%q", h["Cosy-User"], h["Cosy-Machineid"])
	}
	for _, k := range []string{"Cosy-Key", "Cosy-Date", "Cosy-Version", "X-Request-Id"} {
		if h[k] == "" {
			t.Fatalf("missing %s", k)
		}
	}
}

func TestBuildCosyHeadersRequiresIdentity(t *testing.T) {
	if _, err := BuildCosyHeaders(nil, ChatURL, Creds{AuthToken: "t"}); err == nil {
		t.Fatal("want error without user id")
	}
	if _, err := BuildCosyHeaders(nil, ChatURL, Creds{UserID: "u"}); err == nil {
		t.Fatal("want error without token")
	}
}

func TestAESEncryptCBCBase64(t *testing.T) {
	t.Run("valid 16-byte key produces deterministic base64", func(t *testing.T) {
		a, err := aesEncryptCBCBase64([]byte("hello"), "0123456789abcdef")
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if a == "" {
			t.Fatal("got empty base64")
		}
		b, _ := aesEncryptCBCBase64([]byte("hello"), "0123456789abcdef")
		if a != b {
			t.Error("not deterministic for same key+plaintext")
		}
	})

	t.Run("round-trips through CBC decrypt", func(t *testing.T) {
		key := []byte("0123456789abcdef")
		enc, err := aesEncryptCBCBase64([]byte("secret message"), string(key))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("base64 decode: %v", err)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		plain := make([]byte, len(raw))
		cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(plain, raw)
		// strip PKCS7 padding
		pad := int(plain[len(plain)-1])
		plain = plain[:len(plain)-pad]
		if string(plain) != "secret message" {
			t.Errorf("decrypted = %q, want secret message", plain)
		}
	})

	t.Run("wrong key length returns error", func(t *testing.T) {
		cases := []string{"", "short", "0123456789abcdef0123456789abcdef"}
		for _, k := range cases {
			if _, err := aesEncryptCBCBase64([]byte("x"), k); err == nil {
				t.Errorf("key len %d: got nil error, want error", len(k))
			}
		}
	})
}

func TestRSAEncryptBase64(t *testing.T) {
	t.Run("non-empty input produces base64-decodable output", func(t *testing.T) {
		enc, err := rsaEncryptBase64("some-data")
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if enc == "" {
			t.Fatal("got empty base64")
		}
		if _, err := base64.StdEncoding.DecodeString(enc); err != nil {
			t.Errorf("not valid base64: %v", err)
		}
	})

	t.Run("empty input succeeds", func(t *testing.T) {
		// RSA PKCS1v15 can encrypt empty input (produces a block-sized ciphertext).
		if _, err := rsaEncryptBase64(""); err != nil {
			t.Errorf("got %v, want nil for empty input", err)
		}
	})
}

func TestEncryptUserInfo(t *testing.T) {
	t.Run("returns non-empty base64 key and info", func(t *testing.T) {
		key, info, err := encryptUserInfo(map[string]string{
			"uid": "u1", "security_oauth_token": "tok", "name": "n", "email": "e@x",
		})
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if key == "" || info == "" {
			t.Errorf("key len=%d info len=%d, want both non-empty", len(key), len(info))
		}
		if _, err := base64.StdEncoding.DecodeString(key); err != nil {
			t.Errorf("key not base64: %v", err)
		}
		if _, err := base64.StdEncoding.DecodeString(info); err != nil {
			t.Errorf("info not base64: %v", err)
		}
	})
}
