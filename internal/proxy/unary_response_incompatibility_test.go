package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/ir"
)

const (
	unaryServerBlockContent = `{"type":"compaction","content":"server-secret","encrypted_content":"enc-secret"}`
	unaryBackupTextContent  = `{"type":"text","text":"backup-text"}`
	unaryPrimaryTextContent = `{"type":"text","text":"primary-text"},` + unaryServerBlockContent
)

func unaryAnthropicResponse(id, model, content string, in, out int) string {
	return fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","model":%q,"content":[%s],"stop_reason":"end_turn","usage":{"input_tokens":%d,"output_tokens":%d}}`, id, model, content, in, out)
}

func unaryOpenAIResponse(id, model, text string, in, out int) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, id, model, text, in, out, in+out)
}

func unaryRequestBody(path string) string {
	if path == "/v1/responses" {
		return `{"model":"default","input":"hi"}`
	}
	return `{"model":"default","messages":[{"role":"user","content":"hi"}]}`
}

type unaryFixtureTarget struct {
	protocol domain.Protocol
	model    string
	body     string
	stream   bool
}

func newUnaryFixture(t *testing.T, targets ...unaryFixtureTarget) (string, string, *Proxy, *selectorTransport, *selectorCapture, []*atomic.Int32, *bytes.Buffer) {
	t.Helper()
	tr := installSelectorTransport(t)
	captures := newSelectorCapture(len(targets) + 2)
	hits := make([]*atomic.Int32, len(targets))
	selectorTargets := make([]selectorTarget, 0, len(targets))
	for i, target := range targets {
		hit := new(atomic.Int32)
		hits[i] = hit
		body := target.body
		stream := target.stream
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit.Add(1)
			captures.record(captureClaudeSelector(r))
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
			} else {
				w.Header().Set("Content-Type", "application/json")
			}
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(upstream.Close)
		selectorTargets = append(selectorTargets, selectorTarget{
			protocol: target.protocol,
			baseURL:  upstream.URL,
			model:    target.model,
		})
	}
	base, token, px := newSelectorProxy(t, tr, selectorTargets...)
	var logs bytes.Buffer
	px.logger = observability.NewLogger(1, &logs)
	return base, token, px, tr, captures, hits, &logs
}

func assertUnaryResponse(t *testing.T, path string, body []byte, wantID, wantModel, wantText string, wantIn, wantOut int) {
	t.Helper()
	if path == "/v1/responses" {
		var doc struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("response: %v body=%s", err, body)
		}
		if doc.ID != wantID || doc.Model != wantModel || len(doc.Output) != 1 || len(doc.Output[0].Content) != 1 || doc.Output[0].Content[0].Text != wantText {
			t.Fatalf("response = %+v body=%s", doc, body)
		}
		if doc.Usage.InputTokens != wantIn || doc.Usage.OutputTokens != wantOut {
			t.Fatalf("usage = %d/%d want %d/%d", doc.Usage.InputTokens, doc.Usage.OutputTokens, wantIn, wantOut)
		}
		return
	}
	var doc struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("response: %v body=%s", err, body)
	}
	if doc.ID != wantID || doc.Model != wantModel || len(doc.Choices) != 1 || doc.Choices[0].Message.Content != wantText {
		t.Fatalf("response = %+v body=%s", doc, body)
	}
	if doc.Usage.PromptTokens != wantIn || doc.Usage.CompletionTokens != wantOut {
		t.Fatalf("usage = %d/%d want %d/%d", doc.Usage.PromptTokens, doc.Usage.CompletionTokens, wantIn, wantOut)
	}
}

