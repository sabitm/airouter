package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/sse"
)

func TestGrokChatLayoutUsesSourceIndex(t *testing.T) {
	cases := []struct {
		name   string
		sse    string
		layout []string
		replay bool
	}{
		{
			name:   "text before late reasoning",
			sse:    grokIndexedSSE(0, 1, 2, "leading", false),
			layout: []string{"text", "reasoning:rs_late", "tool:call_a"},
			replay: true,
		},
		{
			name:   "reasoning before text",
			sse:    grokIndexedSSE(1, 0, 2, "leading", false),
			layout: []string{"reasoning:rs_late", "text", "tool:call_a"},
		},
		{
			name:   "parallel tools keep source index",
			sse:    grokParallelSSE(),
			layout: []string{"text", "reasoning:rs_late", "tool:call_a", "tool:call_b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				cap := &chatCapture{sse: tc.sse}
				up := httptest.NewServer(cap)
				st := newTestStore(t)
				p := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
				if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
					t.Fatal(err)
				}
				key, err := st.NewAccessKey(t.Context(), "k")
				if err != nil {
					t.Fatal(err)
				}
				ts := startChatProxy(t, st, up.URL)
				body := `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`
				if stream {
					body = `{"model":"combo","stream":true,"messages":[{"role":"user","content":"seed"}]}`
				}
				resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, body)
				if resp.StatusCode != http.StatusOK || bytesContainsError(out) {
					t.Fatalf("stream=%v status %d %s", stream, resp.StatusCode, out)
				}
				if !strings.Contains(string(out), "leading") {
					t.Fatalf("stream=%v dropped text: %s", stream, out)
				}
				got := signedLayout(t, extractChatDetails(t, out))
				if strings.Join(got, ",") != strings.Join(tc.layout, ",") {
					t.Fatalf("stream=%v layout = %v, want %v", stream, got, tc.layout)
				}
				if tc.replay && !stream {
					resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", extractChatDetails(t, out), "leading", `[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]`, false))
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("replay %d %s", resp.StatusCode, out)
					}
					got := strings.Join(inputTypes(t, cap.lastBody()), ",")
					if !strings.Contains(got, "message:,reasoning:rs_late,function_call:fc_a") {
						t.Fatalf("replay order = %s", got)
					}
				}
				up.Close()
			}
		})
	}
}

