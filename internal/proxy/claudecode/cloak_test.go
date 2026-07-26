package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsOAuthToken(t *testing.T) {
	if !IsOAuthToken("sk-ant-oat-abc123") {
		t.Error("sk-ant-oat token should be detected as OAuth")
	}
	if IsOAuthToken("sk-ant-apikey-xyz") {
		t.Error("api key must not be detected as OAuth")
	}
	if IsOAuthToken("") {
		t.Error("empty token must not be detected as OAuth")
	}
}

func TestGenerateBillingHeader(t *testing.T) {
	h := GenerateBillingHeader([]byte(`{"model":"x"}`))
	if !strings.HasPrefix(h, "x-anthropic-billing-header: cc_version=2.1.92.") {
		t.Fatalf("billing header prefix mismatch: %q", h)
	}
	if !strings.Contains(h, "cc_entrypoint=sdk-cli;") {
		t.Fatalf("missing entrypoint: %q", h)
	}
	// cch is 5 hex; build hash is 3 hex. Verify the cch= field has 5 hex digits.
	idx := strings.Index(h, "cch=")
	if idx < 0 {
		t.Fatalf("missing cch: %q", h)
	}
	rest := h[idx+4:]
	semi := strings.Index(rest, ";")
	if semi != 5 {
		t.Fatalf("cch should be 5 hex chars, got %d in %q", semi, rest)
	}
	if !strings.HasSuffix(h, ";") {
		t.Fatalf("billing header must end with ';': %q", h)
	}
}

func TestDeriveUUIDStableAndShape(t *testing.T) {
	a := deriveUUID("account:secret")
	b := deriveUUID("account:secret")
	if a != b {
		t.Fatalf("deriveUUID not stable: %q vs %q", a, b)
	}
	// UUID v4 shape: version nibble 4 at position 14.
	if len(a) != 36 || a[14] != '4' {
		t.Fatalf("bad v4 shape: %q", a)
	}
	// variant nibble at position 19 is 8/9/a/b.
	switch a[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("bad variant nibble %q in %q", string(a[19]), a)
	}
	c := deriveUUID("account:different")
	if a == c {
		t.Fatalf("deriveUUID collided for different seeds")
	}
}

func TestGenerateFakeUserID(t *testing.T) {
	s := GenerateFakeUserID("sess-1", "seed-1")
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("user_id not valid JSON: %v (%s)", err, s)
	}
	if m["device_id"] == "" || len(m["device_id"]) != 64 {
		t.Fatalf("device_id should be 64 hex, got %q", m["device_id"])
	}
	if m["account_uuid"] == "" {
		t.Fatalf("account_uuid empty")
	}
	if m["session_id"] != "sess-1" {
		t.Fatalf("session_id should match arg, got %q", m["session_id"])
	}
	// Stable per seed; session id independent.
	s2 := GenerateFakeUserID("sess-2", "seed-1")
	var m2 map[string]string
	_ = json.Unmarshal([]byte(s2), &m2)
	if m2["device_id"] != m["device_id"] || m2["account_uuid"] != m["account_uuid"] {
		t.Fatalf("device_id/account_uuid should be stable for same seed")
	}
	if m2["session_id"] != "sess-2" {
		t.Fatalf("session_id should be the per-request arg")
	}
	// Empty seed still populates (random fallback).
	s3 := GenerateFakeUserID("sess-3", "")
	var m3 map[string]string
	_ = json.Unmarshal([]byte(s3), &m3)
	if m3["device_id"] == "" || m3["account_uuid"] == "" {
		t.Fatalf("empty seed must still populate device_id/account_uuid")
	}
}

func TestDecloakName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"Bash", "Bash"},         // native/decoy: unchanged
		{"Read", "Read"},         // native/decoy: unchanged
		{"myTool_ide", "myTool"}, // client tool: strip suffix
		{"x_ide_ide", "x_ide"},   // client tool already ending _ide: strip once
		{"plain", "plain"},       // no suffix: unchanged
		{"_ide", "_ide"},         // suffix-only: unchanged (nothing to strip)
	}
	for _, c := range cases {
		if got := DecloakName(c.in); got != c.want {
			t.Errorf("DecloakName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
