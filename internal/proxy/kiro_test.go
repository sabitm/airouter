package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/store"
)

// buildKiroFrame encodes one AWS EventStream message with a single :event-type
// string header and JSON payload, matching what a Kiro upstream sends. It mirrors
// the codec's wire format so the proxy's full translate path can be exercised.
func buildKiroFrame(eventType string, payload string) []byte {
	const stringType = 7
	name := ":event-type"
	var hdr bytes.Buffer
	hdr.WriteByte(byte(len(name)))
	hdr.WriteString(name)
	hdr.WriteByte(stringType)
	var vl [2]byte
	binary.BigEndian.PutUint16(vl[:], uint16(len(eventType)))
	hdr.Write(vl[:])
	hdr.WriteString(eventType)
	headers := hdr.Bytes()

	totalLen := uint32(12 + len(headers) + len(payload) + 4)
	var prelude [12]byte
	binary.BigEndian.PutUint32(prelude[0:4], totalLen)
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))

	var msg bytes.Buffer
	msg.Write(prelude[:])
	msg.Write(headers)
	msg.WriteString(payload)
	var msgCRC [4]byte
	binary.BigEndian.PutUint32(msgCRC[:], crc32.ChecksumIEEE(msg.Bytes()))
	msg.Write(msgCRC[:])
	return msg.Bytes()
}

func kiroTextStream() []byte {
	var buf bytes.Buffer
	buf.Write(buildKiroFrame("assistantResponseEvent", `{"content":"Hello "}`))
	buf.Write(buildKiroFrame("assistantResponseEvent", `{"content":"world"}`))
	buf.Write(buildKiroFrame("metricsEvent", `{"inputTokens":7,"outputTokens":2}`))
	buf.Write(buildKiroFrame("messageStopEvent", `{}`))
	return buf.Bytes()
}

func kiroMetricsOnlyStream() []byte {
	var buf bytes.Buffer
	buf.Write(buildKiroFrame("metricsEvent", `{"inputTokens":7,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4}`))
	buf.Write(buildKiroFrame("messageStopEvent", `{}`))
	return buf.Bytes()
}