func TestGrokChatOmitsContradictoryOrder(t *testing.T) {
	frames := []string{
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_silent","status":"completed","encrypted_content":"cipher-silent","summary":[]}}`,
		`{"type":"response.completed","response":{"id":"resp_bad","status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}"},{"type":"reasoning","id":"rs_silent","encrypted_content":"cipher-silent","summary":[]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	}
	cap := &chatCapture{sse: sseFrames(frames)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || !bytesContains(out, "[DONE]") || bytesContainsError(out) {
		t.Fatalf("status %d %s", resp.StatusCode, out)
	}
	if bytesContains(out, "reasoning_details") || bytesContains(out, grokbuild.ChatBridgeFormat) {
		t.Fatalf("contradictory order was signed: %s", out)
	}
}

func TestGrokChatLargeSnapshotsStayReadable(t *testing.T) {
	cipher := strings.Repeat("a", 4<<20)
	one, err := json.Marshal(cipher)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(id string, index int) string {
		return `{"type":"response.output_item.done","output_index":` + jsonInt(index) + `,"item":{"type":"reasoning","id":"` + id + `","status":"completed","encrypted_content":` + string(one) + `,"summary":[]}}`
	}
	for _, raw := range []string{frame("rs_a", 0), frame("rs_b", 1)} {
		if len(raw) >= sse.EventByteLimit() {
			t.Fatalf("fixture event %d exceeds SSE limit %d", len(raw), sse.EventByteLimit())
		}
	}
	sseBody := sseFrames([]string{
		frame("rs_a", 0),
		frame("rs_b", 1),
		`{"type":"response.output_text.delta","output_index":2,"delta":"readable"}`,
		`{"type":"response.completed","response":{"id":"resp_big","status":"completed","usage":{"input_tokens":3,"output_tokens":4}}}`,
	})
	st := openTestStore(t, false)
	p := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(&chatCapture{sse: sseBody})
	t.Cleanup(up.Close)
	ts := startChatProxy(t, st, up.URL)
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || bytesContainsError(out) || !bytesContains(out, "readable") || !bytesContains(out, "[DONE]") {
		t.Fatalf("no-key status %d %s", resp.StatusCode, trimForLog(out))
	}
	if bytesContains(out, "reasoning_details") || bytesContains(out, "pending stream lifecycle") {
		t.Fatalf("no-key retained or failed: %s", trimForLog(out))
	}
	if !bytesContains(out, `"completion_tokens":4`) {
		t.Fatalf("usage changed: %s", trimForLog(out))
	}

	testGrokChatWireBudget = 32
	t.Cleanup(func() { testGrokChatWireBudget = 0 })
	st = newTestStore(t)
	p = addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err = st.NewAccessKey(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	up2 := httptest.NewServer(&chatCapture{sse: grokChatSSE("readable", "cipher-live", "think", false)})
	t.Cleanup(up2.Close)
	ts = startChatProxy(t, st, up2.URL)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || bytesContainsError(out) || !bytesContains(out, "readable") || !bytesContains(out, "[DONE]") {
		t.Fatalf("budget status %d %s", resp.StatusCode, out)
	}
	if bytesContains(out, "reasoning_details") || bytesContains(out, grokbuild.ChatBridgeFormat) {
		t.Fatalf("budget miss still stamped: %s", out)
	}
	if !bytesContains(out, `"completion_tokens":5`) {
		t.Fatalf("budget miss changed usage: %s", out)
	}
}

func TestGrokChatUnaryCapDropsMarker(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("leading", "cipher-live", "think", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	testGrokChatUnaryCap = 1
	t.Cleanup(func() { testGrokChatUnaryCap = 0 })
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || bytesContainsError(out) || !bytesContains(out, "leading") {
		t.Fatalf("status %d %s", resp.StatusCode, out)
	}
	if bytesContains(out, "reasoning_details") || bytesContains(out, grokbuild.ChatBridgeFormat) {
		t.Fatalf("over-cap body was stamped: %s", out)
	}
}

func TestGrokChatNestedMarkerRejected(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	plain := &chatCapture{sse: "data: {\"choices\":[{\"delta\":{\"content\":\"plain\"}}]}\n\ndata: [DONE]\n\n"}
	upPlain := httptest.NewServer(plain)
	t.Cleanup(upPlain.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	chat := &domain.Provider{Name: "chat", BaseURL: upPlain.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(t.Context(), chat); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: chat.ID, UpstreamModel: "chat-model", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "plain", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: chat.ID, UpstreamModel: "chat-model", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{"Bearer access-conn-a": up.URL}, plain: upPlain.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	wrapped, err := json.Marshal([]any{map[string]json.RawMessage{"unexpected": details[0]}})
	if err != nil {
		t.Fatal(err)
	}
	nested := `{"model":"plain","messages":[{"role":"user","content":"see airouter-grok-build-v1"},{"role":"assistant","content":"from-a","reasoning_details":` + string(wrapped) + `},{"role":"user","content":"next"}]}`
	beforeGrok, beforePlain := cap.n, plain.n
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, nested)
	if resp.StatusCode != http.StatusBadRequest || cap.n != beforeGrok || plain.n != beforePlain {
		t.Fatalf("nested status %d grok %d plain %d body %s", resp.StatusCode, cap.n-beforeGrok, plain.n-beforePlain, out)
	}
	if bytesContains(out, "cipher-a") || bytesContains(out, "prompt_tokens") {
		t.Fatalf("nested error leaked state: %s", out)
	}
	quoted := `{"model":"plain","messages":[{"role":"user","content":"see airouter-grok-build-v1"},{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.text","text":"plain note"}]},{"role":"user","content":"next"}]}`
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, quoted)
	if resp.StatusCode != http.StatusOK || plain.n == beforePlain {
		t.Fatalf("quoted prompt rejected: %d %s", resp.StatusCode, out)
	}
	sent := plain.lastBody()
	if !strings.Contains(sent, "airouter-grok-build-v1") || !strings.Contains(sent, "plain note") {
		t.Fatalf("ordinary details lost: %s", sent)
	}
}

func TestGrokChatEscapedNestedMarkerRejected(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	plain := &chatCapture{sse: "data: {\"choices\":[{\"delta\":{\"content\":\"plain\"}}]}\n\ndata: [DONE]\n\n"}
	upPlain := httptest.NewServer(plain)
	t.Cleanup(upPlain.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	chat := &domain.Provider{Name: "chat", BaseURL: upPlain.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(t.Context(), chat); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: chat.ID, UpstreamModel: "chat-model", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "plain", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: chat.ID, UpstreamModel: "chat-model", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{"Bearer access-conn-a": up.URL}, plain: upPlain.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	wrapped, err := json.Marshal([]any{map[string]any{
		"type": "reasoning.encrypted", "format": "third-party-format", "unexpected": details[0],
	}})
	if err != nil {
		t.Fatal(err)
	}
	beforeGrok, beforePlain := cap.n, plain.n
	reject := func(model, body string) {
		t.Helper()
		resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, body)
		if resp.StatusCode != http.StatusBadRequest || cap.n != beforeGrok || plain.n != beforePlain {
			t.Fatalf("%s status %d grok %d plain %d body %s", model, resp.StatusCode, cap.n-beforeGrok, plain.n-beforePlain, out)
		}
		if bytesContains(out, "cipher-a") || bytesContains(out, "prompt_tokens") {
			t.Fatalf("%s leaked state: %s", model, out)
		}
	}
	nested := `{"model":"plain","messages":[{"role":"assistant","content":"from-a","reasoning_details":` + string(wrapped) + `},{"role":"user","content":"next"}]}`
	reject("passthrough", nested)
	nestedGrok := strings.Replace(nested, `"model":"plain"`, `"model":"combo"`, 1)
	reject("grok", nestedGrok)
	escaped := `{"model":"plain","messages":[{"role":"assistant","reasoning_details":{"type":"reasoning.summary","format":"\u0061irouter-grok-build-v1","summary":"test"}}]}`
	reject("escaped", escaped)
	old := `{"model":"combo","messages":[{"role":"assistant","content":"from-a","reasoning_details":` + string(wrapped) + `},{"role":"user","content":"next"},{"role":"assistant","content":"later"}]}`
	reject("older", old)
	foreign := `{"model":"plain","messages":[{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.encrypted","format":"xai-responses-v1","data":"foreign-cipher"}]},{"role":"user","content":"next"}]}`
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, foreign)
	if resp.StatusCode != http.StatusOK || plain.n == beforePlain {
		t.Fatalf("third-party rejected: %d %s", resp.StatusCode, out)
	}
	if !strings.Contains(plain.lastBody(), "xai-responses-v1") || !strings.Contains(plain.lastBody(), "foreign-cipher") {
		t.Fatalf("third-party details lost: %s", plain.lastBody())
	}
}

func TestGrokChatReasoningFrameBoundary(t *testing.T) {
	enc := openai.NewStreamEncoder("grok-4.7")
	details := []json.RawMessage{[]byte(`{"type":"reasoning.summary","format":"airouter-grok-build-v1","summary":"s"}`)}
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("writer")
	}
	if err := enc.EmitReasoningDetails(w, details); err != nil {
		t.Fatal(err)
	}
	raw := dataPayload(t, rec.Body.Bytes())
	line := len("data: ") + len(raw)
	t.Cleanup(func() { openai.SetTestReasoningFrameLimits(0, 0) })

	openai.SetTestReasoningFrameLimits(line, len(raw))
	accepted := httptest.NewRecorder()
	aw, ok := sse.NewWriter(accepted)
	if !ok {
		t.Fatal("writer")
	}
	if err := enc.EmitReasoningDetails(aw, details); err != nil {
		t.Fatal(err)
	}
	ev, err := sse.NewLimitedReader(bytes.NewReader(accepted.Body.Bytes()), line, len(raw)).Next()
	if err != nil || !bytes.Equal(ev.Data, raw) {
		t.Fatalf("accepted frame: %v %d", err, len(ev.Data))
	}

	openai.SetTestReasoningFrameLimits(line-1, len(raw))
	omitted := httptest.NewRecorder()
	ow, ok := sse.NewWriter(omitted)
	if !ok {
		t.Fatal("writer")
	}
	if err := enc.EmitReasoningDetails(ow, details); !errors.Is(err, openai.ErrReasoningFrameOmitted) {
		t.Fatalf("omitted frame: %v", err)
	}
	if omitted.Body.Len() != 0 {
		t.Fatalf("oversize chunk was written: %s", omitted.Body.String())
	}
	_, err = sse.NewLimitedReader(strings.NewReader("data: "+string(raw)+"\n\n"), line-1, len(raw)).Next()
	var sizeErr *sse.SizeLimitError
	if !errors.As(err, &sizeErr) || sizeErr.Scope != "line" {
		t.Fatalf("parser did not reject the omitted line: %v", err)
	}
}

func TestGrokChatOversizeFrameKeepsReadableStream(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("readable", "cipher-live", "think", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	openai.SetTestReasoningFrameLimits(128, 128)
	t.Cleanup(func() { openai.SetTestReasoningFrameLimits(0, 0) })
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || bytesContainsError(out) || !bytesContains(out, "readable") || !bytesContains(out, "[DONE]") {
		t.Fatalf("status %d %s", resp.StatusCode, out)
	}
	if bytesContains(out, "reasoning_details") || bytesContains(out, grokbuild.ChatBridgeFormat) {
		t.Fatalf("oversize frame was sent: %s", out)
	}
	if !bytesContains(out, `"completion_tokens":5`) || !bytesContains(out, `"finish_reason"`) {
		t.Fatalf("finish or usage missing: %s", out)
	}
	r := sse.NewReader(bytes.NewReader(out))
	for {
		ev, err := r.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if bytes.Contains(ev.Data, []byte("reasoning_details")) {
			t.Fatal("parser saw omitted chunk")
		}
	}
}

func dataPayload(t *testing.T, body []byte) []byte {
	t.Helper()
	const prefix = "data: "
	start := bytes.Index(body, []byte(prefix))
	if start < 0 {
		t.Fatalf("no data line in %s", body)
	}
	rest := body[start+len(prefix):]
	end := bytes.IndexByte(rest, '\n')
	if end < 0 {
		t.Fatalf("unterminated data line")
	}
	return rest[:end]
}

func TestPiDropsProjectedTextUsesECMAScriptTrim(t *testing.T) {
	kept := []string{"", "  hi\n", "\uFEFFhi\uFEFF", "\u0085", "\u180E", "\u200B"}
	for _, s := range kept {
		if piDropsProjectedText(s) {
			t.Fatalf("dropped %q", s)
		}
	}
	dropped := []string{"  \n", "\uFEFF", " \t\n\v\f\r\u00A0\u1680\u2000\u200A\u2028\u2029\u202F\u205F\u3000\uFEFF"}
	for _, s := range dropped {
		if !piDropsProjectedText(s) {
			t.Fatalf("kept %q", s)
		}
	}
	if strings.TrimSpace("\uFEFF") == "" || strings.TrimSpace("\u0085") != "" {
		t.Fatal("TrimSpace no longer differs from ECMAScript trim")
	}
}

func TestGrokChatOrderOmitsOnlyPiDroppedProjection(t *testing.T) {
	cases := []struct {
		name string
		segs []string
		omit bool
	}{
		{name: "joined whitespace", segs: []string{"  ", "\n"}, omit: true},
		{name: "whitespace then visible", segs: []string{"  ", "x"}, omit: false},
		{name: "visible then whitespace", segs: []string{"x", "  "}, omit: false},
		{name: "feff", segs: []string{"\uFEFF"}, omit: true},
		{name: "nel", segs: []string{"\u0085"}, omit: false},
		{name: "empty tool", omit: false},
		{name: "padded", segs: []string{"  hi\n"}, omit: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newTestGrokChatOrder()
			feedGrokChatProjection(o, tc.segs)
			want := strings.Join(tc.segs, "")
			details, ok := o.details()
			if o.visibleText.String() != want {
				t.Fatalf("projection changed: %q", o.visibleText.String())
			}
			if tc.omit {
				if ok || len(details) != 0 {
					t.Fatal("stamped dropped projection")
				}
				return
			}
			raw, err := json.Marshal(details)
			if err != nil || !ok || !bytes.Contains(raw, []byte("cipher-live")) {
				t.Fatal("marker missing")
			}
			var lens []int
			for _, seg := range tc.segs {
				if seg != "" {
					lens = append(lens, len(seg))
				}
			}
			if got := signedTextLens(t, details); strings.Join(ints(got), ",") != strings.Join(ints(lens), ",") {
				t.Fatalf("signed text lengths = %v, want %v", got, lens)
			}
		})
	}
}

func TestGrokChatWhitespaceProjectionSSEAndUnary(t *testing.T) {
	cap := &chatCapture{}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	tool := `fc_a|call_a|read|{}`
	cases := []struct {
		name string
		sse  string
		text string
		omit bool
	}{
		{name: "whitespace", sse: grokChatSSE("  \n", "cipher-live", "think", false, tool), text: "  \n", omit: true},
		{name: "empty", sse: grokChatSSE("", "cipher-live", "think", false, tool), text: "", omit: false},
		{name: "padded", sse: grokChatSSE("  hi\n", "cipher-live", "think", false, tool), text: "  hi\n", omit: false},
		{name: "feff", sse: grokChatSSE("\uFEFF", "cipher-live", "think", false, tool), text: "\uFEFF", omit: true},
		{name: "nel", sse: grokChatSSE("\u0085", "cipher-live", "think", false, tool), text: "\u0085", omit: false},
		{name: "feff-padded", sse: grokChatSSE("\uFEFFhi\uFEFF", "cipher-live", "think", false, tool), text: "\uFEFFhi\uFEFF", omit: false},
		{name: "split-visible", sse: grokSplitTextSSE("  ", "x"), text: "  x", omit: false},
		{name: "split-whitespace", sse: grokSplitTextSSE("  ", "\n"), text: "  \n", omit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				name := "unary"
				if stream {
					name = "sse"
				}
				t.Run(name, func(t *testing.T) {
					cap.mu.Lock()
					cap.sse = tc.sse
					cap.mu.Unlock()
					body := `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`
					if stream {
						body = `{"model":"combo","stream":true,"messages":[{"role":"user","content":"seed"}]}`
					}
					resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, body)
					if resp.StatusCode != http.StatusOK || bytesContainsError(out) {
						t.Fatalf("status %d %s", resp.StatusCode, out)
					}
					if stream && !bytesContains(out, "[DONE]") {
						t.Fatalf("missing done: %s", out)
					}
					if got := chatProjectedText(t, out); got != tc.text {
						t.Fatalf("text = %q, want %q body %s", got, tc.text, out)
					}
					if !bytesContains(out, `"name":"read"`) {
						t.Fatalf("tool missing: %s", out)
					}
					if !bytesContains(out, "think") {
						t.Fatalf("summary missing: %s", out)
					}
					if !bytesContains(out, `"prompt_tokens":4`) || !bytesContains(out, `"completion_tokens":5`) {
						t.Fatalf("usage missing: %s", out)
					}
					if !bytesContains(out, `"finish_reason":"tool_calls"`) {
						t.Fatalf("finish missing: %s", out)
					}
					marked := bytesContains(out, "reasoning_details") || bytesContains(out, "cipher-live") || bytesContains(out, grokbuild.ChatBridgeFormat)
					if tc.omit && marked {
						t.Fatalf("marker stamped: %s", out)
					}
					if !tc.omit && (!bytesContains(out, "cipher-live") || !bytesContains(out, grokbuild.ChatBridgeFormat)) {
						t.Fatalf("marker missing: %s", out)
					}
					if tc.name == "split-visible" && strings.Count(string(mustChatJSON(extractChatDetails(t, out))), `"k":"text"`) != 2 {
						t.Fatalf("split text was not signed as two segments: %s", out)
					}
				})
			}
		})
	}
}

