package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/thinking"
)

// thinkingCapsFor resolves caps through the shared thinking package.
func thinkingCapsFor(t *testing.T, model string) thinking.Caps {
	t.Helper()
	return thinking.CapsFor(model, domain.ProtocolOpencode, domain.ReasoningOpencode)
}

func opencodeTestProvider() *domain.Provider {
	return &domain.Provider{
		Name:     "opencode zen",
		BaseURL:  opencode.ZenBaseURL,
		APIKey:   opencode.PublicKey,
		Protocol: domain.ProtocolOpencode,
	}
}

func TestOpencodeBackendCodecPerModel(t *testing.T) {
	chat := backendCodec(domain.ProtocolOpencode, "big-pickle")
	if chat.id != "opencode-chat" {
		t.Fatalf("big-pickle codec = %q", chat.id)
	}
	if chat.upstreamPath != opencode.ChatPath {
		t.Fatalf("chat path = %q", chat.upstreamPath)
	}
	resp := backendCodec(domain.ProtocolOpencode, "muse-spark-1.2-contributor-free")
	if resp.id != "opencode-responses" {
		t.Fatalf("muse-spark codec = %q", resp.id)
	}
	if resp.upstreamPath != opencode.ResponsesPath {
		t.Fatalf("responses path = %q", resp.upstreamPath)
	}
}

// The distinct codec ids must prevent passthrough for both ingress formats so
// the fingerprint headers and model patches are always applied.
func TestOpencodeNeverPassthrough(t *testing.T) {
	for _, ing := range []codec{openaiCodec, responsesCodec} {
		for _, model := range []string{"big-pickle", "muse-spark-1.2-contributor-free"} {
			if ing.id == backendCodec(domain.ProtocolOpencode, model).id {
				t.Fatalf("ingress %q passes through to opencode model %q", ing.id, model)
			}
		}
	}
}

func TestOpencodePrepareUpstreamRequestChat(t *testing.T) {
	p := opencodeTestProvider()
	body := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]}`)
	trace := &TraceInfo{}
	out, err := prepareUpstreamRequest(WithTraceInfo(context.Background(), trace), backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro"), p, body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"reasoning_content":" "`) {
		t.Fatalf("deepseek chat body missing reasoning echo: %s", out)
	}
	if !strings.HasPrefix(trace.OpencodeSessionID, "ses_") {
		t.Fatalf("trace session id = %q", trace.OpencodeSessionID)
	}
	// Session id is conversation-stable: same body derives the same value.
	out2, err := prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro"), p, body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(out2) {
		t.Fatalf("same conversation produced different bodies: %s vs %s", out, out2)
	}
}

func TestOpencodePrepareUpstreamRequestMuseSpark(t *testing.T) {
	p := opencodeTestProvider()
	body := []byte(`{"model":"muse-spark-1.2-contributor-free","reasoning":{"effort":"max"},"max_output_tokens":400}`)
	out, err := prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backendCodec(domain.ProtocolOpencode, "muse-spark-1.2-contributor-free"), p, body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	r, _ := got["reasoning"].(map[string]any)
	if r == nil || r["effort"] != "xhigh" || r["summary"] != "auto" {
		t.Fatalf("muse-spark reasoning = %+v", r)
	}
}

// The end-to-end translated unary path through the deepseek body: decode chat,
// encode chat, finalize with the opencode dialect, then prepare.
func TestOpencodeTranslatedDeepseekFinalize(t *testing.T) {
	p := opencodeTestProvider()
	ing := openaiCodec
	body := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	req, err := ing.decodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	applyUpstreamModel(req, "deepseek-v4-pro")
	backend := backendCodec(p.Protocol, "deepseek-v4-pro")
	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err = finalizeEncodedBody(upstreamBody, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err = prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backend, p, upstreamBody)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(upstreamBody, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "deepseek-v4-pro" {
		t.Fatalf("model = %v", got["model"])
	}
	// deepseek dialect writes enable_thinking + thinking.type=enabled on chat.
	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Fatalf("deepseek thinking = %+v, want type enabled", th)
	}
}

func TestApplyOpencodeHeaders(t *testing.T) {
	trace := &TraceInfo{OpencodeSessionID: "ses_abc"}
	req, _ := http.NewRequestWithContext(WithTraceInfo(context.Background(), trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	// A forwarded client UA that is not opencode must be replaced.
	req.Header.Set("User-Agent", "some-agent/1.0")
	applyUpstreamHeaders(req, opencodeTestProvider(), http.Header{}, req.Context(), nil)
	if got := req.Header.Get("User-Agent"); got != "opencode" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("x-opencode-session"); got != "ses_abc" {
		t.Fatalf("x-opencode-session = %q", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "desktop" {
		t.Fatalf("x-opencode-client = %q", got)
	}
	if got := req.Header.Get("x-opencode-project"); got != "global" {
		t.Fatalf("x-opencode-project = %q", got)
	}
	if got := req.Header.Get("x-opencode-request"); !strings.HasPrefix(got, "msg_") {
		t.Fatalf("x-opencode-request = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer public" {
		t.Fatalf("Authorization = %q", got)
	}
}

// A forwarded opencode client UA survives the header application.
func TestApplyOpencodeHeadersPreservesClientUA(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	clientHeaders := http.Header{}
	clientHeaders.Set("User-Agent", "opencode/0.16.7")
	clientHeaders.Set("x-opencode-client", "my-cli")
	applyUpstreamHeaders(req, opencodeTestProvider(), clientHeaders, context.Background(), nil)
	if got := req.Header.Get("User-Agent"); got != "opencode/0.16.7" {
		t.Fatalf("client UA clobbered: %q", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "my-cli" {
		t.Fatalf("client x-opencode-client clobbered: %q", got)
	}
}

// muse-spark thinking levels: none is rejected upstream, so the caps must not
// disable and the effective writer keeps an explicit level.
func TestOpencodeMuseSparkCaps(t *testing.T) {
	caps := thinkingCapsFor(t, "muse-spark-1.2-contributor-free")
	if caps.CanDisable {
		t.Fatalf("muse-spark must not allow disabling reasoning")
	}
	if caps.Format != thinking.FormatOpenAIResponses {
		t.Fatalf("muse-spark format = %v", caps.Format)
	}
	found := false
	for _, l := range caps.Levels {
		if l == "xhigh" {
			found = true
		}
		if l == "max" || l == "ultra" {
			t.Fatalf("muse-spark advertises %q", l)
		}
	}
	if !found {
		t.Fatalf("muse-spark missing xhigh level: %v", caps.Levels)
	}
}
