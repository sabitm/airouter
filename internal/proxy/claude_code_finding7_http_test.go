package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/ir"
)

// finding7EvictionBody is a valid, duplicate-free Messages request carrying
// evict_on_complete at every eligible position.
const finding7EvictionBody = `{"model":"default","max_tokens":32,` +
	`"system":[{"type":"text","text":"evict sys","cache_control":{"type":"ephemeral","evict_on_complete":true}}],` +
	`"tools":[{"name":"search","description":"find","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","evict_on_complete":true}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","evict_on_complete":true}}]}]}`

func finding7AssertEvictionWire(t *testing.T, body string) {
	t.Helper()
	var doc struct {
		System []map[string]any `json:"system"`
		Tools  []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("wire decode: %v; body=%s", err, body)
	}
	found := 0
	for _, block := range doc.System {
		if block["text"] != "evict sys" {
			continue
		}
		mark, ok := block["cache_control"].(map[string]any)
		if !ok || mark["type"] != "ephemeral" || mark["evict_on_complete"] != true || len(mark) != 2 {
			t.Fatalf("system eviction marker changed: %v; body=%s", mark, body)
		}
		found++
	}
	if found != 1 {
		t.Fatalf("caller system block lost: %s", body)
	}
	if len(doc.Tools) != 1 {
		t.Fatalf("tools = %d; body=%s", len(doc.Tools), body)
	}
	mark, ok := doc.Tools[0]["cache_control"].(map[string]any)
	if !ok || mark["evict_on_complete"] != true {
		t.Fatalf("tool eviction marker lost: %s", body)
	}
	var msgs []struct {
		Content []map[string]any `json:"content"`
	}
	var whole map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &whole); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(whole["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	blockMark, ok := msgs[0].Content[0]["cache_control"].(map[string]any)
	if !ok || blockMark["evict_on_complete"] != true {
		t.Fatalf("message eviction marker lost: %s", body)
	}
}

func TestFinding7CacheEvictionClaudeCodeHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		label := "unary"
		if stream {
			label = "stream"
		}
		t.Run(label, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, stream)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6",
			})
			body := finding7EvictionBody
			if stream {
				body = strings.Replace(body, `"max_tokens":32,`, `"max_tokens":32,"stream":true,`, 1)
			}
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			wantText := "hello from anthropic"
			if stream {
				wantText = "answer"
			}
			if !strings.Contains(string(out), wantText) {
				t.Fatalf("client response lost content: %s", out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			finding7AssertEvictionWire(t, wire.body)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaCacheEvict, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaPromptCache, 0)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaExtendedTTL, 0)
			assertSelectorUsage(t, "/v1/messages", stream, string(out))
		})
	}
}

func TestFinding7EvictionNestedToolResultStaysInactive(t *testing.T) {
	tr := installSelectorTransport(t)
	captures := newSelectorCapture(2)
	upstream := finding4Upstream(t, captures, false)
	base, token, _ := newSelectorProxy(t, tr, selectorTarget{
		protocol: domain.ProtocolClaudeCode,
		baseURL:  upstream.URL,
		model:    "claude-sonnet-4-6",
	})
	body := `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t0","cache_control":{"type":"ephemeral","evict_on_complete":false},"content":[{"type":"text","text":"nested","cache_control":{"type":"ephemeral","evict_on_complete":true}}]}]}]}`
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	wire := captures.next(t)
	captures.assertNoMore(t)
	// The excluded nested eviction must not activate the beta, and the outer
	// explicit false must not either.
	assertBetaFlagCount(t, wire.beta, claudecode.BetaCacheEvict, 0)
	if strings.Contains(wire.body, `"evict_on_complete":true`) {
		t.Fatalf("nested eviction hoisted onto the wire: %s", wire.body)
	}
	if !strings.Contains(wire.body, `"evict_on_complete":false`) {
		t.Fatalf("outer explicit false dropped: %s", wire.body)
	}
}

