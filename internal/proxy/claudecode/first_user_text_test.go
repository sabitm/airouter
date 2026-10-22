package claudecode

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Selection fixtures use literal texts and assert which text the selector
// feeds the fingerprint. UTF-16 hashing vectors stay in billing_test.go.

func TestFirstUserTextSelection(t *testing.T) {
	later := "later message with enough length for indexes"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "image-only-first-ignores-later-text",
			raw: `[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC"}}]},` +
				`{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			want: "",
		},
		{
			name: "empty-first-text-block-in-same-message",
			raw:  `[{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"second block text"}]}]`,
			want: "",
		},
		{
			name: "empty-first-text-block-ignores-later-user",
			raw:  `[{"role":"user","content":[{"type":"text","text":""}]},{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			want: "",
		},
		{
			name: "image-then-text-uses-first-text-block",
			raw:  `[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC"}},{"type":"text","text":"after image"}]}]`,
			want: "after image",
		},
		{
			name: "tool-result-only-first-never-fall-through",
			raw:  `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t0","content":[{"type":"text","text":"nested result"}]}]},{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			want: "",
		},
		{
			name: "string-content-exact-including-whitespace",
			raw:  `[{"role":"user","content":"  padded \t text  "}]`,
			want: "  padded \t text  ",
		},
		{
			name: "string-content-empty",
			raw:  `[{"role":"user","content":""}]`,
			want: "",
		},
		{
			name: "string-content-unicode-exact",
			raw:  `[{"role":"user","content":"héllo 😀 世界"}]`,
			want: "héllo 😀 世界",
		},
		{
			name: "array-text-unicode-exact",
			raw:  `[{"role":"user","content":[{"type":"text","text":"héllo 😀 世界"}]}]`,
			want: "héllo 😀 世界",
		},
		{
			name: "leading-assistant-then-user",
			raw:  `[{"role":"assistant","content":[{"type":"text","text":"assistant text"}]},{"role":"user","content":[{"type":"text","text":"first user"}]}]`,
			want: "first user",
		},
		{
			name: "no-user-message",
			raw:  `[{"role":"assistant","content":[{"type":"text","text":"assistant text"}]}]`,
			want: "",
		},
		{
			name: "missing-content-stops-scan",
			raw:  `[{"role":"user"},{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			want: "",
		},
		{
			name: "null-content-stops-scan",
			raw:  `[{"role":"user","content":null},{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			want: "",
		},
		{
			name: "empty-content-array-stops-scan",
			raw:  `[{"role":"user","content":[]},{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			want: "",
		},
		{
			name: "scalar-content-stops-scan",
			raw:  `[{"role":"user","content":123},{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			want: "",
		},
		{
			name: "non-block-array-content-stops-scan",
			raw:  `[{"role":"user","content":[1,2,3]},{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			want: "",
		},
		{
			name: "text-block-missing-text-field",
			raw:  `[{"role":"user","content":[{"type":"text"}]}]`,
			want: "",
		},
		{
			// The encoder always writes explicit types. The preexisting type==""
			// leniency stays; this row records it without claiming that shape is
			// an official CLI one.
			name: "typeless-block-leniency-retained",
			raw:  `[{"role":"user","content":[{"text":"no type field"}]}]`,
			want: "no type field",
		},
		{
			name: "not-an-array",
			raw:  `{"role":"user","content":"hi"}`,
			want: "",
		},
		{
			name: "empty-messages",
			raw:  `[]`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := firstUserText(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("firstUserText = %q want %q", got, tc.want)
			}
		})
	}
}

func finding8Prepare(t *testing.T, messages string) []byte {
	t.Helper()
	out, err := Prepare(
		[]byte(`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":`+messages+`}`),
		PrepareInput{
			SessionID: "11111111-1111-4111-8111-111111111111",
			DeviceID:  strings.Repeat("ab", 32),
			Profile:   DefaultProfile(),
			BaseURL:   "https://api.anthropic.com",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func finding8SystemBilling(t *testing.T, prepared []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(prepared))
	dec.UseNumber()
	var doc map[string]json.RawMessage
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode prepared body: %v; body=%s", err, prepared)
	}
	var sys []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(doc["system"], &sys); err != nil {
		t.Fatalf("decode system: %v; body=%s", err, prepared)
	}
	if len(sys) == 0 || !strings.HasPrefix(sys[0].Text, BillingHeaderPrefix) {
		t.Fatalf("billing system block missing: %s", prepared)
	}
	return sys[0].Text
}

func finding8CCVersion(t *testing.T, billing string) string {
	t.Helper()
	i := strings.Index(billing, "cc_version=")
	if i < 0 {
		t.Fatalf("cc_version missing: %s", billing)
	}
	rest := billing[i+len("cc_version="):]
	if j := strings.Index(rest, ";"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func finding8AssertMessagesUnchanged(t *testing.T, prepared []byte, wantMessages string) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(prepared))
	dec.UseNumber()
	var doc map[string]json.RawMessage
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	var want, got any
	dw := json.NewDecoder(strings.NewReader(wantMessages))
	dw.UseNumber()
	if err := dw.Decode(&want); err != nil {
		t.Fatalf("decode want messages: %v", err)
	}
	dg := json.NewDecoder(bytes.NewReader(doc["messages"]))
	dg.UseNumber()
	if err := dg.Decode(&got); err != nil {
		t.Fatalf("decode prepared messages: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("messages changed through prepare:\n want %s\n got  %s", want, got)
	}
}

func TestPrepareFingerprintSelectsFirstUserText(t *testing.T) {
	later := "later message with enough length for indexes"
	cases := []struct {
		name      string
		messages  string
		wantInput string
	}{
		{
			name: "image-only-first-uses-empty-input",
			messages: `[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC"}}]},` +
				`{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			wantInput: "",
		},
		{
			name:      "empty-first-text-later-message-uses-empty-input",
			messages:  `[{"role":"user","content":[{"type":"text","text":""}]},{"role":"user","content":[{"type":"text","text":"` + later + `"}]}]`,
			wantInput: "",
		},
		{
			name:      "empty-first-text-same-message-uses-empty-input",
			messages:  `[{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"second block text"}]}]`,
			wantInput: "",
		},
		{
			name:      "ordinary-text-unchanged",
			messages:  `[{"role":"user","content":"ordinary first text"}]`,
			wantInput: "ordinary first text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := finding8Prepare(t, tc.messages)
			cc := finding8CCVersion(t, finding8SystemBilling(t, out))
			if want := CLIVersion + "." + Fingerprint(tc.wantInput); cc != want {
				t.Fatalf("cc_version = %q want %q; billing=%s", cc, want, finding8SystemBilling(t, out))
			}
			if tc.wantInput == "" && strings.Contains(cc, Fingerprint(later)) {
				t.Fatalf("later message was fingerprinted: %q", cc)
			}
			finding8AssertMessagesUnchanged(t, out, tc.messages)
		})
	}
}

func TestPrepareOfficialHostKeepsLiteralCCH(t *testing.T) {
	out := finding8Prepare(t, `[{"role":"user","content":"ordinary first text"}]`)
	billing := finding8SystemBilling(t, out)
	if !strings.Contains(billing, "cch=00000") {
		t.Fatalf("literal cch missing on official host: %s", billing)
	}
}
