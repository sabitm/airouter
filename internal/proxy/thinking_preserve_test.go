package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/sse"
)

const signedHistory = `{"model":"default","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"chain","signature":"sig-exact"},{"type":"thinking","thinking":"","signature":"sig-empty"},{"type":"redacted_thinking","data":"opaque-data"},{"type":"tool_use","id":"toolu_1","name":"search","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`

func TestAnthropicToClaudeCodePreservesThinkingUnary(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_up","type":"message","role":"assistant","model":"real-model","content":[{"type":"thinking","thinking":"more","signature":"sig-resp"},{"type":"thinking","thinking":"","signature":"sig-empty-resp"},{"type":"redacted_thinking","data":"opaque-resp"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	t.Cleanup(upstream.Close)
	base, token := setupProtocol(t, domain.ProtocolClaudeCode, upstream.URL, "real-model")

	resp, out := post(t, base+"/v1/messages", token, signedHistory)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	assertThinkingWire(t, got, "sig-exact", "sig-empty", "opaque-data")
	assertThinkingWire(t, out, "sig-resp", "sig-empty-resp", "opaque-resp")
	if bytes.Contains(out, []byte(`"encrypted_content"`)) {
		t.Fatalf("response invented encrypted content: %s", out)
	}
}

func TestAnthropicToClaudeCodePreservesThinkingStream(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicThinkingSSE("sig-resp", "opaque-resp"))
	}))
	t.Cleanup(upstream.Close)
	base, token := setupProtocol(t, domain.ProtocolClaudeCode, upstream.URL, "real-model")
	body := strings.Replace(signedHistory, `"max_tokens":64`, `"max_tokens":64,"stream":true`, 1)
	resp, out := post(t, base+"/v1/messages", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	assertThinkingWire(t, got, "sig-exact", "sig-empty", "opaque-data")
	if !bytes.Contains(out, []byte(`"signature":"sig-resp"`)) || !bytes.Contains(out, []byte(`"data":"opaque-resp"`)) {
		t.Fatalf("client stream lost replay data: %s", out)
	}
	sig := bytes.Index(out, []byte(`"signature":"sig-resp"`))
	stop := bytes.Index(out, []byte(`"type":"content_block_stop"`))
	if sig < 0 || stop < sig {
		t.Fatalf("signature not before stop: %s", out)
	}
}

func TestAnthropicPassthroughPreservesSignedHistory(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_up","type":"message","role":"assistant","model":"real-model","content":[{"type":"thinking","thinking":"more","signature":"sig-resp"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	base, token := setupProtocol(t, domain.ProtocolAnthropic, upstream.URL, "real-model")
	resp, out := post(t, base+"/v1/messages", token, signedHistory)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	var req map[string]any
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatal(err)
	}
	if req["model"] != "real-model" {
		t.Fatalf("model = %v", req["model"])
	}
	raw, _ := json.Marshal(req["messages"])
	if !bytes.Contains(raw, []byte(`"signature":"sig-exact"`)) || !bytes.Contains(raw, []byte(`"data":"opaque-data"`)) || !bytes.Contains(raw, []byte(`"signature":"sig-empty"`)) {
		t.Fatalf("passthrough dropped history: %s", got)
	}
	if !bytes.Contains(out, []byte(`"signature":"sig-resp"`)) {
		t.Fatalf("passthrough response dropped signature: %s", out)
	}
}

func TestOpencodeMessagesPreservesTranslatedThinking(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != opencode.MessagesPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_up","type":"message","role":"assistant","model":"qwen3.6-plus","content":[{"type":"thinking","thinking":"","signature":"sig-up"},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)
	base, token := setupProtocol(t, domain.ProtocolOpencode, upstream.URL, "qwen3.6-plus")
	resp, out := post(t, base+"/v1/messages", token, signedHistory)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	assertThinkingWire(t, got, "sig-exact", "sig-empty", "opaque-data")
	if !bytes.Contains(out, []byte(`"signature":"sig-up"`)) || !bytes.Contains(out, []byte(`"thinking":""`)) {
		t.Fatalf("client lost signature-only block: %s", out)
	}
}