func TestFinding7EvictionUnsupportedTargetFilteredToBackup(t *testing.T) {
	tr := installSelectorTransport(t)
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
	}))
	t.Cleanup(primary.Close)
	captures := newFinding6RawCapture()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicUpstreamBody)
	}))
	t.Cleanup(backup.Close)
	base, token, _ := newSelectorProxy(t, tr,
		selectorTarget{protocol: domain.ProtocolOpenAI, baseURL: primary.URL, model: "gpt"},
		selectorTarget{protocol: domain.ProtocolAnthropic, baseURL: backup.URL, model: "claude-sonnet-4-6"},
	)
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, finding7EvictionBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if primaryHits.Load() != 0 {
		t.Fatalf("eviction-intolerant target received %d requests", primaryHits.Load())
	}
	upstreamBody := captures.next(t)
	captures.assertNoMoreUsage(t)
	finding7AssertEvictionWire(t, string(upstreamBody))
	if !strings.Contains(string(out), "hello from anthropic") {
		t.Fatalf("backup response lost content: %s", out)
	}
}

func (c *finding6RawCapture) assertNoMoreUsage(t *testing.T) {
	t.Helper()
	select {
	case body := <-c.ch:
		t.Fatalf("unexpected extra upstream capture: %s", body)
	default:
	}
}

func TestFinding7EvictionFailoverAfterPrimaryFailure(t *testing.T) {
	tr := installSelectorTransport(t)
	captures := newSelectorCapture(2)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(captureClaudeSelector(r))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"primary upstream exploded"}}`)
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(captureClaudeSelector(r))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicUpstreamBody)
	}))
	t.Cleanup(backup.Close)
	base, token, _ := newSelectorProxy(t, tr,
		selectorTarget{protocol: domain.ProtocolAnthropic, baseURL: primary.URL, model: "claude-sonnet-4-6"},
		selectorTarget{protocol: domain.ProtocolAnthropic, baseURL: backup.URL, model: "claude-sonnet-4-6"},
	)
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, finding7EvictionBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if strings.Contains(string(out), "primary upstream exploded") || strings.Contains(strings.ToLower(string(out)), `"error"`) {
		t.Fatalf("failed attempt leaked into final response: %s", out)
	}
	wire1 := captures.next(t)
	wire2 := captures.next(t)
	captures.assertNoMore(t)
	finding7AssertEvictionWire(t, wire1.body)
	finding7AssertEvictionWire(t, wire2.body)
	assertSelectorUsage(t, "/v1/messages", false, string(out))
}

func TestFinding7SpeedTranslatedRejectedBeforeContact(t *testing.T) {
	cases := []struct {
		name  string
		speed string
	}{
		{"fast", `"speed":"fast"`},
		{"empty", `"speed":""`},
		{"false", `"speed":false`},
		{"object", `"speed":{"mode":"fast"}`},
		{"unknown", `"speed":"warp"`},
		{"number", `"speed":1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicUpstreamBody)
			}))
			t.Cleanup(upstream.Close)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6",
			})
			body := `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":"hi"}],` + tc.speed + `}`
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d want 400; body=%s", resp.StatusCode, out)
			}
			if hits.Load() != 0 {
				t.Fatalf("upstream received %d requests for speed %s", hits.Load(), tc.speed)
			}
			var doc struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("error envelope: %v; body=%s", err, out)
			}
			if doc.Error.Type != "invalid_request_error" || doc.Error.Message == "" {
				t.Fatalf("wrong ingress error envelope: %s", out)
			}
			if strings.Contains(doc.Error.Message, tc.speed) {
				t.Fatalf("error echoed caller speed value: %s", out)
			}
		})
	}
}

func TestFinding7SpeedFilteredToNativeAnthropicBackup(t *testing.T) {
	tr := installSelectorTransport(t)
	var claudeHits atomic.Int32
	claudePrimary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claudeHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicUpstreamBody)
	}))
	t.Cleanup(claudePrimary.Close)
	captures := newFinding6RawCapture()
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicUpstreamBody)
	}))
	t.Cleanup(native.Close)
	base, token, _ := newSelectorProxy(t, tr,
		selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: claudePrimary.URL, model: "claude-sonnet-4-6"},
		selectorTarget{protocol: domain.ProtocolAnthropic, baseURL: native.URL, model: "claude-sonnet-4-6"},
	)
	body := `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"speed":"fast"}`
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if claudeHits.Load() != 0 {
		t.Fatalf("translated target received %d requests", claudeHits.Load())
	}
	upstreamBody := captures.next(t)
	captures.assertNoMoreUsage(t)
	if !strings.Contains(string(upstreamBody), `"speed":"fast"`) {
		t.Fatalf("native passthrough lost speed: %s", upstreamBody)
	}
	if !strings.Contains(string(out), "hello from anthropic") {
		t.Fatalf("backup response lost content: %s", out)
	}
}

