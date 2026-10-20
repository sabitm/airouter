package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func preparedSystemText(t *testing.T, body []byte) []string {
	t.Helper()
	var doc struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(doc.System))
	for i, block := range doc.System {
		out[i] = block.Text
	}
	return out
}

func TestPrepareKeepsFlatAndBlockedCallerWhitespace(t *testing.T) {
	const exact = "  lead and trail\n "
	in := PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile()}
	quote := quoteJSONForTest(exact)
	for _, system := range []string{quote, `[{"type":"text","text":` + quote + `}]`} {
		body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":` + system + `,"messages":[{"role":"user","content":"hi"}]}`)
		out, err := Prepare(body, in)
		if err != nil {
			t.Fatal(err)
		}
		blocks := preparedSystemText(t, out)
		if len(blocks) == 0 || blocks[len(blocks)-1] != exact {
			t.Fatalf("caller text changed: %q", blocks)
		}
	}
}

func TestPrepareRemovesOnlyKnownPrepPrefix(t *testing.T) {
	const caller = "  caller text\n "
	billing := BillingHeader(BillingInput{Entrypoint: "cli"})
	combined := billing + "\n" + IdentityCLI + "\n" + caller
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":[{"type":"text","text":` + quoteJSONForTest(combined) + `}],"messages":[{"role":"user","content":"hi"}]}`)
	out, err := Prepare(body, PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile()})
	if err != nil {
		t.Fatal(err)
	}
	blocks := preparedSystemText(t, out)
	if len(blocks) == 0 || blocks[len(blocks)-1] != caller {
		t.Fatalf("known prep prefix removal changed caller bytes: %q", blocks)
	}
	if strings.Count(string(out), BillingHeaderPrefix) != 1 {
		t.Fatalf("billing prefix was duplicated or left behind: %s", out)
	}
}

func TestStripPreparedTextLeavesOrdinaryWhitespaceExact(t *testing.T) {
	for _, input := range []string{
		"  lead and trail\n ",
		"  " + IdentityCLI + "\n ",
		"\ncaller\n",
	} {
		got, dropped := stripPreparedText(input)
		if dropped || got != input {
			t.Fatalf("ordinary text changed: input=%q got=%q dropped=%v", input, got, dropped)
		}
	}
}

func quoteJSONForTest(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
