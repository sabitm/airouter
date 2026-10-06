package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

func kiroTestIsCatalog(r *http.Request) bool {
	target := r.Header.Get("X-Amz-Target")
	return strings.Contains(target, "ListAvailableModels")
}

func kiroTestCatalog(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if model == "" {
		model = "m"
	}
	_, _ = io.WriteString(w, `{"models":[{"modelId":"`+model+`","additionalModelRequestFieldsSchema":{"type":"object","properties":{"output_config":{"type":"object","properties":{"effort":{"enum":["low","medium","high","xhigh"],"default":"medium"}}},"thinking":{"type":"object","properties":{"type":{"enum":["disabled","adaptive"],"default":"adaptive"}}}}}}]}`)
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
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "claude-sonnet-4.5")
			return
		}
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
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "m1")
			return
		}
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
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "m2")
			return
		}
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

func runtimeProvider(name, base string) *domain.Provider {
	return &domain.Provider{
		Name: name, BaseURL: base, Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			KiroAuth: "idc", KiroIDP: "AWSIdC", KiroTransport: "runtime",
			ProfileArn:  "arn:aws:codewhisperer:eu-central-1:1:profile/A",
			AccessToken: "oauth-token", ExpiresAt: 4102444800,
		},
	}
}

func TestKiroRuntimeTransportHeadersAndFailover(t *testing.T) {
	var hits []string
	var headers []http.Header
	var bodies [][]byte
	handler := func(name string, body []byte, failAfter bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if kiroTestIsCatalog(r) {
				kiroTestCatalog(w, "m")
				return
			}
			raw, _ := io.ReadAll(r.Body)
			hits = append(hits, name+" "+r.URL.Path)
			headers = append(headers, r.Header.Clone())
			bodies = append(bodies, raw)
			if r.URL.Path != "/" || r.URL.RawQuery != "" {
				t.Errorf("runtime path = %q query = %q", r.URL.Path, r.URL.RawQuery)
			}
			if r.Header.Get("X-Amz-Target") != "KiroRuntimeService.GenerateAssistantResponse" {
				t.Errorf("runtime target = %q", r.Header.Get("X-Amz-Target"))
			}
			if r.Header.Get("Content-Type") != "application/x-amz-json-1.0" {
				t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
			}
			if values := r.Header.Values("TokenType"); len(values) != 1 || values[0] != "SSO_OIDC" {
				t.Errorf("TokenType = %#v", values)
			}
			if r.Header.Get("X-Kiro-Idp") != "AWSIdC" || r.Header.Get("x-amzn-kiro-profile-arn") != "" {
				t.Errorf("identity headers = %v", r.Header)
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			if failAfter {
				return
			}
			w.(http.Flusher).Flush()
		}
	}
	up1 := httptest.NewServer(handler("bad", []byte{0, 0, 0, 0, 0, 0, 0}, true))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(handler("good", kiroTextStream(), false))
	t.Cleanup(up2.Close)
	st := newTestStore(t)
	ctx := context.Background()
	p1 := runtimeProvider("bad", up1.URL)
	p2 := runtimeProvider("good", up2.URL)
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m", Enabled: true},
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
	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC"}}]}]}`)
	if resp.StatusCode != http.StatusOK || len(hits) != 2 {
		t.Fatalf("status=%d hits=%v body=%s", resp.StatusCode, hits, body)
	}
	for _, raw := range bodies {
		if !bytes.Contains(raw, []byte(`"profileArn":"arn:aws:codewhisperer:eu-central-1:1:profile/A"`)) || !bytes.Contains(raw, []byte(`"bytes":"iVBORw0KGgo`)) {
			t.Fatalf("runtime body missing profile or image: %s", raw)
		}
	}
	if !strings.Contains(body, "Hello") {
		t.Fatalf("client stream = %s", body)
	}
	_ = headers

	var committedHits int
	committed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "m")
			return
		}
		committedHits++
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buildKiroFrame("assistantResponseEvent", `{"content":"visible"}`))
		_, _ = w.Write([]byte{0, 0, 0, 0, 0, 0, 0})
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(committed.Close)
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "m")
			return
		}
		t.Errorf("failover after commitment")
	}))
	t.Cleanup(next.Close)
	st2 := newTestStore(t)
	bad := runtimeProvider("committed", committed.URL)
	good := runtimeProvider("unused", next.URL)
	if err := st2.CreateProvider(ctx, bad); err != nil || st2.CreateProvider(ctx, good) != nil {
		t.Fatal(err)
	}
	if err := st2.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: bad.ID, UpstreamModel: "m", Enabled: true},
		{ProviderID: good.ID, UpstreamModel: "m", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key2, err := st2.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux2 := http.NewServeMux()
	New(st2, nil).Mount(mux2)
	ts2 := httptest.NewServer(mux2)
	t.Cleanup(ts2.Close)
	resp, body = postStream(t, ts2.URL+"/v1/chat/completions", key2.Token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if committedHits != 1 || strings.Contains(body, "unused") {
		t.Fatalf("hits=%d body=%s status=%d", committedHits, body, resp.StatusCode)
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

func TestKiroUpstreamPreservesToolSchemaInteger(t *testing.T) {
	cap := &kiroCapture{}
	base, token, _ := setupKiro(t, kiroTextStream(), cap)
	reqBody := `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer","maximum":9223372036854775807}}}}}]}`
	resp, body := postStream(t, base+"/v1/chat/completions", token, reqBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !bytes.Contains(cap.body, []byte("9223372036854775807")) {
		t.Errorf("schema maximum missing from upstream body:\n%s", cap.body)
	}
	if !bytes.Contains(cap.body, []byte(`"profileArn":"arn:aws:codewhisperer:us-east-1:123:profile/ABC"`)) {
		t.Errorf("profileArn missing from upstream body:\n%s", cap.body)
	}
}

// TestKiroToolStream verifies a Kiro toolUseEvent stream reassembles into an
// OpenAI tool_call with concatenated arguments and a tool_calls finish reason.
func TestKiroToolStream(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildKiroFrame("toolUseEvent", `{"toolUseId":"call_1","name":"get_weather","input":"{\"city\":"}`))
	buf.Write(buildKiroFrame("toolUseEvent", `{"toolUseId":"call_1","input":"\"paris\"}"}`))
	buf.Write(buildKiroFrame("metricsEvent", `{"inputTokens":11,"outputTokens":3}`))
	buf.Write(buildKiroFrame("messageStopEvent", `{}`))

	cap := &kiroCapture{}
	base, token, _ := setupKiro(t, buf.Bytes(), cap)
	reqBody := `{"model":"default","stream":true,"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"id":{"maximum":9223372036854775807}}}}}]}`
	resp, body := postStream(t, base+"/v1/chat/completions", token, reqBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	name, args, finish := collectOpenAIToolStream(t, body)
	if name != "get_weather" {
		t.Errorf("tool = %q", name)
	}
	if !strings.Contains(body, `"id":"call_1"`) {
		t.Errorf("tool id missing: %s", body)
	}
	if args != `{"city":"paris"}` {
		t.Errorf("tool args = %q", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
	if !bytes.Contains(cap.body, []byte(`"name":"get_weather"`)) || bytes.Contains(cap.body, []byte(`"name":"get_weather_ide"`)) {
		t.Errorf("upstream declaration not normalized:\n%s", cap.body)
	}
	if !bytes.Contains(cap.body, []byte("9223372036854775807")) {
		t.Errorf("schema number lost:\n%s", cap.body)
	}
	in, out, _ := collectOpenAIUsage(t, body)
	if in != 11 || out != 3 {
		t.Errorf("usage = %d/%d", in, out)
	}
	if cap.target != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" || cap.accept != "application/vnd.amazon.eventstream" || cap.auth != "Bearer up-key" || cap.tokentype != "API_KEY" {
		t.Errorf("headers changed: target=%q accept=%q auth=%q tokentype=%q", cap.target, cap.accept, cap.auth, cap.tokentype)
	}
}

func TestKiroToolCloakMatrix(t *testing.T) {
	stream := kiroToolStream("call_9", "lookup", `{"id":9050000000000000001}`)
	cases := []struct {
		name    string
		ingress string
		body    string
	}{
		{
			name:    "openai chat",
			ingress: "/v1/chat/completions",
			body:    `{"model":"default","messages":[{"role":"user","content":"lookup"},{"role":"assistant","tool_calls":[{"id":"call_old","type":"function","function":{"name":"lookup","arguments":"{\"id\":1}"}}]},{"role":"tool","tool_call_id":"call_old","content":"ok"}],"tools":[{"type":"function","function":{"name":"lookup","description":"find","parameters":{"type":"object","properties":{"id":{"maximum":9223372036854775807}}}}}]}`,
		},
		{
			name:    "anthropic messages",
			ingress: "/v1/messages",
			body:    `{"model":"default","max_tokens":20,"messages":[{"role":"user","content":"lookup"},{"role":"assistant","content":[{"type":"tool_use","id":"call_old","name":"lookup","input":{"id":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_old","content":"ok"}]}],"tools":[{"name":"lookup","description":"find","input_schema":{"type":"object","properties":{"id":{"maximum":9223372036854775807}}}}]}`,
		},
		{
			name:    "responses",
			ingress: "/v1/responses",
			body:    `{"model":"default","input":[{"type":"message","role":"user","content":"lookup"},{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{\"id\":1}"},{"type":"function_call_output","call_id":"call_old","output":"ok"}],"tools":[{"type":"function","name":"lookup","description":"find","parameters":{"type":"object","properties":{"id":{"maximum":9223372036854775807}}}}]}`,
		},
	}
	methods := []domain.AuthMethod{domain.AuthAPIKey, domain.AuthOAuth}
	for _, method := range methods {
		for _, tc := range cases {
			for _, streamMode := range []bool{true, false} {
				t.Run(string(method)+"/"+tc.name+"/stream="+boolString(streamMode), func(t *testing.T) {
					cap := &kiroCapture{}
					base, token, _ := setupKiroMethod(t, stream, cap, method)
					body := tc.body
					if streamMode {
						body = strings.Replace(body, "{\"model\":", "{\"stream\":true,\"model\":", 1)
						resp, out := postStream(t, base+tc.ingress, token, body)
						if resp.StatusCode != http.StatusOK {
							t.Fatalf("status = %d body = %s", resp.StatusCode, out)
						}
						assertKiroToolClient(t, tc.ingress, out, true)
					} else {
						resp, out := post(t, base+tc.ingress, token, body)
						if resp.StatusCode != http.StatusOK {
							t.Fatalf("status = %d body = %s", resp.StatusCode, out)
						}
						assertKiroToolClient(t, tc.ingress, string(out), false)
					}
					assertKiroCloakedRequest(t, cap, method)
				})
			}
		}
	}
}

func TestKiroToolCloakFailover(t *testing.T) {
	bad := buildKiroFrame("toolUseEvent", `{"toolUseId":"call_bad","name":"fs_read","input":"{\"secret\":\"hidden\"}"}`)
	good := kiroToolStream("call_9", "lookup", `{"id":1}`)
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "m1")
			return
		}
		n1++
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bad)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "m2")
			return
		}
		n2++
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(good)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "kiro-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	p2 := &domain.Provider{Name: "kiro-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
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

	req := `{"model":"default","stream":true,"messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token, req)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d, want pre-commit failover", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "hidden") || strings.Contains(body, "fs_read") {
		t.Fatalf("rejected tool leaked: %s", body)
	}
	name, args, finish := collectOpenAIToolStream(t, body)
	if name != "lookup" || args != `{"id":1}` || finish != "tool_calls" {
		t.Fatalf("failover tool = %q %q %q", name, args, finish)
	}
}

func TestKiroToolCloakPostCommitNoFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "m1")
			return
		}
		n1++
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buildKiroFrame("assistantResponseEvent", `{"content":"visible"}`))
		_, _ = w.Write(buildKiroFrame("toolUseEvent", `{"toolUseId":"call_bad","name":"fs_read","input":"{\"secret\":\"hidden\"}"}`))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "m2")
			return
		}
		n2++
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(kiroTextStream())
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "kiro-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	p2 := &domain.Provider{Name: "kiro-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
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

	req := `{"model":"default","stream":true,"messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token, req)
	if n1 != 1 || n2 != 0 {
		t.Fatalf("hits n1=%d n2=%d, want no failover after commitment", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "hidden") || strings.Contains(body, "fs_read") || strings.Contains(body, "call_bad") || strings.Contains(body, "Hello world") {
		t.Fatalf("rejected tool or failover content leaked: %s", body)
	}
	if !strings.Contains(body, "upstream tool call is not available") || !strings.Contains(body, "visible") {
		t.Fatalf("committed failure missing: %s", body)
	}
}

func TestKiroToolCloakUnaryDoesNotReturnPartial(t *testing.T) {
	var hits [2]int
	var servers [2]*httptest.Server
	for i := range servers {
		i := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if kiroTestIsCatalog(r) {
				kiroTestCatalog(w, "m")
				return
			}
			hits[i]++
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buildKiroFrame("assistantResponseEvent", `{"content":"partial"}`))
			_, _ = w.Write(buildKiroFrame("toolUseEvent", `{"toolUseId":"call_bad","name":"unknown_tool","input":"{\"secret\":\"hidden\"}"}`))
			w.(http.Flusher).Flush()
		}))
		t.Cleanup(servers[i].Close)
	}
	st := newTestStore(t)
	ctx := context.Background()
	var providers [2]*domain.Provider
	for i := range providers {
		providers[i] = &domain.Provider{Name: fmt.Sprintf("kiro-%d", i), BaseURL: servers[i].URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
		if err := st.CreateProvider(ctx, providers[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: providers[0].ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: providers[1].ID, UpstreamModel: "m2", Enabled: true},
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

	req := `{"model":"default","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	resp, raw := post(t, ts.URL+"/v1/chat/completions", key.Token, req)
	body := string(raw)
	if hits[0] != 1 || hits[1] != 1 {
		t.Fatalf("hits = %v, want both targets attempted before commitment", hits)
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unary returned partial success: %s", body)
	}
	if strings.Contains(body, "partial") || strings.Contains(body, "hidden") || strings.Contains(body, "unknown_tool") || strings.Contains(body, "call_bad") {
		t.Fatalf("partial tool content leaked: %s", body)
	}
	if !strings.Contains(body, "upstream tool call is not available") {
		t.Fatalf("safe failure missing: %s", body)
	}
}

func TestKiroToolCloakRejectsUnboundAndChangedIdentity(t *testing.T) {
	cases := []struct {
		name       string
		withTools  bool
		validStart bool
		invalid    string
	}{
		{name: "nameless without declarations", invalid: `{"toolUseId":"call_hidden","input":"hidden"}`},
		{name: "nameless with declarations", withTools: true, invalid: `{"toolUseId":"call_hidden","input":"hidden"}`},
		{name: "changed declared identity", withTools: true, validStart: true, invalid: `{"toolUseId":"call_valid","name":"other","input":"hidden"}`},
	}
	ingresses := []struct {
		path  string
		body  string
		tools string
	}{
		{
			path:  "/v1/chat/completions",
			body:  `"messages":[{"role":"user","content":"lookup"}]`,
			tools: `"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},{"type":"function","function":{"name":"other","parameters":{"type":"object"}}}]`,
		},
		{
			path:  "/v1/messages",
			body:  `"max_tokens":20,"messages":[{"role":"user","content":"lookup"}]`,
			tools: `"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"name":"other","input_schema":{"type":"object"}}]`,
		},
		{
			path:  "/v1/responses",
			body:  `"input":[{"role":"user","content":"lookup"}]`,
			tools: `"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function","name":"other","parameters":{"type":"object"}}]`,
		},
	}
	for _, tc := range cases {
		for _, ingress := range ingresses {
			for _, stream := range []bool{false, true} {
				t.Run(tc.name+ingress.path+"/stream="+boolString(stream), func(t *testing.T) {
					var frames bytes.Buffer
					frames.Write(buildKiroFrame("assistantResponseEvent", `{"content":"visible"}`))
					if tc.validStart {
						frames.Write(buildKiroFrame("toolUseEvent", `{"toolUseId":"call_valid","name":"lookup"}`))
					}
					frames.Write(buildKiroFrame("toolUseEvent", tc.invalid))
					frames.Write(buildKiroFrame("messageStopEvent", `{}`))
					base, token, _ := setupKiro(t, frames.Bytes(), nil)
					req := `{"model":"default","stream":` + boolString(stream) + `,` + ingress.body
					if tc.withTools {
						req += `,` + ingress.tools
					}
					resp, raw := post(t, base+ingress.path, token, req+`}`)
					body := string(raw)
					if stream {
						if resp.StatusCode != http.StatusOK || !strings.Contains(body, "visible") {
							t.Fatalf("expected committed response, status=%d body=%s", resp.StatusCode, body)
						}
					} else if resp.StatusCode == http.StatusOK || strings.Contains(body, "visible") || strings.Contains(body, "call_valid") {
						t.Fatalf("unary returned partial success: status=%d body=%s", resp.StatusCode, body)
					}
					if !strings.Contains(body, "upstream tool call is not available") {
						t.Fatalf("generic tool failure missing: %s", body)
					}
					for _, leaked := range []string{"hidden", "other"} {
						if strings.Contains(body, leaked) {
							t.Fatalf("rejected data %q leaked: %s", leaked, body)
						}
					}
				})
			}
		}
	}
}