func TestUnaryResponseIncompatibilityFailsOver(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		primaryProto domain.Protocol
		primary      string
		secrets      []string
		backupID     string
	}{
		{name: "chat-server-only", path: "/v1/chat/completions", primaryProto: domain.ProtocolAnthropic, primary: unaryServerBlockContent, secrets: []string{"server-secret", "enc-secret", "msg_primary"}, backupID: "msg_backup"},
		{name: "chat-mixed", path: "/v1/chat/completions", primaryProto: domain.ProtocolAnthropic, primary: unaryPrimaryTextContent, secrets: []string{"server-secret", "enc-secret", "primary-text", "msg_primary"}, backupID: "msg_backup"},
		{name: "responses-server-only", path: "/v1/responses", primaryProto: domain.ProtocolAnthropic, primary: unaryServerBlockContent, secrets: []string{"server-secret", "enc-secret", "msg_primary"}, backupID: "msg_backup"},
		{name: "responses-mixed", path: "/v1/responses", primaryProto: domain.ProtocolAnthropic, primary: unaryPrimaryTextContent, secrets: []string{"server-secret", "enc-secret", "primary-text", "msg_primary"}, backupID: "msg_backup"},
		{name: "chat-server-only-claude-code", path: "/v1/chat/completions", primaryProto: domain.ProtocolClaudeCode, primary: unaryServerBlockContent, secrets: []string{"server-secret", "enc-secret", "msg_primary"}, backupID: "msg_backup"},
		{name: "chat-mixed-claude-code", path: "/v1/chat/completions", primaryProto: domain.ProtocolClaudeCode, primary: unaryPrimaryTextContent, secrets: []string{"server-secret", "enc-secret", "primary-text", "msg_primary"}, backupID: "msg_backup"},
		{name: "responses-server-only-claude-code", path: "/v1/responses", primaryProto: domain.ProtocolClaudeCode, primary: unaryServerBlockContent, secrets: []string{"server-secret", "enc-secret", "msg_primary"}, backupID: "msg_backup"},
		{name: "responses-mixed-claude-code", path: "/v1/responses", primaryProto: domain.ProtocolClaudeCode, primary: unaryPrimaryTextContent, secrets: []string{"server-secret", "enc-secret", "primary-text", "msg_primary"}, backupID: "msg_backup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primaryBody := unaryAnthropicResponse("msg_primary", "primary-model", tc.primary, 11, 3)
			backupBody := unaryAnthropicResponse("msg_backup", "backup-model", unaryBackupTextContent, 7, 2)
			base, token, px, tr, captures, hits, logs := newUnaryFixture(t,
				unaryFixtureTarget{protocol: tc.primaryProto, model: "primary-model", body: primaryBody},
				unaryFixtureTarget{protocol: domain.ProtocolAnthropic, model: "backup-model", body: backupBody},
			)
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, unaryRequestBody(tc.path), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body=%s", resp.StatusCode, out)
			}
			assertUnaryResponse(t, tc.path, out, tc.backupID, "backup-model", "backup-text", 7, 2)
			for _, secret := range tc.secrets {
				if strings.Contains(string(out), secret) {
					t.Fatalf("client response leaked %q: %s", secret, out)
				}
			}
			if hits[0].Load() != 1 || hits[1].Load() != 1 {
				t.Fatalf("hits = %d/%d, want one attempt each", hits[0].Load(), hits[1].Load())
			}
			if wire := captures.next(t); wire.model != "primary-model" {
				t.Fatalf("primary wire model = %q", wire.model)
			}
			if wire := captures.next(t); wire.model != "backup-model" {
				t.Fatalf("backup wire model = %q", wire.model)
			}
			captures.assertNoMore(t)

			logsOut := logs.String()
			for _, secret := range tc.secrets {
				if strings.Contains(logsOut, secret) {
					t.Fatalf("terminal logs leaked %q: %s", secret, logsOut)
				}
			}
			requestLogs := waitForLogs(t, px.store, 2)
			failed := findLogByProvider(t, requestLogs, "p0")
			if failed.InputTokens != 11 || failed.OutputTokens != 3 || failed.ErrMsg == "" {
				t.Fatalf("failed row = %+v, want 11/3 with error", failed)
			}
			winner := findLogByProvider(t, requestLogs, "p1")
			if winner.InputTokens != 7 || winner.OutputTokens != 2 || winner.ErrMsg != "" || winner.UsageEstimated {
				t.Fatalf("winner row = %+v, want 7/2 without error or estimate", winner)
			}
		})
	}
}