func TestThinkingFilterSkipsIncompatibleAndPreservesBackoff(t *testing.T) {
	oaiHits := atomic.Int64{}
	anthHits := atomic.Int64{}
	oai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oaiHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"signature":"sig-exact"`)) {
			t.Errorf("fallback lost signature: %s", body)
		}
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, anthropicThinkingSSE("sig-fallback", "opaque-fallback"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicUpstreamBody)
	}))
	t.Cleanup(oai.Close)
	t.Cleanup(anth.Close)
	base, token, px := setupTwo(t, domain.ProtocolOpenAI, oai.URL, domain.ProtocolAnthropic, anth.URL)
	combo, err := px.store.GetComboByName(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	var oaiID int64
	for _, tgt := range combo.Targets {
		if tgt.Provider != nil && tgt.Provider.Protocol == domain.ProtocolOpenAI {
			oaiID = tgt.ProviderID
		}
	}
	px.penalizeProvider(oaiID)
	before := backoffSkips(px, oaiID)

	resp, out := post(t, base+"/v1/messages", token, signedHistory)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if oaiHits.Load() != 0 || anthHits.Load() != 1 {
		t.Fatalf("hits oai=%d anth=%d", oaiHits.Load(), anthHits.Load())
	}
	if backoffSkips(px, oaiID) != before {
		t.Fatalf("excluded provider backoff changed %d -> %d", before, backoffSkips(px, oaiID))
	}

	streamBody := strings.Replace(signedHistory, `"max_tokens":64`, `"max_tokens":64,"stream":true`, 1)
	resp, out = post(t, base+"/v1/messages", token, streamBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", resp.StatusCode, out)
	}
	if oaiHits.Load() != 0 {
		t.Fatalf("stream contacted incompatible target")
	}
}

func TestThinkingAllIncompatibleIs400(t *testing.T) {
	hits := atomic.Int64{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(up.Close)
	base, token := setupProtocol(t, domain.ProtocolOpenAI, up.URL, "gpt-4o")
	resp, out := post(t, base+"/v1/messages", token, signedHistory)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if hits.Load() != 0 {
		t.Fatal("incompatible target contacted")
	}
	if !bytes.Contains(out, []byte("signed or redacted thinking")) || !bytes.Contains(out, []byte("invalid_request_error")) {
		t.Fatalf("error = %s", out)
	}
}

func TestNoTargetsStaysExistingError(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/messages", key.Token, signedHistory)
	if resp.StatusCode != http.StatusInternalServerError || !bytes.Contains(out, []byte("combo has no targets")) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
}

func TestCrossFormatDropsOpaqueOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_up","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"readable","signature":"sig-secret"},{"type":"redacted_thinking","data":"redacted-secret"},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`)
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	logger := observability.NewLogger(1, &logs)
	st := newTestStore(t)
	prov := &domain.Provider{Name: "a", BaseURL: upstream.URL, APIKey: "k", Protocol: domain.ProtocolAnthropic}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "m", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, logger).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !bytes.Contains(out, []byte(`"reasoning_content":"readable"`)) || !bytes.Contains(out, []byte(`"content":"answer"`)) {
		t.Fatalf("readable projection lost: %s", out)
	}
	if bytes.Contains(out, []byte("sig-secret")) || bytes.Contains(out, []byte("redacted-secret")) || bytes.Contains(out, []byte("encrypted_content")) {
		t.Fatalf("opaque data leaked: %s", out)
	}
	if strings.Count(logs.String(), "msg=thinking_opaque_omitted") != 1 {
		t.Fatalf("diagnostic count wrong: %s", logs.String())
	}
	if bytes.Contains(logs.Bytes(), []byte("sig-secret")) || bytes.Contains(logs.Bytes(), []byte("redacted-secret")) {
		t.Fatalf("log leaked opaque data: %s", logs.String())
	}
}

func TestUnsignedReasoningStillTranslates(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-x","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok","reasoning_content":"chain"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	base, token := setupProtocol(t, domain.ProtocolOpenAI, upstream.URL, "gpt-4o")
	body := `{"model":"default","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"unsigned"},{"type":"text","text":"prior"}]},{"role":"user","content":"next"}]}`
	resp, out := post(t, base+"/v1/messages", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !bytes.Contains(got, []byte("unsigned")) {
		t.Fatalf("unsigned reasoning dropped: %s", got)
	}
}

func TestTranslatedSinkOpaqueCommitAndBound(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := anthropicCodec.newStreamEncoder("m")
	sink := &translatedSink{w: rec, res: &reqResult{}, enc: enc, ingressID: anthropicCodec.id}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "msg_1"}); err != nil {
		t.Fatal(err)
	}
	if sink.committed {
		t.Fatal("message start committed")
	}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningStart, Index: 0}); err != nil {
		t.Fatal(err)
	}
	if sink.committed {
		t.Fatal("empty thinking start committed before signature")
	}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: 0, Signature: "sig"}); err != nil {
		t.Fatal(err)
	}
	if !sink.committed {
		t.Fatal("signature-only event did not commit anthropic ingress")
	}

	rec = httptest.NewRecorder()
	sink = &translatedSink{w: rec, res: &reqResult{}, enc: openaiCodec.newStreamEncoder("m"), ingressID: openaiCodec.id}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "msg_1"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningSignature, Signature: "sig"}); err != nil {
		t.Fatal(err)
	}
	if sink.committed {
		t.Fatal("openai ingress committed on opaque signature")
	}
	err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningSignature, Signature: strings.Repeat("x", translatedPendingMaxBytes+1)})
	if err != nil {
		t.Fatalf("projected signature should be discarded, not retained: %v", err)
	}
	if sink.committed || len(sink.pending) != 1 {
		t.Fatalf("projected signature changed pending state: committed=%v pending=%d", sink.committed, len(sink.pending))
	}
}

