package proxy

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/cursor"
	"airouter/internal/store"
)

// cursorAgentCapture records what the fake AgentService received: the initial
// run frame and any mid-stream client replies.
type cursorAgentCapture struct {
	mu          sync.Mutex
	path        string
	auth        string
	contentType string
	accept      string
	runPayload  []byte // initial AgentClientMessage payload (unframed)
	clientRepl  [][]byte
	contextErr  string
}

func (c *cursorAgentCapture) recordContextError(msg string) {
	c.mu.Lock()
	c.contextErr = msg
	c.mu.Unlock()
}

// agentServerFrames builds an AgentService response: a KV get request, text
// deltas, and turn end. The server asserts a KV reply arrives before finishing.
func serveAgentRun(capture *cursorAgentCapture, kvWG *sync.WaitGroup) http.HandlerFunc {
	return serveAgentRunGate(capture, kvWG, false)
}

// serveAgentRunGate is serveAgentRun with an optional request-context gate.
// The context request is sent before text. Only a KV reply (top-level field 3)
// releases kvWG, and it is released once. A context reply is not a KV reply.
func serveAgentRunGate(capture *cursorAgentCapture, kvWG *sync.WaitGroup, askContext bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		capture.mu.Lock()
		capture.path = r.URL.Path
		capture.auth = r.Header.Get("Authorization")
		capture.contentType = r.Header.Get("Content-Type")
		capture.accept = r.Header.Get("Accept")
		capture.mu.Unlock()

		var (
			kvOnce      sync.Once
			kvReply     = make(chan struct{})
			contextRepl = make(chan []byte, 1)
		)
		// Read the initial run frame, then keep reading for duplex replies.
		// Request cancellation closes the body, so this read cannot outlive the
		// handler. Do not call t.Fatal from this goroutine.
		go func() {
			defer close(contextRepl)
			for {
				_, payload, err := readConnectFrameForTest(r.Body)
				if err != nil {
					return
				}
				capture.mu.Lock()
				if capture.runPayload == nil {
					capture.runPayload = append([]byte(nil), payload...)
					capture.mu.Unlock()
					continue
				}
				capture.clientRepl = append(capture.clientRepl, append([]byte(nil), payload...))
				capture.mu.Unlock()
				if kvWG != nil && isKVClientReply(payload) {
					kvOnce.Do(func() {
						kvWG.Done()
						close(kvReply)
					})
				}
				if isExecClientReply(payload) {
					select {
					case contextRepl <- append([]byte(nil), payload...):
					default:
					}
				}
			}
		}()

		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)

		if askContext {
			// ExecServerMessage{10: request_context_args{}, 19: span}. Numeric id
			// and exec_id are omitted, matching the captured upstream request.
			ex := teField(10, teBytes, nil)
			ex = append(ex, teField(19, teBytes, teSpanContext("c2c12bbfe63300373e14a63561373372", "bcb0d995fe395270"))...)
			_, _ = w.Write(wrapFrameForTest(teField(2, teBytes, ex)))
			fl.Flush()
			var reply []byte
			select {
			case reply = <-contextRepl:
			case <-r.Context().Done():
				return
			}
			if !hasEmptyRequestContext(reply) {
				capture.recordContextError("request_context missing")
				_, _ = w.Write(wrapTrailerForTest([]byte(`{"error":{"code":"internal","message":"Failed to get request context"}}`)))
				fl.Flush()
				return
			}
		}

		// Ask for a blob; the proxy must reply on the request body before the
		// turn can be considered healthy (the decoder replies immediately).
		// KvServerMessage{1: id=7, 2: get_blob_args{}} inside field 4.
		kv := teVarintField(1, 7)
		kv = append(kv, teField(2, teBytes, nil)...)
		_, _ = w.Write(wrapFrameForTest(teField(4, teBytes, kv)))
		fl.Flush()

		// If a KV gate was supplied, wait for the client's reply before sending
		// content. This proves the duplex write path works end to end.
		if kvWG != nil {
			select {
			case <-kvReply:
			case <-r.Context().Done():
				return
			}
		}

		writeTextDelta(w, "Hello ")
		writeTextDelta(w, "world")
		te := teVarintField(1, 12)
		te = append(te, teVarintField(2, 3)...)
		_, _ = w.Write(wrapFrameForTest(teField(1, teBytes, teField(14, teBytes, te))))
		fl.Flush()
		// End-stream trailer. Connect requires this JSON object after turn_ended.
		_, _ = w.Write(wrapTrailerForTest([]byte(`{}`)))
		fl.Flush()
	}
}

