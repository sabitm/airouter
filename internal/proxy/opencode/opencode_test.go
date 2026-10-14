package opencode

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAnthropicEffortShapes(t *testing.T) {
	cases := []struct {
		model string
		want  AnthropicEffortShape
	}{
		{"claude-opus-4-5", AnthropicEffortEnabled},
		{"claude-opus-4-6", AnthropicEffortAdaptive},
		{"claude-sonnet-4-6", AnthropicEffortAdaptive},
		{"claude-opus-4-7", AnthropicEffortSummarized},
		{"claude-fable-5", AnthropicEffortSummarized},
		{"claude-opus-4-20250514", AnthropicEffortNone},
		{"qwen3.8-flash", AnthropicEffortNone},
	}
	for _, tc := range cases {
		if got := AnthropicEffort(tc.model); got != tc.want {
			t.Errorf("AnthropicEffort(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
	if got := Opus45BudgetTokens(64000); got != 16000 {
		t.Fatalf("opus budget = %d, want 16000", got)
	}
}

func TestIsResponsesModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"muse-spark-1.2", true},
		{"muse-spark-1.2-contributor-free", true},
		{"Muse-Spark-2", false},
		{"big-pickle", false},
		{"gpt-5.5", true},
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

func TestEffectiveBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"new default", ZenBaseURL, ZenBaseURL},
		{"exact legacy", LegacyZenBaseURL, ZenBaseURL},
		{"legacy one trailing slash", LegacyZenBaseURL + "/", ZenBaseURL},
		{"legacy multiple trailing slashes", LegacyZenBaseURL + "///", ZenBaseURL},
		{"legacy go", GoBaseURL, GoBaseURL},
		{"legacy go trailing slash", GoBaseURL + "/", GoBaseURL + "/"},
		{"custom host", "https://custom.example/v1", "https://custom.example/v1"},
		{"custom localhost", "http://127.0.0.1:8080/zen/v1", "http://127.0.0.1:8080/zen/v1"},
		{"custom example zen path", "https://example.test/zen/v1", "https://example.test/zen/v1"},
		{"http official host", "http://opencode.ai/zen/v1", "http://opencode.ai/zen/v1"},
		{"alternate port", "https://opencode.ai:8443/zen/v1", "https://opencode.ai:8443/zen/v1"},
		{"longer path", "https://opencode.ai/zen/v1/chat", "https://opencode.ai/zen/v1/chat"},
		{"query string", "https://opencode.ai/zen/v1?x=1", "https://opencode.ai/zen/v1?x=1"},
		{"uppercase host", "https://OpenCode.ai/zen/v1", "https://OpenCode.ai/zen/v1"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveBaseURL(tc.in); got != tc.want {
				t.Fatalf("EffectiveBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDeriveSessionIDStable(t *testing.T) {
	a := DeriveSessionID("public", "assistant text one")
	b := DeriveSessionID("public", "assistant text one")
	if a != b {
		t.Fatalf("session id not stable across calls: %q vs %q", a, b)
	}
	assertNativeSessionID(t, a)
	c := DeriveSessionID("public", "assistant text two")
	if a == c {
		t.Fatalf("distinct conversations derived the same session id")
	}
	assertNativeSessionID(t, c)
	// Distinct accounts with identical transcripts stay distinct.
	d := DeriveSessionID("sk-real-key", "assistant text one")
	if a == d {
		t.Fatalf("distinct providers derived the same session id")
	}
}

func TestNativeSessionIDPreservesAndMaps(t *testing.T) {
	native := "ses_0123456789abABCDEFGHIJKLMN"
	if !IsNativeSessionID(native) {
		t.Fatalf("fixture is not native: %q", native)
	}
	if got := NativeSessionID(native); got != native {
		t.Fatalf("native session mutated: %q", got)
	}
	mapped := NativeSessionID("client-req")
	assertNativeSessionID(t, mapped)
	if mapped == "client-req" || mapped == native {
		t.Fatalf("generic session not mapped: %q", mapped)
	}
	if NativeSessionID("client-req") != mapped {
		t.Fatal("generic session mapping is not stable")
	}
	if NativeSessionID("other-client") == mapped {
		t.Fatal("distinct generic sessions collided")
	}
	if NativeSessionID("") != "" {
		t.Fatal("empty candidate should stay empty")
	}
}

func TestNewRequestIDNativeShape(t *testing.T) {
	a := NewRequestID()
	b := NewRequestID()
	assertNativeRequestID(t, a)
	assertNativeRequestID(t, b)
	if a == b {
		t.Fatalf("request ids collided: %q", a)
	}
	assertAscendingTime(t, a, b)
}

func TestNewSessionIDDescendingTimestamp(t *testing.T) {
	before := uint64(time.Now().UnixMilli())
	a := NewSessionID()
	b := NewSessionID()
	after := uint64(time.Now().UnixMilli())
	assertNativeSessionID(t, a)
	assertNativeSessionID(t, b)
	if a == b {
		t.Fatalf("session ids collided: %q", a)
	}
	assertDescendingWindow(t, a, before, after)
	assertDescendingWindow(t, b, before, after)
	assertDescendingTime(t, a, b)
}

func assertAscendingTime(t *testing.T, first, second string) {
	t.Helper()
	if identifierTime(t, first) >= identifierTime(t, second) {
		t.Fatalf("request ids are not ascending: %q then %q", first, second)
	}
}

func assertDescendingTime(t *testing.T, first, second string) {
	t.Helper()
	if identifierTime(t, first) <= identifierTime(t, second) {
		t.Fatalf("session ids are not descending: %q then %q", first, second)
	}
}

func assertDescendingWindow(t *testing.T, id string, before, after uint64) {
	t.Helper()
	got := identifierTime(t, id)
	// The official layout stores only the low 48 bits. Compare the same mask
	// instead of reconstructing a full Unix millisecond timestamp.
	low := got & 0xffffffffffff
	start := (^((before << 12) + 1)) & 0xffffffffffff
	end := (^((after << 12) + 0xfff)) & 0xffffffffffff
	if !uint48Between(low, end, start) {
		t.Fatalf("session time %x outside [%x, %x] for %q", low, end, start, id)
	}
}

func identifierTime(t *testing.T, id string) uint64 {
	t.Helper()
	payload := id[4:]
	raw, err := hex.DecodeString(payload[:identifierTimeLen])
	if err != nil || len(raw) != 6 {
		t.Fatalf("time prefix %q: %v", id, err)
	}
	var v uint64
	for _, b := range raw {
		v = (v << 8) | uint64(b)
	}
	return v
}

func uint48Between(v, start, end uint64) bool {
	const mask = uint64(0xffffffffffff)
	v &= mask
	start &= mask
	end &= mask
	if start <= end {
		return v >= start && v <= end
	}
	return v >= start || v <= end
}

func TestIsNativeIDRejectsLegacyHex(t *testing.T) {
	legacySes := "ses_" + strings.Repeat("ab", 32)
	if IsNativeSessionID(legacySes) {
		t.Fatalf("64-hex session accepted: %q", legacySes)
	}
	legacyMsg := "msg_" + strings.Repeat("cd", 16)
	if IsNativeRequestID(legacyMsg) {
		t.Fatalf("32-hex request accepted: %q", legacyMsg)
	}
	if IsNativeSessionID("ses_client") || IsNativeRequestID("msg_client") {
		t.Fatal("short unofficial ids accepted")
	}
}

func TestIsVersionedUserAgent(t *testing.T) {
	keep := []string{
		"opencode/1.18.31",
		FreeUserAgent,
		"opencode/1.17.7 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
		"opencode/0.16.7",
		"OpenCode/1.18.31",
		"opencode/latest/2.0.27/opencode",
		"opencode/beta/2.0.27/desktop",
	}
	for _, ua := range keep {
		if !IsVersionedUserAgent(ua) {
			t.Errorf("rejected valid UA %q", ua)
		}
	}
	reject := []string{
		"",
		"opencode",
		"opencode/",
		"curl/8.0 opencode/1.18.31",
		"my-opencode-agent/1.0",
		"opencode/abc",
		"opencode/1",
		"opencode/latest/2.0.27",
		"opencode/latest/nope/opencode",
		"opencode/lat est/2.0.27/opencode",
		"opencode/1.18.31\r\nX-Injected: 1",
	}
	for _, ua := range reject {
		if IsVersionedUserAgent(ua) {
			t.Errorf("accepted invalid UA %q", ua)
		}
	}
}

func assertNativeSessionID(t *testing.T, id string) {
	t.Helper()
	if !IsNativeSessionID(id) {
		t.Fatalf("session id %q is not native-shaped", id)
	}
}

func assertNativeRequestID(t *testing.T, id string) {
	t.Helper()
	if !IsNativeRequestID(id) {
		t.Fatalf("request id %q is not native-shaped", id)
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
		{
			name: "zen 1.3 clamps and floors",
			in:   `{"model":"muse-spark-1.3","reasoning":{"effort":"max"},"max_output_tokens":8}`,
			want: map[string]any{"reasoning": map[string]any{"effort": "xhigh", "summary": "auto"}, "max_output_tokens": 16.0},
		},
		{
			name: "zen 1.3 free drops none",
			in:   `{"model":"muse-spark-1.3-contributor-free","reasoning":{"effort":"none"},"max_output_tokens":8}`,
			want: map[string]any{"max_output_tokens": 16.0},
		},
		{
			name: "go 1.2 clamps ultra",
			in:   `{"model":"muse-spark-1.2-contributor","reasoning":{"effort":"ultra"}}`,
			want: map[string]any{"reasoning": map[string]any{"effort": "xhigh", "summary": "auto"}},
		},
		{
			name: "go 1.3 floors and defaults summary",
			in:   `{"model":"muse-spark-1.3-contributor","reasoning":{"effort":"high"},"max_output_tokens":1}`,
			want: map[string]any{"reasoning": map[string]any{"effort": "high", "summary": "auto"}, "max_output_tokens": 16.0},
		},
		{
			name: "vendor prefix and level suffix still match",
			in:   `{"model":"openai/muse-spark-1.2(high)","reasoning":{"effort":"max"},"max_output_tokens":4}`,
			want: map[string]any{"reasoning": map[string]any{"effort": "xhigh", "summary": "auto"}, "max_output_tokens": 16.0},
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
			if _, wantReasoning := tc.want["reasoning"]; !wantReasoning {
				if _, ok := got["reasoning"]; ok {
					t.Fatalf("reasoning remained: %v", got["reasoning"])
				}
			}
		})
	}

	t.Run("preserves large sibling numbers while flooring", func(t *testing.T) {
		in := []byte(`{"model":"muse-spark-1.2","max_output_tokens":8,"n":9050000000000000001,"huge":1e400}`)
		out, err := PrepareMuseSparkResponse(in)
		if err != nil {
			t.Fatalf("PrepareMuseSparkResponse: %v", err)
		}
		s := string(out)
		if !strings.Contains(s, `"max_output_tokens":16`) {
			t.Errorf("floor missing: %s", out)
		}
		if !strings.Contains(s, "9050000000000000001") {
			t.Errorf("large integer lost: %s", out)
		}
		if !strings.Contains(s, "1e400") {
			t.Errorf("1e400 lost: %s", out)
		}
	})

	t.Run("fractional max_output_tokens unchanged", func(t *testing.T) {
		in := []byte(`{"model":"muse-spark-1.2","max_output_tokens":8.5}`)
		out, err := PrepareMuseSparkResponse(in)
		if err != nil {
			t.Fatalf("PrepareMuseSparkResponse: %v", err)
		}
		if !strings.Contains(string(out), `"max_output_tokens":8.5`) {
			t.Errorf("fractional token changed: %s", out)
		}
	})

	t.Run("null floor and effort keep original muse behavior", func(t *testing.T) {
		in := []byte(`{"model":"muse-spark-1.2","reasoning":{"effort":null,"summary":"detailed"},"max_output_tokens":null}`)
		out, err := PrepareMuseSparkResponse(in)
		if err != nil {
			t.Fatalf("PrepareMuseSparkResponse: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		if string(got["max_output_tokens"]) != "16" {
			t.Fatalf("max_output_tokens = %s, want 16", got["max_output_tokens"])
		}
		var reasoning map[string]json.RawMessage
		if err := json.Unmarshal(got["reasoning"], &reasoning); err != nil {
			t.Fatalf("unmarshal reasoning: %v", err)
		}
		if _, ok := reasoning["effort"]; ok {
			t.Fatalf("null effort remained: %s", reasoning["effort"])
		}
		if string(reasoning["summary"]) != `"detailed"` {
			t.Fatalf("summary = %s, want detailed", reasoning["summary"])
		}
	})

	t.Run("nested schema bytes survive muse mutation", func(t *testing.T) {
		// Duplicate members do not survive decoding into a map and encoding
		// again. The outer marshal compacts insignificant whitespace, so the
		// fixture uses duplicates rather than spacing as the visible difference.
		schema := `{"type":"object","type":"string","description":"keep  me"}`
		in := []byte(`{"model":"muse-spark-1.2","reasoning":{"effort":"max"},"max_output_tokens":8,"tools":[{"schema":` + schema + `}]}`)
		out, err := PrepareMuseSparkResponse(in)
		if err != nil {
			t.Fatalf("PrepareMuseSparkResponse: %v", err)
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(out, &root); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		var tools []json.RawMessage
		if err := json.Unmarshal(root["tools"], &tools); err != nil {
			t.Fatalf("unmarshal tools: %v", err)
		}
		if len(tools) != 1 {
			t.Fatalf("tools = %d, want 1", len(tools))
		}
		var tool map[string]json.RawMessage
		if err := json.Unmarshal(tools[0], &tool); err != nil {
			t.Fatalf("unmarshal tool: %v", err)
		}
		if string(tool["schema"]) != schema {
			t.Fatalf("schema reserialized: got %q, want %q", tool["schema"], schema)
		}
		var reasoning map[string]json.RawMessage
		if err := json.Unmarshal(root["reasoning"], &reasoning); err != nil {
			t.Fatalf("unmarshal reasoning: %v", err)
		}
		if string(reasoning["effort"]) != `"xhigh"` || string(reasoning["summary"]) != `"auto"` {
			t.Fatalf("reasoning not updated: %s", root["reasoning"])
		}
		if string(root["max_output_tokens"]) != "16" {
			t.Fatalf("max_output_tokens = %s, want 16", root["max_output_tokens"])
		}
	})

	t.Run("non-muse responses body is not mutated", func(t *testing.T) {
		for _, in := range []string{
			`{"model":"gpt-5.6-luna","reasoning":{"effort":"max"},"max_output_tokens":8}`,
			`{"model":"gpt-5.6-luna","reasoning":{"effort":"none"},"max_output_tokens":8}`,
			`{"model":"gpt-5.6-luna","reasoning":{"effort":null},"max_output_tokens":null}`,
			`{"model":"muse-spark-1.4","reasoning":{"effort":"max"},"max_output_tokens":8}`,
			`{"model":"muse-spark","reasoning":{"effort":"none"},"max_output_tokens":4}`,
			`{"model":"not-muse-spark-1.2","reasoning":{"effort":"ultra"},"max_output_tokens":1}`,
			`{"reasoning":{"effort":"max"},"max_output_tokens":8}`,
		} {
			out, err := PrepareMuseSparkResponse([]byte(in))
			if err != nil {
				t.Fatalf("PrepareMuseSparkResponse(%s): %v", in, err)
			}
			if string(out) != in {
				t.Fatalf("mutated %s into %s", in, out)
			}
		}
	})

	t.Run("top-level null rejected", func(t *testing.T) {
		if _, err := PrepareMuseSparkResponse([]byte("null")); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("malformed json rejected", func(t *testing.T) {
		if _, err := PrepareMuseSparkResponse([]byte(`{"model":`)); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("trailing data rejected", func(t *testing.T) {
		if _, err := PrepareMuseSparkResponse([]byte(`{"model":"gpt-5.6-luna"} {"b":1}`)); err == nil {
			t.Fatal("expected error")
		}
	})
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
	assertNativeSessionID(t, h.Get("x-opencode-session"))
	if h.Get("x-opencode-session") == "ses_test" {
		t.Fatal("generic session forwarded verbatim")
	}
	assertNativeRequestID(t, h.Get("x-opencode-request"))

	bare := http.Header{}
	bare.Set("User-Agent", "opencode")
	FingerprintHeaders(bare, "")
	if got := bare.Get("User-Agent"); got != UserAgent {
		t.Fatalf("bare opencode UA kept: %q", got)
	}

	generic := http.Header{}
	generic.Set("x-opencode-session", "ses_client")
	FingerprintHeaders(generic, "ses_derived")
	assertNativeSessionID(t, generic.Get("x-opencode-session"))
	if generic.Get("x-opencode-session") == "ses_client" {
		t.Fatal("existing generic session forwarded verbatim")
	}

	// A versioned OpenCode client UA is preserved; a native session wins.
	h2 := http.Header{}
	h2.Set("User-Agent", "opencode/0.16.7")
	h2.Set("x-opencode-client", "my-terminal")
	native := "ses_0123456789abABCDEFGHIJKLMN"
	h2.Set("x-opencode-session", native)
	FingerprintHeaders(h2, "ses_derived")
	if got := h2.Get("User-Agent"); got != "opencode/0.16.7" {
		t.Fatalf("client UA clobbered: %q", got)
	}
	if got := h2.Get("x-opencode-client"); got != "my-terminal" {
		t.Fatalf("client header clobbered: %q", got)
	}
	if got := h2.Get("x-opencode-session"); got != native {
		t.Fatalf("client session clobbered: %q", got)
	}
	if got := h2.Get(SessionIDHeader); got != "" {
		t.Fatalf("legacy fingerprint synthesized session id: %q", got)
	}
}

func TestFingerprintFreeHeaders(t *testing.T) {
	native := "ses_0123456789abABCDEFGHIJKLMN"
	malicious := native + "\r\nX-Injected: 1"

	empty := http.Header{}
	FingerprintFreeHeaders(empty, "ses_generic")
	if got := empty.Get("User-Agent"); got != FreeUserAgent {
		t.Fatalf("free UA = %q, want %q", got, FreeUserAgent)
	}
	if got := empty.Get("x-opencode-client"); got != FreeClient {
		t.Fatalf("free client = %q", got)
	}
	mapped := empty.Get("x-opencode-session")
	assertNativeSessionID(t, mapped)
	if mapped == "ses_generic" {
		t.Fatal("generic fallback forwarded verbatim")
	}
	if got := empty.Get(SessionIDHeader); got != mapped {
		t.Fatalf("session id mirror = %q, want %q", got, mapped)
	}
	if len(empty.Values(SessionIDHeader)) != 1 {
		t.Fatalf("session id values = %v", empty.Values(SessionIDHeader))
	}

	preserved := http.Header{}
	preserved.Set("User-Agent", "opencode/1.2.3 extra")
	preserved.Set("x-opencode-client", "my-cli")
	preserved.Set("x-opencode-session", native)
	preserved.Set(SessionIDHeader, native)
	FingerprintFreeHeaders(preserved, "ses_other")
	if preserved.Get("User-Agent") != "opencode/1.2.3 extra" || preserved.Get("x-opencode-client") != "my-cli" {
		t.Fatalf("valid caller identity clobbered: %v", preserved)
	}
	if preserved.Get("x-opencode-session") != native || preserved.Get(SessionIDHeader) != native {
		t.Fatalf("paired native session drifted: %v", preserved)
	}

	conflict := http.Header{}
	conflict.Set("x-opencode-session", native)
	conflict.Add(SessionIDHeader, "ses_0123456789abOTHERSESSIONID")
	conflict.Add(SessionIDHeader, malicious)
	FingerprintFreeHeaders(conflict, "")
	if conflict.Get("x-opencode-session") != native || conflict.Get(SessionIDHeader) != native || len(conflict.Values(SessionIDHeader)) != 1 {
		t.Fatalf("conflicting mirror won: %v", conflict)
	}

	channel := http.Header{}
	channel.Set("User-Agent", "opencode/beta/2.0.27/desktop")
	FingerprintFreeHeaders(channel, native)
	if channel.Get("User-Agent") != "opencode/beta/2.0.27/desktop" {
		t.Fatalf("channel UA replaced: %q", channel.Get("User-Agent"))
	}
	if channel.Get(SessionIDHeader) != native {
		t.Fatalf("native fallback not mirrored: %q", channel.Get(SessionIDHeader))
	}
}