func TestEmptyIndexedThinkingDoesNotCommitBeforeSignature(t *testing.T) {
	rec := httptest.NewRecorder()
	res := &reqResult{}
	sink := &translatedSink{w: rec, res: res, enc: anthropicCodec.newStreamEncoder("m"), ingressID: anthropicCodec.id}
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m"},
		{Kind: ir.EventReasoningStart, Index: 0},
		{Kind: ir.EventReasoningDelta, Index: 0, Text: "", Indexed: true},
	} {
		if err := sink.handle(ev); err != nil {
			t.Fatal(err)
		}
	}
	if sink.committed {
		t.Fatal("empty indexed thinking committed before signature")
	}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: 0, Signature: "sig-exact"}); err != nil {
		t.Fatal(err)
	}
	if !sink.committed {
		t.Fatal("signature did not commit anthropic ingress")
	}

	rec = httptest.NewRecorder()
	sink = &translatedSink{w: rec, res: &reqResult{}, enc: openaiCodec.newStreamEncoder("m"), ingressID: openaiCodec.id}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningDelta, Index: 0, Text: "", Indexed: true}); err != nil {
		t.Fatal(err)
	}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: 0, Signature: "sig-secret"}); err != nil {
		t.Fatal(err)
	}
	if sink.committed || len(sink.pending) != 0 {
		t.Fatalf("openai retained or committed setup/opaque events: committed=%v pending=%d", sink.committed, len(sink.pending))
	}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn}); err != nil {
		t.Fatal(err)
	}
	if !sink.committed {
		t.Fatal("successful empty projection did not finish")
	}
	if strings.Contains(rec.Body.String(), "sig-secret") {
		t.Fatalf("openai stream leaked signature: %s", rec.Body.String())
	}
}

func TestProjectedOpaqueDoesNotFillPending(t *testing.T) {
	rec := httptest.NewRecorder()
	res := &reqResult{}
	var logs bytes.Buffer
	logger := observability.NewLogger(1, &logs)
	sink := &translatedSink{
		w: rec, res: res, enc: responsesCodec.newStreamEncoder("m"), ingressID: responsesCodec.id, logger: logger,
	}
	big := strings.Repeat("z", translatedPendingMaxBytes)
	for i := 0; i < 8; i++ {
		if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: i, Signature: big}); err != nil {
			t.Fatal(err)
		}
		if err := sink.handle(ir.StreamEvent{Kind: ir.EventRedactedReasoning, Index: i, Data: big}); err != nil {
			t.Fatal(err)
		}
	}
	if sink.committed || len(sink.pending) != 0 || sink.pendingBytes != 0 {
		t.Fatalf("opaque events retained: committed=%v n=%d bytes=%d", sink.committed, len(sink.pending), sink.pendingBytes)
	}
	if strings.Count(logs.String(), "msg=thinking_opaque_omitted") != 1 {
		t.Fatalf("diagnostic count = %s", logs.String())
	}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningStart, Index: 0, Text: "readable"}); err != nil {
		t.Fatal(err)
	}
	if !sink.committed || !strings.Contains(rec.Body.String(), "readable") {
		t.Fatalf("initial readable thinking did not project: committed=%v body=%s", sink.committed, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), big[:16]) {
		t.Fatalf("projected stream leaked opaque bytes")
	}
}

func TestUnsignedLifecycleDoesNotLogOpaqueOmission(t *testing.T) {
	for _, ingress := range []codec{openaiCodec, responsesCodec} {
		t.Run(ingress.id, func(t *testing.T) {
			rec := httptest.NewRecorder()
			res := &reqResult{}
			var logs bytes.Buffer
			logger := observability.NewLogger(1, &logs)
			sink := &translatedSink{w: rec, res: res, enc: ingress.newStreamEncoder("m"), ingressID: ingress.id, logger: logger}
			for _, ev := range []ir.StreamEvent{
				{Kind: ir.EventReasoningStart, Index: 0},
				{Kind: ir.EventReasoningDelta, Index: 0, Indexed: true},
				{Kind: ir.EventReasoningEnd, Index: 0},
				{Kind: ir.EventReasoningSignature, Index: 1},
			} {
				if err := sink.handle(ev); err != nil {
					t.Fatal(err)
				}
			}
			if res.opaqueOmitted || strings.Contains(logs.String(), "thinking_opaque_omitted") {
				t.Fatalf("empty lifecycle logged an omission: %s", logs.String())
			}
			if sink.committed || len(sink.pending) != 0 || sink.pendingBytes != 0 {
				t.Fatalf("empty lifecycle retained: committed=%v pending=%d", sink.committed, len(sink.pending))
			}
			if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: 1, Signature: "sig-later"}); err != nil {
				t.Fatal(err)
			}
			if !res.opaqueOmitted || strings.Count(logs.String(), "msg=thinking_opaque_omitted") != 1 {
				t.Fatalf("later omission missing: marker=%v logs=%s", res.opaqueOmitted, logs.String())
			}
			if strings.Contains(logs.String(), "sig-later") || sink.committed || sink.pendingBytes != 0 {
				t.Fatalf("later omission retained payload: committed=%v logs=%s", sink.committed, logs.String())
			}
		})
	}
}