func TestUnaryResponseIncompatibilityAllTargetsFail(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		targets int
	}{
		{name: "chat-one-target", path: "/v1/chat/completions", targets: 1},
		{name: "chat-two-targets", path: "/v1/chat/completions", targets: 2},
		{name: "responses-one-target", path: "/v1/responses", targets: 1},
		{name: "responses-two-targets", path: "/v1/responses", targets: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixtures := make([]unaryFixtureTarget, 0, tc.targets)
			fixtures = append(fixtures, unaryFixtureTarget{
				protocol: domain.ProtocolAnthropic,
				model:    "primary-model",
				body:     unaryAnthropicResponse("msg_primary", "primary-model", unaryPrimaryTextContent, 11, 3),
			})
			if tc.targets == 2 {
				fixtures = append(fixtures, unaryFixtureTarget{
					protocol: domain.ProtocolAnthropic,
					model:    "second-model",
					body:     unaryAnthropicResponse("msg_second", "second-model", unaryServerBlockContent, 21, 4),
				})
			}
			base, token, px, tr, _, hits, logs := newUnaryFixture(t, fixtures...)
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, unaryRequestBody(tc.path), nil)
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%s", resp.StatusCode, out)
			}
			assertIngressErrorEnvelope(t, tc.path, out, "api_error")
			var envelope struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out, &envelope); err != nil {
				t.Fatalf("error envelope: %v body=%s", err, out)
			}
			if envelope.Error.Message != "upstream response is not representable in the ingress format" {
				t.Fatalf("error = %q", envelope.Error.Message)
			}
			for _, secret := range []string{"primary-text", "server-secret", "enc-secret", "msg_primary"} {
				if strings.Contains(string(out), secret) || strings.Contains(logs.String(), secret) {
					t.Fatalf("final failure leaked %q: body=%s logs=%s", secret, out, logs.String())
				}
			}
			if hits[0].Load() != 1 || (tc.targets == 2 && hits[1].Load() != 1) {
				t.Fatalf("hits did not match targets")
			}
			requestLogs := waitForLogs(t, px.store, tc.targets)
			first := findLogByProvider(t, requestLogs, "p0")
			if first.InputTokens != 11 || first.OutputTokens != 3 || first.ErrMsg == "" {
				t.Fatalf("first row = %+v, want 11/3", first)
			}
			if tc.targets == 2 {
				last := findLogByProvider(t, requestLogs, "p1")
				if last.InputTokens != 21 || last.OutputTokens != 4 || last.ErrMsg == "" {
					t.Fatalf("last row = %+v, want 21/4", last)
				}
			}
		})
	}
}

func TestUnaryFinalFailureWithoutUsageDoesNotInheritEarlier(t *testing.T) {
	primaryBody := unaryAnthropicResponse("msg_primary", "primary-model", unaryPrimaryTextContent, 11, 3)
	base, token, px, tr, _, _, _ := newUnaryFixture(t,
		unaryFixtureTarget{protocol: domain.ProtocolAnthropic, model: "primary-model", body: primaryBody},
		unaryFixtureTarget{protocol: domain.ProtocolAnthropic, model: "second-model", body: `{invalid`},
	)
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/chat/completions", token, unaryRequestBody("/v1/chat/completions"), nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("all-targets failure became success: %s", out)
	}
	requestLogs := waitForLogs(t, px.store, 2)
	failed := findLogByProvider(t, requestLogs, "p0")
	if failed.InputTokens != 11 || failed.OutputTokens != 3 {
		t.Fatalf("first row = %+v, want 11/3", failed)
	}
	last := findLogByProvider(t, requestLogs, "p1")
	if last.InputTokens != 0 || last.OutputTokens != 0 || last.UsageEstimated {
		t.Fatalf("last row = %+v, want 0/0 without inherited usage", last)
	}
}

func TestUnaryResponseIncompatibilityWinnerUsageNotInherited(t *testing.T) {
	cases := []struct {
		name        string
		backupProto domain.Protocol
		backupBody  string
		backupModel string
		backupID    string
		backupText  string
	}{
		{
			name:        "translated-zero-usage",
			backupProto: domain.ProtocolAnthropic,
			backupBody:  unaryAnthropicResponse("msg_backup", "backup-model", unaryBackupTextContent, 0, 0),
			backupModel: "backup-model",
			backupID:    "msg_backup",
			backupText:  "backup-text",
		},
		{
			name:        "passthrough-zero-usage",
			backupProto: domain.ProtocolOpenAI,
			backupBody:  unaryOpenAIResponse("chatcmpl_backup", "backup-model", "backup-text", 0, 0),
			backupModel: "backup-model",
			backupID:    "chatcmpl_backup",
			backupText:  "backup-text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primaryBody := unaryAnthropicResponse("msg_primary", "primary-model", unaryPrimaryTextContent, 11, 3)
			base, token, px, tr, _, _, _ := newUnaryFixture(t,
				unaryFixtureTarget{protocol: domain.ProtocolAnthropic, model: "primary-model", body: primaryBody},
				unaryFixtureTarget{protocol: tc.backupProto, model: tc.backupModel, body: tc.backupBody},
			)
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/chat/completions", token, unaryRequestBody("/v1/chat/completions"), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body=%s", resp.StatusCode, out)
			}
			assertUnaryResponse(t, "/v1/chat/completions", out, tc.backupID, tc.backupModel, tc.backupText, 0, 0)
			requestLogs := waitForLogs(t, px.store, 2)
			failed := findLogByProvider(t, requestLogs, "p0")
			if failed.InputTokens != 11 || failed.OutputTokens != 3 {
				t.Fatalf("failed row = %+v, want 11/3", failed)
			}
			winner := findLogByProvider(t, requestLogs, "p1")
			if winner.InputTokens != 0 || winner.OutputTokens != 0 || winner.UsageEstimated || winner.ErrMsg != "" {
				t.Fatalf("winner row = %+v, want explicit 0/0 without estimate", winner)
			}
		})
	}
}

