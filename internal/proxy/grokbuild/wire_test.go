package grokbuild

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/thinking"
)

func TestVersionPin(t *testing.T) {
	if Version != "1.0.45" {
		t.Fatalf("version = %s, want 1.0.45 from commit %s", Version, SourceCommit)
	}
	if !strings.Contains(UserAgent(), "grok-shell/1.0.45") {
		t.Fatalf("user agent = %s", UserAgent())
	}
}

func TestFixedHostsRejectUserHosts(t *testing.T) {
	if !IsFixedBase(BaseURL) || !IsFixedInferenceURL(ModelsURL) || !IsFixedTokenURL(TokenURL) {
		t.Fatal("official URLs were rejected")
	}
	for _, raw := range []string{
		"https://api.x.ai/v1",
		"https://cli-chat-proxy.grok.com.evil/v1",
		"http://cli-chat-proxy.grok.com/v1",
		"https://user:pw@cli-chat-proxy.grok.com/v1",
		"https://cli-chat-proxy.grok.com/v1?x=1",
		"https://cli-chat-proxy.grok.com/v1/other",
	} {
		if IsFixedBase(raw) || IsFixedInferenceURL(raw) {
			t.Fatalf("accepted unsafe URL %s", raw)
		}
	}
	if err := ValidateVerificationURI("https://evil.example/login"); err == nil {
		t.Fatal("accepted a third-party verification URL")
	}
	if err := ValidateVerificationURI("https://accounts.x.ai/oauth/device?user_code=ABCD"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProviderKeepsXAISeparate(t *testing.T) {
	xai := &domain.Provider{
		Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		BaseURL: "https://api.x.ai/v1",
		OAuthCreds: &domain.OAuthCreds{
			Preset: "xai", ClientID: ClientID, AccessToken: "xai-token", RefreshToken: "xai-refresh",
		},
	}
	if err := ValidateProvider(xai); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCredentialUse(xai); err != nil {
		t.Fatal(err)
	}
	if err := RejectUnsafeSend(xai, "https://api.x.ai/v1/chat/completions"); err != nil {
		t.Fatal(err)
	}
	bad := &domain.Provider{
		Protocol: domain.ProtocolGrokBuild, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		BaseURL:    "https://api.x.ai/v1",
		OAuthCreds: &domain.OAuthCreds{GrokBuildAuth: true, AccessToken: "sub", RefreshToken: "rot", ClientID: ClientID},
	}
	if err := RejectUnsafeSend(bad, "https://api.x.ai/v1/responses"); err == nil {
		t.Fatal("subscription token was allowed onto api.x.ai")
	}
	presetOnly := &domain.Provider{
		Protocol: domain.ProtocolOpenAI, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		BaseURL:    "https://api.x.ai/v1",
		OAuthCreds: &domain.OAuthCreds{Preset: Preset, AccessToken: "sub", RefreshToken: "rot"},
	}
	if !Involved(presetOnly) || !Marked(presetOnly.OAuthCreds) {
		t.Fatal("preset-only row was treated as generic")
	}
	if err := ValidateProvider(presetOnly); err == nil {
		t.Fatal("preset-only row passed provider validation")
	}
	if err := ValidateCredentialUse(presetOnly); err == nil {
		t.Fatal("preset-only row passed credential use")
	}
	if err := RejectUnsafeSend(presetOnly, "https://api.x.ai/v1/chat/completions"); err == nil {
		t.Fatal("preset-only row passed send backstop")
	}
}

func TestApplyHeadersDropsCallerIdentity(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer caller")
	h.Set("X-Email", "user@example.com")
	h.Set("X-Userid", "user-1")
	h.Set("X-Grok-User-Id", "user-1")
	body := []byte(`{"model":"grok-build","input":[{"role":"user","content":"hi"}]}`)
	ApplyHeaders(h, body, "grok-build", "acct-1", ScopedIDs("tenant", 7, "session-a", "req-1"))
	if h.Get("Authorization") != "Bearer caller" {
		t.Fatal("auth header was rewritten by identity application")
	}
	if h.Get("X-XAI-Token-Auth") != TokenAuthValue || h.Get("X-Grok-Client-Version") != Version {
		t.Fatalf("missing subscription headers: %v", h)
	}
	if h.Get("X-Grok-Model-Override") != "grok-build" || h.Get("X-Grok-Req-Id") != "req-1" {
		t.Fatalf("model or request id mismatch: %v", h)
	}
	if h.Get("X-Email") != "" || h.Get("X-Grok-User-Id") != "" {
		t.Fatalf("account email leaked: %v", h)
	}
	if h.Get("X-Userid") != "acct-1" {
		t.Fatalf("account id = %s", h.Get("X-Userid"))
	}
	if h.Get("X-Grok-Turn-Idx") != "1" {
		t.Fatalf("turn = %s", h.Get("X-Grok-Turn-Idx"))
	}
	a := ScopedIDs("tenant", 7, "session-a", "req-1")
	b := ScopedIDs("tenant", 8, "session-a", "req-1")
	if a.SessionID == b.SessionID || a.AgentID == b.AgentID {
		t.Fatal("provider scope did not change upstream ids")
	}
}

func TestParseModelsKeepsAccountEffort(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"grok-build","capabilities":{"reasoning_effort":["low","high","bogus"]}},
		{"id":"grok-4.7","capabilities":{"reasoning_effort":["xhigh"]}}
	]}`)
	models, err := ParseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "grok-build" || len(models[0].Efforts) != 2 {
		t.Fatalf("models = %+v", models)
	}
	if models[1].ID != "grok-4.7" || models[1].Efforts[0] != "xhigh" {
		t.Fatalf("second = %+v", models[1])
	}
	if _, err := ParseModels([]byte(`{"data":[]}`)); err == nil {
		t.Fatal("empty catalog was accepted")
	}
}

func TestEncodeRequestForcesStoreFalseAndAccountEffort(t *testing.T) {
	req := &ir.Request{
		Model: "grok-build", Stream: false,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking: thinking.ToIR(thinking.FromOpenAIEffort("high")),
	}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["store"] != false || doc["stream"] != true {
		t.Fatalf("store/stream = %v %v", doc["store"], doc["stream"])
	}
	reasoning, _ := doc["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != nil {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	if !strings.Contains(string(raw), encryptedInclude) {
		t.Fatalf("missing encrypted include: %s", raw)
	}
	auto := &ir.Request{Model: "m", Thinking: thinking.ToIR(&thinking.Config{Mode: thinking.ModeAuto})}
	raw, err = EncodeRequest(auto)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"reasoning"`) {
		t.Fatalf("auto injected reasoning: %s", raw)
	}
	unknown := &ir.Request{Model: "m", Thinking: thinking.ToIR(&thinking.Config{Mode: thinking.ModeLevel, Level: "max", Effort: "max"})}
	if _, err := EncodeRequest(unknown); err == nil {
		t.Fatal("unknown effort was rewritten")
	}
}

