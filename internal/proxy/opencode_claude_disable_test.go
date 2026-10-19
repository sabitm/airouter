package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/thinking"
)

var opencodeClaudeDisableRows = []string{
	"claude-sonnet-4",
	"claude-opus-4-1",
	"claude-haiku-4-5",
	"claude-sonnet-4-5",
	"claude-opus-4-5",
	"claude-sonnet-4-6",
	"claude-opus-4-6",
	"claude-opus-4-7",
	"claude-opus-4-8",
}

// finalizeClaudeDisable runs the complete decode -> encode -> finalize ->
// prepare path for one OpenCode Messages Claude disable request.
func finalizeClaudeDisable(t *testing.T, p *domain.Provider, ingress codec, raw []byte, model string, stream bool) (map[string]any, []byte, *ir.Request) {
	t.Helper()
	rawInput := append([]byte(nil), raw...)
	req := decodeOpencodeIngress(t, ingress, raw, model)
	if stream {
		req.Stream = true
	}
	backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
	if backend.id != codecOpencodeMessages {
		t.Fatalf("codec = %s", backend.id)
	}
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	_, out := finalizeAndPrepareQwenObserved(t, p, req, backend, encoded)
	if !bytes.Equal(raw, rawInput) {
		t.Fatal("processing changed the caller body")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got, out, req
}

func assertClaudeDisabled(t *testing.T, got map[string]any, model string, cap int, stream bool) {
	t.Helper()
	if got["model"] != model || got["max_tokens"] != float64(cap) {
		t.Fatalf("model/cap = %v/%v, want %s/%d", got["model"], got["max_tokens"], model, cap)
	}
	if s, _ := got["stream"].(bool); s != stream {
		t.Fatalf("stream = %v, want %v", s, stream)
	}
	th, _ := got["thinking"].(map[string]any)
	if len(th) != 1 || th["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want exactly type disabled", th)
	}
	for _, field := range []string{"reasoning_effort", "enable_thinking", "thinking_budget", "reasoning"} {
		if _, ok := got[field]; ok {
			t.Fatalf("%s leaked: %#v", field, got[field])
		}
	}
	if oc, ok := got["output_config"].(map[string]any); ok {
		if _, has := oc["effort"]; has {
			t.Fatalf("output_config.effort leaked: %#v", oc)
		}
	}
}

func TestOpencodeClaudeDisableFinalizerAllRows(t *testing.T) {
	p := opencodeTestProvider()
	bodies := []struct {
		name   string
		raw    string
		suffix string
	}{
		{name: "thinking-disabled", raw: `{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":8192}}`},
		{name: "enable-false-positive", raw: `{"model":"combo","reasoning_effort":"high","enable_thinking":false,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "suffix-none", raw: `{"model":"combo","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`, suffix: "(none)"},
	}
	for _, model := range opencodeClaudeDisableRows {
		for _, body := range bodies {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream%v", model, body.name, stream), func(t *testing.T) {
					got, _, req := finalizeClaudeDisable(t, p, anthropicCodec, []byte(body.raw), model+body.suffix, stream)
					assertClaudeDisabled(t, got, model, 128, stream)
					if req.MaxTokens != 128 || req.Model != model || req.Stream != stream {
						t.Fatalf("request state changed: %+v", req)
					}
				})
			}
		}
	}
}