func TestStreamOnlyUnaryResponseIncompatibilityRetryable(t *testing.T) {
	const stream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_stream","model":"primary-model","usage":{"input_tokens":9,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"compaction","content":"","encrypted_content":"e1"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"compaction_delta","content":"server-secret","encrypted_content":"e2"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	for _, ingress := range []struct {
		name  string
		codec codec
	}{
		{name: "chat", codec: openaiCodec},
		{name: "responses", codec: responsesCodec},
	} {
		t.Run(ingress.name, func(t *testing.T) {
			_, _, px, _, _, hits, _ := newUnaryFixture(t, unaryFixtureTarget{
				protocol: domain.ProtocolClaudeCode,
				model:    "primary-model",
				body:     stream,
				stream:   true,
			})
			combo, err := px.store.GetComboByName(t.Context(), "default")
			if err != nil {
				t.Fatal(err)
			}
			provider := combo.Targets[0].Provider
			rec := httptest.NewRecorder()
			res := &reqResult{status: http.StatusOK}
			req := &ir.Request{Model: "primary-model", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}}}
			ar := px.serveStreamOnlyUnary(rec, t.Context(), res, ingress.codec, claudeCodeCodec, provider, "primary-model", []byte(`{"model":"primary-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`), req)
			if !ar.retry || ar.written || ar.status != http.StatusBadGateway {
				t.Fatalf("result = %+v, want retryable 502", ar)
			}
			if !ar.hasTok || ar.inTok != 9 || ar.outTok != 4 || ar.usageEstimated {
				t.Fatalf("attempt usage = %+v, want reported 9/4", ar)
			}
			if ar.errMsg != "upstream response is not representable in the ingress format" || strings.Contains(ar.logErr, "server-secret") {
				t.Fatalf("result error = %q / %q", ar.errMsg, ar.logErr)
			}
			if res.inTok != 0 || res.outTok != 0 || rec.Body.Len() != 0 {
				t.Fatalf("failed attempt mutated committed result: %+v body=%s", res, rec.Body.String())
			}
			if hits[0].Load() != 1 {
				t.Fatalf("upstream hits = %d, want 1", hits[0].Load())
			}
		})
	}
}

func encodeFaultingIngress(base codec) codec {
	ingress := base
	ingress.encodeResponse = func(*ir.Response) ([]byte, error) {
		return nil, errors.New("response-encoder-secret")
	}
	return ingress
}