func newTestGrokChatOrder() *grokChatOrder {
	return newGrokChatOrder(grokbuild.ChatBind{
		Scope: "caller", ComboID: 7, ProviderID: 9, Generation: "gen",
		Principal: "user", Model: "grok-4.7", RouteModel: "grok-4.7",
	}, func(payload []byte) ([]byte, bool) {
		sum := sha256.Sum256(payload)
		return sum[:], true
	}, nil, nil, "oai-chat")
}

func feedGrokChatProjection(o *grokChatOrder, segs []string) {
	cipher := "cipher-live"
	rr := &ir.ResponsesReasoning{
		ID: "rs_out", Status: "completed", EncryptedContent: &cipher,
		Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "think"}},
	}
	o.observe(ir.StreamEvent{
		Kind: ir.EventReasoningItemDone, ReasoningID: rr.ID, Reasoning: rr,
		ReasoningIndexSet: true, ReasoningIndex: 0,
	})
	for i, seg := range segs {
		ev := ir.StreamEvent{Kind: ir.EventTextDelta, Text: seg, SourceOutputSet: true, SourceOutputIndex: i + 1}
		o.observe(ev)
		o.noteVisible(ir.StreamEvent{Kind: ir.EventTextDelta, Text: seg})
	}
	o.observe(ir.StreamEvent{
		Kind: ir.EventToolCallStart, ToolID: "call_a", ToolName: "read", ResponsesItemID: "fc_a",
		SourceOutputSet: true, SourceOutputIndex: len(segs) + 2,
	})
	o.noteVisible(ir.StreamEvent{Kind: ir.EventToolCallStart, ToolID: "call_a", ToolName: "read", ResponsesItemID: "fc_a"})
}

