package claudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

const (
	cchSourceFixtureSHA = "5c578e23c91216400f0cfdeb440f992d9e417255b3d309247be4218ede1bcc63"
	cchBinarySHA        = "708c77b204db30086ff6f1ed34577bda03e1be74ced80557cbf7b54249ebf0bd"
)

func TestCCHVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/cch_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("/home/")) || bytes.Contains(raw, []byte("sabit")) {
		t.Fatal("testdata contains a machine-local path")
	}
	var doc struct {
		Schema     int `json:"schema"`
		Provenance struct {
			SourceFixtureSHA256      string `json:"source_fixture_sha256"`
			MachineLocalPathsRemoved bool   `json:"machine_local_paths_removed"`
			Binary                   struct {
				SHA256 string `json:"sha256"`
			} `json:"binary"`
			NotProven string `json:"not_proven"`
		} `json:"provenance"`
		Counts struct {
			Vectors    int            `json:"vectors"`
			Categories map[string]int `json:"categories"`
		} `json:"counts"`
		Vectors []struct {
			ID            string  `json:"id"`
			Category      string  `json:"category"`
			URLB64        string  `json:"url_b64"`
			BodyB64       string  `json:"body_b64"`
			BodySHA256    string  `json:"body_sha256"`
			Kind          string  `json:"kind"`
			CCH           *string `json:"cch"`
			PlaceholderAt *int    `json:"placeholder_at"`
			IncludedLen   *int    `json:"included_len"`
			Digest        *string `json:"digest"`
			RerunKind     *string `json:"rerun_kind"`
			ResetCCH      *string `json:"reset_cch"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != 1 || doc.Provenance.SourceFixtureSHA256 != cchSourceFixtureSHA || doc.Provenance.Binary.SHA256 != cchBinarySHA || !doc.Provenance.MachineLocalPathsRemoved {
		t.Fatalf("provenance = %+v", doc.Provenance)
	}
	if !strings.Contains(doc.Provenance.NotProven, "gzip") || !strings.Contains(doc.Provenance.NotProven, "provider acceptance") {
		t.Fatalf("not_proven = %q", doc.Provenance.NotProven)
	}
	if doc.Counts.Vectors != 95 || len(doc.Vectors) != 95 {
		t.Fatalf("vectors = %d / %d", doc.Counts.Vectors, len(doc.Vectors))
	}
	wantCategories := map[string]int{
		"basic": 2, "url-gate": 7, "activation-noop": 6, "placeholder-window": 18,
		"model-skip": 8, "field-order": 3, "exclusion": 17, "interaction": 2,
		"comma": 6, "unicode": 4, "raw": 1, "whitespace": 4, "length-boundary": 17,
	}
	if len(doc.Counts.Categories) != len(wantCategories) {
		t.Fatalf("categories = %v", doc.Counts.Categories)
	}
	for name, n := range wantCategories {
		if doc.Counts.Categories[name] != n {
			t.Fatalf("category %s = %d, want %d", name, doc.Counts.Categories[name], n)
		}
	}
	seen := map[string]bool{}
	mutate, noop := 0, 0
	for _, row := range doc.Vectors {
		if seen[row.ID] {
			t.Fatalf("duplicate %s", row.ID)
		}
		seen[row.ID] = true
		url, err := base64.StdEncoding.DecodeString(row.URLB64)
		if err != nil {
			t.Fatal(row.ID, err)
		}
		body, err := base64.StdEncoding.DecodeString(row.BodyB64)
		if err != nil {
			t.Fatal(row.ID, err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != row.BodySHA256 {
			t.Fatalf("%s body sha mismatch", row.ID)
		}
		input := append([]byte(nil), body...)
		out := ApplyCCH(string(url), input)
		if !bytes.Equal(input, body) {
			t.Fatalf("%s mutated the input", row.ID)
		}
		switch row.Kind {
		case "noop":
			noop++
			if row.CCH != nil || row.PlaceholderAt != nil || row.IncludedLen != nil || row.Digest != nil {
				t.Fatalf("%s noop has patch fields", row.ID)
			}
			if !bytes.Equal(out, body) || len(out) != len(body) {
				t.Fatalf("%s noop changed bytes", row.ID)
			}
		case "mutate":
			mutate++
			if row.CCH == nil || row.PlaceholderAt == nil || row.IncludedLen == nil || row.Digest == nil || row.RerunKind == nil || row.ResetCCH == nil {
				t.Fatalf("%s missing mutate fields", row.ID)
			}
			at := *row.PlaceholderAt
			if at < 0 || at+9 > len(body) || !bytes.Equal(body[at:at+9], cchPlaceholder) {
				t.Fatalf("%s placeholder at %d", row.ID, at)
			}
			if len(out) != len(body) || !bytes.Equal(out[:at+4], body[:at+4]) || !bytes.Equal(out[at+9:], body[at+9:]) {
				t.Fatalf("%s changed bytes outside the five digits", row.ID)
			}
			if string(out[at+4:at+9]) != *row.CCH || *row.ResetCCH != *row.CCH {
				t.Fatalf("%s cch %s reset %v want %s", row.ID, out[at+4:at+9], row.ResetCCH, *row.CCH)
			}
			included, ok := cchIncluded(body)
			if !ok || len(included) != *row.IncludedLen {
				t.Fatalf("%s included %d ok %v want %d", row.ID, len(included), ok, *row.IncludedLen)
			}
			got := cchXXH64(included)
			if hex.EncodeToString([]byte{
				byte(got >> 56), byte(got >> 48), byte(got >> 40), byte(got >> 32),
				byte(got >> 24), byte(got >> 16), byte(got >> 8), byte(got),
			}) != *row.Digest || string(cchDigits(got)) != *row.CCH {
				t.Fatalf("%s digest %016x want %s", row.ID, got, *row.Digest)
			}
			again := ApplyCCH(string(url), out)
			switch *row.RerunKind {
			case "noop":
				if !bytes.Equal(again, out) {
					t.Fatalf("%s second pass changed a single placeholder", row.ID)
				}
			case "mutate":
				if bytes.Equal(again, out) {
					t.Fatalf("%s second pass was a no-op", row.ID)
				}
				diffs := cchDiffs(out, again)
				next := cchRepeatTarget(out, at, diffs)
				if next < 0 || !bytes.Equal(again[:next+4], out[:next+4]) || !bytes.Equal(again[next+9:], out[next+9:]) {
					t.Fatalf("%s second pass did not keep the first digits", row.ID)
				}
			default:
				t.Fatalf("%s rerun %s", row.ID, *row.RerunKind)
			}
			restored := append([]byte(nil), out...)
			copy(restored[at+4:at+9], []byte("00000"))
			if !bytes.Equal(restored, body) {
				t.Fatalf("%s restore did not match the input", row.ID)
			}
			if reset := ApplyCCH(string(url), restored); !bytes.Equal(reset, out) {
				t.Fatalf("%s reset did not reproduce the patch", row.ID)
			}
		default:
			t.Fatalf("%s kind %s", row.ID, row.Kind)
		}
	}
	if mutate != 77 || noop != 18 {
		t.Fatalf("mutate %d noop %d", mutate, noop)
	}
}

func TestCCHHashPins(t *testing.T) {
	pins := []struct {
		data   []byte
		digest uint64
	}{
		{nil, 0xB8B30E7DE65B46C5},
		{[]byte("a"), 0xDE28AE28B0E07BB2},
		{bytes.Repeat([]byte("x"), 32), 0x7AA9F75436524E7D},
		{bytes.Repeat([]byte("x"), 33), 0x887C9E32DDAE69A1},
	}
	for i, pin := range pins {
		if got := cchXXH64(pin.data); got != pin.digest {
			t.Fatalf("pin %d = %016x, want %016x", i, got, pin.digest)
		}
	}
	if cchSeed != 0x4D659218E32A3268 {
		t.Fatalf("seed = %x", cchSeed)
	}
}

func TestCCHQuirks(t *testing.T) {
	url := "https://api.anthropic.com/v1/messages"
	basic := []byte(`{"model":"claude-x","max_tokens":8,"system":[{"type":"text","text":" cch=00000;"}],"messages":[]}`)
	if got := ApplyCCH(url+"?beta=true", basic); !bytes.Equal(got, ApplyCCH("/v1/messages", basic)) || string(got[73:78]) != "8ca93" {
		t.Fatalf("url bytes entered the hash or basic digits changed: %s", got)
	}
	if !bytes.Equal(ApplyCCH("https://api.anthropic.com/v1/complete", basic), basic) || !bytes.Equal(ApplyCCH("/v1/MessagesXXXX", basic), basic) {
		t.Fatal("url gate patched a non-activating url")
	}
	lead := []byte(`x,"max_tokens":3}{"system":[cch=00000]}`)
	trail := []byte(`x,"max_tokens":3,{"system":[cch=00000]}`)
	leadIn, ok := cchIncluded(lead)
	trailIn, ok2 := cchIncluded(trail)
	if !ok || !ok2 || string(leadIn) != `x}{"system":[cch=00000]}` || string(trailIn) != `x,{"system":[cch=00000]}` {
		t.Fatalf("comma inclusion lead %q trail %q", leadIn, trailIn)
	}
	if string(ApplyCCH(url, lead)[32:37]) != "c27b1" || string(ApplyCCH(url, trail)[32:37]) != "1d70d" {
		t.Fatal("comma checksum mismatch")
	}
	plain := []byte(`{"model":"ab","system":[{"t":"cch=00000"}]}`)
	escaped := []byte(`{"model":"a\"b","system":[{"t":"cch=00000"}]}`)
	plainIn, _ := cchIncluded(plain)
	escIn, _ := cchIncluded(escaped)
	if string(plainIn) != `{"model":"","system":[{"t":"cch=00000"}]}` || string(escIn) != `{"model":""b","system":[{"t":"cch=00000"}]}` {
		t.Fatalf("model skip plain %q escaped %q", plainIn, escIn)
	}
	if string(ApplyCCH(url, plain)[34:39]) != "05160" || string(ApplyCCH(url, escaped)[36:41]) != "b6bdb" {
		t.Fatal("model skip checksum mismatch")
	}
	credit := []byte(`{"fallback_credit_token":"a\"b","system":[{"text":"cch=00000"}]}`)
	suffix := []byte(`{"fallback_credit_token":"a\"z","system":[{"text":"cch=00000"}]}`)
	creditIn, _ := cchIncluded(credit)
	suffixIn, _ := cchIncluded(suffix)
	if string(creditIn) != `{b","system":[{"text":"cch=00000"}]}` || string(suffixIn) != `{z","system":[{"text":"cch=00000"}]}` {
		t.Fatalf("credit skip %q / %q", creditIn, suffixIn)
	}
	if string(ApplyCCH(url, credit)[55:60]) != "0d8ef" || string(ApplyCCH(url, suffix)[55:60]) != "8ed50" {
		t.Fatal("credit checksum mismatch")
	}
	sameA := ApplyCCH(url, []byte(`{"model":"m","max_tokens":12,"system":[{"text":"cch=00000"}]}`))
	sameB := ApplyCCH(url, []byte(`{"model":"m","max_tokens":99,"system":[{"text":"cch=00000"}]}`))
	if string(sameA[52:57]) != "81da4" || string(sameB[52:57]) != "81da4" {
		t.Fatal("equal-width max_tokens values changed the hash")
	}
	spaced := ApplyCCH(url, []byte(`{"model":"m","max_tokens": 16,"system":[{"text":"cch=00000"}]}`))
	if string(spaced[53:58]) != "c1448" {
		t.Fatalf("spaced max_tokens = %s", spaced)
	}
	fit := append(append([]byte(`{"system":[`), bytes.Repeat([]byte("A"), 281)...), []byte("cch=00000]}")...)
	miss := append(append([]byte(`{"system":[`), bytes.Repeat([]byte("A"), 282)...), []byte("cch=00000]}")...)
	fitOut := ApplyCCH(url, fit)
	if !bytes.Equal(ApplyCCH(url, miss), miss) || string(fitOut[296:301]) != "44dc6" || bytes.Equal(fitOut, fit) {
		t.Fatal("300-byte window edge changed")
	}
	twice := []byte(`{"system":[{"text":"cch=00000 and cch=00000"}]}`)
	first := ApplyCCH(url, twice)
	second := ApplyCCH(url, first)
	third := ApplyCCH(url, second)
	if string(first[24:29]) != "3cd0f" || !bytes.Equal(first[34:43], cchPlaceholder) || bytes.Contains(second, cchPlaceholder) || !bytes.Equal(third, second) {
		t.Fatalf("repeat semantics first %s second %s", first, second)
	}
	nul := ApplyCCH(url, []byte("{\"system\":[{\"text\":\"cch=00000\"}],\"z\":\"\x00\"}"))
	if string(nul[24:29]) != "72291" {
		t.Fatalf("nul checksum %s", nul)
	}
}

