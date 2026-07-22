package cursor

import "testing"

func TestChecksumAtGolden(t *testing.T) {
	// ts=0 -> six zero bytes; jyh cipher with key=165 and +i%256 term yields
	// bytes [165,166,168,171,175,180]; base64url-no-pad = "paaoq6-0".
	got := checksumAt(0, "m1")
	if got != "paaoq6-0m1" {
		t.Errorf("checksumAt(0,m1) = %q, want paaoq6-0m1", got)
	}
}

func TestChecksumAtSuffixIsMachineID(t *testing.T) {
	got := checksumAt(1234567890, "machine-xyz")
	if len(got) <= len("machine-xyz") {
		t.Fatalf("checksum too short: %q", got)
	}
	if got[len(got)-len("machine-xyz"):] != "machine-xyz" {
		t.Errorf("checksum does not end with machine id: %q", got)
	}
	// The base64 prefix is 8 chars (6 bytes encoded, no padding).
	prefix := got[:len(got)-len("machine-xyz")]
	if len(prefix) != 8 {
		t.Errorf("prefix len = %d, want 8 (%q)", len(prefix), prefix)
	}
}

func TestGenerateChecksumUsesNow(t *testing.T) {
	orig := nowMillis
	nowMillis = func() int64 { return 1_000_000_000_000 } // -> /1e6 = 1000000
	t.Cleanup(func() { nowMillis = orig })
	got := generateChecksum("mid")
	want := checksumAt(1000000, "mid")
	if got != want {
		t.Errorf("generateChecksum = %q, want %q", got, want)
	}
}

func TestStripColonPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"tok", "tok"},
		{"sess::tok", "tok"},
		{"a::b::c", "c"},
		{"::tok", "tok"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripColonPrefix(c.in); got != c.want {
			t.Errorf("stripColonPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClientKeyIsSHA256Hex(t *testing.T) {
	got := clientKey("tok")
	if len(got) != 64 {
		t.Errorf("clientKey len = %d, want 64", len(got))
	}
	// Deterministic.
	if got != clientKey("tok") {
		t.Error("clientKey not deterministic")
	}
	if got == clientKey("other") {
		t.Error("clientKey collision")
	}
}

func TestSessionIDStableForToken(t *testing.T) {
	a := sessionID("tok")
	b := sessionID("tok")
	if a != b {
		t.Errorf("sessionID not stable: %q != %q", a, b)
	}
	if a == sessionID("other") {
		t.Error("sessionID collision")
	}
}