func TestRejectStoredAndHostedItems(t *testing.T) {
	if err := RejectUnsupportedIngress([]byte(`{"tools":[{"type":"web_search"}]}`)); err == nil {
		t.Fatal("hosted tool accepted")
	}
	if err := RejectUnsupportedIngress([]byte(`{"input":["rs_123"]}`)); err == nil {
		t.Fatal("stored item id accepted")
	}
	if err := RejectUnsupportedIngress([]byte(`{"input":[{"type":"item_reference","id":"rs_1"}]}`)); err == nil {
		t.Fatal("item reference accepted")
	}
	if err := RejectUnsupportedIngress([]byte(`{"tools":[{"type":"function","name":"read"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := RejectUnsupportedIngress([]byte("\n {\"previous_response_id\":\"resp_1\"}")); err == nil {
		t.Fatal("previous_response_id accepted")
	}
	if err := RejectUnsupportedIngress([]byte(`{"previous_response_id":""}`)); err != nil {
		t.Fatal(err)
	}
	if err := RejectUnsupportedIngress([]byte(`{"input":[" plain text "]}`)); err == nil {
		t.Fatal("string item reference accepted")
	}
}

func TestListModelsDoesNotFollowRedirect(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"grok-build"}]}`)
	}))
	defer upstream.Close()
	SetTestTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "cli-chat-proxy.grok.com" {
			t.Fatalf("host = %s", r.URL.Host)
		}
		w := httptest.NewRecorder()
		http.Redirect(w, r, upstream.URL+"/stolen", http.StatusFound)
		return w.Result(), nil
	}))
	defer SetTestTransport(nil)
	if _, err := ListModels(context.Background(), "secret-token", "acct-1"); err == nil {
		t.Fatal("redirect was followed")
	}
	if seen != "" {
		t.Fatal("token reached the redirect target")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
