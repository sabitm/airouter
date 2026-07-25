package qoder

import (
	"strings"
	"testing"
)

func TestEncodeBodyLengthAndAlphabet(t *testing.T) {
	// 6 bytes -> 8 base64 chars
	enc := EncodeBody([]byte("abcdef"))
	if len(enc) != 8 {
		t.Fatalf("len=%d want 8", len(enc))
	}
	// 5 bytes -> 8 base64 chars with padding
	enc2 := EncodeBody([]byte("hello"))
	if len(enc2) != 8 {
		t.Fatalf("hello len=%d want 8", len(enc2))
	}
	if len(EncodeBody(nil)) != 0 {
		t.Fatal("empty should be empty")
	}
	allowed := "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!$"
	for _, ch := range string(EncodeBody([]byte("hello world this is a longer string for testing 0123456789"))) {
		if !strings.ContainsRune(allowed, ch) {
			t.Fatalf("unexpected char %q", ch)
		}
	}
	a := string(EncodeBody([]byte("abc")))
	b := string(EncodeBody([]byte("abc")))
	if a != b {
		t.Fatal("not deterministic")
	}
	if a == string(EncodeBody([]byte("xyz"))) {
		t.Fatal("different inputs should differ")
	}
}

func TestComputeSigPath(t *testing.T) {
	if got := computeSigPath(ChatURL); got != "/api/v2/service/pro/sse/agent_chat_generation" {
		t.Fatalf("sigpath=%q", got)
	}
	if got := computeSigPath(ModelListURL); got != "/api/v2/model/list" {
		t.Fatalf("model list sigpath=%q", got)
	}
}

func TestComputeSigPathEdges(t *testing.T) {
	t.Run("path without algo prefix returned unchanged", func(t *testing.T) {
		got := computeSigPath("https://api.qoder.sh/api/v2/other")
		if got != "/api/v2/other" {
			t.Errorf("got %q, want /api/v2/other", got)
		}
	})

	t.Run("algo prefix stripped", func(t *testing.T) {
		got := computeSigPath("https://api.qoder.sh/algo/api/v2/model/list")
		if got != "/api/v2/model/list" {
			t.Errorf("got %q, want /api/v2/model/list", got)
		}
	})

	t.Run("malformed url returns empty", func(t *testing.T) {
		// url.Parse rejects control characters in the URL.
		got := computeSigPath("://\x7f bad url")
		if got != "" {
			t.Errorf("got %q, want empty for malformed URL", got)
		}
	})

	t.Run("empty path returns empty path", func(t *testing.T) {
		got := computeSigPath("https://api.qoder.sh")
		if got != "" {
			t.Errorf("got %q, want empty path", got)
		}
	})
}
