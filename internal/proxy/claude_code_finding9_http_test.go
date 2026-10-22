package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/sse"
)

// finding9LateFragHTTP is the argument tail a malformed upstream sends after
// the tool block already stopped.
const finding9LateFragHTTP = "LATE_TAIL_BYTES"

const finding9ToolUnaryBody = `{"id":"msg_9","type":"message","role":"assistant","model":"claude-sonnet-4-6",` +
	`"content":[{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{"city":"SF"}}],` +
	`"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":4}}`

func finding9SSE(frames ...string) string {
	return strings.Join(frames, "\n\n") + "\n\n"
}

func finding9Frame(name, data string) string {
	return "event: " + name + "\ndata: " + data
}

// finding9ToolSSE is a valid fragmented ordinary tool_use stream.
func finding9ToolSSE() string {
	return finding9SSE(
		finding9Frame("message_start", `{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":3}}}`),
		finding9Frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`),
		finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`),
		finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ty\":\"SF\"}"}}`),
		finding9Frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		finding9Frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`),
		finding9Frame("message_stop", `{"type":"message_stop"}`),
	)
}

// finding9LateFragmentSSE closes the tool and then sends one more fragment.
func finding9LateFragmentSSE() string {
	return finding9SSE(
		finding9Frame("message_start", `{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":3}}}`),
		finding9Frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`),
		finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"SF\"}"}}`),
		finding9Frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"`+finding9LateFragHTTP+`"}}`),
		finding9Frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`),
		finding9Frame("message_stop", `{"type":"message_stop"}`),
	)
}

// finding9ReuseSSE commits text on index 0 and then starts a tool_use on the
// same spent index.
func finding9ReuseSSE() string {
	return finding9SSE(
		finding9Frame("message_start", `{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":3}}}`),
		finding9Frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`),
		finding9Frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		finding9Frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`),
		finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`),
		finding9Frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		finding9Frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`),
		finding9Frame("message_stop", `{"type":"message_stop"}`),
	)
}

func finding9Server(t *testing.T, captures *selectorCapture, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(captureClaudeSelector(r))
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type finding9ToolCall struct {
	id   string
	name string
	args string
}

// finding9ChatToolCall folds Chat SSE tool_calls deltas into one expected call.
func finding9ChatToolCall(t *testing.T, body string) (finding9ToolCall, string) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var call finding9ToolCall
	finish := ""
	saw := false
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(ev.Data) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(ev.Data, &chunk) != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Index != 0 {
					t.Fatalf("unexpected second tool index %d: %s", tc.Index, ev.Data)
				}
				saw = true
				if tc.ID != "" {
					call.id = tc.ID
				}
				if tc.Function.Name != "" {
					call.name = tc.Function.Name
				}
				call.args += tc.Function.Arguments
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	if !saw {
		t.Fatalf("no tool call chunks in chat stream: %s", body)
	}
	return call, finish
}

// finding9ResponsesToolCall folds Responses function_call events into one
// expected call.
func finding9ResponsesToolCall(t *testing.T, body string) finding9ToolCall {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var call finding9ToolCall
	saw := false
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Name {
		case "response.output_item.added", "response.output_item.done":
			var d struct {
				Item struct {
					Type string `json:"type"`
					Name string `json:"name"`
					ID   string `json:"id"`
				} `json:"item"`
			}
			if json.Unmarshal(ev.Data, &d) != nil || d.Item.Type != "function_call" {
				continue
			}
			saw = true
			call.name = d.Item.Name
			call.id = d.Item.ID
		case "response.function_call_arguments.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			call.args += d.Delta
		}
	}
	if !saw {
		t.Fatalf("no function_call item in responses stream: %s", body)
	}
	return call
}

func finding9AssertUsage(t *testing.T, path string, stream bool, body string) {
	t.Helper()
	wantIn, wantOut, wantTotal := 3, 4, 7
	if stream {
		var in, out, total int
		if path == "/v1/responses" {
			in, out, total = collectResponsesUsage(t, body)
		} else {
			in, out, total = collectOpenAIUsage(t, body)
		}
		if in != wantIn || out != wantOut || total != wantTotal {
			t.Fatalf("stream usage = in %d/out %d/total %d want %d/%d/%d; body=%s", in, out, total, wantIn, wantOut, wantTotal, body)
		}
		return
	}
	var doc selectorUnaryUsage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode unary usage: %v; body=%s", err, body)
	}
	if doc.Usage.PromptTokens != wantIn || doc.Usage.CompletionTokens != wantOut || doc.Usage.TotalTokens != wantTotal {
		if path == "/v1/responses" {
			if doc.Usage.InputTokens != wantIn || doc.Usage.OutputTokens != wantOut || doc.Usage.TotalTokens != wantTotal {
				t.Fatalf("responses unary usage = %+v want %d/%d/%d; body=%s", doc.Usage, wantIn, wantOut, wantTotal, body)
			}
			return
		}
		t.Fatalf("chat unary usage = %+v want %d/%d/%d; body=%s", doc.Usage, wantIn, wantOut, wantTotal, body)
	}
	if path == "/v1/responses" {
		t.Fatalf("responses unary carried chat prompt_tokens fields: %s", body)
	}
}

