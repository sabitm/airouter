package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

func completionIngresses(stream bool) []betaPolicyIngress {
	extra := ""
	if stream {
		extra = `,"stream":true`
	}
	return []betaPolicyIngress{
		{"chat", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hi"}]` + extra + `}`},
		{"responses", "/v1/responses", `{"model":"default","input":"hi"` + extra + `}`},
		{"messages", "/v1/messages", `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":"hi"}]` + extra + `}`},
	}
}

func completionFrame(name, data string) string {
	return finding9Frame(name, data)
}

func completionSSE(frames ...string) string {
	return finding9SSE(frames...)
}

func completionTextSSE(text, reason string, stop bool) string {
	frames := []string{
		completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_completion","type":"message","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":3}}}`),
		completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`),
		completionFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		completionFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+reason+`"},"usage":{"output_tokens":2}}`),
	}
	if stop {
		frames = append(frames, completionFrame("message_stop", `{"type":"message_stop"}`))
	}
	return completionSSE(frames...)
}

func completionToolSSE(closed bool) string {
	frames := []string{
		completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_tool","type":"message","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":3}}}`),
		completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`),
		completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`),
		completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ty\":\"SF\"}"}}`),
	}
	if closed {
		frames = append(frames,
			completionFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			completionFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`),
			completionFrame("message_stop", `{"type":"message_stop"}`),
		)
	}
	return completionSSE(frames...)
}

func completionHTTPResponse(req *http.Request, status int, contentType, body string, bodyReader io.ReadCloser) *http.Response {
	if bodyReader == nil {
		bodyReader = io.NopCloser(strings.NewReader(body))
	}
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        http.Header{"Content-Type": []string{contentType}},
		Body:          bodyReader,
		ContentLength: -1,
		Request:       req,
	}
}

