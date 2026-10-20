package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestFingerprintMatchesPinnedJSUnits(t *testing.T) {
	// Node Qm: [4,7,20].map(i => text[i] ?? "0").join(""), then hash.
	// 'abcd😀fZ' selects the high surrogate, 'Z', and a missing unit. The
	// neighboring low surrogate is not selected. Node vector: 9a3.
	if got := Fingerprint("abcd😀fZ"); got != "9a3" {
		t.Fatalf("fingerprint=%s want 9a3", got)
	}
	// A selected high surrogate followed by a selected low surrogate joins
	// before UTF-8 encoding. Node vector String.fromCharCode(0xD83D, 0xDE00,
	// "0") hashes to cb4.
	pair := []uint16{'a', 'b', 'c', 'd', 0xD83D, 'x', 'y', 0xDE00}
	if got := selectedUnits(pair, 4, 7, 20); got != "😀0" {
		t.Fatalf("selected pair = %q, want joined scalar", got)
	}
	sum := sha256.Sum256([]byte(BillingFingerprintSalt + "😀0" + CLIVersion))
	if got := hex.EncodeToString(sum[:])[:3]; got != "cb4" {
		t.Fatalf("selected pair node vector = %s, want cb4", got)
	}
	// Lone low: Node vector 9a3 (replacement, 'Z', missing '0').
	lowUnits := []uint16{'a', 'b', 'c', 'd', 0xDE00, 'x', 'y', 'Z'}
	if selectedUnits(lowUnits, 4) != "\uFFFD" {
		t.Fatal("lone low surrogate was decoded as a full scalar")
	}
	if selectedUnits(lowUnits, 4, 7, 20) != "\uFFFDZ0" {
		t.Fatalf("lone low selection = %q", selectedUnits(lowUnits, 4, 7, 20))
	}
	// NUL is a selected unit and stays U+0000. Node vector: 1df.
	nulUnits := []uint16{1, 2, 3, 4, 0, 6, 7, 'A', 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 'B'}
	if got := selectedUnits(nulUnits, 4, 7, 20); got != "\x00AB" {
		t.Fatalf("NUL selection = %q", got)
	}
	sum = sha256.Sum256([]byte(BillingFingerprintSalt + "\x00AB" + CLIVersion))
	if got := hex.EncodeToString(sum[:])[:3]; got != "1df" {
		t.Fatalf("NUL node vector = %s, want 1df", got)
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

func TestBillingHeaderKeepsLiteralCCH(t *testing.T) {
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
