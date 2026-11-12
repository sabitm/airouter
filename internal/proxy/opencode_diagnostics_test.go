package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/opencode"
)

func diagnosticProxy(level slog.Level) (*Proxy, *bytes.Buffer) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: level}))
	return New(nil, logger), &logs
}

func diagnosticEvents(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var events []map[string]any
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "opencode_upstream_send" {
			events = append(events, event)
		}
	}
	return events
}

func diagnosticContext(p *Proxy, candidate, requestID string) context.Context {
	headers := http.Header{}
	if candidate != "" {
		headers.Set("session_id", candidate)
	}
	ctx := observability.WithRequestID(context.Background(), requestID)
	return WithTraceInfo(withOpencodeRequest(ctx, p.opencodeNonce, headers, nil), &TraceInfo{RequestID: requestID})
}

func TestOpencodeDiagnosticsSessionMetadata(t *testing.T) {
	p, _ := diagnosticProxy(observability.LevelTrace)
	provider := opencodeTestProvider()
	body := []byte(`{"model":"big-pickle","messages":[]}`)
	cases := []struct {
		name, candidate, resolution, cache string
	}{
		{"client miss", "client-secret", "mapped_client", "miss"},
		{"client hit", "client-secret", "mapped_client", "hit"},
		{"native", "ses_0123456789abABCDEFGHIJKLMN", "native", "not_applicable"},
		{"fallback miss", "", "fallback", "miss"},
		{"fallback hit", "", "fallback", "hit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := diagnosticContext(p, tc.candidate, "request")
			if _, err := p.prepareUpstreamRequest(ctx, backendCodec(domain.ProtocolOpencode, "big-pickle"), provider, body); err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL(provider, opencode.ChatPath), nil)
			applyUpstreamHeaders(req, provider, nil, ctx, body)
			attrs := p.opencodeSessionDiagnosticAttrs(req)
			got := make(map[string]any)
			for i := 0; i < len(attrs); i += 2 {
				got[attrs[i].(string)] = attrs[i+1]
			}
			if got["session_resolution"] != tc.resolution || got["session_cache"] != tc.cache {
				t.Fatalf("attrs = %v", got)
			}
			if tc.cache != "not_applicable" {
				age, ok := got["session_cache_age_ms"].(int64)
				if !ok || age < 0 {
					t.Fatalf("cache age = %v", got["session_cache_age_ms"])
				}
			}
			if got["upstream_session"] != p.opencodeSessionPseudonym("upstream", req.Header.Get("x-opencode-session")) {
				t.Fatalf("wire session mismatch: %v", got)
			}
			if tc.candidate == "" && got["client_session"] != "unknown" {
				t.Fatalf("missing client session = %v", got)
			}
			st := opencodeStateFrom(ctx)
			if tc.cache != "not_applicable" {
				st.sessionLookup.CreatedAt = time.Now().Add(-2 * time.Second)
				aged := p.opencodeSessionDiagnosticAttrs(req)
				if aged[len(aged)-1].(int64) < 2000 {
					t.Fatal("cache age was not measured from creation")
				}
			}
			st.session = "mismatched-session"
			unknown := p.opencodeSessionDiagnosticAttrs(req)
			if unknown[len(unknown)-1] != "unknown" {
				t.Fatal("mismatched session reported known cache age")
			}
		})
	}
}

func TestOpencodeDiagnosticsPseudonyms(t *testing.T) {
	p, _ := diagnosticProxy(observability.LevelTrace)
	other, _ := diagnosticProxy(observability.LevelTrace)
	value := p.opencodeSessionPseudonym("client", "secret")
	if len(value) != 32 || value != p.opencodeSessionPseudonym("client", "secret") {
		t.Fatalf("unstable pseudonym: %q", value)
	}
	for _, otherValue := range []string{
		other.opencodeSessionPseudonym("client", "secret"),
		p.opencodeSessionPseudonym("upstream", "secret"),
		p.opencodeSessionPseudonym("client", "different"),
	} {
		if value == otherValue {
			t.Fatal("pseudonym namespace or instance collided")
		}
	}
	if p.opencodeSessionPseudonym("client", "") != "unknown" {
		t.Fatal("empty session was hashed")
	}
	p.opencodeNonce = ""
	if p.opencodeSessionPseudonym("client", "secret") != "unknown" {
		t.Fatal("missing key fell back to an unkeyed hash")
	}
}

