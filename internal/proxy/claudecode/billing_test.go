package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestFingerprintMatchesPinnedJSUnits(t *testing.T) {
	// Pinned Du: [4,7,20].map(i => text[i] || "0").join(""), then hash.
	// 'abcd😀fZ' selects the high surrogate, 'Z', and a missing unit. The
	// neighboring low surrogate is not selected. CLI 2.1.318 vector: 5ab.
	if got := Fingerprint("abcd😀fZ"); got != "5ab" {
		t.Fatalf("fingerprint=%s want 5ab", got)
	}
	// A selected high surrogate followed by a selected low surrogate joins
	// before UTF-8 encoding. Node vector String.fromCharCode(0xD83D, 0xDE00,
	// "0") hashes to 2f9.
	pair := []uint16{'a', 'b', 'c', 'd', 0xD83D, 'x', 'y', 0xDE00}
	if got := selectedUnits(pair, 4, 7, 20); got != "😀0" {
		t.Fatalf("selected pair = %q, want joined scalar", got)
	}
	sum := sha256.Sum256([]byte(BillingFingerprintSalt + "😀0" + CLIVersion))
	if got := hex.EncodeToString(sum[:])[:3]; got != "2f9" {
		t.Fatalf("selected pair node vector = %s, want 2f9", got)
	}
	// Lone low: Node vector 5ab (replacement, 'Z', missing '0').
	lowUnits := []uint16{'a', 'b', 'c', 'd', 0xDE00, 'x', 'y', 'Z'}
	if selectedUnits(lowUnits, 4) != "\uFFFD" {
		t.Fatal("lone low surrogate was decoded as a full scalar")
	}
	if selectedUnits(lowUnits, 4, 7, 20) != "\uFFFDZ0" {
		t.Fatalf("lone low selection = %q", selectedUnits(lowUnits, 4, 7, 20))
	}
	// NUL is a selected unit and stays U+0000. Node vector: bd2.
	nulUnits := []uint16{1, 2, 3, 4, 0, 6, 7, 'A', 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 'B'}
	if got := selectedUnits(nulUnits, 4, 7, 20); got != "\x00AB" {
		t.Fatalf("NUL selection = %q", got)
	}
	sum = sha256.Sum256([]byte(BillingFingerprintSalt + "\x00AB" + CLIVersion))
	if got := hex.EncodeToString(sum[:])[:3]; got != "bd2" {
		t.Fatalf("NUL node vector = %s, want bd2", got)
	}
}

func TestFingerprintUsesUTF16Indexes(t *testing.T) {
	text := "hello world, this is a test"
	units := utf16.Encode([]rune(text))
	if unitString(units, 4, 7, 20) != "oo " {
		t.Fatalf("selected units = %q", unitString(units, 4, 7, 20))
	}
	got := Fingerprint(text)
	if len(got) != 3 {
		t.Fatalf("fingerprint length = %d (%q)", len(got), got)
	}
	if Fingerprint("") != Fingerprint("abc") {
		t.Fatal("texts shorter than index 4 must share the 000 fallback")
	}
	nul := string(utf16.Decode([]uint16{1, 2, 3, 4, 0, 6, 7, 'A', 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 'B'}))
	if !strings.Contains(selectedUnits(utf16.Encode([]rune(nul)), 4, 7, 20), "\x00") {
		t.Fatal("NUL unit was replaced")
	}
	hi, lo := uint16(0xD83D), uint16(0xDE00)
	// Qm selects one UTF-16 unit. The neighboring low unit is not selected,
	// so the high unit is an isolated surrogate and becomes U+FFFD.
	gotPair := selectedUnits([]uint16{1, 2, 3, 4, hi, lo, 7, 'Z'}, 4)
	if gotPair != "\uFFFD" {
		t.Fatalf("selected high surrogate = %q, want replacement", gotPair)
	}
	both := selectedUnits([]uint16{hi, lo}, 0, 1)
	if both != "😀" {
		t.Fatalf("selected pair units = %q, want one scalar", both)
	}
	isolated := selectedUnits([]uint16{1, 2, 3, 4, 0xD800}, 4)
	if isolated != "\uFFFD" {
		t.Fatalf("isolated = %q", isolated)
	}
}

func unitString(units []uint16, indexes ...int) string {
	return selectedUnits(units, indexes...)
}

