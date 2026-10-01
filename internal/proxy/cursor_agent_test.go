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
}

// agentServerFrames builds an AgentService response: a KV get request, text
// deltas, and turn end. The server asserts a KV reply arrives before finishing.
func serveAgentRun(capture *cursorAgentCapture, kvWG *sync.WaitGroup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		capture.mu.Lock()
		capture.path = r.URL.Path
		capture.auth = r.Header.Get("Authorization")
		capture.contentType = r.Header.Get("Content-Type")
		capture.accept = r.Header.Get("Accept")
		capture.mu.Unlock()

		// Read the initial run frame, then keep reading for duplex replies.
		go func() {
			for {
				_, payload, err := readConnectFrameForTest(r.Body)
				if err != nil {
					return
				}
				capture.mu.Lock()
				if capture.runPayload == nil {
					capture.runPayload = payload
				} else {
					capture.clientRepl = append(capture.clientRepl, payload)
				}
				capture.mu.Unlock()
				if kvWG != nil && len(capture.clientRepl) > 0 {
					kvWG.Done()
				}
			}
		}()

		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)

		// Ask for a blob; the proxy must reply on the request body before the
		// turn can be considered healthy (the decoder replies immediately).
		// KvServerMessage{1: id=7, 2: get_blob_args{}} inside field 4.
		kv := teVarintField(1, 7)
		kv = append(kv, teField(2, teBytes, nil)...)
		_, _ = w.Write(wrapFrameForTest(teField(4, teBytes, kv)))
		fl.Flush()

		// If a KV gate was supplied, wait for the client's reply before sending
		// content — proves the duplex write path works end to end.
		if kvWG != nil {
			kvWG.Wait()
		}

		writeTextDelta(w, "Hello ")
		writeTextDelta(w, "world")
		te := teVarintField(1, 12)
		te = append(te, teVarintField(2, 3)...)
		_, _ = w.Write(wrapFrameForTest(teField(1, teBytes, teField(14, teBytes, te))))
		fl.Flush()
		// End-stream trailer.
		_, _ = w.Write(wrapTrailerForTest([]byte(`{}`)))
		fl.Flush()
	}
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
	st := newTestStore(t)
	ctx := context.Background()

	// HTTP/2 is required for the duplex test path: over HTTP/1.1 the Go
	// server blocks flushing response headers until the chunked request body
	// is drained, which deadlocks a bidi stream.
	upstream := httptest.NewUnstartedServer(serveAgentRun(capture, kvWG))
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
