package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
)

// Responses request with instructions, a function_call and its output should
// translate into an Anthropic request with system, a tool_use block, and a
// tool_result block in a user message.
func TestResponsesToAnthropicRequest(t *testing.T) {
	in := []byte(`{
		"model":"default",
		"instructions":"be terse",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]
	}`)
	req, err := responses.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "be terse" {
		t.Errorf("system = %q", req.System)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Content[0].Type != "tool_use" || req.Messages[1].Content[0].ToolName != "get_weather" {
		t.Errorf("expected tool_use, got %+v", req.Messages[1])
	}
	if req.Messages[2].Content[0].Type != "tool_result" || req.Messages[2].Content[0].ToolUseID != "call_1" {
		t.Errorf("expected tool_result, got %+v", req.Messages[2])
	}

	out, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"system":"be terse"`) || !strings.Contains(string(out), `"tool_result"`) {
		t.Errorf("anthropic request missing fields: %s", out)
	}
}

func TestResponsesUnaryMatrix(t *testing.T) {
	cases := []struct {
		name     string
		backend  domain.Protocol
		wantText string
		wantPath string
	}{
		{"responses->openai", domain.ProtocolOpenAI, "hello from openai", "/chat/completions"},
		{"responses->anthropic", domain.ProtocolAnthropic, "hello from anthropic", "/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap capturedUpstream
			base, token := setup(t, tc.backend, &cap)
			resp, out := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
			}
			if cap.path != tc.wantPath || cap.model != "real-model" {
				t.Errorf("upstream path=%q model=%q", cap.path, cap.model)
			}
			if got := parseResponsesText(t, out); got != tc.wantText {
				t.Errorf("output text = %q, want %q", got, tc.wantText)
			}
		})
	}
}

func TestResponsesStreamText(t *testing.T) {
	base, token := setupStreaming(t, domain.ProtocolOpenAI, anthropicSSE)
	resp, body := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	text, completed, _, _ := collectResponsesStream(t, body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !completed {
		t.Error("missing response.completed")
	}
}

func TestResponsesStreamTool(t *testing.T) {
	base, token := setupStreaming(t, domain.ProtocolAnthropic, anthropicToolSSE)
	resp, body := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"weather?"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, completed, name, args := collectResponsesStream(t, body)
	if name != "get_weather" {
		t.Errorf("tool name = %q", name)
	}
	if args != `{"city":"paris"}` {
		t.Errorf("tool args = %q", args)
	}
	if !completed {
		t.Error("missing response.completed")
	}
}

func TestModelsList(t *testing.T) {
	var cap capturedUpstream
	base, token := setup(t, domain.ProtocolOpenAI, &cap)
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "default" {
		t.Errorf("models list = %s", out)
	}
}

func TestModelsAuthRequired(t *testing.T) {
	var cap capturedUpstream
	base, _ := setup(t, domain.ProtocolOpenAI, &cap)
	resp, err := http.DefaultClient.Get(base + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// --- helpers ---

func parseResponsesText(t *testing.T, body []byte) string {
	t.Helper()
	var r struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad responses body: %s", body)
	}
	for _, item := range r.Output {
		if item.Type == "message" {
			for _, c := range item.Content {
				if c.Type == "output_text" {
					return c.Text
				}
			}
		}
	}
	return ""
}

func collectResponsesStream(t *testing.T, body string) (text string, completed bool, toolName, toolArgs string) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var textBuf, argBuf strings.Builder
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Name {
		case "response.output_text.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			textBuf.WriteString(d.Delta)
		case "response.output_item.added":
			var d struct {
				Item struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"item"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			if d.Item.Type == "function_call" && d.Item.Name != "" {
				toolName = d.Item.Name
			}
		case "response.function_call_arguments.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			argBuf.WriteString(d.Delta)
		case "response.completed":
			completed = true
		}
	}
	return textBuf.String(), completed, toolName, argBuf.String()
}