func completionUnaryBody() string {
	return `{"id":"msg_unary","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"unary-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
}

func assertCompletionSuccess(t *testing.T, ingress, body string) {
	t.Helper()
	switch ingress {
	case "chat":
		if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "[DONE]") {
			t.Fatalf("chat success missing trailer: %s", body)
		}
	case "responses":
		if !strings.Contains(body, "response.completed") {
			t.Fatalf("responses success missing trailer: %s", body)
		}
	case "messages":
		if !strings.Contains(body, `"type":"message_stop"`) || !strings.Contains(body, `"stop_reason":"end_turn"`) {
			t.Fatalf("messages success missing trailer: %s", body)
		}
	default:
		t.Fatalf("unknown ingress %s", ingress)
	}
}

func assertNoCompletionTrailer(t *testing.T, ingress, body string) {
	t.Helper()
	switch ingress {
	case "chat":
		if strings.Contains(body, "[DONE]") || strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, `"finish_reason":"tool_calls"`) || strings.Contains(body, `"finish_reason":"length"`) {
			t.Fatalf("chat success trailer: %s", body)
		}
	case "responses":
		if strings.Contains(body, "response.completed") || strings.Contains(body, "response.incomplete") {
			t.Fatalf("responses success trailer: %s", body)
		}
	case "messages":
		if strings.Contains(body, `"type":"message_stop"`) || strings.Contains(body, "event: message_stop") {
			t.Fatalf("messages success trailer: %s", body)
		}
	}
	if !strings.Contains(body, "error") {
		t.Fatalf("missing terminal error: %s", body)
	}
}

func TestClaudeCodeStreamCompletionSuccessAndUnary(t *testing.T) {
	for _, stop := range []bool{true, false} {
		for _, ingress := range completionIngresses(true) {
			name := ingress.name + "/terminal-eof"
			if stop {
				name = ingress.name + "/message-stop"
			}
			t.Run(name, func(t *testing.T) {
				base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
					protocol: domain.ProtocolClaudeCode,
					baseURL:  "https://completion-primary.invalid/v1",
					model:    "claude-sonnet-4-6",
				}}, func(req *http.Request, _ []byte) *http.Response {
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionTextSSE("stream-ok", "end_turn", stop), nil)
				})
				resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status %d: %s", resp.StatusCode, out)
				}
				if !strings.Contains(string(out), "stream-ok") {
					t.Fatalf("text missing: %s", out)
				}
				assertCompletionSuccess(t, ingress.name, string(out))
			})
		}
	}
	for _, ingress := range completionIngresses(false) {
		t.Run(ingress.name+"/unary", func(t *testing.T) {
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  "https://completion-primary.invalid/v1",
				model:    "claude-sonnet-4-6",
			}}, func(req *http.Request, _ []byte) *http.Response {
				return completionHTTPResponse(req, http.StatusOK, "application/json", completionUnaryBody(), nil)
			})
			resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "unary-ok") {
				t.Fatalf("status %d: %s", resp.StatusCode, out)
			}
		})
	}
}

func TestClaudeCodeStreamCompletionPreCommitFailover(t *testing.T) {
	primaryBodies := map[string]string{
		"start-only":     completionSSE(completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_primary","model":"m","usage":{"input_tokens":1}}}`)),
		"lifecycle-only": completionSSE(completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_primary","model":"m","usage":{"input_tokens":1}}}`), completionFrame("ping", `{}`)),
		"usage-only":     completionSSE(completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_primary","model":"m","usage":{"input_tokens":1}}}`), completionFrame("message_delta", `{"type":"message_delta","delta":{},"usage":{"output_tokens":1}}`)),
		"provider-error": completionSSE(completionFrame("error", `{"type":"error","error":{"type":"api_error","message":"provider-failed"}}`)),
		"missing-start-index": completionSSE(
			completionFrame("message_start", `{"message":{"id":"msg_primary","model":"m","usage":{"input_tokens":1}}}`),
			completionFrame("content_block_start", `{"content_block":{"type":"text","text":"PAYLOAD_SECRET_4A"}}`),
		),
		"null-start-index": completionSSE(
			completionFrame("message_start", `{"message":{"id":"msg_primary","model":"m","usage":{"input_tokens":1}}}`),
			completionFrame("content_block_start", `{"index":null,"content_block":{"type":"text","text":"PAYLOAD_SECRET_4A"}}`),
		),
		"read-failure": "",
	}
	for name, primary := range primaryBodies {
		for _, ingress := range completionIngresses(true) {
			t.Run(name+"/"+ingress.name, func(t *testing.T) {
				var primaryHits, backupHits atomic.Int32
				base, token, client := newBetaPolicyProxy(t, []selectorTarget{
					{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-primary.invalid/v1", model: "claude-sonnet-4-6"},
					{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-backup.invalid/v1", model: "claude-sonnet-4-6"},
				}, func(req *http.Request, _ []byte) *http.Response {
					if req.URL.Host == "completion-backup.invalid" {
						backupHits.Add(1)
						return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionTextSSE("backup-ok", "end_turn", true), nil)
					}
					primaryHits.Add(1)
					if name == "read-failure" {
						return completionHTTPResponse(req, http.StatusOK, "text/event-stream", "", io.NopCloser(errReadCloser{err: errors.New("upstream read failed")}))
					}
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", primary, nil)
				})
				resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
				if resp.StatusCode != http.StatusOK || primaryHits.Load() != 1 || backupHits.Load() != 1 {
					t.Fatalf("status %d hits %d/%d: %s", resp.StatusCode, primaryHits.Load(), backupHits.Load(), out)
				}
				if !strings.Contains(string(out), "backup-ok") || strings.Contains(string(out), "msg_primary") || strings.Contains(string(out), "provider-failed") || strings.Contains(string(out), "PAYLOAD_SECRET_4A") {
					t.Fatalf("failover body: %s", out)
				}
				assertCompletionSuccess(t, ingress.name, string(out))
			})
		}
	}
}

func TestClaudeCodeStreamCompletionStartOnlyDoesNotCommit(t *testing.T) {
	for _, ingress := range completionIngresses(true) {
		t.Run(ingress.name, func(t *testing.T) {
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  "https://completion-primary.invalid/v1",
				model:    "claude-sonnet-4-6",
			}}, func(req *http.Request, _ []byte) *http.Response {
				return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionSSE(completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_primary","model":"m","usage":{"input_tokens":1}}}`)), nil)
			})
			resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("start-only committed success: %s", out)
			}
			if !strings.Contains(string(out), "stream ended before completion") {
				t.Fatalf("status %d: %s", resp.StatusCode, out)
			}
			assertNoCompletionTrailer(t, ingress.name, string(out))
		})
	}
}