func setupKiroMethod(t *testing.T, upstreamBody []byte, captured *kiroCapture, method domain.AuthMethod) (string, string, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "claude-sonnet-4.5")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if captured != nil {
			captured.path = r.URL.Path
			captured.target = r.Header.Get("X-Amz-Target")
			captured.accept = r.Header.Get("Accept")
			captured.tokentype = r.Header.Get("tokentype")
			captured.auth = r.Header.Get("Authorization")
			captured.body = append([]byte(nil), body...)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(upstreamBody)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	prov := &domain.Provider{
		Name: "kiro", BaseURL: upstream.URL, Protocol: domain.ProtocolKiro, AuthMethod: method,
		OAuthCreds: &domain.OAuthCreds{ProfileArn: "arn:aws:codewhisperer:us-east-1:123:profile/ABC"},
	}
	if method == domain.AuthOAuth {
		prov.OAuthCreds.AccessToken = "oauth-token"
		prov.OAuthCreds.ExpiresAt = 4102444800
	} else {
		prov.APIKey = "up-key"
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

func kiroToolStream(id, name, args string) []byte {
	payload := `{"toolUseId":"` + id + `","name":"` + name + `","input":` + strconvQuote(args) + `}`
	var buf bytes.Buffer
	buf.Write(buildKiroFrame("toolUseEvent", payload))
	buf.Write(buildKiroFrame("metricsEvent", `{"inputTokens":8,"outputTokens":2}`))
	buf.Write(buildKiroFrame("messageStopEvent", `{}`))
	return buf.Bytes()
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func assertKiroCloakedRequest(t *testing.T, cap *kiroCapture, method domain.AuthMethod) {
	t.Helper()
	if !bytes.Contains(cap.body, []byte(`"name":"lookup"`)) {
		t.Fatalf("normalized declaration missing:\n%s", cap.body)
	}
	if bytes.Contains(cap.body, []byte(`"name":"lookup_ide"`)) {
		t.Fatalf("unexpected alias leaked:\n%s", cap.body)
	}
	for _, name := range []string{"execute_bash", "fs_write", "glob", "grep", "web_search", "web_fetch"} {
		if bytes.Contains(cap.body, []byte(`"name":"`+name+`"`)) {
			t.Errorf("decoy %s advertised", name)
		}
	}
	if !bytes.Contains(cap.body, []byte("9223372036854775807")) {
		t.Errorf("schema number lost:\n%s", cap.body)
	}
	if !bytes.Contains(cap.body, []byte(`"toolUseId":"call_old"`)) || !bytes.Contains(cap.body, []byte(`"name":"lookup"`)) {
		t.Errorf("history id/name not preserved:\n%s", cap.body)
	}
	if !bytes.Contains(cap.body, []byte(`"profileArn":"arn:aws:codewhisperer:us-east-1:123:profile/ABC"`)) {
		t.Errorf("profileArn changed:\n%s", cap.body)
	}
	if !strings.Contains(string(cap.body), `"origin":"AI_EDITOR"`) || !strings.Contains(string(cap.body), `"modelId":"claude-sonnet-4.5"`) {
		t.Errorf("origin/model changed:\n%s", cap.body)
	}
	if cap.target != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" || cap.accept != "application/vnd.amazon.eventstream" {
		t.Errorf("target/accept = %q/%q", cap.target, cap.accept)
	}
	switch method {
	case domain.AuthAPIKey:
		if cap.auth != "Bearer up-key" || cap.tokentype != "API_KEY" {
			t.Errorf("api-key auth changed: %q %q", cap.auth, cap.tokentype)
		}
	case domain.AuthOAuth:
		if cap.auth != "Bearer oauth-token" || cap.tokentype != "" {
			t.Errorf("oauth auth changed: %q %q", cap.auth, cap.tokentype)
		}
	}
}

func assertKiroToolClient(t *testing.T, ingress, body string, stream bool) {
	t.Helper()
	if strings.Contains(body, "lookup_ide") || strings.Contains(body, "execute_bash") {
		t.Fatalf("wire name leaked to client: %s", body)
	}
	switch {
	case strings.HasSuffix(ingress, "/chat/completions") && stream:
		name, args, finish := collectOpenAIToolStream(t, body)
		if name != "lookup" || args != `{"id":9050000000000000001}` || finish != "tool_calls" || !strings.Contains(body, `"id":"call_9"`) {
			t.Fatalf("openai stream tool = %q %q %q body=%s", name, args, finish, body)
		}
		in, out, _ := collectOpenAIUsage(t, body)
		if in != 8 || out != 2 {
			t.Fatalf("usage = %d/%d", in, out)
		}
	case strings.HasSuffix(ingress, "/chat/completions"):
		var got struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Choices) != 1 || len(got.Choices[0].Message.ToolCalls) != 1 {
			t.Fatalf("choices = %+v", got.Choices)
		}
		call := got.Choices[0].Message.ToolCalls[0]
		if call.ID != "call_9" || call.Function.Name != "lookup" || call.Function.Arguments != `{"id":9050000000000000001}` || got.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("unary tool = %+v finish=%s", call, got.Choices[0].FinishReason)
		}
		if got.Usage.PromptTokens != 8 || got.Usage.CompletionTokens != 2 {
			t.Fatalf("usage = %+v", got.Usage)
		}
	case strings.HasSuffix(ingress, "/messages") && stream:
		name, args, stop, stopped := collectAnthropicToolStream(t, body)
		if name != "lookup" || args != `{"id":9050000000000000001}` || stop != "tool_use" || !stopped || !strings.Contains(body, `"id":"call_9"`) {
			t.Fatalf("anthropic stream tool = %q %q %q stopped=%v body=%s", name, args, stop, stopped, body)
		}
	case strings.HasSuffix(ingress, "/messages"):
		var got struct {
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Content) != 1 || got.Content[0].ID != "call_9" || got.Content[0].Name != "lookup" || string(got.Content[0].Input) != `{"id":9050000000000000001}` || got.StopReason != "tool_use" {
			t.Fatalf("anthropic unary = %+v", got)
		}
		if got.Usage.InputTokens != 8 || got.Usage.OutputTokens != 2 {
			t.Fatalf("usage = %+v", got.Usage)
		}
	case strings.HasSuffix(ingress, "/responses") && stream:
		name, args, done := collectResponsesToolStream(t, body)
		if name != "lookup" || args != `{"id":9050000000000000001}` || done != args || !strings.Contains(body, `"call_id":"call_9"`) {
			t.Fatalf("responses stream tool = %q %q %q body=%s", name, args, done, body)
		}
	default:
		var got struct {
			Output []struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"output"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Output) != 1 || got.Output[0].CallID != "call_9" || got.Output[0].Name != "lookup" || got.Output[0].Arguments != `{"id":9050000000000000001}` {
			t.Fatalf("responses unary = %+v", got.Output)
		}
		if got.Usage.InputTokens != 8 || got.Usage.OutputTokens != 2 {
			t.Fatalf("usage = %+v", got.Usage)
		}
	}
}