func TestOpencodeDiagnosticsScope(t *testing.T) {
	cases := []struct {
		name     string
		level    slog.Level
		protocol domain.Protocol
		base     string
		url      string
		model    string
		want     bool
	}{
		{"official free", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, opencode.ZenOpenAIBaseURL + opencode.ChatPath, "big-pickle", true},
		{"responses", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, opencode.ZenOpenAIBaseURL + opencode.ResponsesPath, "big-pickle", true},
		{"messages", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, opencode.ZenAnthropicBaseURL + opencode.MessagesPath, "big-pickle", true},
		{"legacy", observability.LevelTrace, domain.ProtocolOpencode, opencode.LegacyZenBaseURL, opencode.LegacyZenBaseURL + opencode.ChatPath, "big-pickle", true},
		{"debug off", slog.LevelDebug, domain.ProtocolOpencode, opencode.ZenBaseURL, opencode.ZenOpenAIBaseURL + opencode.ChatPath, "big-pickle", false},
		{"paid", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, opencode.ZenOpenAIBaseURL + opencode.ChatPath, "paid-model", false},
		{"go", observability.LevelTrace, domain.ProtocolOpencode, opencode.GoBaseURL, opencode.ZenOpenAIBaseURL + opencode.ChatPath, "big-pickle", false},
		{"custom base", observability.LevelTrace, domain.ProtocolOpencode, "https://mirror.example", "https://mirror.example/v1/chat/completions", "big-pickle", false},
		{"custom url", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, "https://mirror.example/v1/chat/completions", "big-pickle", false},
		{"discovery", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, opencode.ZenBaseURL + "/models", "big-pickle", false},
		{"nondefault port", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, "https://opencode.ai:8443/inference/openai/v1/chat/completions", "big-pickle", false},
		{"userinfo", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, "https://user:password@opencode.ai/inference/openai/v1/chat/completions", "big-pickle", false},
		{"go endpoint", observability.LevelTrace, domain.ProtocolOpencode, opencode.ZenBaseURL, opencode.GoOpenAIBaseURL + opencode.ChatPath, "big-pickle", false},
		{"other protocol", observability.LevelTrace, domain.ProtocolOpenAI, opencode.ZenBaseURL, opencode.ZenOpenAIBaseURL + opencode.ChatPath, "big-pickle", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, logs := diagnosticProxy(tc.level)
			var traced bool
			p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				traced = httptrace.ContextClientTrace(req.Context()) != nil
				return sizedResponse(req, 0), nil
			})}
			provider := &domain.Provider{Protocol: tc.protocol, BaseURL: tc.base}
			body, _ := json.Marshal(map[string]string{"model": tc.model})
			req, _ := http.NewRequest(http.MethodPost, tc.url, bytes.NewReader(body))
			resp, err := p.doUpstreamRequest(req, provider, body, false)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if traced != tc.want || (len(diagnosticEvents(t, logs)) == 1) != tc.want {
				t.Fatalf("traced=%v want=%v logs=%s", traced, tc.want, logs.String())
			}
		})
	}
}