func TestClaudeCodeStreamCompletionPostCommitNoRetry(t *testing.T) {
	cases := map[string]string{
		"text": completionSSE(
			completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_completion","model":"m","usage":{"input_tokens":1}}}`),
			completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"committed-text"}}`),
			completionFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		),
		"tool": completionToolSSE(false),
	}
	for name, primary := range cases {
		for _, ingress := range completionIngresses(true) {
			t.Run(name+"/"+ingress.name, func(t *testing.T) {
				var backupHits atomic.Int32
				base, token, client := newBetaPolicyProxy(t, []selectorTarget{
					{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-primary.invalid/v1", model: "claude-sonnet-4-6"},
					{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-backup.invalid/v1", model: "claude-sonnet-4-6"},
				}, func(req *http.Request, _ []byte) *http.Response {
					if req.URL.Host == "completion-backup.invalid" {
						backupHits.Add(1)
						return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionTextSSE("must-not-appear", "end_turn", true), nil)
					}
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", primary, nil)
				})
				resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
				body := string(out)
				if resp.StatusCode != http.StatusOK || backupHits.Load() != 0 {
					t.Fatalf("status %d backup %d: %s", resp.StatusCode, backupHits.Load(), body)
				}
				if strings.Contains(body, "must-not-appear") {
					t.Fatalf("retried after commit: %s", body)
				}
				if name == "text" && !strings.Contains(body, "committed-text") {
					t.Fatalf("committed text missing: %s", body)
				}
				if name == "tool" && !strings.Contains(body, "get_weather") {
					t.Fatalf("committed tool missing: %s", body)
				}
				assertNoCompletionTrailer(t, ingress.name, body)
			})
		}
	}
}

func TestClaudeCodeStreamCompletionStrictBlockLifecycleHTTP(t *testing.T) {
	frame := func(name, data string) string {
		return completionSSE(completionFrame(name, data))
	}
	text := completionTextSSE("committed-text", "end_turn", false)
	redacted := frame("content_block_start", `{"index":1,"content_block":{"type":"redacted_thinking","data":"opaque"}}`)
	redactedStop := frame("content_block_stop", `{"index":1}`)
	terminal := frame("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
	cases := []struct {
		name, body, wantErr string
	}{
		{"missing-stop-index", completionToolSSE(false) + frame("content_block_stop", `{"note":"PAYLOAD_SECRET_4A"}`) + terminal, "malformed content block index"},
		{"null-stop-index", completionToolSSE(false) + frame("content_block_stop", `{"index":null,"note":"PAYLOAD_SECRET_4A"}`) + terminal, "malformed content block index"},
		{"bare-stop", completionToolSSE(false) + frame("content_block_stop", `{}`) + terminal, "malformed content block index"},
		{"open-redacted", text + redacted + terminal, "stream ended with an open content block"},
		{"reused-redacted", text + redacted + redactedStop + frame("content_block_start", `{"index":1,"content_block":{"type":"redacted_thinking","data":"PAYLOAD_SECRET_4A"}}`) + terminal, "redacted thinking block restarted after stop"},
		{"closed-redacted", text + redacted + redactedStop + terminal, ""},
	}
	for _, kind := range []string{"start", "delta"} {
		for _, index := range []struct{ name, field string }{{"missing", ""}, {"null", `,"index":null`}} {
			data := `{"note":"PAYLOAD_SECRET_4A"` + index.field
			if kind == "start" {
				data += `,"content_block":{"type":"text","text":"PAYLOAD_SECRET_4A"}}`
			} else {
				data += `,"delta":{"type":"text_delta","text":"PAYLOAD_SECRET_4A"}}`
			}
			cases = append(cases, struct{ name, body, wantErr string }{
				index.name + "-" + kind + "-index", text + frame("content_block_"+kind, data) + terminal, "malformed content block index",
			})
		}
	}
	for _, tc := range cases {
		for _, ending := range []struct{ name, body string }{{"terminal-eof", ""}, {"message-stop", frame("message_stop", `{}`)}} {
			for _, ingress := range completionIngresses(true) {
				t.Run(tc.name+"/"+ending.name+"/"+ingress.name, func(t *testing.T) {
					var primaryHits, backupHits atomic.Int32
					base, token, client := newBetaPolicyProxy(t, []selectorTarget{
						{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-primary.invalid/v1", model: "claude-sonnet-4-6"},
						{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-backup.invalid/v1", model: "claude-sonnet-4-6"},
					}, func(req *http.Request, _ []byte) *http.Response {
						if req.URL.Host == "completion-backup.invalid" {
							backupHits.Add(1)
							return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionTextSSE("must-not-appear", "end_turn", true), nil)
						}
						primaryHits.Add(1)
						return completionHTTPResponse(req, http.StatusOK, "text/event-stream", tc.body+ending.body, nil)
					})
					resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
					body := string(out)
					if resp.StatusCode != http.StatusOK || primaryHits.Load() != 1 || backupHits.Load() != 0 {
						t.Fatalf("status %d hits %d/%d: %s", resp.StatusCode, primaryHits.Load(), backupHits.Load(), body)
					}
					if strings.Contains(body, "must-not-appear") || strings.Contains(body, "PAYLOAD_SECRET_4A") {
						t.Fatalf("retry or invalid payload emitted: %s", body)
					}
					if !strings.Contains(body, "committed-text") && !strings.Contains(body, "get_weather") {
						t.Fatalf("visible content missing: %s", body)
					}
					if tc.wantErr == "" {
						assertCompletionSuccess(t, ingress.name, body)
						if strings.Contains(body, `"error"`) {
							t.Fatalf("closed redacted block failed: %s", body)
						}
						return
					}
					if !strings.Contains(body, tc.wantErr) || !strings.Contains(body, "invalid_request_error") {
						t.Fatalf("terminal error missing: %s", body)
					}
					assertNoCompletionTrailer(t, ingress.name, body)
				})
			}
		}
	}
}

func TestClaudeCodeStreamCompletionReadAndProviderErrors(t *testing.T) {
	for _, ingress := range completionIngresses(true) {
		t.Run("read/"+ingress.name, func(t *testing.T) {
			var backupHits atomic.Int32
			prefix := completionSSE(
				completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_completion","model":"m","usage":{"input_tokens":1}}}`),
				completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"committed-text"}}`),
				completionFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
				completionFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
			)
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-primary.invalid/v1", model: "claude-sonnet-4-6"},
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-backup.invalid/v1", model: "claude-sonnet-4-6"},
			}, func(req *http.Request, _ []byte) *http.Response {
				if req.URL.Host == "completion-backup.invalid" {
					backupHits.Add(1)
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionTextSSE("must-not-appear", "end_turn", true), nil)
				}
				return completionHTTPResponse(req, http.StatusOK, "text/event-stream", "", io.NopCloser(&prefixThenError{rest: []byte(prefix), err: errors.New("upstream read failed")}))
			})
			resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
			body := string(out)
			if resp.StatusCode != http.StatusOK || backupHits.Load() != 0 || strings.Contains(body, "must-not-appear") {
				t.Fatalf("status %d backup %d: %s", resp.StatusCode, backupHits.Load(), body)
			}
			if !strings.Contains(body, "committed-text") || !strings.Contains(body, "upstream read failed") {
				t.Fatalf("read failure body: %s", body)
			}
			assertNoCompletionTrailer(t, ingress.name, body)
		})
		t.Run("provider/"+ingress.name, func(t *testing.T) {
			var backupHits atomic.Int32
			primary := completionSSE(
				completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_completion","model":"m","usage":{"input_tokens":1}}}`),
				completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"committed-text"}}`),
				completionFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
				completionFrame("error", `{"type":"error","error":{"type":"api_error","message":"provider-failed"}}`),
			)
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-primary.invalid/v1", model: "claude-sonnet-4-6"},
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-backup.invalid/v1", model: "claude-sonnet-4-6"},
			}, func(req *http.Request, _ []byte) *http.Response {
				if req.URL.Host == "completion-backup.invalid" {
					backupHits.Add(1)
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionTextSSE("must-not-appear", "end_turn", true), nil)
				}
				return completionHTTPResponse(req, http.StatusOK, "text/event-stream", primary, nil)
			})
			resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
			body := string(out)
			if resp.StatusCode != http.StatusOK || backupHits.Load() != 0 || strings.Contains(body, "must-not-appear") {
				t.Fatalf("status %d backup %d: %s", resp.StatusCode, backupHits.Load(), body)
			}
			if !strings.Contains(body, "committed-text") || !strings.Contains(body, "provider-failed") {
				t.Fatalf("provider error body: %s", body)
			}
			assertNoCompletionTrailer(t, ingress.name, body)
		})
	}
}

func TestClaudeCodeStreamCompletionCancelNoRetry(t *testing.T) {
	var backupHits atomic.Int32
	var saw atomic.Bool
	base, token, client := newBetaPolicyProxy(t, []selectorTarget{
		{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-primary.invalid/v1", model: "claude-sonnet-4-6"},
		{protocol: domain.ProtocolClaudeCode, baseURL: "https://completion-backup.invalid/v1", model: "claude-sonnet-4-6"},
	}, func(req *http.Request, _ []byte) *http.Response {
		if req.URL.Host == "completion-backup.invalid" {
			backupHits.Add(1)
			return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionTextSSE("must-not-appear", "end_turn", true), nil)
		}
		saw.Store(true)
		prefix := completionSSE(
			completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_completion","model":"m","usage":{"input_tokens":1}}}`),
			completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"committed-text"}}`),
		)
		return completionHTTPResponse(req, http.StatusOK, "text/event-stream", "", &cancelBlockReader{prefix: []byte(prefix), ctx: req.Context()})
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(completionIngresses(true)[0].body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	tmp := make([]byte, 256)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !bytes.Contains(buf.Bytes(), []byte("committed-text")) {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if readErr != nil {
			break
		}
	}
	if !bytes.Contains(buf.Bytes(), []byte("committed-text")) {
		t.Fatalf("committed text not visible before cancel: %s", buf.String())
	}
	cancel()
	_, _ = io.Copy(io.Discard, resp.Body)
	if backupHits.Load() != 0 || bytes.Contains(buf.Bytes(), []byte("must-not-appear")) || bytes.Contains(buf.Bytes(), []byte("[DONE]")) {
		t.Fatalf("cancel retried or completed: backup=%d body=%s", backupHits.Load(), buf.String())
	}
	if !saw.Load() {
		t.Fatal("primary was not called")
	}
}

func TestClaudeCodeStreamCompletionCollectionAndWiring(t *testing.T) {
	resp, err := collectStreamResponse(strings.NewReader(completionToolSSE(true)), claudeCodeCodec, nil, "claude-sonnet-4-6", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ToolName != "get_weather" || string(resp.Content[0].ToolInput) != `{"city":"SF"}` || resp.StopReason != ir.StopToolUse {
		t.Fatalf("collected = %+v", resp)
	}
	terminal := completionTextSSE("stream-ok", "max_tokens", false)
	resp, err = collectStreamResponse(strings.NewReader(terminal), claudeCodeCodec, nil, "claude-sonnet-4-6", nil)
	if err != nil || resp.StopReason != ir.StopMaxTokens || len(resp.Content) != 1 || resp.Content[0].Text != "stream-ok" {
		t.Fatalf("terminal collection err=%v resp=%+v", err, resp)
	}
	_, err = collectStreamResponse(strings.NewReader(completionToolSSE(false)), claudeCodeCodec, nil, "claude-sonnet-4-6", nil)
	if err == nil || strings.Contains(err.Error(), "SF") {
		t.Fatalf("incomplete collection err=%v", err)
	}
	var events []ir.StreamEvent
	err = claudeCodeCodec.decodeStream(strings.NewReader(completionSSE(completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_primary","model":"m"}}`))), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "stream ended before completion") {
		t.Fatalf("codec wiring err=%v events=%+v", err, events)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			t.Fatal("codec wiring emitted finish")
		}
	}
	events = nil
	err = anthropic.DecodeStream(strings.NewReader(completionSSE(completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_primary","model":"m"}}`))), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := false
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			finished = true
		}
	}
	if !finished {
		t.Fatal("default decoder no longer finishes a start-only stream")
	}
}

type errReadCloser struct {
	err error
}

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

type prefixThenError struct {
	rest []byte
	err  error
}

func (p *prefixThenError) Read(buf []byte) (int, error) {
	if len(p.rest) == 0 {
		return 0, p.err
	}
	n := copy(buf, p.rest)
	p.rest = p.rest[n:]
	return n, nil
}

func (p *prefixThenError) Close() error { return nil }

type cancelBlockReader struct {
	prefix []byte
	ctx    context.Context
	off    int
}

func (b *cancelBlockReader) Read(p []byte) (int, error) {
	if b.off < len(b.prefix) {
		n := copy(p, b.prefix[b.off:])
		b.off += n
		return n, nil
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(3 * time.Second):
		return 0, errors.New("completion cancel reader timed out")
	}
}

func (b *cancelBlockReader) Close() error { return nil }