func TestUnaryResponseEncodeFailureStaysTerminal(t *testing.T) {
	cases := []struct {
		name    string
		ingress codec
		path    string
		in      int
		out     int
	}{
		{name: "chat-reported", ingress: openaiCodec, path: "/v1/chat/completions", in: 7, out: 2},
		{name: "responses-reported", ingress: responsesCodec, path: "/v1/responses", in: 7, out: 2},
		{name: "chat-zero", ingress: openaiCodec, path: "/v1/chat/completions"},
		{name: "responses-zero", ingress: responsesCodec, path: "/v1/responses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backupBody := unaryAnthropicResponse("msg_backup", "backup-model", unaryBackupTextContent, tc.in, tc.out)
			_, _, px, _, _, hits, _ := newUnaryFixture(t, unaryFixtureTarget{
				protocol: domain.ProtocolAnthropic,
				model:    "backup-model",
				body:     backupBody,
			})
			combo, err := px.store.GetComboByName(t.Context(), "default")
			if err != nil {
				t.Fatal(err)
			}
			ingress := encodeFaultingIngress(tc.ingress)
			rec := httptest.NewRecorder()
			res := &reqResult{status: http.StatusOK}
			ar := px.serveTranslated(rec, t.Context(), res, ingress, anthropicCodec, combo.Targets[0].Provider, "backup-model", []byte(unaryRequestBody(tc.path)), &attachmentPrep{})
			if ar.retry || ar.written || ar.status != http.StatusInternalServerError {
				t.Fatalf("result = %+v, want terminal 500", ar)
			}
			if !ar.hasTok || ar.inTok != tc.in || ar.outTok != tc.out || ar.usageEstimated {
				t.Fatalf("attempt usage = %+v, want reported %d/%d", ar, tc.in, tc.out)
			}
			if ar.errMsg != "failed to encode response" || ar.logErr != "failed to encode response" {
				t.Fatalf("result error = %q / %q", ar.errMsg, ar.logErr)
			}
			if strings.Contains(ar.errMsg+ar.logErr, "response-encoder-secret") {
				t.Fatal("encoder detail leaked")
			}
			if res.inTok != 0 || res.outTok != 0 || res.usageEstimated || rec.Body.Len() != 0 {
				t.Fatalf("failed attempt mutated committed result before encoding: %+v body=%s", res, rec.Body.String())
			}
			if hits[0].Load() != 1 {
				t.Fatalf("upstream hits = %d, want 1 direct attempt", hits[0].Load())
			}
		})
	}
}

func TestUnaryResponseEncodeFailureFinalLogKeepsUsage(t *testing.T) {
	cases := []struct {
		name    string
		ingress codec
		path    string
		in      int
		out     int
	}{
		{name: "chat-reported", ingress: openaiCodec, path: "/v1/chat/completions", in: 11, out: 3},
		{name: "responses-reported", ingress: responsesCodec, path: "/v1/responses", in: 11, out: 3},
		{name: "chat-zero", ingress: openaiCodec, path: "/v1/chat/completions"},
		{name: "responses-zero", ingress: responsesCodec, path: "/v1/responses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primaryBody := unaryAnthropicResponse("msg_primary", "primary-model", unaryBackupTextContent, tc.in, tc.out)
			backupBody := unaryAnthropicResponse("msg_backup", "backup-model", unaryBackupTextContent, 7, 2)
			_, token, px, _, _, hits, _ := newUnaryFixture(t,
				unaryFixtureTarget{protocol: domain.ProtocolAnthropic, model: "primary-model", body: primaryBody},
				unaryFixtureTarget{protocol: domain.ProtocolAnthropic, model: "backup-model", body: backupBody},
			)
			ingress := encodeFaultingIngress(tc.ingress)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(unaryRequestBody(tc.path))).WithContext(t.Context())
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			px.serve(rec, req, ingress)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want terminal 500; body=%s", rec.Code, rec.Body.String())
			}
			assertIngressErrorEnvelope(t, tc.path, rec.Body.Bytes(), "api_error")
			if strings.Contains(rec.Body.String(), "response-encoder-secret") {
				t.Fatal("encoder detail leaked")
			}
			if hits[0].Load() != 1 || hits[1].Load() != 0 {
				t.Fatalf("hits = %d/%d, terminal encode error must not fail over", hits[0].Load(), hits[1].Load())
			}
			requestLogs := waitForLogs(t, px.store, 1)
			row := findLogByProvider(t, requestLogs, "p0")
			if row.Status != http.StatusInternalServerError || row.InputTokens != tc.in || row.OutputTokens != tc.out || row.UsageEstimated || row.ErrMsg == "" {
				t.Fatalf("final row = %+v, want 500 with reported %d/%d", row, tc.in, tc.out)
			}
			for _, l := range requestLogs {
				if l.Provider == "p1" {
					t.Fatalf("backup attempt was logged after terminal encode error: %+v", l)
				}
			}
		})
	}
}