// setupKiro wires a Kiro-backed provider whose upstream returns the given binary
// EventStream, and records the last upstream request body and headers.
func setupKiro(t *testing.T, upstreamBody []byte, captured *kiroCapture) (string, string, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if captured != nil {
			captured.path = r.URL.Path
			captured.target = r.Header.Get("X-Amz-Target")
			captured.accept = r.Header.Get("Accept")
			captured.tokentype = r.Header.Get("tokentype")
			captured.auth = r.Header.Get("Authorization")
			captured.body = body
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(upstreamBody)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	prov := &domain.Provider{
		Name: "kiro", BaseURL: upstream.URL, APIKey: "up-key", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthAPIKey,
		OAuthCreds: &domain.OAuthCreds{ProfileArn: "arn:aws:codewhisperer:us-east-1:123:profile/ABC"},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-4.5", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token, st
}

type kiroCapture struct {
	path      string
	target    string
	accept    string
	tokentype string
	auth      string
	body      []byte
}

// TestKiroStreamTranslate exercises OpenAI and Anthropic ingress streaming to a
// Kiro backend: the binary EventStream is decoded to IR and re-encoded as the
// ingress SSE format. It also asserts the Kiro upstream headers and profileArn
// injection.
func TestKiroStreamTranslate(t *testing.T) {
	cases := []struct{ name, ingress string }{
		{"openai->kiro", "/v1/chat/completions"},
		{"anthropic->kiro", "/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &kiroCapture{}
			base, token, _ := setupKiro(t, kiroTextStream(), cap)
			resp, body := postStream(t, base+tc.ingress, token, `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			text, finished := collectStreamText(t, tc.ingress, body)
			if text != "Hello world" {
				t.Errorf("text = %q", text)
			}
			if !finished {
				t.Errorf("stream did not signal completion")
			}
			// Upstream request assertions.
			if !strings.HasSuffix(cap.path, "/generateAssistantResponse") {
				t.Errorf("upstream path = %q", cap.path)
			}
			if cap.target != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
				t.Errorf("x-amz-target = %q", cap.target)
			}
			if cap.accept != "application/vnd.amazon.eventstream" {
				t.Errorf("accept = %q", cap.accept)
			}
			if cap.tokentype != "API_KEY" {
				t.Errorf("tokentype = %q", cap.tokentype)
			}
			if cap.auth != "Bearer up-key" {
				t.Errorf("auth = %q", cap.auth)
			}
			var reqBody struct {
				ProfileArn        string `json:"profileArn"`
				ConversationState struct {
					CurrentMessage struct {
						UserInputMessage struct {
							ModelID string `json:"modelId"`
						} `json:"userInputMessage"`
					} `json:"currentMessage"`
				} `json:"conversationState"`
			}
			if err := json.Unmarshal(cap.body, &reqBody); err != nil {
				t.Fatalf("upstream body: %v\n%s", err, cap.body)
			}
			if reqBody.ProfileArn != "arn:aws:codewhisperer:us-east-1:123:profile/ABC" {
				t.Errorf("profileArn = %q", reqBody.ProfileArn)
			}
			if reqBody.ConversationState.CurrentMessage.UserInputMessage.ModelID != "claude-sonnet-4.5" {
				t.Errorf("modelId = %q", reqBody.ConversationState.CurrentMessage.UserInputMessage.ModelID)
			}
		})
	}
}

// TestKiroTruncatedStreamFailover verifies that a Kiro upstream whose
// EventStream dies mid-prelude does not fabricate a clean finish: the proxy must
// treat it as a pre-commit decode failure and fail over to the next target.
func TestKiroTruncatedStreamFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		w.Write(buildKiroFrame("metricsEvent", `{"inputTokens":7,"outputTokens":2}`))
		// Die 7 bytes into the next frame's 12-byte prelude. metricsEvent is
		// non-committing (only a buffered MessageStart), so the proxy must treat
		// this as a pre-commit decode failure and fail over.
		w.Write([]byte{0, 0, 0, 0, 0, 0, 0})
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		w.Write(kiroTextStream())
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "kiro-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthAPIKey,
		OAuthCreds: &domain.OAuthCreds{ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/BAD"}}
	p2 := &domain.Provider{Name: "kiro-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthAPIKey,
		OAuthCreds: &domain.OAuthCreds{ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/GOOD"}}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d, want failover to second target", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "partial") {
		t.Errorf("truncated first-target bytes leaked: %s", body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream did not finish cleanly on second target")
	}
}

// TestKiroUnaryCollected verifies a non-streaming client request to the
// stream-only Kiro backend is collected from the EventStream into a unary
// response and usage is recorded.
func TestKiroUnaryCollected(t *testing.T) {
	base, token, st := setupKiro(t, kiroTextStream(), nil)
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
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "Hello world" {
		t.Errorf("content = %+v", got.Choices)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.InputTokens != 7 || l.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 7/2", l.InputTokens, l.OutputTokens)
	}
}

func TestKiroMetricsOnlyUsagePropagates(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		base, token, st := setupKiro(t, kiroMetricsOnlyStream(), nil)
		resp, body := postStream(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		in, out, total := collectOpenAIUsage(t, body)
		if in != 14 || out != 2 || total != 16 {
			t.Errorf("client usage = %d/%d/%d, want 14/2/16", in, out, total)
		}
		l := waitForLogs(t, st, 1)[0]
		if l.InputTokens != 14 || l.OutputTokens != 2 {
			t.Errorf("logged tokens = %d/%d, want 14/2", l.InputTokens, l.OutputTokens)
		}
	})

	t.Run("unary", func(t *testing.T) {
		base, token, st := setupKiro(t, kiroMetricsOnlyStream(), nil)
		resp, body := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		var got struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Usage.PromptTokens != 14 || got.Usage.CompletionTokens != 2 || got.Usage.TotalTokens != 16 {
			t.Errorf("client usage = %+v, want 14/2/16", got.Usage)
		}
		l := waitForLogs(t, st, 1)[0]
		if l.InputTokens != 14 || l.OutputTokens != 2 {
			t.Errorf("logged tokens = %d/%d, want 14/2", l.InputTokens, l.OutputTokens)
		}
	})
}

// TestKiroToolStream verifies a Kiro toolUseEvent stream reassembles into an
// OpenAI tool_call with concatenated arguments and a tool_calls finish reason.
func TestKiroToolStream(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildKiroFrame("toolUseEvent", `{"toolUseId":"call_1","name":"get_weather","input":"{\"city\":"}`))
	buf.Write(buildKiroFrame("toolUseEvent", `{"toolUseId":"call_1","input":"\"paris\"}"}`))
	buf.Write(buildKiroFrame("messageStopEvent", `{}`))

	base, token, _ := setupKiro(t, buf.Bytes(), nil)
	resp, body := postStream(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	name, args, finish := collectOpenAIToolStream(t, body)
	if name != "get_weather" {
		t.Errorf("tool name = %q", name)
	}
	if args != `{"city":"paris"}` {
		t.Errorf("tool args = %q", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
}