func TestOpencodeClaudeDisableFinalizerIngresses(t *testing.T) {
	p := opencodeTestProvider()
	cases := []struct {
		name   string
		c      codec
		model  string
		stream bool
		cap    int
		body   string
	}{
		{
			name: "chat-none", c: openaiCodec, model: "claude-sonnet-4-5", cap: 128,
			body: `{"model":"combo","reasoning_effort":"none","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-disabled-stream", c: openaiCodec, model: "claude-opus-4-5", stream: true, cap: 100000,
			body: `{"model":"combo","stream":true,"thinking":{"type":"disabled","budget_tokens":1000},"max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-off", c: responsesCodec, model: "claude-opus-4-6", cap: 128,
			body: `{"model":"combo","reasoning":{"effort":"off"},"max_output_tokens":128,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
		{
			name: "responses-none-stream", c: responsesCodec, model: "claude-opus-4-7", stream: true, cap: 100000,
			body: `{"model":"combo","stream":true,"reasoning":{"effort":"none"},"max_output_tokens":100000,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
		{
			name: "anthropic-disabled", c: anthropicCodec, model: "claude-opus-4-8", cap: 128,
			body: `{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high","format":{"type":"json_object"}}}`,
		},
		{
			name: "anthropic-enabled-false-stream", c: anthropicCodec, model: "claude-sonnet-4-6", stream: true, cap: 100000,
			body: `{"model":"combo","stream":true,"enable_thinking":false,"reasoning_effort":"high","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "anthropic-suffix-none", c: anthropicCodec, model: "claude-opus-4-1(none)", cap: 128,
			body: `{"model":"combo","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, req := finalizeClaudeDisable(t, p, tc.c, []byte(tc.body), tc.model, tc.stream)
			base := opencode.NormalizeModelID(tc.model)
			assertClaudeDisabled(t, got, base, tc.cap, tc.stream)
			if req.Model != base || req.MaxTokens != tc.cap {
				t.Fatalf("request state: %+v", req)
			}
			if oc, ok := got["output_config"].(map[string]any); ok {
				format, _ := oc["format"].(map[string]any)
				if _, has := oc["effort"]; has || format["type"] != "json_object" {
					t.Fatalf("output_config = %#v", oc)
				}
			}
		})
	}
}

func TestOpencodeClaudeDisableCapRestore(t *testing.T) {
	p := opencodeTestProvider()
	backend := backendCodecFor(p.Protocol, p.BaseURL, "claude-sonnet-4-5")
	baseReq := func(cap int) *ir.Request {
		return &ir.Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: cap,
			Thinking:  &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableDisabled},
			Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
	}

	t.Run("raised restored", func(t *testing.T) {
		req := baseReq(128)
		encoded := []byte(`{"model":"claude-sonnet-4-5","max_tokens":25600,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":24576},"metadata":{"keep":true}}`)
		finalized, out := finalizeAndPrepareQwenObserved(t, p, req, backend, encoded)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertClaudeDisabled(t, got, "claude-sonnet-4-5", 128, false)
		if meta, _ := got["metadata"].(map[string]any); meta["keep"] != true {
			t.Fatalf("metadata = %#v", got["metadata"])
		}
		assertExactJSONNumber(t, finalized, []string{"max_tokens"}, "128")
	})

	t.Run("lowered restored", func(t *testing.T) {
		req := baseReq(100000)
		encoded := []byte(`{"model":"claude-sonnet-4-5","max_tokens":64000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":4096}}`)
		finalized, out := finalizeAndPrepareQwenObserved(t, p, req, backend, encoded)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertClaudeDisabled(t, got, "claude-sonnet-4-5", 100000, false)
		assertExactJSONNumber(t, finalized, []string{"max_tokens"}, "100000")
	})

	t.Run("omitted resets default", func(t *testing.T) {
		req := baseReq(0)
		encoded := []byte(`{"model":"claude-sonnet-4-5","max_tokens":25600,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":24576}}`)
		finalized, out := finalizeAndPrepareQwenObserved(t, p, req, backend, encoded)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertClaudeDisabled(t, got, "claude-sonnet-4-5", 4096, false)
		assertExactJSONNumber(t, finalized, []string{"max_tokens"}, "4096")
	})

	t.Run("matching cap fast path", func(t *testing.T) {
		req := baseReq(128)
		body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)
		out, err := restoreOpencodeClaudeDisableCap(body, req, backend, p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, body) {
			t.Fatalf("fast path changed the body: %s", out)
		}
	})

	t.Run("large numeric fields preserved", func(t *testing.T) {
		const large = "9007199254740993"
		req := baseReq(128)
		encoded := []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024},"metadata":{"keep":` + large + `},"output_config":{"raw":{"n":` + large + `},"format":{"type":"json_object"}}}`)
		finalized, out := finalizeAndPrepareQwenObserved(t, p, req, backend, encoded)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertClaudeDisabled(t, got, "claude-sonnet-4-5", 128, false)
		oc, _ := got["output_config"].(map[string]any)
		format, _ := oc["format"].(map[string]any)
		if format["type"] != "json_object" {
			t.Fatalf("output_config sibling lost: %#v", oc)
		}
		assertExactJSONNumber(t, finalized, []string{"metadata", "keep"}, large)
		assertExactJSONNumber(t, finalized, []string{"output_config", "raw", "n"}, large)
		assertExactJSONNumber(t, finalized, []string{"max_tokens"}, "128")
	})

	t.Run("guard negatives leave pre-encoded disabled body unchanged", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":25600,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)
		user := []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}}
		disabled := &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableDisabled}
		assistantFinal := append(append([]ir.Message(nil), user...), ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "done"}}})
		cases := []struct {
			name string
			req  *ir.Request
		}{
			{name: "no intent", req: &ir.Request{Model: "claude-sonnet-4-5", MaxTokens: 128, Messages: user}},
			{name: "positive level intent", req: &ir.Request{Model: "claude-sonnet-4-5", MaxTokens: 128, Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"}, Messages: user}},
			{name: "positive budget intent", req: &ir.Request{Model: "claude-sonnet-4-5", MaxTokens: 128, Thinking: &ir.Thinking{Mode: ir.ThinkingBudget, Budget: 8192}, Messages: user}},
			{name: "assistant final", req: &ir.Request{Model: "claude-sonnet-4-5", MaxTokens: 128, Thinking: disabled, Messages: assistantFinal}},
			{name: "empty history", req: &ir.Request{Model: "claude-sonnet-4-5", MaxTokens: 128, Thinking: disabled}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out, err := restoreOpencodeClaudeDisableCap(append([]byte(nil), body...), tc.req, backend, p)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(out, body) {
					t.Fatalf("guard rewrote body: %s", out)
				}
			})
		}
	})
}

// A numeric suffix selects ModeBudget, but an explicit disable in the same
// request must win. The writer emits the disabled shape, the budget validator
// is exempted, and the caller cap is restored exactly.
func TestOpencodeClaudeDisableScopedNumericBudget(t *testing.T) {
	p := opencodeTestProvider()
	cases := []struct {
		name    string
		ingress codec
		model   string
		stream  bool
		cap     int
		body    string
	}{
		{
			name: "body disabled plus numeric suffix", ingress: anthropicCodec,
			model: "claude-sonnet-4-5(8192)", cap: 128,
			body: `{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":4096}}`,
		},
		{
			name: "enable false plus numeric suffix", ingress: openaiCodec,
			model: "claude-opus-4-5(8192)", cap: 128,
			body: `{"model":"combo","enable_thinking":false,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "enable false plus body budget", ingress: openaiCodec,
			model: "claude-opus-4-5", cap: 128,
			body: `{"model":"combo","enable_thinking":false,"thinking_budget":8192,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "enable false plus none suffix stream", ingress: responsesCodec,
			model: "claude-opus-4-6(none)", stream: true, cap: 100000,
			body: `{"model":"combo","stream":true,"enable_thinking":false,"max_output_tokens":100000,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, req := finalizeClaudeDisable(t, p, tc.ingress, []byte(tc.body), tc.model, tc.stream)
			assertClaudeDisabled(t, got, opencode.NormalizeModelID(tc.model), tc.cap, tc.stream)
			if req.MaxTokens != tc.cap {
				t.Fatalf("req cap = %d, want %d", req.MaxTokens, tc.cap)
			}
		})
	}
}

// Capture precedence: an explicit thinking.type enabled overrides a same-body
// enable_thinking=false. Positive intent reaches range validation, so the
// disable exemption must not swallow it.
func TestOpencodeClaudeDisablePositiveBudgetValidated(t *testing.T) {
	p := opencodeTestProvider()
	body := []byte(`{"model":"combo","enable_thinking":false,"thinking":{"type":"enabled","budget_tokens":64001},"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
	err := explicitBudgetFinalizeError(t, p, anthropicCodec, body, "claude-sonnet-4-5")
	if !errors.Is(err, thinking.ErrOpencodeExplicitBudget) {
		t.Fatalf("err = %v, want %v", err, thinking.ErrOpencodeExplicitBudget)
	}
}

func TestOpencodeClaudeDisablePassthroughGates(t *testing.T) {
	p := opencodeTestProvider()

	t.Run("positive unchanged", func(t *testing.T) {
		got, _, _ := finalizeClaudeDisable(t, p, openaiCodec, []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`), "claude-opus-4-5", false)
		th, _ := got["thinking"].(map[string]any)
		if th["type"] == "disabled" {
			t.Fatalf("positive wrote disabled: %#v", got)
		}
		if got["max_tokens"] != float64(32000) {
			t.Fatalf("positive cap = %v", got["max_tokens"])
		}
		if effortOf(t, got) == nil {
			t.Fatalf("positive effort missing: %#v", got)
		}
	})

	t.Run("no intent unchanged", func(t *testing.T) {
		got, _, req := finalizeClaudeDisable(t, p, openaiCodec, []byte(`{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`), "claude-sonnet-4-5", false)
		if _, ok := got["thinking"]; ok {
			t.Fatalf("no-intent wrote thinking: %#v", got)
		}
		if got["max_tokens"] != float64(128) || req.MaxTokens != 128 {
			t.Fatalf("no-intent cap = %v req=%d", got["max_tokens"], req.MaxTokens)
		}
	})

	t.Run("assistant final gate", func(t *testing.T) {
		body := []byte(`{"model":"combo","reasoning_effort":"none","max_tokens":512,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"done"}]}`)
		got, _, req := finalizeClaudeDisable(t, p, openaiCodec, body, "claude-sonnet-4-5", false)
		if _, ok := got["thinking"]; ok {
			t.Fatalf("assistant-final wrote thinking: %#v", got)
		}
		if got["max_tokens"] != float64(512) || req.MaxTokens != 512 {
			t.Fatalf("assistant-final cap = %v req=%d", got["max_tokens"], req.MaxTokens)
		}
	})

	t.Run("scope negatives", func(t *testing.T) {
		body := []byte(`{"model":"combo","reasoning_effort":"none","enable_thinking":false,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
		for _, model := range []string{"claude-sonnet-4-5-extra", "claude-sonnet-4.5", "claude-opus-4", "unknown-model"} {
			got := prepareOpencodeTranslated(t, openaiCodec, body, model)
			if _, ok := got["thinking"]; ok {
				t.Fatalf("%s wrote thinking: %#v", model, got)
			}
		}
	})

	t.Run("numeric suffix with body disable", func(t *testing.T) {
		// A numeric suffix supplies ModeBudget, but the body disable must win so
		// the budget validator never rejects the request.
		body := []byte(`{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)
		got, _, req := finalizeClaudeDisable(t, p, anthropicCodec, body, "claude-sonnet-4-5(8192)", false)
		assertClaudeDisabled(t, got, "claude-sonnet-4-5", 128, false)
		if req.MaxTokens != 128 {
			t.Fatalf("req cap = %d", req.MaxTokens)
		}
	})

	t.Run("claude five unchanged", func(t *testing.T) {
		got, _, _ := finalizeClaudeDisable(t, p, openaiCodec, []byte(`{"model":"combo","reasoning_effort":"none","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`), "claude-opus-5", false)
		th, _ := got["thinking"].(map[string]any)
		if th["type"] == "disabled" {
			t.Fatalf("claude-opus-5 unexpectedly disabled: %#v", got)
		}
	})
}

func TestOpencodeClaudeDisableHTTP(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		path   string
		stream bool
		cap    int
		body   string
	}{
		{name: "chat-none", model: "claude-sonnet-4-5", path: "/v1/chat/completions", cap: 128, body: `{"model":"default","reasoning_effort":"none","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "chat-disabled-stream", model: "claude-opus-4-5", path: "/v1/chat/completions", stream: true, cap: 100000, body: `{"model":"default","stream":true,"thinking":{"type":"disabled","budget_tokens":1000},"max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "responses-off", model: "claude-opus-4-6", path: "/v1/responses", cap: 128, body: `{"model":"default","reasoning":{"effort":"off"},"max_output_tokens":128,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "anthropic-disabled", model: "claude-opus-4-7", path: "/v1/messages", cap: 128, body: `{"model":"default","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high","format":{"type":"json_object"}}}`},
		{name: "anthropic-enable-false-stream", model: "claude-sonnet-4-6", path: "/v1/messages", stream: true, cap: 100000, body: `{"model":"default","stream":true,"enable_thinking":false,"reasoning_effort":"high","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "suffix-none", model: "claude-opus-4-8(none)", path: "/v1/messages", cap: 128, body: `{"model":"default","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			var captured []byte
			base, token, _ := setupOpencodeTranslatedModel(t, tc.model, func(w http.ResponseWriter, r *http.Request) {
				hits++
				captured, _ = io.ReadAll(r.Body)
				if r.Header.Get("Accept") == "text/event-stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, anthropicSSE)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			})

			req, err := http.NewRequest(http.MethodPost, base+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if hits != 1 {
				t.Fatalf("hits=%d", hits)
			}
			var got map[string]any
			if err := json.Unmarshal(captured, &got); err != nil {
				t.Fatal(err)
			}
			assertClaudeDisabled(t, got, opencode.NormalizeModelID(tc.model), tc.cap, tc.stream)
			if tc.stream {
				if !bytes.Contains(out, []byte("data:")) {
					t.Fatalf("stream response = %s", out)
				}
			} else {
				var ingress map[string]any
				if err := json.Unmarshal(out, &ingress); err != nil {
					t.Fatalf("unary response: %v body=%s", err, out)
				}
				if ingress["id"] == nil {
					t.Fatalf("unary response missing id: %s", out)
				}
			}
			if tc.name == "anthropic-disabled" {
				if oc, ok := got["output_config"].(map[string]any); ok {
					if _, has := oc["effort"]; has {
						t.Fatalf("output_config.effort leaked: %#v", oc)
					}
				}
			}
		})
	}
}
