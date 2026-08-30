package opencode

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestIsResponsesModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"muse-spark-1.2", true},
		{"muse-spark-1.2-contributor-free", true},
		{"Muse-Spark-2", true},
		{"big-pickle", false},
		{"gpt-5.5", false},
		{"kimi-k3", false},
		{"deepseek-v4-pro", false},
	}
	for _, tc := range cases {
		if got := IsResponsesModel(tc.model); got != tc.want {
			t.Errorf("IsResponsesModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestTier(t *testing.T) {
	if got := Tier("https://opencode.ai/zen/v1"); got != "zen" {
		t.Errorf("zen base = %q", got)
	}
	if got := Tier("https://opencode.ai/zen/go/v1"); got != "go" {
		t.Errorf("go base = %q", got)
	}
	if got := Tier("https://custom.example/v1"); got != "zen" {
		t.Errorf("custom base = %q, want zen-style default", got)
	}
}

func TestDeriveSessionIDStable(t *testing.T) {
	a := DeriveSessionID("public", "assistant text one")
	b := DeriveSessionID("public", "assistant text one")
	if a != b {
		t.Fatalf("session id not stable across calls: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "ses_") {
		t.Fatalf("session id %q missing ses_ prefix", a)
	}
	c := DeriveSessionID("public", "assistant text two")
	if a == c {
		t.Fatalf("distinct conversations derived the same session id")
	}
	// Distinct accounts with identical transcripts stay distinct.
	d := DeriveSessionID("sk-real-key", "assistant text one")
	if a == d {
		t.Fatalf("distinct providers derived the same session id")
	}
}

func TestPrepareMuseSparkResponse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			name: "clamp max to xhigh",
			in:   `{"model":"muse-spark-1.2-contributor-free","reasoning":{"effort":"max"},"max_output_tokens":400}`,
			want: map[string]any{"reasoning": map[string]any{"effort": "xhigh", "summary": "auto"}, "max_output_tokens": 400.0},
		},
		{
			name: "clamp ultra to xhigh",
			in:   `{"model":"muse-spark-1.2-contributor-free","reasoning":{"effort":"ultra"},"max_output_tokens":400}`,
			want: map[string]any{"reasoning": map[string]any{"effort": "xhigh", "summary": "auto"}, "max_output_tokens": 400.0},
		},
		{
			name: "drop explicit none",
			in:   `{"model":"muse-spark-1.2","reasoning":{"effort":"none"}}`,
			want: map[string]any{},
		},
		{
			name: "floor max_output_tokens",
			in:   `{"model":"muse-spark-1.2","max_output_tokens":8}`,
			want: map[string]any{"max_output_tokens": 16.0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := PrepareMuseSparkResponse([]byte(tc.in))
			if err != nil {
				t.Fatalf("PrepareMuseSparkResponse: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}
			for k, wantV := range tc.want {
				gotV, ok := got[k]
				if !ok {
					t.Fatalf("missing key %q in %v", k, got)
				}
				wj, _ := json.Marshal(wantV)
				gj, _ := json.Marshal(gotV)
				if string(wj) != string(gj) {
					t.Fatalf("key %q = %s, want %s", k, gj, wj)
				}
			}
		})
	}
}

func TestInjectReasoningEcho(t *testing.T) {
	deep := `{
		"model":"deepseek-v4-pro",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello"},
			{"role":"user","content":"again"}
		],
		"stream":true,
		"stream_options":{"include_usage":true},
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":"auto",
		"max_tokens":128,
		"temperature":0.2,
		"thinking":{"type":"enabled"},
		"reasoning_effort":"high",
		"vendor_extension":{"mode":"strict","nested":[1,true]}
	}`
	out, err := InjectReasoningEcho([]byte(deep), "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("InjectReasoningEcho: %v", err)
	}
	assertJSONEqual(t, out, `{
		"model":"deepseek-v4-pro",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello","reasoning_content":" "},
			{"role":"user","content":"again"}
		],
		"stream":true,
		"stream_options":{"include_usage":true},
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":"auto",
		"max_tokens":128,
		"temperature":0.2,
		"thinking":{"type":"enabled"},
		"reasoning_effort":"high",
		"vendor_extension":{"mode":"strict","nested":[1,true]}
	}`)

	kimi := `{
		"model":"kimi-k3",
		"messages":[
			{"role":"assistant","content":"plain"},
			{"role":"assistant","content":"","tool_calls":[{"id":"1","type":"function","function":{"name":"f"}}]}
		],
		"stream":true,
		"stream_options":{"include_usage":true},
		"tools":[{"type":"function","function":{"name":"f"}}],
		"max_tokens":256,
		"reasoning_effort":"medium",
		"vendor_extension":{"trace":true}
	}`
	out, err = InjectReasoningEcho([]byte(kimi), "kimi-k3")
	if err != nil {
		t.Fatalf("InjectReasoningEcho: %v", err)
	}
	assertJSONEqual(t, out, `{
		"model":"kimi-k3",
		"messages":[
			{"role":"assistant","content":"plain"},
			{"role":"assistant","content":"","tool_calls":[{"id":"1","type":"function","function":{"name":"f"}}],"reasoning_content":" "}
		],
		"stream":true,
		"stream_options":{"include_usage":true},
		"tools":[{"type":"function","function":{"name":"f"}}],
		"max_tokens":256,
		"reasoning_effort":"medium",
		"vendor_extension":{"trace":true}
	}`)

	hasRC := `{"model":"deepseek-v4-pro","messages":[{"role":"assistant","content":"x","reasoning_content":"real chain"}]}`
	out, err = InjectReasoningEcho([]byte(hasRC), "deepseek-v4-pro")
	if err != nil || string(out) != hasRC {
		t.Fatalf("existing reasoning_content body changed: %s (%v)", out, err)
	}

	noKimiToolCall := `{"model":"kimi-k3","messages":[{"role":"assistant","content":"plain"}],"stream":true}`
	out, err = InjectReasoningEcho([]byte(noKimiToolCall), "kimi-k3")
	if err != nil || string(out) != noKimiToolCall {
		t.Fatalf("kimi no-op body changed: %s (%v)", out, err)
	}

	unrelated := `{"model":"gpt-5.5","messages":[{"role":"assistant","content":"x"}]}`
	out, err = InjectReasoningEcho([]byte(unrelated), "gpt-5.5")
	if err != nil || string(out) != unrelated {
		t.Fatalf("unrelated model body changed: %s (%v)", out, err)
	}
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("unmarshal expected JSON: %v", err)
	}
	gotJSON, _ := json.Marshal(gotValue)
	wantJSON, _ := json.Marshal(wantValue)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("JSON mismatch:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestAccumulateAssistantText(t *testing.T) {
	chat := `{"model":"m","messages":[{"role":"assistant","content":"alpha"},{"role":"user","content":"ignored"},{"role":"assistant","content":[{"type":"text","text":"beta"}]}]}`
	if got := AccumulateAssistantText([]byte(chat)); got != "alphabeta" {
		t.Fatalf("chat accumulate = %q", got)
	}
	resp := `{"model":"m","input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"gamma"}]},{"type":"function_call","name":"f"}]}`
	if got := AccumulateAssistantText([]byte(resp)); got != "gamma" {
		t.Fatalf("responses accumulate = %q", got)
	}
}

func TestFingerprintHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "my-agent/1.0")
	FingerprintHeaders(h, "ses_test")
	if got := h.Get("User-Agent"); got != UserAgent {
		t.Fatalf("UA replaced = %q, want %q", got, UserAgent)
	}
	if got := h.Get("x-opencode-session"); got != "ses_test" {
		t.Fatalf("session = %q", got)
	}
	if got := h.Get("x-opencode-request"); !strings.HasPrefix(got, "msg_") {
		t.Fatalf("request id = %q", got)
	}
	// An opencode client UA is preserved; a client-set fingerprint wins.
	h2 := http.Header{}
	h2.Set("User-Agent", "opencode/0.16.7")
	h2.Set("x-opencode-client", "my-terminal")
	h2.Set("x-opencode-session", "ses_client")
	FingerprintHeaders(h2, "ses_derived")
	if got := h2.Get("User-Agent"); got != "opencode/0.16.7" {
		t.Fatalf("client UA clobbered: %q", got)
	}
	if got := h2.Get("x-opencode-client"); got != "my-terminal" {
		t.Fatalf("client header clobbered: %q", got)
	}
	if got := h2.Get("x-opencode-session"); got != "ses_client" {
		t.Fatalf("client session clobbered: %q", got)
	}
}
