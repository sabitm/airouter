package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

func mustJSON(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
}

func TestCloakToolsRenamesAndAppendsDecoys(t *testing.T) {
	body := &messagesBody{
		Tools: []wireTool{
			{Name: "search", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "Bash", Description: "mine"}, // native name but client tool: still suffixed
		},
	}
	CloakTools(body)
	names := toolNames(body.Tools)
	if names[0] != "search_ide" {
		t.Errorf("client tool not suffixed: %v", names)
	}
	if names[1] != "Bash_ide" {
		t.Errorf("native-named client tool not suffixed: %v", names)
	}
	// Decoys appended after, unsuffixed native names.
	if len(body.Tools) != 2+len(DecoyTools) {
		t.Fatalf("decoys not appended: %d tools", len(body.Tools))
	}
	if body.Tools[2].Name != "Task" || body.Tools[2].Description != "This tool is currently unavailable." {
		t.Errorf("first decoy wrong: %+v", body.Tools[2])
	}
}

func TestCloakToolsSkipsServerTools(t *testing.T) {
	body := &messagesBody{
		Tools: []wireTool{
			{Type: "web_search_20250305", Name: "web_search"},
			{Name: "client_tool"},
		},
	}
	CloakTools(body)
	if body.Tools[0].Name != "web_search" {
		t.Errorf("server tool must keep exact name, got %q", body.Tools[0].Name)
	}
	if body.Tools[1].Name != "client_tool_ide" {
		t.Errorf("client tool must be suffixed, got %q", body.Tools[1].Name)
	}
}

func TestCloakToolsRewritesHistoryAndChoice(t *testing.T) {
	blocks := []wireBlock{
		{Type: "text", Text: "hi"},
		{Type: "tool_use", ID: "t1", Name: "search", Input: json.RawMessage(`{}`)},
	}
	content, _ := json.Marshal(blocks)
	body := &messagesBody{
		Tools:      []wireTool{{Name: "search"}},
		ToolChoice: &wireToolChoice{Type: "tool", Name: "search"},
		Messages:   []wireMessage{{Role: "assistant", Content: content}},
	}
	CloakTools(body)

	var got []wireBlock
	mustJSON(t, body.Messages[0].Content, &got)
	if got[1].Name != "search_ide" {
		t.Errorf("history tool_use not suffixed: %+v", got[1])
	}
	if body.ToolChoice.Name != "search_ide" {
		t.Errorf("forced tool_choice not rewritten: %q", body.ToolChoice.Name)
	}
}

func TestCloakToolsNoOpOnEmpty(t *testing.T) {
	body := &messagesBody{}
	CloakTools(body)
	if len(body.Tools) != 0 {
		t.Errorf("empty tools must not inject decoys, got %d", len(body.Tools))
	}
}

func TestCloakToolsChoiceNotRewrittenForDecoyName(t *testing.T) {
	// A tool_choice pointing at a native/decoy name that is NOT a client tool
	// must not be rewritten (it was not renamed).
	body := &messagesBody{
		Tools:      []wireTool{{Name: "client"}},
		ToolChoice: &wireToolChoice{Type: "tool", Name: "Bash"}, // Bash is a decoy, not a client tool here
	}
	CloakTools(body)
	if body.ToolChoice.Name != "Bash" {
		t.Errorf("tool_choice on decoy name must not be rewritten, got %q", body.ToolChoice.Name)
	}
}

func TestApplyOAuthCloakingNoOpForAPIKey(t *testing.T) {
	in := []byte(`{"model":"x","messages":[],"max_tokens":10,"tools":[{"name":"t"}]}`)
	out, err := ApplyOAuthCloaking(in, "sk-ant-apikey-xyz", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(in) {
		t.Errorf("apikey token must leave body unchanged")
	}
}

func TestApplyOAuthCloakingInjectsBillingAndMetadata(t *testing.T) {
	in := []byte(`{"model":"x","system":"hello","messages":[],"max_tokens":10}`)
	out, err := ApplyOAuthCloaking(in, "sk-ant-oat-token", "sess-123", "seed-1")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("cloaked body invalid: %v", err)
	}
	// system promoted to [billing, {text: "hello"}]
	var blocks []wireBlock
	mustJSON(t, m.System, &blocks)
	if len(blocks) != 2 || !strings.HasPrefix(blocks[0].Text, "x-anthropic-billing-header:") {
		t.Fatalf("billing not injected as system[0]: %s", m.System)
	}
	if blocks[1].Text != "hello" {
		t.Errorf("existing system text not preserved: %+v", blocks[1])
	}
	// metadata.user_id set with the session id.
	if m.Metadata == nil || m.Metadata.UserID == "" {
		t.Fatalf("metadata.user_id not set")
	}
	if !strings.Contains(m.Metadata.UserID, "sess-123") {
		t.Errorf("user_id session_id mismatch: %s", m.Metadata.UserID)
	}
}