func TestFinding7SpeedNativeAnthropicPassthroughUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		speed     string
		wantWire  string
		wantNoKey bool
	}{
		{name: "fast", speed: `,"speed":"fast"`, wantWire: `"speed":"fast"`},
		{name: "null", speed: `,"speed":null`},
		{name: "absent", speed: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newFinding6RawCapture()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captures.record(r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicUpstreamBody)
			}))
			t.Cleanup(upstream.Close)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolAnthropic,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6",
			})
			body := `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":"hi"}]` + tc.speed + `}`
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			upstreamBody := captures.next(t)
			captures.assertNoMoreUsage(t)
			if tc.wantWire != "" && !strings.Contains(string(upstreamBody), tc.wantWire) {
				t.Fatalf("native passthrough lost speed: %s", upstreamBody)
			}
			if tc.wantWire == "" && strings.Contains(string(upstreamBody), `"speed":"fast"`) {
				t.Fatalf("absent speed invented: %s", upstreamBody)
			}
			if !strings.Contains(string(out), "hello from anthropic") {
				t.Fatalf("passthrough response lost content: %s", out)
			}
			assertSelectorUsage(t, "/v1/messages", false, string(out))
		})
	}
}

// TestFinding7GuardCapabilityMatrix pins the codec capability sets and the IR
// walks used by the pre-network filter. HTTP fixtures above cover the
// selector-supported protocols; this matrix covers the rest.
func TestFinding7GuardCapabilityMatrix(t *testing.T) {
	evictCarriers := []string{"anth-msg", "claude-code", "opencode-messages"}
	for _, id := range evictCarriers {
		if !preservesCacheEviction(id) {
			t.Fatalf("codec %s lost eviction carriage", id)
		}
	}
	for _, id := range []string{"oai-chat", "oai-responses", "oai-codex", "cursor", "kiro", "kiro-runtime", "qoder", "antigravity", "opencode-chat", "opencode-responses"} {
		if preservesCacheEviction(id) {
			t.Fatalf("codec %s must not carry eviction", id)
		}
		if preservesAnthropicSpeed(id) {
			t.Fatalf("codec %s must not carry Messages speed", id)
		}
	}
	for _, id := range evictCarriers {
		if preservesAnthropicSpeed(id) && id != "anth-msg" {
			t.Fatalf("codec %s must not carry Messages speed", id)
		}
	}
	if !preservesAnthropicSpeed("anth-msg") {
		t.Fatal("anth-msg lost speed carriage")
	}

	truePtr := boolPtrLocal(true)
	falsePtr := boolPtrLocal(false)
	hint := func(evict *bool) *ir.CacheControl { return &ir.CacheControl{Type: "ephemeral", EvictOnComplete: evict} }
	base := func() *ir.Request {
		return &ir.Request{
			Model: "m", MaxTokens: 1,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
	}

	// Enabled eviction at each eligible IR position requires a preserving target.
	positions := []struct {
		name string
		mut  func(*ir.Request)
	}{
		{"top-level", func(r *ir.Request) { r.Anthropic = &ir.AnthropicRequest{TopLevelCache: hint(truePtr)} }},
		{"system", func(r *ir.Request) { r.SystemBlocks = []ir.SystemBlock{{Text: "s", CacheControl: hint(truePtr)}} }},
		{"tool", func(r *ir.Request) { r.Tools = []ir.Tool{{Name: "t", CacheControl: hint(truePtr)}} }},
		{"block", func(r *ir.Request) {
			r.Messages[0].Content[0].CacheControl = hint(truePtr)
		}},
		{"server raw", func(r *ir.Request) {
			r.Messages[0].Content[0] = ir.ContentBlock{Type: ir.BlockServer, AnthropicServer: &ir.AnthropicServerBlock{
				WireType: "server_tool_use",
				Raw:      json.RawMessage(`{"type":"server_tool_use","cache_control":{"type":"ephemeral","evict_on_complete":true}}`),
			}}
		}},
	}
	for _, pos := range positions {
		t.Run("evict/"+pos.name, func(t *testing.T) {
			req := base()
			pos.mut(req)
			if !requestCacheEviction(req) {
				t.Fatalf("eviction at %s not detected", pos.name)
			}
			prep := &attachmentPrep{}
			if err := prep.inspectDecoded(req); err != nil {
				t.Fatal(err)
			}
			if !prep.needsAnthropicState() {
				t.Fatalf("eviction at %s did not arm the filter", pos.name)
			}
			if reason := prep.checkThinking(openaiCodec); reason == "" {
				t.Fatalf("openai target not filtered for eviction at %s", pos.name)
			}
			if reason := prep.checkThinking(claudeCodeCodec); reason != "" {
				t.Fatalf("claude-code target filtered for eviction at %s: %s", pos.name, reason)
			}
		})
	}
	// Ordinary hints never arm the guard: explicit false, omitted, nested-only.
	nested := base()
	nested.Messages[0].Content[0] = ir.ContentBlock{Type: ir.BlockToolResult, ToolUseID: "t0", ToolResult: []ir.ContentBlock{
		{Type: ir.BlockText, Text: "nested", CacheControl: hint(truePtr)},
	}}
	if requestCacheEviction(nested) {
		t.Fatal("nested-only eviction armed the guard")
	}
	for _, evict := range []*bool{nil, falsePtr} {
		req := base()
		req.Messages[0].Content[0].CacheControl = hint(evict)
		if requestCacheEviction(req) {
			t.Fatalf("eviction %v armed the guard", evict)
		}
	}
	// Null-shaped server raw is not eviction.
	req := base()
	req.Messages[0].Content[0] = ir.ContentBlock{Type: ir.BlockServer, AnthropicServer: &ir.AnthropicServerBlock{
		WireType: "server_tool_use",
		Raw:      json.RawMessage(`{"type":"server_tool_use","cache_control":{"type":"ephemeral","evict_on_complete":false}}`),
	}}
	if requestCacheEviction(req) {
		t.Fatal("explicit false server raw armed the guard")
	}

	// Speed: any non-null shape requires the native codec; null and absence do not.
	speeds := []struct {
		raw  string
		want bool
	}{
		{`"fast"`, true}, {`""`, true}, {`false`, true}, {`{"mode":"fast"}`, true}, {`0`, true}, {`"null"`, true},
	}
	for _, tc := range speeds {
		req := base()
		req.Anthropic = &ir.AnthropicRequest{Speed: json.RawMessage(tc.raw)}
		if requestAnthropicSpeed(req) != tc.want {
			t.Fatalf("speed %s detection = %v", tc.raw, tc.want)
		}
		prep := &attachmentPrep{}
		if err := prep.inspectDecoded(req); err != nil {
			t.Fatal(err)
		}
		if !prep.needsAnthropicState() {
			t.Fatalf("speed %s did not arm the filter", tc.raw)
		}
		if reason := prep.checkThinking(claudeCodeCodec); reason == "" {
			t.Fatalf("claude-code target not filtered for speed %s", tc.raw)
		}
		if reason := prep.checkThinking(opencodeMessagesCodec); reason == "" {
			t.Fatalf("opencode-messages target not filtered for speed %s", tc.raw)
		}
		if reason := prep.checkThinking(anthropicCodec); reason != "" {
			t.Fatalf("anth-msg target filtered for speed %s: %s", tc.raw, reason)
		}
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null")} {
		req := base()
		req.Anthropic = &ir.AnthropicRequest{Speed: raw}
		if requestAnthropicSpeed(req) {
			t.Fatalf("speed %s armed the guard", raw)
		}
	}
	absent := base()
	if requestAnthropicSpeed(absent) {
		t.Fatal("absent Anthropic state armed the speed guard")
	}
}

func boolPtrLocal(v bool) *bool { return &v }