func teSpanContext(traceID, spanID string) []byte {
	out := teField(1, teBytes, []byte(traceID))
	out = append(out, teField(2, teBytes, []byte(spanID))...)
	return append(out, teVarintField(3, 0)...)
}

func isKVClientReply(payload []byte) bool {
	return hasTopLevelField(payload, 3)
}

func isExecClientReply(payload []byte) bool {
	return hasTopLevelField(payload, 2)
}

func hasTopLevelField(payload []byte, want int) bool {
	off := 0
	for off < len(payload) {
		tag, n, ok := teDecodeVarint(payload, off)
		if !ok {
			return false
		}
		off = n
		fieldNum := int(tag >> 3)
		wire := int(tag & 7)
		switch wire {
		case teVarint:
			_, n, ok = teDecodeVarint(payload, off)
			if !ok {
				return false
			}
			off = n
		case teBytes:
			ln, n, ok := teDecodeVarint(payload, off)
			if !ok || n+int(ln) > len(payload) {
				return false
			}
			off = n + int(ln)
		default:
			return false
		}
		if fieldNum == want {
			return true
		}
	}
	return false
}

func teDecodeVarint(b []byte, off int) (uint64, int, bool) {
	var result uint64
	var shift uint
	for {
		if off >= len(b) || shift > 63 {
			return 0, off, false
		}
		cur := b[off]
		off++
		result |= uint64(cur&0x7f) << shift
		if cur < 0x80 {
			return result, off, true
		}
		shift += 7
	}
}

// hasEmptyRequestContext accepts only the nested shape upstream requires:
// ExecClientMessage field 2 -> result field 10 -> success field 1 ->
// request_context field 1, present and empty. Tools field 7 must be absent.
func hasEmptyRequestContext(payload []byte) bool {
	exec, ok := teNestedMessage(payload, 2)
	if !ok {
		return false
	}
	result, ok := teNestedMessage(exec, 10)
	if !ok {
		return false
	}
	success, ok := teNestedMessage(result, 1)
	if !ok {
		return false
	}
	ctx, ok := teFieldBytes(success, 1)
	if !ok || len(ctx) != 0 {
		return false
	}
	if _, ok := teFieldBytes(success, 7); ok {
		return false
	}
	return true
}

func teNestedMessage(payload []byte, fieldNum int) ([]byte, bool) {
	return teFieldBytes(payload, fieldNum)
}

func teFieldBytes(payload []byte, want int) ([]byte, bool) {
	off := 0
	for off < len(payload) {
		tag, n, ok := teDecodeVarint(payload, off)
		if !ok {
			return nil, false
		}
		off = n
		fieldNum := int(tag >> 3)
		wire := int(tag & 7)
		switch wire {
		case teVarint:
			_, n, ok = teDecodeVarint(payload, off)
			if !ok {
				return nil, false
			}
			off = n
		case teBytes:
			ln, n, ok := teDecodeVarint(payload, off)
			if !ok || n+int(ln) > len(payload) {
				return nil, false
			}
			val := payload[n : n+int(ln)]
			off = n + int(ln)
			if fieldNum == want {
				return val, true
			}
		default:
			return nil, false
		}
	}
	return nil, false
}