func TestCCHNoSharedState(t *testing.T) {
	url := "https://api.anthropic.com/v1/messages"
	left := []byte(`{"system":[{"text":" cch=00000;"}]}`)
	right := []byte(`{"system":[{"text":"other cch=00000"}]}`)
	wantLeft := ApplyCCH(url, left)
	wantRight := ApplyCCH(url, right)
	var wg sync.WaitGroup
	errc := make(chan string, 32)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if !bytes.Equal(ApplyCCH(url, left), wantLeft) {
				errc <- "left"
			}
		}()
		go func() {
			defer wg.Done()
			if !bytes.Equal(ApplyCCH(url, right), wantRight) {
				errc <- "right"
			}
		}()
	}
	wg.Wait()
	close(errc)
	for msg := range errc {
		t.Fatal(msg)
	}
}

func TestBillingHeaderStillEmitsPlaceholder(t *testing.T) {
	h := BillingHeader(BillingInput{OfficialHost: true, Entrypoint: "cli", FirstUserText: "hello"})
	if !strings.Contains(h, "cch=00000;") || strings.Contains(h, "cch="+Fingerprint("hello")) {
		t.Fatalf("builder placeholder changed: %s", h)
	}
	if strings.Contains(BillingHeader(BillingInput{Entrypoint: "cli"}), "cch=") {
		t.Fatal("custom-host builder gained cch")
	}
}

// cchRepeatTarget is the next placeholder whose five digits contain every
// second-pass difference. A zero digit does not have to change.
func cchRepeatTarget(body []byte, first int, diffs []int) int {
	if len(diffs) == 0 || len(diffs) > 5 {
		return -1
	}
	search := 0
	for {
		rel := bytes.Index(body[search:], cchPlaceholder)
		if rel < 0 {
			return -1
		}
		next := search + rel
		if next > first && diffs[0] >= next+4 && diffs[len(diffs)-1] < next+9 {
			inside := true
			for _, d := range diffs {
				if d < next+4 || d >= next+9 {
					inside = false
				}
			}
			if inside {
				return next
			}
		}
		search = next + 1
	}
}

func cchDiffs(a, b []byte) []int {
	if len(a) != len(b) {
		return []int{-1}
	}
	var out []int
	for i := range a {
		if a[i] != b[i] {
			out = append(out, i)
		}
	}
	return out
}
