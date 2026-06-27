package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewVerifierLengthAndCharset(t *testing.T) {
	v, err := newVerifier()
	if err != nil {
		t.Fatal(err)
	}
	// 96 raw bytes -> 128 base64url chars (96*8/6).
	if len(v) != 128 {
		t.Errorf("verifier len = %d, want 128", len(v))
	}
	// base64url (no padding) uses A-Za-z0-9-_ only.
	if strings.ContainsAny(v, "+/=") {
		t.Errorf("verifier has non-url-safe chars: %q", v)
	}
	// Two verifiers must differ (high entropy).
	v2, _ := newVerifier()
	if v == v2 {
		t.Error("two verifiers identical")
	}
}

func TestChallengeS256MatchesSpec(t *testing.T) {
	v := "test-verifier"
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := challengeS256(v); got != want {
		t.Errorf("challenge = %q, want %q", got, want)
	}
}

func TestLoopbackPort(t *testing.T) {
	cases := []struct {
		uri     string
		want    int
		wantErr bool
	}{
		{"http://127.0.0.1:56121/callback", 56121, false},
		{"http://localhost:8080/callback", 8080, false},
		{"http://127.0.0.1:0/callback", 0, false},     // ephemeral, allowed
		{"https://127.0.0.1:56121/callback", 0, true}, // not http
		{"http://example.com:80/callback", 0, true},   // not loopback
		{"http://127.0.0.1/callback", 0, true},        // no port
		{"://bad", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			got, err := loopbackPort(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error for %q", tc.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("port = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEmailFromIDToken(t *testing.T) {
	tok := idToken(t, "alice@example.com")
	email, ok := emailFromIDToken(tok)
	if !ok || email != "alice@example.com" {
		t.Errorf("email = %q, ok = %v", email, ok)
	}
	if _, ok := emailFromIDToken("not-a-jwt"); ok {
		t.Error("malformed token should not yield an email")
	}
	if _, ok := emailFromIDToken(""); ok {
		t.Error("empty token should not yield an email")
	}
}