func TestBillingHeaderKeepsBuilderCCHPlaceholder(t *testing.T) {
	h := BillingHeader(BillingInput{
		Entrypoint:    "cli",
		OfficialHost:  true,
		FirstUserText: "hello world, this is a test",
	})
	if !strings.Contains(h, "cch=00000;") {
		t.Fatalf("literal cch missing: %s", h)
	}
	if strings.Contains(h, "cch="+Fingerprint("hello world, this is a test")) {
		t.Fatalf("fingerprint was written into cch: %s", h)
	}
	if !strings.Contains(h, "cc_version="+CLIVersion+"."+Fingerprint("hello world, this is a test")) {
		t.Fatalf("version fingerprint missing: %s", h)
	}
	other := BillingHeader(BillingInput{Entrypoint: "cli", OfficialHost: false, FirstUserText: "hello"})
	if strings.Contains(other, "cch=") {
		t.Fatalf("non-official host included cch: %s", other)
	}
	if IsOfficialHost("https://evil.example/api.anthropic.com") || IsOfficialHost("https://api.anthropic.com.evil") {
		t.Fatal("substring host matched")
	}
	if !IsOfficialHost("https://api.anthropic.com/v1") {
		t.Fatal("official host rejected")
	}
}

func TestBillingOmitsInvalidOptionalFields(t *testing.T) {
	bad := 0
	h := BillingHeader(BillingInput{
		Entrypoint:        "cli",
		OfficialHost:      true,
		PreviousRequestID: "not-a-req",
		PromptID:          "nope",
		TurnOrigin:        "Bad",
		PromptIndex:       &bad,
		TurnIndex:         &bad,
	})
	for _, banned := range []string{"cc_prev_req", "cc_prompt_id", "cc_turn_origin", "cc_prompt_index"} {
		if strings.Contains(h, banned) {
			t.Fatalf("invalid field emitted %s in %s", banned, h)
		}
	}
}

func TestOfficialHostMatchesNormalizedURLHost(t *testing.T) {
	for _, raw := range []string{
		"https://api.anthropic.com/v1", "http://api.anthropic.com/v1",
		"https://API.ANTHROPIC.COM:443/v1", "http://api.anthropic.com:80/v1",
		"https://api.anthropic.com:00443/v1", "https://api.anthropic.com:/v1",
	} {
		if !IsOfficialHost(raw) {
			t.Errorf("official host rejected: %q", raw)
		}
	}
	for _, raw := range []string{
		"", "api.anthropic.com", "ftp://api.anthropic.com",
		"https://api.anthropic.com:8443/v1", "https://api.anthropic.com:80/v1",
		"http://api.anthropic.com:443/v1", "https://api.anthropic.com:65536/v1",
		"https://api.anthropic.com:bad/v1", "https://api.anthropic.com.evil/v1",
		"https://evil.example/api.anthropic.com", "https://api.anthropic.com@evil.example/v1",
		"https://api.anthropic.com./v1", "https://[::1]/v1", "https://[api.anthropic.com]/v1",
	} {
		if IsOfficialHost(raw) {
			t.Errorf("nonofficial host accepted: %q", raw)
		}
	}
}

func TestBillingProjectValidatorsMatchPinnedForms(t *testing.T) {
	suffix := "01" + strings.Repeat("A", 22)
	for _, prefix := range []string{"claude_proj_", "chan_"} {
		for _, env := range []string{"", "staging_", "local_"} {
			for _, source := range []string{"harness", "user", "project", "local", "flag", "managed", "admin_default"} {
				id := prefix + env + suffix
				if !validProject(id, source) {
					t.Fatalf("valid project rejected: %q / %q", id, source)
				}
				h := BillingHeader(BillingInput{AttributionProjectID: id, AttributionProjectSource: source})
				if !strings.Contains(h, " cc_project="+id+"; cc_project_src="+source+";") {
					t.Fatalf("project attribution missing: %s", h)
				}
			}
		}
	}
	for _, pair := range [][2]string{
		{"chan_" + suffix, "unknown"}, {"channel_" + suffix, "user"},
		{"claude_proj_" + suffix + "0", "flag"}, {"chan_01" + strings.Repeat("0", 22), "harness"},
		{"chan_" + suffix, "user; cc_gateway=1"}, {"", "project"},
	} {
		if validProject(pair[0], pair[1]) {
			t.Fatalf("invalid project accepted: %q / %q", pair[0], pair[1])
		}
	}
	if h := BillingHeader(BillingInput{}); strings.Contains(h, "cc_project=") {
		t.Fatalf("project was synthesized: %s", h)
	}
}

func TestSystemBlocksKeepCallerBoundaries(t *testing.T) {
	blocks := SystemBlocks("x-anthropic-billing-header: cc_version=1;", IdentityCLI, []SystemBlock{{Text: "follow the user", CacheControl: &CacheMark{TTL: "1h"}}}, true, "5m")
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if _, ok := blocks[0]["cache_control"]; ok {
		t.Fatal("billing block must not carry cache_control")
	}
	caller := blocks[2]["cache_control"].(map[string]any)
	if caller["ttl"] != "1h" {
		t.Fatalf("caller marker changed: %#v", caller)
	}
}