func signedTextLens(t *testing.T, details []json.RawMessage) []int {
	t.Helper()
	var item struct {
		Layout []struct {
			K string `json:"k"`
			N int    `json:"n"`
		} `json:"layout"`
	}
	if err := json.Unmarshal(details[0], &item); err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, it := range item.Layout {
		if it.K == "text" {
			out = append(out, it.N)
		}
	}
	return out
}

func ints(v []int) []string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = jsonInt(n)
	}
	return out
}

func chatProjectedText(t *testing.T, body []byte) string {
	t.Helper()
	if bytes.Contains(body, []byte("data:")) {
		var b strings.Builder
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(line), &chunk) != nil || len(chunk.Choices) == 0 {
				continue
			}
			b.WriteString(chunk.Choices[0].Delta.Content)
		}
		return b.String()
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Choices) == 0 {
		t.Fatalf("content: %s", body)
	}
	return doc.Choices[0].Message.Content
}

func grokSplitTextSSE(a, b string) string {
	message := func(index int, id, text string) string {
		return `{"type":"response.output_item.done","output_index":` + jsonInt(index) + `,"item":{"id":"` + id + `","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":` + grokJSONString(text) + `}]}}`
	}
	return sseFrames([]string{
		message(0, "m1", a),
		message(1, "m2", b),
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":3,"item":{"type":"reasoning","id":"rs_out","status":"completed","encrypted_content":"cipher-live","summary":[{"type":"summary_text","text":"think"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_split","status":"completed","usage":{"input_tokens":4,"output_tokens":5}}}`,
	})
}

func TestGrokChatWireBudgetMatchesLimits(t *testing.T) {
	if grokbuild.ChatWireBudget != sse.EventByteLimit() {
		t.Fatalf("wire budget %d, SSE limit %d", grokbuild.ChatWireBudget, sse.EventByteLimit())
	}
	if int64(grokbuild.ChatWireBudget) > maxUnaryUpstreamResponseBytes {
		t.Fatalf("wire budget %d exceeds unary cap %d", grokbuild.ChatWireBudget, maxUnaryUpstreamResponseBytes)
	}
}

func grokIndexedSSE(textIndex, reasonIndex, toolIndex int, text string, withFinal bool) string {
	frames := []string{
		`{"type":"response.output_item.done","output_index":` + jsonInt(textIndex) + `,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + grokJSONString(text) + `}]}}`,
		`{"type":"response.output_item.added","output_index":` + jsonInt(toolIndex) + `,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":` + jsonInt(reasonIndex) + `,"item":{"type":"reasoning","id":"rs_late","status":"completed","encrypted_content":"cipher-late","summary":[]}}`,
	}
	completed := `{"type":"response.completed","response":{"id":"resp_idx","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`
	if withFinal {
		completed = `{"type":"response.completed","response":{"id":"resp_idx","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` + grokJSONString(text) + `}]},{"type":"reasoning","id":"rs_late","encrypted_content":"cipher-late","summary":[]},{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`
	}
	frames = append(frames, completed)
	return sseFrames(frames)
}

