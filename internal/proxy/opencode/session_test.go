package opencode

import (
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeIdentity(t *testing.T) {
	if got := NormalizeIdentity("  ses_ok  "); got != "ses_ok" {
		t.Fatalf("trim = %q", got)
	}
	if NormalizeIdentity("") != "" || NormalizeIdentity("   ") != "" {
		t.Fatal("empty should reject")
	}
	if NormalizeIdentity(strings.Repeat("a", maxIdentityBytes+1)) != "" {
		t.Fatal("oversized should reject")
	}
	if NormalizeIdentity("ses_\nnext") != "" || NormalizeIdentity("ses_\rinject") != "" {
		t.Fatal("control characters should reject")
	}
	if NormalizeIdentity("ses_\x00x") != "" {
		t.Fatal("NUL should reject")
	}
	keep := "ses_exact-value_OK"
	if got := NormalizeIdentity(keep); got != keep {
		t.Fatalf("valid value mutated: %q", got)
	}
}

func TestCaptureIdentityPriority(t *testing.T) {
	h := http.Header{}
	h.Set("x-opencode-session", "ses_explicit")
	h.Set("x-client-request-id", "client-req")
	body := []byte(`{"prompt_cache_key":"from-body"}`)
	id := CaptureIdentity(h, body)
	if id.Session != "ses_explicit" {
		t.Fatalf("explicit session lost: %+v", id)
	}

	h = http.Header{}
	h.Set("x-client-request-id", "client-req")
	id = CaptureIdentity(h, body)
	if id.Session != "client-req" {
		t.Fatalf("generic header lost: %+v", id)
	}

	h = http.Header{}
	h.Set("x-session-id", "from-x-session")
	h.Set("session-id", "from-session-id")
	id = CaptureIdentity(h, nil)
	if id.Session != "from-x-session" {
		t.Fatalf("x-session-id should win among generics: %+v", id)
	}

	id = CaptureIdentity(http.Header{}, []byte(`{"session_id":"body-session","conversation_id":"later"}`))
	if id.Session != "body-session" {
		t.Fatalf("body session_id = %+v", id)
	}

	id = CaptureIdentity(http.Header{}, []byte(`{"conversation_id":"conv-1"}`))
	if id.Session != "conv-1" {
		t.Fatalf("conversation_id = %+v", id)
	}

	id = CaptureIdentity(http.Header{}, []byte(`{"metadata":{"user_id":"user-1"}}`))
	if id.Session != "user-1" {
		t.Fatalf("metadata.user_id = %+v", id)
	}

	id = CaptureIdentity(http.Header{}, []byte(`{"metadata":{"user_id":123}}`))
	if id.Session != "" {
		t.Fatalf("non-string metadata.user_id accepted: %+v", id)
	}

	id = CaptureIdentity(http.Header{}, []byte(`{"prompt_cache_key":"cache-1","session_id":"later"}`))
	if id.Session != "cache-1" {
		t.Fatalf("prompt_cache_key should win body fields: %+v", id)
	}
}

func TestCaptureIdentityOpenCodeFields(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "opencode/0.16.7")
	h.Set("x-opencode-client", "my-cli")
	h.Set("x-opencode-project", "proj-a")
	h.Set("x-opencode-request", "msg_client")
	h.Set("Authorization", "Bearer secret")
	h.Set("Cookie", "sid=1")
	h.Set("X-Custom", "nope")
	id := CaptureIdentity(h, []byte(`{"metadata":{"secret":"x","user_id":{"nested":true}}}`))
	if id.UserAgent != "opencode/0.16.7" || id.Client != "my-cli" || id.Project != "proj-a" || id.Request != "msg_client" {
		t.Fatalf("identity fields: %+v", id)
	}
	if id.Session != "" {
		t.Fatalf("nested metadata.user_id should be ignored: %+v", id)
	}

	h = http.Header{}
	h.Set("User-Agent", "curl/8.0")
	id = CaptureIdentity(h, nil)
	if id.UserAgent != "" {
		t.Fatalf("non-opencode UA captured: %+v", id)
	}
}

func TestCaptureIdentityRejectsInvalid(t *testing.T) {
	h := http.Header{}
	h.Set("x-opencode-session", "ses_ok\r\nX-Injected: 1")
	h.Set("x-opencode-client", strings.Repeat("c", maxIdentityBytes+1))
	h.Set("x-opencode-project", "pro\nj")
	id := CaptureIdentity(h, []byte(`{"prompt_cache_key":"bad\nvalue"}`))
	if id.Session != "" || id.Client != "" || id.Project != "" {
		t.Fatalf("invalid candidates leaked: %+v", id)
	}

	h = http.Header{}
	h.Add("x-opencode-session", "first")
	h.Add("x-opencode-session", "second")
	if id := CaptureIdentity(h, nil); id.Session != "" {
		t.Fatalf("ambiguous duplicate session accepted: %+v", id)
	}
}

func TestResolveSessionPriorityAndFallback(t *testing.T) {
	id := Identity{Session: "ses_client"}
	if got := ResolveSession(id, "nonce", 1, "https://opencode.ai/zen/v1", "public", "transcript"); got != "ses_client" {
		t.Fatalf("client session lost: %q", got)
	}

	a := FallbackSessionID("nonce", 1, "https://opencode.ai/zen/v1", "public", "")
	b := FallbackSessionID("nonce", 1, "https://opencode.ai/zen/v1", "public", "")
	if a != b || !strings.HasPrefix(a, "ses_") {
		t.Fatalf("fallback not stable: %q %q", a, b)
	}
	if ResolveSession(Identity{}, "nonce", 1, "https://opencode.ai/zen/v1", "public", "") != a {
		t.Fatal("empty identity should use fallback")
	}
	otherProvider := FallbackSessionID("nonce", 2, "https://opencode.ai/zen/v1", "public", "")
	if otherProvider == a {
		t.Fatal("distinct providers collided on zen/public first-turn")
	}
	otherNonce := FallbackSessionID("nonce-b", 1, "https://opencode.ai/zen/v1", "public", "")
	if otherNonce == a {
		t.Fatal("distinct proxy nonces collided on zen/public first-turn")
	}
	otherTranscript := FallbackSessionID("nonce", 1, "https://opencode.ai/zen/v1", "public", "hello")
	if otherTranscript == a {
		t.Fatal("transcript did not change fallback")
	}
}

func TestSanitizeIdentityHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "curl/8.0")
	h.Set("x-opencode-session", "ses_ok\nnext")
	h.Set("x-opencode-client", "desktop")
	h.Add("x-opencode-project", "one")
	h.Add("x-opencode-project", "two")
	SanitizeIdentityHeaders(h)
	if h.Get("User-Agent") != "" {
		t.Fatalf("non-opencode UA kept: %q", h.Get("User-Agent"))
	}
	if h.Get("x-opencode-session") != "" {
		t.Fatalf("invalid session kept: %q", h.Get("x-opencode-session"))
	}
	if h.Get("x-opencode-client") != "desktop" {
		t.Fatalf("valid client dropped: %q", h.Get("x-opencode-client"))
	}
	if h.Get("x-opencode-project") != "" {
		t.Fatalf("ambiguous duplicate project kept: %q", h.Values("x-opencode-project"))
	}
}