func TestInitialSignatureOmissionAndCommitment(t *testing.T) {
	for _, ingress := range []codec{openaiCodec, responsesCodec} {
		t.Run(ingress.id, func(t *testing.T) {
			rec := httptest.NewRecorder()
			res := &reqResult{}
			var logs bytes.Buffer
			logger := observability.NewLogger(1, &logs)
			sink := &translatedSink{w: rec, res: res, enc: ingress.newStreamEncoder("m"), ingressID: ingress.id, logger: logger}
			if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningStart, Index: 0, Signature: "sig-empty-start"}); err != nil {
				t.Fatal(err)
			}
			if sink.committed || !res.opaqueOmitted || strings.Count(logs.String(), "msg=thinking_opaque_omitted") != 1 {
				t.Fatalf("empty signed start committed or was not logged: committed=%v logs=%s", sink.committed, logs.String())
			}
			if strings.Contains(logs.String(), "sig-empty-start") || len(sink.pending) != 0 {
				t.Fatalf("empty signed start retained: pending=%d logs=%s", len(sink.pending), logs.String())
			}

			rec = httptest.NewRecorder()
			res = &reqResult{}
			logs.Reset()
			sink = &translatedSink{w: rec, res: res, enc: ingress.newStreamEncoder("m"), ingressID: ingress.id, logger: logger}
			if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningStart, Index: 2, Text: "readable", Signature: "sig-with-text"}); err != nil {
				t.Fatal(err)
			}
			if !sink.committed || !strings.Contains(rec.Body.String(), "readable") || strings.Contains(rec.Body.String(), "sig-with-text") {
				t.Fatalf("readable start projection wrong: committed=%v body=%s", sink.committed, rec.Body.String())
			}
			if !res.opaqueOmitted || strings.Count(logs.String(), "msg=thinking_opaque_omitted") != 1 || strings.Contains(logs.String(), "sig-with-text") {
				t.Fatalf("initial signature omission wrong: %s", logs.String())
			}
		})
	}

	rec := httptest.NewRecorder()
	res := &reqResult{}
	var logs bytes.Buffer
	logger := observability.NewLogger(1, &logs)
	sink := &translatedSink{w: rec, res: res, enc: anthropicCodec.newStreamEncoder("m"), ingressID: anthropicCodec.id, logger: logger}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningStart, Index: 0, Signature: "sig-only"}); err != nil {
		t.Fatal(err)
	}
	if !sink.committed || res.opaqueOmitted || strings.Contains(logs.String(), "thinking_opaque_omitted") {
		t.Fatalf("anthropic initial signature did not commit cleanly: committed=%v logs=%s", sink.committed, logs.String())
	}
	if !strings.Contains(rec.Body.String(), "sig-only") {
		t.Fatalf("anthropic initial signature dropped: %s", rec.Body.String())
	}
}

func TestInitialSignatureFailoverBoundary(t *testing.T) {
	badHits := atomic.Int64{}
	goodHits := atomic.Int64{}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case bytes.Contains(body, []byte("empty-preamble")):
			_, _ = io.WriteString(w, anthropicEventSSE(
				"message_start", `{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":1}}}`,
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				"error", `{"type":"error","error":{"type":"api_error","message":"early"}}`,
			))
		case bytes.Contains(body, []byte("signed-start")):
			_, _ = io.WriteString(w, anthropicEventSSE(
				"message_start", `{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":1}}}`,
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"sig-initial"}}`,
				"error", `{"type":"error","error":{"type":"api_error","message":"after-signature"}}`,
			))
		default:
			_, _ = io.WriteString(w, anthropicEventSSE(
				"message_start", `{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":1}}}`,
				"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"sig-projected"}}`,
				"error", `{"type":"error","error":{"type":"api_error","message":"projected-early"}}`,
			))
		}
	}))
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicThinkingSSE("sig-good", "opaque-good"))
	}))
	t.Cleanup(bad.Close)
	t.Cleanup(good.Close)

	base, token, px := setupTwo(t, domain.ProtocolClaudeCode, bad.URL, domain.ProtocolClaudeCode, good.URL)
	setComboModels(t, px, "m", "m")
	pre := `{"model":"default","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"empty-preamble"}]}`
	resp, out := post(t, base+"/v1/messages", token, pre)
	if resp.StatusCode != http.StatusOK || badHits.Load() != 1 || goodHits.Load() != 1 || bytes.Contains(out, []byte("early")) || !bytes.Contains(out, []byte("sig-good")) {
		t.Fatalf("empty preamble failover status=%d hits=%d/%d body=%s", resp.StatusCode, badHits.Load(), goodHits.Load(), out)
	}

	clearFirstBackoff(t, px)
	badBefore := badHits.Load()
	goodBefore := goodHits.Load()
	signed := `{"model":"default","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"signed-start"}]}`
	resp, out = post(t, base+"/v1/messages", token, signed)
	if resp.StatusCode != http.StatusOK || badHits.Load() != badBefore+1 || goodHits.Load() != goodBefore {
		t.Fatalf("anthropic initial signature failed over status=%d hits=%d/%d body=%s", resp.StatusCode, badHits.Load(), goodHits.Load(), out)
	}
	if !bytes.Contains(out, []byte(`"signature":"sig-initial"`)) || !bytes.Contains(out, []byte("after-signature")) {
		t.Fatalf("anthropic post-signature body=%s", out)
	}

	chatBase, chatToken, chatPx := setupTwo(t, domain.ProtocolAnthropic, bad.URL, domain.ProtocolAnthropic, good.URL)
	setComboModels(t, chatPx, "m", "m")
	badBefore = badHits.Load()
	goodBefore = goodHits.Load()
	projected := `{"model":"default","stream":true,"messages":[{"role":"user","content":"projected-signature"}]}`
	resp, out = post(t, chatBase+"/v1/chat/completions", chatToken, projected)
	if resp.StatusCode != http.StatusOK || badHits.Load() != badBefore+1 || goodHits.Load() != goodBefore+1 {
		t.Fatalf("projected signature did not fail over status=%d hits=%d/%d body=%s", resp.StatusCode, badHits.Load(), goodHits.Load(), out)
	}
	if bytes.Contains(out, []byte("sig-projected")) || bytes.Contains(out, []byte("projected-early")) || !bytes.Contains(out, []byte("answer")) {
		t.Fatalf("projected failover body=%s", out)
	}

	respBase, respToken, respPx := setupTwo(t, domain.ProtocolAnthropic, bad.URL, domain.ProtocolAnthropic, good.URL)
	setComboModels(t, respPx, "m", "m")
	badBefore = badHits.Load()
	goodBefore = goodHits.Load()
	resp, out = post(t, respBase+"/v1/responses", respToken, `{"model":"default","stream":true,"input":"projected-signature"}`)
	if resp.StatusCode != http.StatusOK || badHits.Load() != badBefore+1 || goodHits.Load() != goodBefore+1 {
		t.Fatalf("responses projected signature did not fail over status=%d hits=%d/%d body=%s", resp.StatusCode, badHits.Load(), goodHits.Load(), out)
	}
	if bytes.Contains(out, []byte("sig-projected")) || !bytes.Contains(out, []byte("answer")) {
		t.Fatalf("responses projected failover body=%s", out)
	}
}