func grokParallelSSE() string {
	return sseFrames([]string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"leading"}]}}`,
		`{"type":"response.output_item.added","output_index":3,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"list","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_late","status":"completed","encrypted_content":"cipher-late","summary":[]}}`,
		`{"type":"response.completed","response":{"id":"resp_par","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	})
}

func sseFrames(frames []string) string {
	var b strings.Builder
	for _, frame := range frames {
		b.WriteString("data: " + frame + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func signedLayout(t *testing.T, details []json.RawMessage) []string {
	t.Helper()
	var item struct {
		Layout []struct {
			K  string `json:"k"`
			ID string `json:"id"`
		} `json:"layout"`
	}
	if err := json.Unmarshal(details[0], &item); err != nil {
		t.Fatalf("layout: %s", details[0])
	}
	var out []string
	for _, it := range item.Layout {
		if it.ID != "" {
			out = append(out, it.K+":"+it.ID)
			continue
		}
		out = append(out, it.K)
	}
	return out
}

func bytesContains(body []byte, needle string) bool {
	return strings.Contains(string(body), needle)
}

func bytesContainsError(body []byte) bool {
	return bytesContains(body, `"error"`)
}

func trimForLog(body []byte) string {
	if len(body) > 500 {
		return string(body[:500])
	}
	return string(body)
}