func TestOpencodeDiagnosticsForwardingAndPrivacy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p, logs := diagnosticProxy(observability.LevelTrace)
			provider := opencodeTestProvider()
			provider.ID = 91
			provider.APIKey = "credential-secret"
			ctx := diagnosticContext(p, "client-session-secret", "request-correlation")
			body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"prompt-secret"}]}`)
			if _, err := p.prepareUpstreamRequest(ctx, backendCodec(domain.ProtocolOpencode, "big-pickle"), provider, body); err != nil {
				t.Fatal(err)
			}
			wireSession := opencodeStateFrom(ctx).session
			var expected http.Header
			calls := 0
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				payload, err := io.ReadAll(req.Body)
				if err != nil || !bytes.Equal(payload, body) {
					t.Fatalf("forwarded body changed: %q %v", payload, err)
				}
				if req.Context().Value(diagnosticContextKey{}) != "context-value" {
					t.Fatal("request context lost")
				}
				if req.Header.Get("Authorization") != "Bearer credential-secret" || req.Header.Get("x-opencode-session") != wireSession {
					t.Fatal("auth or session changed")
				}
				if calls == 1 {
					expected = req.Header.Clone()
				} else if !reflect.DeepEqual(req.Header, expected) {
					t.Fatal("diagnostics changed outgoing headers")
				}
				return &http.Response{StatusCode: 429, Header: http.Header{"X-Arbitrary": {"header-secret"}}, Body: io.NopCloser(strings.NewReader("response-secret")), Request: req}, nil
			})
			client := &http.Client{Transport: transport}
			p.client, p.streamClient = client, client
			ctx = context.WithValue(ctx, diagnosticContextKey{}, "context-value")
			for _, enabled := range []bool{false, true} {
				if enabled {
					p.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: observability.LevelTrace}))
				} else {
					p.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
				}
				clientHeaders := http.Header{"X-Extra": {"extra-header-secret"}, "X-Opencode-Request": {"msg_0123456789abABCDEFGHIJKLMN"}}
				if stream {
					resp, err := p.forwardStream(ctx, provider, opencode.ChatPath, body, clientHeaders, "")
					if err != nil || resp.StatusCode != 429 {
						t.Fatalf("stream response=%v err=%v", resp, err)
					}
					got, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || string(got) != "response-secret" {
						t.Fatalf("stream body changed: %q %v", got, err)
					}
				} else {
					status, got, err := p.forward(ctx, provider, opencode.ChatPath, body, clientHeaders)
					if err != nil || status != 429 || string(got) != "response-secret" {
						t.Fatalf("unary status=%d body=%q err=%v", status, got, err)
					}
				}
			}
			if calls != 2 {
				t.Fatalf("unexpected retry: calls=%d", calls)
			}
			events := diagnosticEvents(t, logs)
			if len(events) != 1 {
				t.Fatalf("events=%v logs=%s", events, logs.String())
			}
			event := events[0]
			if event["request_id"] != "request-correlation" || event["provider_id"] != float64(91) || event["status"] != float64(429) || event["connection_reused"] != "unknown" || event["connection_acquisitions"] != float64(0) || event["first_byte_ms"] != "unknown" || event["error_category"] != "none" {
				t.Fatalf("event=%v", event)
			}
			for _, secret := range []string{"client-session-secret", wireSession, "credential-secret", "prompt-secret", "response-secret", "header-secret", "extra-header-secret", provider.APIKey, p.opencodeNonce} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("secret leaked into logs: %q", secret)
				}
			}
		})
	}
}

type diagnosticContextKey struct{}

func TestOpencodeDiagnosticsLocalConnectionReuse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	local, _ := url.Parse(server.URL)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	p, logs := diagnosticProxy(observability.LevelTrace)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = local.Scheme, local.Host
		return transport.RoundTrip(copy)
	})}
	p.client, p.streamClient = client, client
	provider := opencodeTestProvider()
	body := []byte(`{"model":"big-pickle","messages":[]}`)
	for _, stream := range []bool{false, true} {
		ctx := diagnosticContext(p, "stable-client", "local-request")
		if _, err := p.prepareUpstreamRequest(ctx, backendCodec(domain.ProtocolOpencode, "big-pickle"), provider, body); err != nil {
			t.Fatal(err)
		}
		if stream {
			resp, err := p.forwardStream(ctx, provider, opencode.ChatPath, body, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		} else if status, _, err := p.forward(ctx, provider, opencode.ChatPath, body, nil); err != nil || status != 200 {
			t.Fatalf("status=%d err=%v", status, err)
		}
	}
	events := diagnosticEvents(t, logs)
	if len(events) != 2 {
		t.Fatalf("events=%v", events)
	}
	for i, event := range events {
		if event["connection_reused"] != (i == 1) || event["connection_was_idle"] != (i == 1) || event["connection_acquisitions"] != float64(1) || event["status"] != float64(200) {
			t.Fatalf("event %d = %v", i, event)
		}
		for _, field := range []string{"first_byte_ms", "headers_ms", "connection_idle_ms"} {
			if _, ok := event[field].(float64); !ok {
				t.Fatalf("%s missing: %v", field, event)
			}
		}
	}
	if events[0]["session_cache"] != "miss" || events[1]["session_cache"] != "hit" || events[0]["upstream_session"] != events[1]["upstream_session"] || events[0]["client_session"] != events[1]["client_session"] {
		t.Fatalf("session continuity changed: %v", events)
	}
	if strings.Contains(logs.String(), local.Host) {
		t.Fatal("local connection address leaked")
	}
}

func TestOpencodeDiagnosticsStreamingDoesNotReadBody(t *testing.T) {
	p, logs := diagnosticProxy(observability.LevelTrace)
	provider := opencodeTestProvider()
	body := []byte(`{"model":"big-pickle"}`)
	responseBody := &diagnosticResponseBody{}
	p.streamClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: responseBody, Request: req}, nil
	})}
	resp, err := p.forwardStream(context.Background(), provider, opencode.ChatPath, body, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Body != responseBody || responseBody.reads != 0 || responseBody.closed {
		t.Fatal("diagnostics consumed, wrapped, or closed the streaming body")
	}
	events := diagnosticEvents(t, logs)
	if len(events) != 1 || events[0]["status"] != float64(200) || events[0]["error_category"] != "none" {
		t.Fatalf("header event = %v", events)
	}
	resp.Body.Close()
}

type diagnosticResponseBody struct {
	reads  int
	closed bool
}

func (b *diagnosticResponseBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("response-body-secret")
}

func (b *diagnosticResponseBody) Close() error {
	b.closed = true
	return nil
}

func TestOpencodeDiagnosticsTraceCompositionAndErrors(t *testing.T) {
	p, logs := diagnosticProxy(observability.LevelTrace)
	provider := opencodeTestProvider()
	body := []byte(`{"model":"big-pickle"}`)
	var existingCalls atomic.Int32
	ctx := httptrace.WithClientTrace(WithTraceInfo(context.Background(), &TraceInfo{RequestID: "trace-only-request"}), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { existingCalls.Add(1) },
	})
	p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				trace.GotConn(httptrace.GotConnInfo{Reused: true, WasIdle: true, IdleTime: time.Second})
				trace.GotFirstResponseByte()
			}()
		}
		wg.Wait()
		return nil, errors.New("raw-error-secret")
	})}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL(provider, opencode.ChatPath), nil)
	_, err := p.doUpstreamRequest(req, provider, body, false)
	if err == nil || !strings.Contains(err.Error(), "raw-error-secret") || existingCalls.Load() != 16 {
		t.Fatalf("error or original trace changed: err=%v calls=%d", err, existingCalls.Load())
	}
	events := diagnosticEvents(t, logs)
	if len(events) != 1 || events[0]["connection_acquisitions"] != float64(16) || events[0]["error_category"] != "transport" || events[0]["headers_ms"] != "unknown" || events[0]["request_id"] != "trace-only-request" || events[0]["session_cache"] != "unknown" {
		t.Fatalf("events=%v", events)
	}
	if strings.Contains(logs.String(), "raw-error-secret") {
		t.Fatal("raw transport error leaked")
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "none"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "canceled"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout"},
		{&net.DNSError{IsTimeout: true}, "timeout"},
		{errors.New("secret"), "transport"},
	} {
		if got := opencodeSendErrorCategory(tc.err); got != tc.want {
			t.Fatalf("category=%q want=%q", got, tc.want)
		}
	}
}