func TestMalformedThinkingOverlapFailover(t *testing.T) {
	malformed := anthropicEventSSE(
		"message_start", `{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":1}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":"overlap"}}`,
	)
	visible := anthropicEventSSE(
		"message_start", `{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":1}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"visible"}}`,
		"content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":"conflict"}}`,
	)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"anthropic", "/v1/messages"},
		{"openai", "/v1/chat/completions"},
		{"responses", "/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badHits := atomic.Int64{}
			goodHits := atomic.Int64{}
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				badHits.Add(1)
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if bytes.Contains(body, []byte("visible-overlap")) {
					_, _ = io.WriteString(w, visible)
					return
				}
				_, _ = io.WriteString(w, malformed)
			}))
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				goodHits.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, anthropicThinkingSSE("sig-good", "opaque-good"))
			}))
			t.Cleanup(bad.Close)
			t.Cleanup(good.Close)
			backend := domain.ProtocolAnthropic
			if tc.name == "anthropic" {
				backend = domain.ProtocolClaudeCode
			}
			base, token, px := setupTwo(t, backend, bad.URL, backend, good.URL)
			setComboModels(t, px, "m", "m")

			pre := streamRequest(tc.path, "hidden-overlap")
			resp, out := post(t, base+tc.path, token, pre)
			if resp.StatusCode != http.StatusOK || badHits.Load() != 1 || goodHits.Load() != 1 {
				t.Fatalf("pre-commit status=%d hits=%d/%d body=%s", resp.StatusCode, badHits.Load(), goodHits.Load(), out)
			}
			if bytes.Contains(out, []byte("overlap")) || bytes.Contains(out, []byte("thinking block started")) {
				t.Fatalf("pre-commit leaked malformed stream: %s", out)
			}

			clearFirstBackoff(t, px)
			badBefore := badHits.Load()
			goodBefore := goodHits.Load()
			resp, out = post(t, base+tc.path, token, streamRequest(tc.path, "visible-overlap"))
			if badHits.Load() != badBefore+1 || goodHits.Load() != goodBefore {
				t.Fatalf("visible overlap failed over hits=%d/%d body=%s", badHits.Load(), goodHits.Load(), out)
			}
			if !bytes.Contains(out, []byte("visible")) || bytes.Contains(out, []byte("conflict")) {
				t.Fatalf("post-commit body=%s", out)
			}
			if tc.name == "anthropic" && (bytes.Contains(out, []byte(`"type":"message_stop"`)) || !bytes.Contains(out, []byte("content block started while thinking block is open"))) {
				t.Fatalf("anthropic post-commit body=%s", out)
			}
		})
	}
}

func streamRequest(path, content string) string {
	switch path {
	case "/v1/messages":
		return `{"model":"default","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"` + content + `"}]}`
	case "/v1/responses":
		return `{"model":"default","stream":true,"input":"` + content + `"}`
	default:
		return `{"model":"default","stream":true,"messages":[{"role":"user","content":"` + content + `"}]}`
	}
}

func anthropicEventSSE(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString("event: ")
		b.WriteString(pairs[i])
		b.WriteString("\ndata: ")
		b.WriteString(pairs[i+1])
		b.WriteString("\n\n")
	}
	return b.String()
}