func TestApplyOAuthCloakingBillingIdempotent(t *testing.T) {
	in := []byte(`{"model":"x","messages":[],"max_tokens":10}`)
	out1, err := ApplyOAuthCloaking(in, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	// Second pass on the already-cloaked body must not add a second billing block.
	out2, err := ApplyOAuthCloaking(out1, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	_ = json.Unmarshal(out2, &m)
	var blocks []wireBlock
	mustJSON(t, m.System, &blocks)
	if len(blocks) != 1 || !strings.HasPrefix(blocks[0].Text, "x-anthropic-billing-header:") {
		t.Fatalf("idempotent re-cloak should keep a single billing block, got %d blocks", len(blocks))
	}
}

func TestApplyOAuthCloakingCloaksTools(t *testing.T) {
	in := []byte(`{"model":"x","messages":[],"max_tokens":10,"tools":[{"name":"search"}]}`)
	out, err := ApplyOAuthCloaking(in, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	_ = json.Unmarshal(out, &m)
	if len(m.Tools) == 0 || m.Tools[0].Name != "search_ide" {
		t.Fatalf("tools not cloaked in apply: %+v", m.Tools)
	}
	// Decoys present.
	found := false
	for _, tt := range m.Tools {
		if tt.Name == "Bash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("decoys not appended in apply")
	}
}

func TestApplyOAuthCloakingInvalidJSON(t *testing.T) {
	_, err := ApplyOAuthCloaking([]byte(`{not json`), "sk-ant-oat-token", "sess", "seed")
	if err == nil {
		t.Fatal("invalid JSON should error")
	}
}

// encodeCloaked runs IR through the real encoder then the OAuth cloak, the same
// seam pairing the proxy uses (encode -> finalize -> prepare/cloak).
func encodeCloaked(t *testing.T, req *ir.Request) messagesBody {
	t.Helper()
	body, err := anthropic.EncodeRequestClaudeCode(req)
	if err != nil {
		t.Fatal(err)
	}
	cloaked, err := ApplyOAuthCloaking(body, "sk-ant-oat-token", "sess-1", "seed-1")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	if err := json.Unmarshal(cloaked, &m); err != nil {
		t.Fatalf("cloaked body invalid: %v (%s)", err, cloaked)
	}
	if !strings.HasPrefix(string(m.System), "[{\"type\":\"text\",\"text\":\"x-anthropic-billing-header:") {
		t.Fatalf("billing header not injected: %s", m.System)
	}
	return m
}

func TestApplyOAuthCloakingPreservesBudgetThinking(t *testing.T) {
	req := &ir.Request{
		Model:     "claude-haiku-x",
		MaxTokens: 1000,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking:  &ir.Thinking{Mode: ir.ThinkingBudget, Budget: 2048},
	}
	m := encodeCloaked(t, req)
	if m.Thinking == nil || m.Thinking.Type != "enabled" || m.Thinking.BudgetTokens != 2048 {
		t.Fatalf("budget thinking lost by cloak: %+v", m.Thinking)
	}
	// applyThinking bumps max_tokens to budget+1024; the bump is only coherent
	// when thinking survives with it.
	if m.MaxTokens < 2048+1024 {
		t.Errorf("max_tokens = %d, want >= %d (budget+1024)", m.MaxTokens, 2048+1024)
	}
}

func TestApplyOAuthCloakingPreservesAdaptiveThinking(t *testing.T) {
	req := &ir.Request{
		Model:     "claude-sonnet-4.6",
		MaxTokens: 1000,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking:  &ir.Thinking{Mode: ir.ThinkingLevel, Level: "medium"},
	}
	m := encodeCloaked(t, req)
	if m.Thinking == nil || m.Thinking.Type != "adaptive" {
		t.Fatalf("adaptive thinking lost by cloak: %+v", m.Thinking)
	}
	if m.OutputConfig == nil || m.OutputConfig.Effort != "medium" {
		t.Fatalf("adaptive output_config.effort lost by cloak: %+v", m.OutputConfig)
	}
}

func TestApplyOAuthCloakingPreservesDisabledThinking(t *testing.T) {
	req := &ir.Request{
		Model:     "claude-haiku-x",
		MaxTokens: 100,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking:  &ir.Thinking{Mode: ir.ThinkingNone},
	}
	m := encodeCloaked(t, req)
	if m.Thinking == nil || m.Thinking.Type != "disabled" {
		t.Fatalf("disabled thinking lost by cloak: %+v", m.Thinking)
	}
}

func TestApplyOAuthCloakingPreservesDocumentTitle(t *testing.T) {
	// The title is only exercised at runtime when cloakMessageContent actually
	// remarshals the content, which happens only when a tool_use in the SAME
	// message is renamed (changed=true). A document in a tool_use-free message
	// passes through as raw bytes and would not catch a dropped Title. Keep the
	// document and the renamed tool_use in one message so the []wireBlock
	// remarshal runs over the document block.
	content, _ := json.Marshal([]wireBlock{
		{Type: "tool_use", ID: "t1", Name: "search", Input: json.RawMessage(`{}`)},
		{Type: "document", Title: "report.pdf", Source: &wireSource{Type: "base64", MediaType: "application/pdf", Data: "Zm9v"}},
	})
	in, _ := json.Marshal(messagesBody{
		Model:     "claude-x",
		MaxTokens: 10,
		Tools:     []wireTool{{Name: "search"}},
		Messages: []wireMessage{
			{Role: "assistant", Content: content},
		},
	})
	out, err := ApplyOAuthCloaking(in, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("cloaked body invalid: %v", err)
	}
	if m.Messages[0].Content == nil {
		t.Fatal("assistant message content missing after cloak")
	}
	var blocks []wireBlock
	mustJSON(t, m.Messages[0].Content, &blocks)
	// Assert the remarshal actually ran (tool_use renamed) so a raw passthrough
	// cannot mask a dropped Title.
	if len(blocks) != 2 || blocks[0].Type != "tool_use" || blocks[0].Name != "search"+ToolSuffix {
		t.Fatalf("expected tool_use remarshal to run: %+v", blocks)
	}
	if blocks[1].Type != "document" || blocks[1].Title != "report.pdf" {
		t.Fatalf("document title lost by cloak remarshal: %+v", blocks)
	}
}

// TestMessagesBodyPreservesExercisedEncoderFields guards the "remarshal drops
// nothing" invariant for every encoder field exercised by this fixture, at
// every depth the cloak actually decodes. Two seams remarshal typed structs and
// can drop unmodeled fields:
//
//   - the top level, via messagesBody (ApplyOAuthCloaking); and
//   - one level of content blocks per message, via []wireBlock
//     (cloakMessageContent), plus the system-array path (injectSystemBilling).
//
// messagesBody.Messages[].Content is json.RawMessage, so a plain messagesBody
// round trip keeps content bytes verbatim and cannot catch block drift; the
// block loss only happens where the cloak decodes []wireBlock. This test
// reproduces exactly those decode points, then asserts every key this fixture
// caused the encoder to emit survives. When the encoder gains an optional field,
// this fixture must exercise it before the guard can detect a missing DTO field.
func TestMessagesBodyPreservesExercisedEncoderFields(t *testing.T) {
	// Exercise every field and block kind currently emitted by the encoder (with
	// nested sources and tool_result content) so their keys exist to be checked.
	req := &ir.Request{
		Model:         "claude-sonnet-4.6",
		MaxTokens:     1000,
		Temperature:   ptrFloat(0.7),
		TopP:          ptrFloat(0.9),
		StopSequences: []string{"END"},
		Stream:        true,
		Tools:         []ir.Tool{{Name: "search", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`), CacheControl: cacheControlHint("5m")}},
		ToolChoice:    &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: "search"},
		System:        "sys",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "hi", CacheControl: cacheControlHint("1h")},
				{Type: ir.BlockImage, Image: &ir.Image{Data: "aW1n", MediaType: "image/png"}, CacheControl: cacheControlHint("5m")},
				{Type: ir.BlockFile, File: &ir.File{Filename: "report.pdf", MediaType: "application/pdf", Data: "Zm9v"}, CacheControl: cacheControlHint("1h")},
			}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockToolUse, ToolID: "t1", ToolName: "search", ToolInput: json.RawMessage(`{"q":"x"}`), CacheControl: cacheControlHint("5m")},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "t1", IsError: true, CacheControl: cacheControlHint("1h"), ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "err"}}},
			}},
		},
		Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "medium"},
	}
	encoded, err := anthropic.EncodeRequestClaudeCode(req)
	if err != nil {
		t.Fatal(err)
	}
	remarshaled := remarshalLikeCloak(t, encoded)

	var in, got any
	if err := json.Unmarshal(encoded, &in); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(remarshaled, &got); err != nil {
		t.Fatal(err)
	}
	assertKeysSurvive(t, "", in, got)
}

// remarshalLikeCloak reproduces every typed decode/reencode the OAuth cloak
// performs, so the result contains exactly the fields the DTOs preserve. It
// round-trips the body through messagesBody (top level), each message's content
// array through []wireBlock (cloakMessageContent), and an array-shaped system
// through []wireBlock (injectSystemBilling). Nested tool_result content stays a
// json.RawMessage in both the cloak and here, so it is intentionally not
// re-decoded.
func remarshalLikeCloak(t *testing.T, encoded []byte) []byte {
	t.Helper()
	var m messagesBody
	if err := json.Unmarshal(encoded, &m); err != nil {
		t.Fatal(err)
	}
	for i := range m.Messages {
		m.Messages[i].Content = remarshalBlocks(t, m.Messages[i].Content)
	}
	if len(m.System) > 0 && m.System[0] == '[' {
		m.System = remarshalBlocks(t, m.System)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// remarshalBlocks decodes an array of content blocks into []wireBlock and
// remarshals, mirroring the single-level block decode the cloak does. Non-array
// content passes through unchanged, matching cloakMessageContent.
func remarshalBlocks(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	if len(raw) == 0 || raw[0] != '[' {
		return raw
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertKeysSurvive walks want and asserts every object key present there also
// exists in got, recursing through nested objects and arrays (matched by index).
// It checks key presence, not value equality, so it flags a dropped field
// without coupling to encoder value choices.
func assertKeysSurvive(t *testing.T, path string, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: shape changed, want object got %T", path, got)
			return
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				t.Errorf("key %q dropped by cloak remarshal (add it to messagesBody/wireBlock in types.go)", join(path, k))
				continue
			}
			assertKeysSurvive(t, join(path, k), wv, gv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			t.Errorf("%s: shape changed, want array got %T", path, got)
			return
		}
		for i := range w {
			if i >= len(g) {
				t.Errorf("%s[%d] dropped by cloak remarshal", path, i)
				continue
			}
			assertKeysSurvive(t, fmt.Sprintf("%s[%d]", path, i), w[i], g[i])
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func ptrFloat(f float64) *float64 { return &f }

func cacheControlHint(ttl string) *ir.CacheControl {
	return &ir.CacheControl{Type: "ephemeral", TTL: ttl}
}

// TestApplyOAuthCloakingPreservesCacheControl forces the cloak block remarshal
// with an assistant text plus tool_use message and checks caller markers, tool
// suffixing, choice/history pairing, and unmarked decoys. Synthetic token only.
func TestApplyOAuthCloakingPreservesCacheControl(t *testing.T) {
	req := &ir.Request{
		Model:     "claude-sonnet-4.6",
		MaxTokens: 1000,
		Tools: []ir.Tool{
			{Name: "search", Description: "Find one record", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`), CacheControl: cacheControlHint("5m")},
			{Name: "Read", Description: "read a file", Parameters: json.RawMessage(`{"type":"object"}`), CacheControl: cacheControlHint("1h")},
		},
		ToolChoice: &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: "search"},
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "answer", CacheControl: cacheControlHint("1h")},
				{Type: ir.BlockToolUse, ToolID: "t1", ToolName: "search", ToolInput: json.RawMessage(`{"q":"x"}`), CacheControl: cacheControlHint("5m")},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "t1", CacheControl: cacheControlHint("5m"), ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "result", CacheControl: cacheControlHint("1h")}}},
			}},
		},
	}
	m := encodeCloaked(t, req)

	if len(m.Tools) != 2+len(DecoyTools) {
		t.Fatalf("tools = %d, want 2 caller + %d decoys", len(m.Tools), len(DecoyTools))
	}
	if m.Tools[0].Name != "search"+ToolSuffix || string(m.Tools[0].CacheControl) != `{"type":"ephemeral","ttl":"5m"}` {
		t.Fatalf("caller tool 0 not cloaked with marker: %+v", m.Tools[0])
	}
	if m.Tools[1].Name != "Read"+ToolSuffix || string(m.Tools[1].CacheControl) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Fatalf("caller tool 1 not cloaked with marker: %+v", m.Tools[1])
	}
	if m.Tools[0].Description != "Find one record" || string(m.Tools[0].InputSchema) != `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}` {
		t.Fatalf("caller tool metadata changed: %+v", m.Tools[0])
	}
	for _, d := range m.Tools[2:] {
		if len(d.CacheControl) != 0 {
			t.Fatalf("decoy %q carries cache_control", d.Name)
		}
	}
	if m.ToolChoice == nil || m.ToolChoice.Name != "search"+ToolSuffix {
		t.Fatalf("tool_choice not rewritten with history: %+v", m.ToolChoice)
	}

	var assistant []wireBlock
	mustJSON(t, m.Messages[0].Content, &assistant)
	if len(assistant) != 2 {
		t.Fatalf("assistant blocks = %d", len(assistant))
	}
	if assistant[0].Type != "text" || assistant[0].Text != "answer" || string(assistant[0].CacheControl) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Fatalf("assistant text marker lost: %+v", assistant[0])
	}
	if assistant[1].Type != "tool_use" || assistant[1].Name != "search"+ToolSuffix || assistant[1].ID != "t1" || string(assistant[1].CacheControl) != `{"type":"ephemeral","ttl":"5m"}` {
		t.Fatalf("assistant tool_use marker or rename lost: %+v", assistant[1])
	}

	var user []wireBlock
	mustJSON(t, m.Messages[1].Content, &user)
	if len(user) != 1 || user[0].Type != "tool_result" || user[0].ToolUseID != "t1" {
		t.Fatalf("outer tool_result changed: %+v", user)
	}
	if string(user[0].CacheControl) != `{"type":"ephemeral","ttl":"5m"}` {
		t.Fatalf("outer tool_result marker lost: %+v", user[0])
	}
	if strings.Contains(string(user[0].Content), "cache_control") {
		t.Fatalf("nested tool_result content gained a marker: %s", user[0].Content)
	}
}

