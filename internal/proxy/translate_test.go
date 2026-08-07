package proxy

import (
	"encoding/json"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/thinking"
)

// OpenAI chat request with a system prompt and tool definition should land in
// the IR and re-encode into a well-formed Anthropic request.
func TestOpenAIToAnthropicRequest(t *testing.T) {
	in := []byte(`{
		"model":"default",
		"max_tokens":256,
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hi"}
		],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}],
		"tool_choice":"auto"
	}`)
	req, err := openai.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "be brief" {
		t.Errorf("system = %q", req.System)
	}
	if req.MaxTokens != 256 {
		t.Errorf("max_tokens = %d", req.MaxTokens)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Errorf("tools = %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceAuto {
		t.Errorf("tool_choice = %+v", req.ToolChoice)
	}

	out, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Tools     []struct {
			Name string `json:"name"`
		} `json:"tools"`
		ToolChoice struct {
			Type string `json:"type"`
		} `json:"tool_choice"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.System != "be brief" || got.MaxTokens != 256 || got.ToolChoice.Type != "auto" {
		t.Errorf("anthropic req = %s", out)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Errorf("anthropic tools = %s", out)
	}
}

// A tool call round-trips from an Anthropic response into an OpenAI response.
func TestAnthropicToolUseToOpenAIResponse(t *testing.T) {
	in := []byte(`{
		"id":"msg_1","model":"claude","role":"assistant","type":"message",
		"stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"paris"}}],
		"usage":{"input_tokens":5,"output_tokens":7}
	}`)
	resp, err := anthropic.DecodeResponse(in)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}

	out, err := openai.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %s", out)
	}
	c := got.Choices[0]
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", c.FinishReason)
	}
	if c.Message.Content != nil {
		t.Errorf("content should be null, got %v", *c.Message.Content)
	}
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool_calls = %s", out)
	}
	if c.Message.ToolCalls[0].Function.Arguments != `{"city":"paris"}` {
		t.Errorf("arguments = %q", c.Message.ToolCalls[0].Function.Arguments)
	}
	if got.Usage.PromptTokens != 5 || got.Usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

// OpenAI tool result messages (role:"tool") must fold into an Anthropic user
// message carrying tool_result blocks.
func TestOpenAIToolResultToAnthropic(t *testing.T) {
	in := []byte(`{
		"model":"default",
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"paris\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"sunny"}
		]
	}`)
	req, err := openai.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	last := req.Messages[2]
	if last.Role != ir.RoleUser || len(last.Content) != 1 || last.Content[0].Type != ir.BlockToolResult {
		t.Fatalf("tool result message = %+v", last)
	}
	if last.Content[0].ToolUseID != "call_1" {
		t.Errorf("tool_use_id = %q", last.Content[0].ToolUseID)
	}

	out, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	tr := got.Messages[2]
	if tr.Role != "user" || tr.Content[0].Type != "tool_result" || tr.Content[0].ToolUseID != "call_1" {
		t.Errorf("anthropic tool result = %s", out)
	}
}

func TestThinkingOpenAIToAnthropicAdaptive(t *testing.T) {
	in := []byte(`{
		"model":"default",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high"
	}`)
	req, err := openai.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking == nil || req.Thinking.Mode != ir.ThinkingLevel || req.Thinking.Level != "high" {
		t.Fatalf("thinking = %+v", req.Thinking)
	}
	req.Model = "claude-opus-4-7"
	out, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Thinking *struct {
			Type string `json:"type"`
		} `json:"thinking"`
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking == nil || got.Thinking.Type != "adaptive" {
		t.Fatalf("thinking = %+v", got.Thinking)
	}
	if got.OutputConfig == nil || got.OutputConfig.Effort != "high" {
		t.Fatalf("output_config = %+v", got.OutputConfig)
	}
}

func TestThinkingOpenAIToAnthropicBudget(t *testing.T) {
	in := []byte(`{
		"model":"default",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high"
	}`)
	req, err := openai.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	req.Model = "claude-haiku-4.5"
	out, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Thinking *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens != 24576 {
		t.Fatalf("thinking = %+v body=%s", got.Thinking, out)
	}
	if got.MaxTokens < 24576+1024 {
		t.Fatalf("max_tokens = %d, want bump", got.MaxTokens)
	}
}

func TestThinkingAnthropicToOpenAI(t *testing.T) {
	in := []byte(`{
		"model":"claude",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hi"}],
		"thinking":{"type":"enabled","budget_tokens":8192}
	}`)
	req, err := anthropic.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking == nil || req.Thinking.Mode != ir.ThinkingBudget || req.Thinking.Budget != 8192 {
		t.Fatalf("thinking = %+v", req.Thinking)
	}
	req.Model = "gpt-5"
	out, err := openai.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.ReasoningEffort != "medium" {
		t.Fatalf("effort = %q", got.ReasoningEffort)
	}
}

func TestThinkingSuffixOverridesBody(t *testing.T) {
	in := []byte(`{
		"model":"default",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"low"
	}`)
	req, err := openai.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	applyUpstreamModel(req, "gpt-5(high)")
	if req.Model != "gpt-5" {
		t.Fatalf("model = %q", req.Model)
	}
	if req.Thinking == nil || req.Thinking.Level != "high" {
		t.Fatalf("thinking = %+v", req.Thinking)
	}
}

func TestThinkingNoneToAnthropic(t *testing.T) {
	req := &ir.Request{
		Model:    "claude-haiku-4.5",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingNone},
	}
	out, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Thinking *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking == nil || got.Thinking.Type != "disabled" {
		t.Fatalf("thinking = %+v", got.Thinking)
	}
}

func TestRewriteModelWithThinkingPassthrough(t *testing.T) {
	body := []byte(`{"model":"combo","messages":[],"foo":1,"reasoning_effort":"low"}`)
	out, err := rewriteModelWithThinking(body, "gpt-5", "oai-chat")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "gpt-5" || m["foo"] != float64(1) || m["reasoning_effort"] != "low" {
		t.Fatalf("no-suffix passthrough broken: %s", out)
	}

	out, err = rewriteModelWithThinking(body, "gpt-5(high)", "oai-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "gpt-5" || m["reasoning_effort"] != "high" || m["foo"] != float64(1) {
		t.Fatalf("suffix patch: %s", out)
	}
}

func TestThinkingMaxClampsOnOpenAIDialect(t *testing.T) {
	in := []byte(`{
		"model":"default",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	req, err := openai.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	applyUpstreamModel(req, "gpt-5(max)")
	if req.Model != "gpt-5" {
		t.Fatalf("model = %q", req.Model)
	}
	if req.Thinking == nil || req.Thinking.Level != "max" {
		t.Fatalf("thinking = %+v", req.Thinking)
	}
	out, err := openai.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	// OpenAI dialect clamps max -> xhigh unless model capability permits max.
	if got.ReasoningEffort != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh", got.ReasoningEffort)
	}
}