func unaryCollectedTextSSE(in, out int) string {
	return "event: message_start\n" +
		fmt.Sprintf(`data: {"type":"message_start","message":{"id":"msg_stream","model":"primary-model","usage":{"input_tokens":%d,"output_tokens":0}}}`, in) + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"stream-text"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, out) + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func TestStreamOnlyUnaryEncodeFailureStaysTerminal(t *testing.T) {
	cases := []struct {
		name    string
		ingress codec
		in      int
		out     int
	}{
		{name: "chat-reported", ingress: openaiCodec, in: 9, out: 4},
		{name: "responses-reported", ingress: responsesCodec, in: 9, out: 4},
		{name: "chat-zero", ingress: openaiCodec},
		{name: "responses-zero", ingress: responsesCodec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, px, _, _, hits, _ := newUnaryFixture(t, unaryFixtureTarget{
				protocol: domain.ProtocolClaudeCode,
				model:    "primary-model",
				body:     unaryCollectedTextSSE(tc.in, tc.out),
				stream:   true,
			})
			combo, err := px.store.GetComboByName(t.Context(), "default")
			if err != nil {
				t.Fatal(err)
			}
			provider := combo.Targets[0].Provider
			ingress := encodeFaultingIngress(tc.ingress)
			rec := httptest.NewRecorder()
			res := &reqResult{status: http.StatusOK}
			req := &ir.Request{Model: "primary-model", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}}}
			ar := px.serveStreamOnlyUnary(rec, t.Context(), res, ingress, claudeCodeCodec, provider, "primary-model", []byte(`{"model":"primary-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`), req)
			if ar.retry || ar.written || ar.status != http.StatusInternalServerError {
				t.Fatalf("result = %+v, want terminal 500", ar)
			}
			if !ar.hasTok || ar.inTok != tc.in || ar.outTok != tc.out || ar.usageEstimated {
				t.Fatalf("attempt usage = %+v, want reported %d/%d", ar, tc.in, tc.out)
			}
			if ar.errMsg != "failed to encode response" || ar.logErr != "failed to encode response" || strings.Contains(ar.logErr, "response-encoder-secret") {
				t.Fatalf("result error = %q / %q", ar.errMsg, ar.logErr)
			}
			if res.inTok != 0 || res.outTok != 0 || res.usageEstimated || rec.Body.Len() != 0 {
				t.Fatalf("failed attempt mutated committed result before encoding: %+v body=%s", res, rec.Body.String())
			}
			if hits[0].Load() != 1 {
				t.Fatalf("upstream hits = %d, want 1 direct attempt", hits[0].Load())
			}
		})
	}
}

type cancelReadCloser struct {
	io.ReadCloser
	once   *sync.Once
	cancel context.CancelFunc
}

func (b *cancelReadCloser) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.cancel)
	}
	return n, err
}

func (b *cancelReadCloser) Close() error {
	b.once.Do(b.cancel)
	return b.ReadCloser.Close()
}

type cancelOnFirstUpstreamTransport struct {
	next   http.RoundTripper
	once   *sync.Once
	cancel context.CancelFunc
}

func (tr *cancelOnFirstUpstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := tr.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cancelReadCloser{ReadCloser: resp.Body, once: tr.once, cancel: tr.cancel}
	return resp, nil
}

func TestUnaryIncompatibilityCancellationStopsFailover(t *testing.T) {
	primaryBody := unaryAnthropicResponse("msg_primary", "primary-model", unaryPrimaryTextContent, 11, 3)
	backupBody := unaryAnthropicResponse("msg_backup", "backup-model", unaryBackupTextContent, 7, 2)
	_, token, px, tr, _, hits, _ := newUnaryFixture(t,
		unaryFixtureTarget{protocol: domain.ProtocolAnthropic, model: "primary-model", body: primaryBody},
		unaryFixtureTarget{protocol: domain.ProtocolAnthropic, model: "backup-model", body: backupBody},
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var once sync.Once
	px.client = &http.Client{Transport: &cancelOnFirstUpstreamTransport{next: tr, once: &once, cancel: cancel}}

	mux := http.NewServeMux()
	px.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(unaryRequestBody("/v1/chat/completions")))
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if hits[0].Load() != 1 || hits[1].Load() != 0 {
		t.Fatalf("hits = %d/%d, cancellation must not dispatch the backup", hits[0].Load(), hits[1].Load())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want cancellation 502; body=%s", rec.Code, rec.Body.String())
	}
	assertIngressErrorEnvelope(t, "/v1/chat/completions", rec.Body.Bytes(), "api_error")
	if strings.Contains(rec.Body.String(), "server-secret") || strings.Contains(rec.Body.String(), "primary-text") {
		t.Fatalf("cancellation response leaked primary content: %s", rec.Body.String())
	}
}