func setComboModels(t *testing.T, px *Proxy, first, second string) {
	t.Helper()
	combo, err := px.store.GetComboByName(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	combo.Targets[0].UpstreamModel = first
	combo.Targets[1].UpstreamModel = second
	if err := px.store.UpdateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
}

func clearFirstBackoff(t *testing.T, px *Proxy) {
	t.Helper()
	combo, err := px.store.GetComboByName(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	px.clearBackoff(combo.Targets[0].ProviderID)
}

func TestPendingLifecycleBoundsAllPayloads(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := &translatedSink{w: rec, res: &reqResult{}, enc: anthropicCodec.newStreamEncoder("m"), ingressID: anthropicCodec.id}
	err := sink.handle(ir.StreamEvent{Kind: ir.EventMessageStart, ID: strings.Repeat("i", translatedPendingMaxBytes+1), Model: "m"})
	if err == nil || sink.committed || len(sink.pending) != 0 {
		t.Fatalf("oversized identity retained err=%v pending=%d", err, len(sink.pending))
	}
	if strings.Contains(err.Error(), "iiii") {
		t.Fatalf("limit error leaked payload: %v", err)
	}
	for i := 0; i < translatedPendingMaxEvents; i++ {
		if err := sink.handle(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.handle(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "overflow"}); err == nil {
		t.Fatal("event count bound was not enforced")
	}
}

func TestTwoRequestToolContinuationPreservesReplay(t *testing.T) {
	var bodies [][]byte
	var oauth bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if strings.Contains(r.Header.Get("Authorization"), "sk-ant-oat") {
			oauth = true
		}
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"plan","signature":"sig-turn"},{"type":"thinking","thinking":"","signature":"sig-empty-turn"},{"type":"redacted_thinking","data":"redacted-turn"},{"type":"tool_use","id":"toolu_1","name":"search_ide","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"final"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	prov := &domain.Provider{
		Name: "cc", BaseURL: upstream.URL, Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "sk-ant-oat-test", RefreshToken: "rt", ExpiresAt: 0},
	}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(WithTraceInfo(r.Context(), &TraceInfo{})))
	}))
	t.Cleanup(ts.Close)

	first := `{"model":"default","max_tokens":64,"tools":[{"name":"search","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"find x"}]}`
	resp, out := post(t, ts.URL+"/v1/messages", key.Token, first)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d body=%s", resp.StatusCode, out)
	}
	if !bytes.Contains(out, []byte(`"signature":"sig-turn"`)) || !bytes.Contains(out, []byte(`"signature":"sig-empty-turn"`)) || !bytes.Contains(out, []byte(`"data":"redacted-turn"`)) || !bytes.Contains(out, []byte(`"name":"search"`)) {
		t.Fatalf("client replay data missing: %s", out)
	}
	if bytes.Contains(out, []byte("_ide")) {
		t.Fatalf("client saw cloaked tool name: %s", out)
	}

	second := `{"model":"default","max_tokens":64,"tools":[{"name":"search","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"find x"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"sig-turn"},{"type":"thinking","thinking":"","signature":"sig-empty-turn"},{"type":"redacted_thinking","data":"redacted-turn"},{"type":"tool_use","id":"toolu_1","name":"search","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`
	resp, out = post(t, ts.URL+"/v1/messages", key.Token, second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status=%d body=%s", resp.StatusCode, out)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream hits=%d", len(bodies))
	}
	if !oauth {
		t.Fatal("oauth token was not used")
	}
	assertThinkingWire(t, bodies[1], "sig-turn", "sig-empty-turn", "redacted-turn")
	if !bytes.Contains(bodies[1], []byte(`"name":"search_ide"`)) {
		t.Fatalf("continuation did not cloak tool: %s", bodies[1])
	}
	if !bytes.Contains(out, []byte(`"text":"final"`)) {
		t.Fatalf("final response = %s", out)
	}
}

func TestThinkingStreamFailoverBoundary(t *testing.T) {
	badHits := atomic.Int64{}
	goodHits := atomic.Int64{}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if bytes.Contains(body, []byte("post-commit")) {
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"model\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"secret\"}}\n\n")
			_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"boom\"}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"model\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"early\"}}\n\n")
	}))
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicThinkingSSE("sig-good", "opaque-good"))
	}))
	t.Cleanup(bad.Close)
	t.Cleanup(good.Close)
	base, token, px := setupTwo(t, domain.ProtocolClaudeCode, bad.URL, domain.ProtocolClaudeCode, good.URL)
	combo, err := px.store.GetComboByName(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	for i := range combo.Targets {
		combo.Targets[i].UpstreamModel = "claude"
	}
	if err := px.store.UpdateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}

	pre := `{"model":"default","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"pre-commit"}]}`
	resp, out := post(t, base+"/v1/messages", token, pre)
	if resp.StatusCode != http.StatusOK || badHits.Load() != 1 || goodHits.Load() != 1 {
		t.Fatalf("pre-commit status=%d hits=%d/%d body=%s", resp.StatusCode, badHits.Load(), goodHits.Load(), out)
	}
	if bytes.Contains(out, []byte("early")) || !bytes.Contains(out, []byte("sig-good")) {
		t.Fatalf("pre-commit failover body=%s", out)
	}

	combo, err = px.store.GetComboByName(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	px.clearBackoff(combo.Targets[0].ProviderID)
	postBody := `{"model":"default","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"post-commit"}]}`
	resp, out = post(t, base+"/v1/messages", token, postBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post status=%d body=%s", resp.StatusCode, out)
	}
	if goodHits.Load() != 1 {
		t.Fatalf("post-commit failed over, good hits=%d bad=%d body=%s", goodHits.Load(), badHits.Load(), out)
	}
	if !bytes.Contains(out, []byte(`"signature":"secret"`)) || !bytes.Contains(out, []byte("boom")) {
		t.Fatalf("post-commit body=%s", out)
	}
}

