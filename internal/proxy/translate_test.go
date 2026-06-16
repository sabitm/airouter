package proxy

import (
	"encoding/json"
	"testing"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
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