func TestFinding9ToolFragmentTranslationHTTP(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		stream bool
		body   string
	}{
		{
			name: "chat-stream", path: "/v1/chat/completions", stream: true,
			body: `{"model":"default","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-stream", path: "/v1/responses", stream: true,
			body: `{"model":"default","stream":true,"max_output_tokens":32,"input":"hi"}`,
		},
		{
			name: "chat-unary", path: "/v1/chat/completions",
			body: `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-unary", path: "/v1/responses",
			body: `{"model":"default","max_output_tokens":32,"input":"hi"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			contentType, upstreamBody := "application/json", finding9ToolUnaryBody
			if tc.stream {
				contentType, upstreamBody = "text/event-stream", finding9ToolSSE()
			}
			upstream := finding9Server(t, captures, contentType, upstreamBody)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6",
			})
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if tc.stream {
				if tc.path == "/v1/chat/completions" {
					call, finish := finding9ChatToolCall(t, string(out))
					if call.name != "get_weather" || call.args != `{"city":"SF"}` {
						t.Fatalf("chat tool call = %+v", call)
					}
					if finish != "tool_calls" {
						t.Fatalf("finish_reason = %q, want tool_calls", finish)
					}
				} else {
					call := finding9ResponsesToolCall(t, string(out))
					if call.name != "get_weather" || call.args != `{"city":"SF"}` {
						t.Fatalf("responses tool call = %+v", call)
					}
				}
			} else {
				var doc struct {
					Choices []struct {
						Message struct {
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
					Output []struct {
						Type      string `json:"type"`
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"output"`
				}
				if err := json.Unmarshal(out, &doc); err != nil {
					t.Fatalf("decode unary: %v; body=%s", err, out)
				}
				if tc.path == "/v1/chat/completions" {
					if len(doc.Choices) != 1 || doc.Choices[0].FinishReason != "tool_calls" {
						t.Fatalf("chat unary choices = %+v; body=%s", doc.Choices, out)
					}
					calls := doc.Choices[0].Message.ToolCalls
					if len(calls) != 1 || calls[0].Function.Name != "get_weather" || calls[0].Function.Arguments != `{"city":"SF"}` {
						t.Fatalf("chat unary tool calls = %+v; body=%s", calls, out)
					}
					if calls[0].ID != "toolu_9" {
						t.Fatalf("chat unary tool id = %q, want toolu_9", calls[0].ID)
					}
				} else {
					var found *struct {
						Type      string `json:"type"`
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}
					for i := range doc.Output {
						if doc.Output[i].Type == "function_call" {
							found = &doc.Output[i]
						}
					}
					if found == nil || found.Name != "get_weather" || found.Arguments != `{"city":"SF"}` {
						t.Fatalf("responses unary output = %+v; body=%s", doc.Output, out)
					}
				}
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			if wire.model != "claude-sonnet-4-6" {
				t.Fatalf("wire model = %q; body=%s", wire.model, wire.body)
			}
			finding9AssertUsage(t, tc.path, tc.stream, string(out))
		})
	}
}

// A fragment sent after the tool block stopped is rejected after commitment:
// the valid prefix stays visible, the offending bytes never reach the client,
// a terminal error frame ends the stream, and the backup receives no request.
func TestFinding9LateToolFragmentRejectedAfterCommitHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{"chat", "/v1/chat/completions", `{"model":"default","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"default","stream":true,"max_output_tokens":32,"input":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			primary := newSelectorCapture(2)
			backup := newSelectorCapture(2)
			upstream := finding9Server(t, primary, "text/event-stream", finding9LateFragmentSSE())
			backupSrv := finding9Server(t, backup, "text/event-stream", finding9ToolSSE())
			base, token, _ := newSelectorProxy(t, tr,
				selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: upstream.URL, model: "claude-sonnet-4-6"},
				selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: backupSrv.URL, model: "claude-sonnet-4-6"},
			)
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			// The relayed arguments are JSON-escaped on the wire; assert on the
			// unescaped-free substrings instead of the raw JSON text.
			if !strings.Contains(string(out), "get_weather") || !strings.Contains(string(out), "SF") {
				t.Fatalf("valid tool prefix lost: %s", out)
			}
			if strings.Contains(string(out), finding9LateFragHTTP) {
				t.Fatalf("late fragment relayed to client: %s", out)
			}
			if !strings.Contains(string(out), `"invalid_request_error"`) {
				t.Fatalf("terminal error frame missing: %s", out)
			}
			wire := primary.next(t)
			if wire.model != "claude-sonnet-4-6" {
				t.Fatalf("wire model = %q; body=%s", wire.model, wire.body)
			}
			primary.assertNoMore(t)
			backup.assertNoMore(t)
		})
	}
}

// A tool start on an index already spent by a stopped text block is rejected
// after commitment: earlier text stays visible, the reused tool identity never
// reaches the client, and the backup receives no request.
func TestFinding9ToolStartReuseRejectedAfterCommitHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{"chat", "/v1/chat/completions", `{"model":"default","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"default","stream":true,"max_output_tokens":32,"input":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			primary := newSelectorCapture(2)
			backup := newSelectorCapture(2)
			upstream := finding9Server(t, primary, "text/event-stream", finding9ReuseSSE())
			backupSrv := finding9Server(t, backup, "text/event-stream", finding9ToolSSE())
			base, token, _ := newSelectorProxy(t, tr,
				selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: upstream.URL, model: "claude-sonnet-4-6"},
				selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: backupSrv.URL, model: "claude-sonnet-4-6"},
			)
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if !strings.Contains(string(out), "hello") {
				t.Fatalf("committed text lost: %s", out)
			}
			if strings.Contains(string(out), "get_weather") {
				t.Fatalf("reused tool identity relayed to client: %s", out)
			}
			if !strings.Contains(string(out), `"invalid_request_error"`) {
				t.Fatalf("terminal error frame missing: %s", out)
			}
			wire := primary.next(t)
			if wire.model != "claude-sonnet-4-6" {
				t.Fatalf("wire model = %q; body=%s", wire.model, wire.body)
			}
			primary.assertNoMore(t)
			backup.assertNoMore(t)
		})
	}
}