func TestStreamingToolContinuationReplaysSignedThinking(t *testing.T) {
	var bodies [][]byte
	var oauth bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if strings.Contains(r.Header.Get("Authorization"), "sk-ant-oat") {
			oauth = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			_, _ = io.WriteString(w, anthropicToolThinkingSSE())
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"model\":\"m\",\"usage\":{\"input_tokens\":4}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"final\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	prov := &domain.Provider{
		Name: "cc", BaseURL: upstream.URL, Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "sk-ant-oat-test", RefreshToken: "rt"},
	}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	first := `{"model":"default","max_tokens":64,"stream":true,"tools":[{"name":"search","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"find x"}]}`
	resp, out := post(t, ts.URL+"/v1/messages", key.Token, first)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d body=%s", resp.StatusCode, out)
	}
	assistant := assistantContentFromAnthropicSSE(t, out)
	if !bytes.Contains(assistant, []byte(`"thinking":"plan"`)) || !bytes.Contains(assistant, []byte(`"signature":"sig-turn"`)) || !bytes.Contains(assistant, []byte(`"data":"redacted-turn"`)) || !bytes.Contains(assistant, []byte(`"name":"search"`)) {
		t.Fatalf("reconstructed assistant = %s", assistant)
	}
	if bytes.Contains(assistant, []byte("_ide")) {
		t.Fatalf("client content kept cloak: %s", assistant)
	}

	second := `{"model":"default","max_tokens":64,"stream":true,"tools":[{"name":"search","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"find x"},{"role":"assistant","content":` + string(assistant) + `},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`
	resp, out = post(t, ts.URL+"/v1/messages", key.Token, second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status=%d body=%s", resp.StatusCode, out)
	}
	if len(bodies) != 2 || !oauth {
		t.Fatalf("hits=%d oauth=%v", len(bodies), oauth)
	}
	if !bytes.Contains(bodies[1], []byte(`"thinking":"plan"`)) || !bytes.Contains(bodies[1], []byte(`"signature":"sig-turn"`)) || !bytes.Contains(bodies[1], []byte(`"data":"redacted-turn"`)) {
		t.Fatalf("continuation lost thinking: %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"name":"search_ide"`)) {
		t.Fatalf("continuation did not cloak: %s", bodies[1])
	}
	plan := bytes.Index(bodies[1], []byte(`"type":"thinking"`))
	red := bytes.Index(bodies[1], []byte(`"type":"redacted_thinking"`))
	tool := bytes.Index(bodies[1], []byte(`"type":"tool_use"`))
	if plan < 0 || red < plan || tool < red {
		t.Fatalf("replay order wrong: %s", bodies[1])
	}
}

func TestCrossFormatStreamProjectionOmitsOpaque(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			hits := atomic.Int64{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, omittedThinkingThenAnswerSSE())
			}))
			t.Cleanup(upstream.Close)
			base, token := setupProtocol(t, domain.ProtocolAnthropic, upstream.URL, "m")
			body := `{"model":"default","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
			resp, out := post(t, base+path, token, body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if bytes.Contains(out, []byte("sig-secret")) || bytes.Contains(out, []byte("redacted-secret")) {
				t.Fatalf("opaque leaked: %s", out)
			}
			if !bytes.Contains(out, []byte("readable")) || !bytes.Contains(out, []byte("answer")) {
				t.Fatalf("readable projection lost: %s", out)
			}
			if hits.Load() != 1 {
				t.Fatalf("hits=%d", hits.Load())
			}
		})
	}
}

func TestOpaqueOmissionLoggedOnceAcrossFailover(t *testing.T) {
	var logs bytes.Buffer
	logger := observability.NewLogger(1, &logs)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, omittedThinkingThenErrorSSE())
	}))
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, omittedThinkingThenAnswerSSE())
	}))
	t.Cleanup(bad.Close)
	t.Cleanup(good.Close)
	st := newTestStore(t)
	a := &domain.Provider{Name: "a", BaseURL: bad.URL, APIKey: "k", Protocol: domain.ProtocolAnthropic}
	b := &domain.Provider{Name: "b", BaseURL: good.URL, APIKey: "k", Protocol: domain.ProtocolAnthropic}
	if err := st.CreateProvider(context.Background(), a); err != nil || st.CreateProvider(context.Background(), b) != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "m", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "m", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, logger).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if strings.Count(logs.String(), "msg=thinking_opaque_omitted") != 1 {
		t.Fatalf("diagnostics = %s", logs.String())
	}
	if bytes.Contains(logs.Bytes(), []byte("sig-secret")) || bytes.Contains(logs.Bytes(), []byte("redacted-secret")) {
		t.Fatalf("log leaked payload: %s", logs.String())
	}
}

func TestCombinedAttachmentAndThinkingFilter(t *testing.T) {
	hits := atomic.Int64{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(up.Close)
	base, token := setupProtocol(t, domain.ProtocolOpenAI, up.URL, "gpt-4o")
	body := `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testPNGB64 + `"}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig-exact"}]},{"role":"user","content":"next"}]}`
	resp, out := post(t, base+"/v1/messages", token, body)
	if resp.StatusCode != http.StatusBadRequest || hits.Load() != 0 {
		t.Fatalf("status=%d hits=%d body=%s", resp.StatusCode, hits.Load(), out)
	}
	if !bytes.Contains(out, []byte("signed or redacted thinking")) && !bytes.Contains(out, []byte("attachment not supported")) {
		t.Fatalf("missing filter diagnostic: %s", out)
	}
	streamBody := strings.Replace(body, `"max_tokens":32`, `"max_tokens":32,"stream":true`, 1)
	resp, out = post(t, base+"/v1/messages", token, streamBody)
	if resp.StatusCode != http.StatusBadRequest || hits.Load() != 0 {
		t.Fatalf("stream status=%d hits=%d body=%s", resp.StatusCode, hits.Load(), out)
	}
}