// TestApplyOAuthCloakingCacheControlNoOpForAPIKey keeps the non-OAuth path
// byte-identical even when the body carries markers.
func TestApplyOAuthCloakingCacheControlNoOpForAPIKey(t *testing.T) {
	req := &ir.Request{
		Model:     "claude-sonnet-4.6",
		MaxTokens: 1000,
		System:    "sys",
		SystemBlocks: []ir.SystemBlock{
			{Text: "sys", CacheControl: cacheControlHint("1h")},
		},
		Tools:    []ir.Tool{{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`), CacheControl: cacheControlHint("5m")}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi", CacheControl: cacheControlHint("1h")}}}},
	}
	body, err := anthropic.EncodeRequestClaudeCode(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyOAuthCloaking(body, "sk-ant-api03-synthetic", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("apikey token changed a body with markers:\n got  %s\n want %s", out, body)
	}
}

// TestApplyOAuthCloakingCacheControlWithoutTools keeps top-level block markers
// when no tool forces a content remarshal, and adds no decoys.
func TestApplyOAuthCloakingCacheControlWithoutTools(t *testing.T) {
	req := &ir.Request{
		Model:     "claude-sonnet-4.6",
		MaxTokens: 1000,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi", CacheControl: cacheControlHint("1h")}}},
		},
	}
	m := encodeCloaked(t, req)
	if len(m.Tools) != 0 {
		t.Fatalf("no-tools cloak injected %d tools", len(m.Tools))
	}
	var blocks []wireBlock
	mustJSON(t, m.Messages[0].Content, &blocks)
	if len(blocks) != 1 || string(blocks[0].CacheControl) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Fatalf("marker lost without tool remarshal: %+v", blocks)
	}
}

func toolNames(tools []wireTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// TestApplyOAuthCloakingPreservesSystemCacheControl forces the OAuth cloak
// system-array path: the injected billing block is first and unmarked, and the
// caller's ordered text blocks and hints survive the wireBlock remarshal.
func TestApplyOAuthCloakingPreservesSystemCacheControl(t *testing.T) {
	req := &ir.Request{
		Model:     "claude-sonnet-4.6",
		MaxTokens: 100,
		System:    "sys one sys two",
		SystemBlocks: []ir.SystemBlock{
			{Text: "sys one"},
			{Text: " sys two", CacheControl: cacheControlHint("1h")},
		},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	}
	body, err := anthropic.EncodeRequestClaudeCode(req)
	if err != nil {
		t.Fatal(err)
	}
	cloaked, err := ApplyOAuthCloaking(body, "sk-ant-oat-token", "sess-sys", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	if err := json.Unmarshal(cloaked, &m); err != nil {
		t.Fatalf("cloaked body invalid: %v (%s)", err, cloaked)
	}
	var blocks []wireBlock
	mustJSON(t, m.System, &blocks)
	if len(blocks) != 3 {
		t.Fatalf("system blocks = %d, want 3: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "text" || !strings.HasPrefix(blocks[0].Text, "x-anthropic-billing-header:") || len(blocks[0].CacheControl) != 0 {
		t.Fatalf("billing block = %+v", blocks[0])
	}
	if blocks[1].Text != "sys one" || len(blocks[1].CacheControl) != 0 {
		t.Fatalf("caller block 0 = %+v", blocks[1])
	}
	if blocks[2].Text != " sys two" || string(blocks[2].CacheControl) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Fatalf("caller block 1 = %+v", blocks[2])
	}
	if m.Metadata == nil || !strings.Contains(m.Metadata.UserID, "sess-sys") {
		t.Fatalf("metadata session lost: %+v", m.Metadata)
	}

	// A second cloak pass must not duplicate the billing block or drop hints.
	again, err := ApplyOAuthCloaking(cloaked, "sk-ant-oat-token", "sess-sys", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m2 messagesBody
	if err := json.Unmarshal(again, &m2); err != nil {
		t.Fatal(err)
	}
	var blocks2 []wireBlock
	mustJSON(t, m2.System, &blocks2)
	if len(blocks2) != 3 || string(blocks2[2].CacheControl) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Fatalf("re-cloak changed system: %+v", blocks2)
	}
}

// TestApplyOAuthCloakingSystemWithoutHintsUsesString keeps the legacy path: an
// unmarked system still encodes as a flat string before the cloak promotes it
// to [billing, {text: string}].
func TestApplyOAuthCloakingSystemWithoutHintsUsesString(t *testing.T) {
	req := &ir.Request{
		Model:        "claude-sonnet-4.6",
		MaxTokens:    100,
		System:       "plain sys",
		SystemBlocks: []ir.SystemBlock{{Text: "plain sys"}},
		Messages:     []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	}
	body, err := anthropic.EncodeRequestClaudeCode(req)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	var sysText string
	if err := json.Unmarshal(raw["system"], &sysText); err != nil {
		t.Fatalf("unmarked system not a flat string: %s", raw["system"])
	}
	if sysText != "plain sys" {
		t.Fatalf("system = %q", sysText)
	}
	cloaked, err := ApplyOAuthCloaking(body, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	if err := json.Unmarshal(cloaked, &m); err != nil {
		t.Fatal(err)
	}
	var blocks []wireBlock
	mustJSON(t, m.System, &blocks)
	if len(blocks) != 2 || !strings.HasPrefix(blocks[0].Text, "x-anthropic-billing-header:") || blocks[1].Text != "plain sys" {
		t.Fatalf("promoted system = %+v", blocks)
	}
}