func TestThinkingMaxOnDeepSeekDialect(t *testing.T) {
	body := []byte(`{"model":"combo","messages":[],"foo":1}`)
	out, err := thinking.FinalizeBody(body, "deepseek-v4-flash-free(max)", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningDeepSeek)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "deepseek-v4-flash-free" {
		t.Fatalf("model = %v", m["model"])
	}
	if m["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %v, want max", m["reasoning_effort"])
	}
	th, _ := m["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Fatalf("thinking = %v", th)
	}
}

func TestThinkingClaudeAdaptivePreservesMax(t *testing.T) {
	req := &ir.Request{
		Model:    "claude-opus-4-7",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "max"},
	}
	out, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.OutputConfig == nil || got.OutputConfig.Effort != "max" {
		t.Fatalf("output_config = %+v, want max", got.OutputConfig)
	}
}

func TestThinkingClaudeCodeHaikuBudget(t *testing.T) {
	req := &ir.Request{
		Model:    "claude-haiku-4.5",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
	}
	out, err := anthropic.EncodeRequestClaudeCode(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Thinking *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens != 24576 {
		t.Fatalf("haiku claude-code should be budget: %+v body=%s", got.Thinking, out)
	}
}

func TestFinalizeBodyFailoverDialectIsolation(t *testing.T) {
	// Body with qwen fields; when failing over to openai dialect only openai fields remain.
	body := []byte(`{"model":"combo","messages":[],"enable_thinking":true,"thinking_budget":4096}`)
	qwenOut, err := thinking.FinalizeBody(body, "qwen3(high)", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningQwen)
	if err != nil {
		t.Fatal(err)
	}
	var qm map[string]any
	if err := json.Unmarshal(qwenOut, &qm); err != nil {
		t.Fatal(err)
	}
	if qm["enable_thinking"] != true {
		t.Fatalf("qwen: %s", qwenOut)
	}

	// Same original body, OpenAI attempt — no leak of qwen fields.
	oaiOut, err := thinking.FinalizeBody(body, "gpt-5(high)", "oai-chat", domain.ProtocolOpenAI, domain.ReasoningOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	var om map[string]any
	if err := json.Unmarshal(oaiOut, &om); err != nil {
		t.Fatal(err)
	}
	if om["reasoning_effort"] != "high" {
		t.Fatalf("openai effort: %s", oaiOut)
	}
	if _, ok := om["enable_thinking"]; ok {
		t.Fatalf("qwen field leaked to openai attempt: %s", oaiOut)
	}
	if _, ok := om["thinking_budget"]; ok {
		t.Fatalf("qwen budget leaked: %s", oaiOut)
	}
}