func TestAnthropicSSEPassthroughPreservesThinking(t *testing.T) {
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicThinkingSSE("sig-pass", "opaque-pass"))
	}))
	t.Cleanup(upstream.Close)
	base, token := setupProtocol(t, domain.ProtocolAnthropic, upstream.URL, "real-model")
	body := strings.Replace(signedHistory, `"max_tokens":64`, `"max_tokens":64,"stream":true`, 1)
	resp, out := post(t, base+"/v1/messages", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !bytes.Contains(got, []byte(`"signature":"sig-exact"`)) || !bytes.Contains(got, []byte(`"model":"real-model"`)) {
		t.Fatalf("passthrough request = %s", got)
	}
	if !bytes.Contains(out, []byte(`"signature":"sig-pass"`)) || !bytes.Contains(out, []byte(`"data":"opaque-pass"`)) {
		t.Fatalf("passthrough stream dropped payloads: %s", out)
	}
}

func TestClaudeCodeDecloakKeepsThinking(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"chain","signature":"sig"},{"type":"tool_use","id":"t1","name":"search_ide","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	resp, err := claudecode.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content[0].AnthropicSignature != "sig" || resp.Content[1].ToolName != "search" {
		t.Fatalf("decloak changed thinking or tool: %+v", resp.Content)
	}
}

func setupProtocol(t *testing.T, protocol domain.Protocol, baseURL, model string) (string, string) {
	t.Helper()
	st := newTestStore(t)
	prov := &domain.Provider{Name: "p", BaseURL: baseURL, APIKey: "k", Protocol: protocol}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: model, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}

func setupTwo(t *testing.T, p1 domain.Protocol, u1 string, p2 domain.Protocol, u2 string) (string, string, *Proxy) {
	t.Helper()
	st := newTestStore(t)
	a := &domain.Provider{Name: "a", BaseURL: u1, APIKey: "k", Protocol: p1}
	b := &domain.Provider{Name: "b", BaseURL: u2, APIKey: "k", Protocol: p2}
	if err := st.CreateProvider(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "gpt-4o", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "claude", Enabled: true},
	}}
	if err := st.CreateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token, px
}

func assertThinkingWire(t *testing.T, body []byte, signed, emptySig, redacted string) {
	t.Helper()
	if !bytes.Contains(body, []byte(`"signature":"`+signed+`"`)) || !bytes.Contains(body, []byte(`"signature":"`+emptySig+`"`)) || !bytes.Contains(body, []byte(`"data":"`+redacted+`"`)) || !bytes.Contains(body, []byte(`"thinking":""`)) {
		t.Fatalf("thinking wire missing values: %s", body)
	}
}

func anthropicToolThinkingSSE() string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":2}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"plan","signature":"sig-turn"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"redacted-turn"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"search_ide","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func omittedThinkingThenAnswerSSE() string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"readable"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-secret"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"redacted-secret"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func omittedThinkingThenErrorSSE() string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_bad","model":"m","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-secret"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"redacted-secret"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","error":{"type":"api_error","message":"early"}}` + "\n\n"
}

func assistantContentFromAnthropicSSE(t *testing.T, body []byte) []byte {
	t.Helper()
	reader := sse.NewReader(bytes.NewReader(body))
	type block struct {
		kind      string
		thinking  string
		signature string
		data      string
		id        string
		name      string
		input     string
		text      string
	}
	var blocks []*block
	open := map[int]*block{}
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if json.Unmarshal(ev.Data, &raw) != nil {
			continue
		}
		switch ev.Name {
		case "content_block_start":
			idx := int(raw["index"].(float64))
			cb, _ := raw["content_block"].(map[string]any)
			b := &block{kind: stringField(cb, "type")}
			b.thinking = stringField(cb, "thinking")
			b.signature = stringField(cb, "signature")
			b.data = stringField(cb, "data")
			b.id = stringField(cb, "id")
			b.name = stringField(cb, "name")
			open[idx] = b
			blocks = append(blocks, b)
		case "content_block_delta":
			idx := int(raw["index"].(float64))
			b := open[idx]
			delta, _ := raw["delta"].(map[string]any)
			switch stringField(delta, "type") {
			case "thinking_delta":
				b.thinking += stringField(delta, "thinking")
			case "signature_delta":
				b.signature += stringField(delta, "signature")
			case "text_delta":
				b.text += stringField(delta, "text")
			case "input_json_delta":
				b.input += stringField(delta, "partial_json")
			}
		}
	}
	var out []map[string]any
	for _, b := range blocks {
		switch b.kind {
		case "thinking":
			out = append(out, map[string]any{"type": "thinking", "thinking": b.thinking, "signature": b.signature})
		case "redacted_thinking":
			out = append(out, map[string]any{"type": "redacted_thinking", "data": b.data})
		case "text":
			out = append(out, map[string]any{"type": "text", "text": b.text})
		case "tool_use":
			input := json.RawMessage(b.input)
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": input})
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func anthropicThinkingSSE(signature, data string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_up","model":"m","usage":{"input_tokens":2}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"more"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"` + data + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}