func writeTextDelta(w io.Writer, text string) {
	// AgentServerMessage{1: InteractionUpdate{1: TextDelta{1: text}}}
	inner := teField(1, teBytes, []byte(text))
	_, _ = w.Write(wrapFrameForTest(teField(1, teBytes, teField(1, teBytes, inner))))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// Minimal protobuf test encoders.
const (
	teVarint = 0
	teBytes  = 2
)

func teVarintVal(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func teField(num, wire int, val []byte) []byte {
	tag := teVarintVal(uint64(num)<<3 | uint64(wire))
	if wire == 0 {
		return tag
	}
	out := append(tag, teVarintVal(uint64(len(val)))...)
	return append(out, val...)
}

func teVarintField(num int, v uint64) []byte {
	return append(teVarintVal(uint64(num)<<3), teVarintVal(v)...)
}

func setupCursorAgent(t *testing.T, capture *cursorAgentCapture, kvWG *sync.WaitGroup) (string, string, *store.Store) {
	t.Helper()
	return setupCursorAgentHandler(t, capture, serveAgentRun(capture, kvWG))
}

func setupCursorAgentContext(t *testing.T, capture *cursorAgentCapture, kvWG *sync.WaitGroup) (string, string, *store.Store) {
	t.Helper()
	return setupCursorAgentHandler(t, capture, serveAgentRunGate(capture, kvWG, true))
}

func setupCursorAgentHandler(t *testing.T, capture *cursorAgentCapture, handler http.Handler) (string, string, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	// HTTP/2 is required for the duplex test path: over HTTP/1.1 the Go
	// server blocks flushing response headers until the chunked request body
	// is drained, which deadlocks a bidi stream.
	upstream := httptest.NewUnstartedServer(handler)
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	prov := &domain.Provider{
		Name: "cursor", BaseURL: upstream.URL, Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthAPIKey, APIKey: "agent-tok",
		OAuthCreds: &domain.OAuthCreds{CursorAuth: true, MachineID: "m-1"},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "default", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p := New(st, nil)
	// The duplex upstream is the httptest TLS server (self-signed); route the
	// proxy's stream client through an h2-capable transport that trusts it.
	p.streamClient = &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // self-signed httptest cert
		TLSHandshakeTimeout: 10 * time.Second,
	}}
	p.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token, st
}

// TestCursorAgentStreamTranslate exercises openai/anthropic ingress to the
// Cursor AgentService backend: initial frame shape, mid-stream KV reply on the
// duplex request body, text deltas, usage, and identity headers.
func TestCursorAgentStreamTranslate(t *testing.T) {
	for _, tc := range []struct{ name, ingress string }{
		{"openai->cursor", "/v1/chat/completions"},
		{"anthropic->cursor", "/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &cursorAgentCapture{}
			var kvWG sync.WaitGroup
			kvWG.Add(1)
			base, token, _ := setupCursorAgent(t, capture, &kvWG)
			resp, body := postStream(t, base+tc.ingress, token, `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			text, finished := collectStreamText(t, tc.ingress, body)
			if text != "Hello world" {
				t.Errorf("text = %q", text)
			}
			if !finished {
				t.Error("stream did not signal completion")
			}

			capture.mu.Lock()
			defer capture.mu.Unlock()
			if capture.path != cursor.AgentRunPath {
				t.Errorf("upstream path = %q, want %s", capture.path, cursor.AgentRunPath)
			}
			if capture.auth != "Bearer agent-tok" {
				t.Errorf("auth = %q", capture.auth)
			}
			if capture.contentType != cursor.ConnectContentType {
				t.Errorf("content-type = %q", capture.contentType)
			}
			if capture.accept != cursor.StreamAccept {
				t.Errorf("accept = %q", capture.accept)
			}
			if len(capture.runPayload) == 0 {
				t.Fatal("no initial run frame captured")
			}
			if len(capture.clientRepl) == 0 {
				t.Fatal("no mid-stream client reply captured; duplex write path broken")
			}
		})
	}
}

// TestCursorAgentUnaryCollected verifies a non-streaming client request against
// the stream-only Cursor backend collects into a unary response with usage.
func TestCursorUnaryCollected(t *testing.T) {
	capture := &cursorAgentCapture{}
	var kvWG sync.WaitGroup
	kvWG.Add(1)
	base, token, _ := setupCursorAgent(t, capture, &kvWG)
	resp, body := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got struct {
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
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "Hello world" {
		t.Errorf("choices = %+v", got.Choices)
	}
	if got.Usage.PromptTokens != 12 || got.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v, want 12/3", got.Usage)
	}
}

// TestCursorAgentRequestContextGate sends the omitted-id context request before
// text. A missing nested request_context reproduces the captured Connect error
// and the ingress 502. A valid empty context lets the stream finish.
func TestCursorAgentRequestContextGate(t *testing.T) {
	for _, tc := range []struct {
		name, ingress, body string
		unary               bool
	}{
		{name: "openai stream", ingress: "/v1/chat/completions", body: `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "anthropic stream", ingress: "/v1/messages", body: `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "openai unary", ingress: "/v1/chat/completions", body: `{"model":"default","messages":[{"role":"user","content":"hi"}]}`, unary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &cursorAgentCapture{}
			var kvWG sync.WaitGroup
			kvWG.Add(1)
			base, token, _ := setupCursorAgentContext(t, capture, &kvWG)
			var resp *http.Response
			var body []byte
			if tc.unary {
				resp, body = post(t, base+tc.ingress, token, tc.body)
			} else {
				var text string
				resp, text = postStream(t, base+tc.ingress, token, tc.body)
				body = []byte(text)
			}
			capture.mu.Lock()
			contextErr := capture.contextErr
			repls := append([][]byte(nil), capture.clientRepl...)
			capture.mu.Unlock()
			if contextErr != "" {
				t.Fatalf("context gate: %s", contextErr)
			}
			if !sawEmptyContextReply(repls) {
				t.Fatal("no empty request_context reply captured")
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if tc.unary {
				var got struct {
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
				}
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Choices) != 1 || got.Choices[0].Message.Content != "Hello world" {
					t.Fatalf("choices = %+v", got.Choices)
				}
				return
			}
			text, finished := collectStreamText(t, tc.ingress, string(body))
			if text != "Hello world" || !finished {
				t.Fatalf("text=%q finished=%v", text, finished)
			}
		})
	}
}

func sawEmptyContextReply(repls [][]byte) bool {
	for _, repl := range repls {
		if hasEmptyRequestContext(repl) {
			return true
		}
	}
	return false
}

// TestCursorAgentRequestContextOldShape502 proves the captured failure: a
// success message without nested request_context gets the Connect internal
// error, and the ingress reports 502 before any text is committed.
func TestCursorAgentRequestContextOldShape502(t *testing.T) {
	capture := &cursorAgentCapture{}
	var kvWG sync.WaitGroup
	kvWG.Add(1)
	base, token, _ := setupCursorAgentHandler(t, capture, serveOldContextShape(capture, &kvWG))
	resp, body := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Failed to get request context") {
		t.Fatalf("body = %s, want captured context error", body)
	}
	if strings.Contains(body, "Hello") {
		t.Fatalf("text committed after context rejection: %s", body)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.contextErr != "request_context missing" {
		t.Fatalf("context gate = %q", capture.contextErr)
	}
}

// serveOldContextShape sends the same omitted-id context request, then answers
// it with the rejected encoder shape: result 10 -> success 1 -> empty success,
// with no nested request_context. The shared gate must then emit the captured
// error instead of text.
func serveOldContextShape(capture *cursorAgentCapture, kvWG *sync.WaitGroup) http.HandlerFunc {
	inner := serveAgentRunGate(capture, kvWG, true)
	return func(w http.ResponseWriter, r *http.Request) {
		pr, pw := io.Pipe()
		req := r.Clone(r.Context())
		req.Body = pr
		go func() {
			defer pw.Close()
			_, payload, err := readConnectFrameForTest(r.Body)
			if err != nil {
				return
			}
			if _, err := pw.Write(wrapFrameForTest(payload)); err != nil {
				return
			}
			_, reply, err := readConnectFrameForTest(r.Body)
			if err != nil {
				return
			}
			old := oldRequestContextReply(reply)
			if _, err := pw.Write(wrapFrameForTest(old)); err != nil {
				return
			}
			_, _ = io.Copy(pw, r.Body)
		}()
		inner(w, req)
	}
}

func oldRequestContextReply(reply []byte) []byte {
	exec, ok := teFieldBytes(reply, 2)
	if !ok {
		return reply
	}
	var kept []byte
	off := 0
	for off < len(exec) {
		start := off
		tag, n, ok := teDecodeVarint(exec, off)
		if !ok {
			return reply
		}
		off = n
		fieldNum := int(tag >> 3)
		wire := int(tag & 7)
		switch wire {
		case teVarint:
			_, n, ok = teDecodeVarint(exec, off)
			if !ok {
				return reply
			}
			off = n
		case teBytes:
			ln, n, ok := teDecodeVarint(exec, off)
			if !ok || n+int(ln) > len(exec) {
				return reply
			}
			off = n + int(ln)
		default:
			return reply
		}
		if fieldNum == 10 {
			continue
		}
		kept = append(kept, exec[start:off]...)
	}
	// 52 02 0a 00 is result 10 wrapping success 1 with no nested context.
	kept = append(kept, teField(10, teBytes, teField(1, teBytes, nil))...)
	return teField(2, teBytes, kept)
}

// TestCursorTruncatedStreamFailover verifies that a Cursor AgentService stream
// cut mid-frame header is not fabricated into a clean finish: the proxy must
// treat it as a pre-commit decode failure and fail over to the next target.
func TestCursorTruncatedStreamFailover(t *testing.T) {
	goodCap := &cursorAgentCapture{}
	// Gate the good target's turn on the proxy's KV reply, like the other
	// AgentService tests: without the wait, handler return RSTs the duplex
	// request stream and races the proxy's mid-stream reply write.
	var goodKV sync.WaitGroup
	goodKV.Add(1)
	badHits := 0
	serveBad := func(w http.ResponseWriter, r *http.Request) {
		badHits++
		// Drain the duplex request body so handler return does not RST the h2
		// stream before the truncated DATA frames are delivered.
		go io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", cursor.ConnectContentType)
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// Heartbeat-only interaction_update: non-committing for the client (only
		// a buffered MessageStart), so the cut below is a pre-commit failure.
		hb := teField(13, teBytes, nil)
		_, _ = w.Write(wrapFrameForTest(teField(1, teBytes, teField(1, teBytes, hb))))
		fl.Flush()
		// Die 3 bytes into the next frame's 5-byte header.
		_, _ = w.Write([]byte{0, 0, 0})
		fl.Flush()
	}
	up1 := httptest.NewUnstartedServer(http.HandlerFunc(serveBad))
	up1.EnableHTTP2 = true
	up1.StartTLS()
	t.Cleanup(up1.Close)
	up2 := httptest.NewUnstartedServer(serveAgentRun(goodCap, &goodKV))
	up2.EnableHTTP2 = true
	up2.StartTLS()
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "cursor-bad", BaseURL: up1.URL, Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthAPIKey, APIKey: "agent-tok",
		OAuthCreds: &domain.OAuthCreds{CursorAuth: true, MachineID: "m-1"}}
	p2 := &domain.Provider{Name: "cursor-good", BaseURL: up2.URL, Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthAPIKey, APIKey: "agent-tok",
		OAuthCreds: &domain.OAuthCreds{CursorAuth: true, MachineID: "m-1"}}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "default", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "default", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p := New(st, nil)
	p.streamClient = &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // self-signed httptest cert
		TLSHandshakeTimeout: 10 * time.Second,
	}}
	p.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if badHits != 1 {
		t.Fatalf("bad target hits = %d", badHits)
	}
	goodCap.mu.Lock()
	hit2 := goodCap.runPayload != nil
	goodCap.mu.Unlock()
	if !hit2 {
		t.Fatal("failover never reached the second target")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream did not finish cleanly on second target")
	}
}

func TestCursorOversizedFrameLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ingress   string
		body      string
		unary     bool
		committed bool
	}{
		{
			name:    "unary discards primary partial",
			ingress: "/v1/chat/completions",
			body:    `{"model":"default","messages":[{"role":"user","content":"hi"}]}`,
			unary:   true,
		},
		{
			name:    "pre-commit stream fails over",
			ingress: "/v1/messages",
			body:    `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:      "post-commit stream does not fail over",
			ingress:   "/v1/chat/completions",
			body:      `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			committed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var badHits atomic.Int64
			badDone := make(chan struct{})
			bad := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				badHits.Add(1)
				// Handler return RSTs the duplex stream. Stay alive until the proxy
				// closes the request so queued response frames can arrive.
				go func() {
					_, _ = io.Copy(io.Discard, r.Body)
					close(badDone)
				}()
				w.Header().Set("Content-Type", cursor.ConnectContentType)
				w.WriteHeader(http.StatusOK)
				fl := w.(http.Flusher)
				if tc.unary || tc.committed {
					writeTextDelta(w, "partial")
				} else {
					hb := teField(13, teBytes, nil)
					_, _ = w.Write(wrapFrameForTest(teField(1, teBytes, teField(1, teBytes, hb))))
					fl.Flush()
				}
				_, _ = w.Write(oversizedConnectHeaderForTest())
				fl.Flush()
				select {
				case <-r.Context().Done():
				case <-badDone:
				case <-time.After(5 * time.Second):
				}
			}))
			bad.EnableHTTP2 = true
			bad.StartTLS()
			t.Cleanup(bad.Close)

			goodCap := &cursorAgentCapture{}
			var goodKV sync.WaitGroup
			goodKV.Add(1)
			good := httptest.NewUnstartedServer(serveAgentRun(goodCap, &goodKV))
			good.EnableHTTP2 = true
			good.StartTLS()
			t.Cleanup(good.Close)

			base, token := setupCursorFailoverProxy(t, bad.URL, good.URL)
			var resp *http.Response
			var body []byte
			if tc.unary {
				resp, body = post(t, base+tc.ingress, token, tc.body)
			} else {
				var text string
				resp, text = postStream(t, base+tc.ingress, token, tc.body)
				body = []byte(text)
			}
			if badHits.Load() != 1 {
				t.Fatalf("primary hits = %d, want 1", badHits.Load())
			}
			select {
			case <-badDone:
			case <-time.After(5 * time.Second):
				t.Fatal("primary request was not closed")
			}
			goodCap.mu.Lock()
			hitGood := goodCap.runPayload != nil
			goodCap.mu.Unlock()

			if tc.committed {
				if hitGood {
					t.Fatal("post-commit failure used the second target")
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
				}
				if !strings.Contains(string(body), "partial") || !strings.Contains(string(body), "frame too large") {
					t.Fatalf("body = %s, want partial text and ingress size error", body)
				}
				if strings.Contains(string(body), "[DONE]") || strings.Contains(string(body), `"finish_reason":"stop"`) ||
					strings.Contains(string(body), "message_stop") {
					t.Fatalf("successful finish after post-commit error: %s", body)
				}
				return
			}

			if !hitGood {
				t.Fatal("pre-commit failure did not use the second target")
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if strings.Contains(string(body), "partial") || strings.Contains(string(body), "frame too large") {
				t.Fatalf("primary failure leaked into response: %s", body)
			}
			if tc.unary {
				var got struct {
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
				}
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Choices) != 1 || got.Choices[0].Message.Content != "Hello world" {
					t.Fatalf("choices = %+v, want fallback text only", got.Choices)
				}
				return
			}
			text, finished := collectStreamText(t, tc.ingress, string(body))
			if text != "Hello world" || !finished {
				t.Fatalf("text=%q finished=%v, want fallback success", text, finished)
			}
		})
	}
}

func TestCursorMissingTurnEndedLifecycle(t *testing.T) {
	t.Run("precommit eof fails over", func(t *testing.T) {
		var badHits atomic.Int64
		bad := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			badHits.Add(1)
			go io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", cursor.ConnectContentType)
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			hb := teField(13, teBytes, nil)
			_, _ = w.Write(wrapFrameForTest(teField(1, teBytes, teField(13, teBytes, hb))))
			fl.Flush()
		}))
		bad.EnableHTTP2 = true
		bad.StartTLS()
		t.Cleanup(bad.Close)

		goodCap := &cursorAgentCapture{}
		var goodKV sync.WaitGroup
		goodKV.Add(1)
		good := httptest.NewUnstartedServer(serveAgentRun(goodCap, &goodKV))
		good.EnableHTTP2 = true
		good.StartTLS()
		t.Cleanup(good.Close)

		base, token := setupCursorFailoverProxy(t, bad.URL, good.URL)
		resp, body := postStream(t, base+"/v1/chat/completions", token,
			`{"model":"default","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		if badHits.Load() != 1 {
			t.Fatalf("primary hits = %d", badHits.Load())
		}
		goodCap.mu.Lock()
		hitGood := goodCap.runPayload != nil
		goodCap.mu.Unlock()
		if !hitGood {
			t.Fatal("empty stream did not fail over")
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		text, finished := collectStreamText(t, "/v1/chat/completions", body)
		if text != "Hello world" || !finished {
			t.Fatalf("text=%q finished=%v", text, finished)
		}
	})

	t.Run("postcommit eof has no success finish", func(t *testing.T) {
		var hits [2]atomic.Int64
		serve := func(i int) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				hits[i].Add(1)
				go io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", cursor.ConnectContentType)
				w.WriteHeader(http.StatusOK)
				writeTextDelta(w, "partial")
			}
		}
		bad := httptest.NewUnstartedServer(serve(0))
		bad.EnableHTTP2 = true
		bad.StartTLS()
		t.Cleanup(bad.Close)
		good := httptest.NewUnstartedServer(serve(1))
		good.EnableHTTP2 = true
		good.StartTLS()
		t.Cleanup(good.Close)

		base, token := setupCursorFailoverProxy(t, bad.URL, good.URL)
		resp, body := postStream(t, base+"/v1/chat/completions", token,
			`{"model":"default","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		if hits[0].Load() != 1 || hits[1].Load() != 0 {
			t.Fatalf("hits = %d/%d, want 1/0", hits[0].Load(), hits[1].Load())
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "partial") || !strings.Contains(body, "error") {
			t.Fatalf("body = %s, want partial text and ingress error", body)
		}
		if strings.Contains(body, "[DONE]") || strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "message_stop") {
			t.Fatalf("successful finish after post-commit EOF: %s", body)
		}
	})

	t.Run("malformed protobuf fails over before output", func(t *testing.T) {
		var badHits atomic.Int64
		bad := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			badHits.Add(1)
			go io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", cursor.ConnectContentType)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(wrapFrameForTest([]byte{0x0a, 0x05, 0x01}))
			w.(http.Flusher).Flush()
		}))
		bad.EnableHTTP2 = true
		bad.StartTLS()
		t.Cleanup(bad.Close)
		goodCap := &cursorAgentCapture{}
		var goodKV sync.WaitGroup
		goodKV.Add(1)
		good := httptest.NewUnstartedServer(serveAgentRun(goodCap, &goodKV))
		good.EnableHTTP2 = true
		good.StartTLS()
		t.Cleanup(good.Close)
		base, token := setupCursorFailoverProxy(t, bad.URL, good.URL)
		resp, body := postStream(t, base+"/v1/chat/completions", token,
			`{"model":"default","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		if badHits.Load() != 1 || resp.StatusCode != http.StatusOK {
			t.Fatalf("hits=%d status=%d body=%s", badHits.Load(), resp.StatusCode, body)
		}
		goodCap.mu.Lock()
		hitGood := goodCap.runPayload != nil
		goodCap.mu.Unlock()
		if !hitGood {
			t.Fatal("malformed protobuf did not fail over")
		}
	})
}

func setupCursorFailoverProxy(t *testing.T, badURL, goodURL string) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	providers := []*domain.Provider{
		{Name: "cursor-bad", BaseURL: badURL, Protocol: domain.ProtocolCursor,
			AuthMethod: domain.AuthAPIKey, APIKey: "agent-tok",
			OAuthCreds: &domain.OAuthCreds{CursorAuth: true, MachineID: "m-1"}},
		{Name: "cursor-good", BaseURL: goodURL, Protocol: domain.ProtocolCursor,
			AuthMethod: domain.AuthAPIKey, APIKey: "agent-tok",
			OAuthCreds: &domain.OAuthCreds{CursorAuth: true, MachineID: "m-1"}},
	}
	for _, prov := range providers {
		if err := st.CreateProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: providers[0].ID, UpstreamModel: "default", Enabled: true},
		{ProviderID: providers[1].ID, UpstreamModel: "default", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p := New(st, nil)
	p.streamClient = &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		TLSHandshakeTimeout: 10 * time.Second,
	}}
	p.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}

func oversizedConnectHeaderForTest() []byte {
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr[1:5], ^uint32(0))
	return hdr
}

// readConnectFrameForTest reads one 5-byte-prefixed Connect frame.
func readConnectFrameForTest(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

func wrapFrameForTest(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func wrapTrailerForTest(payload []byte) []byte {
	out := wrapFrameForTest(payload)
	out[0] = 2
	return out
}
