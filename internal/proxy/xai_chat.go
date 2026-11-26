package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/proxy/ir"
	"airouter/internal/xaigrant"
)

const (
	xaiChatRejectedMessage = "invalid xai replay state"
	skipLogXAIReplay       = "xai_replay_unavailable"
	skipLogXAIModel        = "xai_replay_model_mismatch"
)

// errXAIReplayUnavailable means the pinned grant, account, endpoint, or final
// wire model is not the source that signed the marker. Ciphertext is not sent.
var errXAIReplayUnavailable = errors.New("xai replay source unavailable")

// errXAIReplayModelMismatch means the prepared wire model is not the signed
// model. The attempt falls back to portable history.
var errXAIReplayModelMismatch = errors.New("xai replay model mismatch")

// testXAIReplayResolved, when set, runs after token resolution and before the
// replay guard compares the grant. Production leaves it nil.
var testXAIReplayResolved func(*domain.Provider)

type xaiReplayGuard struct {
	providerID int64
	generation string
	principal  string
	endpoint   string
	model      string
	routeModel string
}

type xaiReplayKey struct{}

// xaiChatAttempt ties one signable Chat -> official Responses send to the
// transport that carried it. The marker is optional, so a failed transport
// check declines signing instead of falling back to the default redirect path.
type xaiChatAttempt struct {
	bind      grokbuild.ChatBind
	mu        sync.Mutex
	protected bool
}

type xaiChatAttemptKey struct{}

func (a *xaiChatAttempt) markProtected() {
	a.mu.Lock()
	a.protected = true
	a.mu.Unlock()
}

func (a *xaiChatAttempt) transportProtected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.protected
}

// xaiChatAttemptContext marks one translated attempt that may sign its
// response. It uses the same provider, endpoint, wire model, signer key, and
// Chat-only gates as the marker, so other targets keep their client policy.
func (p *Proxy) xaiChatAttemptContext(ctx context.Context, provider *domain.Provider, upstreamModel string, body []byte) context.Context {
	if !p.xaiChatCanSign() {
		return ctx
	}
	emit, ok := grokChatEmitFrom(ctx)
	if !ok {
		return ctx
	}
	bind, ok := xaiChatBind(provider, upstreamModel, strings.TrimSpace(reqModelFromBody(body)), emit.scope, emit.comboID)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, xaiChatAttemptKey{}, &xaiChatAttempt{bind: bind})
}

func xaiChatAttemptFrom(ctx context.Context) *xaiChatAttempt {
	a, _ := ctx.Value(xaiChatAttemptKey{}).(*xaiChatAttempt)
	return a
}

// xaiChatProtectTransport checks the exact outbound request against the
// attempt bind before any bytes leave. A mismatch is a local decline; the
// request is never sent on the default redirect-following client.
func xaiChatProtectTransport(ctx context.Context, provider *domain.Provider, requestURL string, body []byte) error {
	a := xaiChatAttemptFrom(ctx)
	if a == nil {
		return nil
	}
	if !xaiChatSourceMatches(provider, a.bind, requestURL) ||
		strings.TrimSpace(reqModelFromBody(body)) != a.bind.Model {
		return errXAIReplayUnavailable
	}
	a.markProtected()
	return nil
}

func xaiChatTransportProtected(ctx context.Context) bool {
	a := xaiChatAttemptFrom(ctx)
	return a != nil && a.transportProtected()
}

func withXAIReplayGuard(ctx context.Context, g xaiReplayGuard) context.Context {
	return context.WithValue(ctx, xaiReplayKey{}, g)
}

func xaiReplayGuardFrom(ctx context.Context) *xaiReplayGuard {
	g, ok := ctx.Value(xaiReplayKey{}).(xaiReplayGuard)
	if !ok || g.providerID == 0 {
		return nil
	}
	return &g
}

func xaiReplayUnavailableResult() attemptResult {
	return attemptResult{
		retry:   true,
		status:  http.StatusBadGateway,
		errMsg:  "xai replay source unavailable",
		logErr:  skipLogXAIReplay,
		errType: "api_error",
	}
}

func mapXAIReplayErr(err error) (attemptResult, bool) {
	if errors.Is(err, errXAIReplayModelMismatch) {
		return attemptResult{
			retry:   true,
			status:  http.StatusBadGateway,
			errMsg:  "xai replay source unavailable",
			logErr:  skipLogXAIModel,
			errType: "api_error",
		}, true
	}
	if errors.Is(err, errXAIReplayUnavailable) {
		return xaiReplayUnavailableResult(), true
	}
	return attemptResult{}, false
}

// guardXAIReplay refuses a pinned ciphertext send when the trusted live row,
// resolved grant, account, endpoint, sent credential, or body model is not the
// signed source. The check runs on the initial send and after any 401/403 token
// refresh. A same-grant routine refresh stays valid.
func (p *Proxy) guardXAIReplay(ctx context.Context, provider *domain.Provider, requestURL string, body []byte) error {
	g := xaiReplayGuardFrom(ctx)
	if g == nil {
		return nil
	}
	if testXAIReplayResolved != nil {
		testXAIReplayResolved(provider)
	}
	if !xaiReplayGuardMatches(provider, *g, requestURL) {
		return errXAIReplayUnavailable
	}
	// The provider is the request-local hydrated copy. A metadata-only edit or a
	// reconnect can land after hydration, so the live row is the authority for
	// protocol, transport, endpoint, generation, and principal at send time.
	if p == nil || p.store == nil {
		return errXAIReplayUnavailable
	}
	live, err := p.store.GetProvider(ctx, g.providerID)
	if err != nil || live == nil || live.ID != g.providerID {
		return errXAIReplayUnavailable
	}
	if !xaiReplayGuardMatches(live, *g, requestURL) {
		return errXAIReplayUnavailable
	}
	if !xaiReplayCredentialMatches(provider, live, *g) {
		return errXAIReplayUnavailable
	}
	if strings.TrimSpace(reqModelFromBody(body)) != g.model || !xaigrant.IsWireModel(g.model) {
		return errXAIReplayModelMismatch
	}
	return nil
}

// xaiReplayCredentialMatches pairs the bearer about to be sent with the live
// credential row. The generation and principal are the same; only a routine
// refresh may change the access token, and that refresh persists it first.
func xaiReplayCredentialMatches(provider, live *domain.Provider, g xaiReplayGuard) bool {
	if provider == nil || live == nil || provider.ID != live.ID {
		return false
	}
	local, stored := provider.OAuthCreds, live.OAuthCreds
	if local == nil || stored == nil {
		return false
	}
	if local.XAIGrantGeneration != g.generation || stored.XAIGrantGeneration != g.generation {
		return false
	}
	if xaigrant.Principal(local) != g.principal || xaigrant.Principal(stored) != g.principal {
		return false
	}
	if stored.AccessToken == "" || provider.APIKey != stored.AccessToken {
		return false
	}
	return local.AccessToken == stored.AccessToken
}

func xaiReplayGuardMatches(provider *domain.Provider, g xaiReplayGuard, requestURL string) bool {
	if provider == nil || provider.ID != g.providerID || !xaigrant.Eligible(provider) {
		return false
	}
	if g.model == "" || g.routeModel == "" || g.endpoint == "" || g.generation == "" {
		return false
	}
	creds := provider.OAuthCreds
	if creds.XAIGrantGeneration != g.generation || xaigrant.Principal(creds) != g.principal {
		return false
	}
	endpoint, ok := xaigrant.NormalizeEndpoint(provider.BaseURL)
	if !ok || endpoint != g.endpoint {
		return false
	}
	if requestURL != "" && !xaigrant.IsStandardResponsesURL(requestURL) {
		return false
	}
	return true
}

func xaiChatSourceMatches(provider *domain.Provider, bind grokbuild.ChatBind, requestURL string) bool {
	return xaiReplayGuardMatches(provider, xaiReplayGuard{
		providerID: bind.ProviderID,
		generation: bind.Generation,
		principal:  bind.Principal,
		endpoint:   bind.Endpoint,
		model:      bind.Model,
		routeModel: bind.RouteModel,
	}, requestURL)
}

func xaiChatBind(provider *domain.Provider, upstreamModel, wireModel, scope string, comboID int64) (grokbuild.ChatBind, bool) {
	if !xaigrant.Eligible(provider) || comboID == 0 || scope == "" {
		return grokbuild.ChatBind{}, false
	}
	endpoint, ok := xaigrant.NormalizeEndpoint(provider.BaseURL)
	if !ok || !xaigrant.IsWireModel(wireModel) {
		return grokbuild.ChatBind{}, false
	}
	return grokbuild.ChatBind{
		Scope:      scope,
		ComboID:    comboID,
		ProviderID: provider.ID,
		Generation: provider.OAuthCreds.XAIGrantGeneration,
		Principal:  xaigrant.Principal(provider.OAuthCreds),
		Model:      strings.TrimSpace(wireModel),
		RouteModel: upstreamModel,
		Endpoint:   endpoint,
	}, true
}

func (p *Proxy) freshXAIChatTarget(ctx context.Context, t domain.ComboTarget, bind grokbuild.ChatBind) (domain.ComboTarget, bool) {
	if p == nil || p.store == nil || t.Provider == nil {
		return t, false
	}
	live, err := p.store.GetProvider(ctx, bind.ProviderID)
	if err != nil || live == nil || live.Archived {
		return t, false
	}
	t.Provider = live
	t.ProviderID = live.ID
	return t, xaiChatSourceAvailable(t, bind)
}

func xaiChatSourceAvailable(t domain.ComboTarget, bind grokbuild.ChatBind) bool {
	if t.Provider == nil || !t.Enabled || t.Provider.Archived {
		return false
	}
	if t.ProviderID != bind.ProviderID && t.Provider.ID != bind.ProviderID {
		return false
	}
	if t.UpstreamModel != bind.RouteModel {
		return false
	}
	return xaiReplayGuardMatches(t.Provider, xaiReplayGuard{
		providerID: bind.ProviderID,
		generation: bind.Generation,
		principal:  bind.Principal,
		endpoint:   bind.Endpoint,
		model:      bind.Model,
		routeModel: bind.RouteModel,
	}, xaigrant.ResponsesURL)
}

func pinXAIChatTarget(candidates []domain.ComboTarget, bind grokbuild.ChatBind) ([]domain.ComboTarget, bool) {
	idx := -1
	for i, t := range candidates {
		if xaiChatSourceAvailable(t, bind) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return candidates, false
	}
	pin := candidates[idx]
	out := make([]domain.ComboTarget, 0, len(candidates))
	out = append(out, pin)
	for i, t := range candidates {
		if i == idx {
			continue
		}
		if t.ProviderID == pin.ProviderID && t.UpstreamModel == pin.UpstreamModel {
			continue
		}
		out = append(out, t)
	}
	return out, true
}

func (p *Proxy) xaiChatCanSign() bool {
	if p == nil || p.store == nil {
		return false
	}
	_, ok := p.store.XAIChatMAC(nil)
	return ok
}

func (p *Proxy) verifyXAIReplay(payload, mac []byte) bool {
	if p == nil || p.store == nil {
		return false
	}
	return p.store.VerifyXAIChatMAC(payload, mac)
}

func (p *Proxy) xaiChatOrder(ctx context.Context, ingress codec, backend codec, provider *domain.Provider, upstreamModel, wireModel string, res *reqResult) *grokChatOrder {
	if !p.xaiChatCanSign() || ingress.id != "oai-chat" || backend.id != responsesCodec.id {
		return nil
	}
	attempt := xaiChatAttemptFrom(ctx)
	if attempt == nil || !attempt.transportProtected() {
		return nil
	}
	emit, ok := grokChatEmitFrom(ctx)
	if !ok {
		return nil
	}
	bind, ok := xaiChatBind(provider, upstreamModel, wireModel, emit.scope, emit.comboID)
	if !ok || bind != attempt.bind {
		return nil
	}
	return newChatOrder(grokbuild.XAIReplayProfile(), bind, func(payload []byte) ([]byte, bool) {
		if p == nil || p.store == nil {
			return nil, false
		}
		return p.store.XAIChatMAC(payload)
	}, res, observability.Logger(ctx, p.logger), ingress.id)
}

// xaiObserve keeps xAI-only unrepresentable output out of the signed layout.
// The decoder drops such items and parts from the visible Chat projection, so
// only this side channel can see that the turn is partial.
func (o *grokChatOrder) xaiObserve(ev ir.StreamEvent) {
	if o == nil || o.omit {
		return
	}
	if ev.Kind == ir.EventTextDelta && ev.Refusal {
		o.failBudget()
		return
	}
	o.observe(ev)
}

// xaiUnsupported declines the optional marker without failing the stream.
func (o *grokChatOrder) xaiUnsupported() {
	if o != nil {
		o.failBudget()
	}
}

func xaiClientNoRedirect(base *http.Client) *http.Client {
	if base == nil {
		return nil
	}
	clone := *base
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errXAIReplayUnavailable
	}
	return &clone
}

// ensureXAIChatProvider initializes a missing generation only on an eligible
// signed Chat attempt. A store failure leaves the normal portable request.
func (p *Proxy) ensureXAIChatProvider(ctx context.Context, provider *domain.Provider) *domain.Provider {
	if p == nil || p.oauth == nil || provider == nil || provider.OAuthCreds == nil {
		return provider
	}
	if !p.xaiChatCanSign() || !xaigrant.Official(provider) || provider.OAuthCreds.XAIGrantGeneration != "" {
		return provider
	}
	creds, err := p.oauth.EnsureXAIGeneration(ctx, provider)
	if err != nil || creds == nil {
		return provider
	}
	cp := *provider
	cp.OAuthCreds = creds
	return &cp
}

// ensureXAIChatProviderForModel initializes a legacy generation only when the
// request is an eligible Chat translation for the exact signed wire model.
func (p *Proxy) ensureXAIChatProviderForModel(ctx context.Context, provider *domain.Provider, model string) *domain.Provider {
	if _, ok := grokChatEmitFrom(ctx); !ok || !xaigrant.IsWireModel(model) {
		return provider
	}
	return p.ensureXAIChatProvider(ctx, provider)
}

// prepareXAIChatBody adds the stateless envelope to an already finalized xAI
// request. It preserves tools, effort, model, stream, and unknown fields. A
// body that cannot be made bridge-safe is left unchanged.
func (p *Proxy) prepareXAIChatBody(ctx context.Context, body []byte, provider *domain.Provider) ([]byte, error) {
	_, ok := grokChatEmitFrom(ctx)
	if !ok || !p.xaiChatCanSign() || !xaigrant.Eligible(provider) {
		return body, nil
	}
	model := strings.TrimSpace(reqModelFromBody(body))
	if !xaigrant.IsWireModel(model) {
		return body, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return body, nil
	}
	root["store"] = json.RawMessage("false")
	includes, ok := xaiReasoningIncludes(root["include"])
	if !ok {
		return body, nil
	}
	rawInclude, err := json.Marshal(includes)
	if err != nil {
		return nil, err
	}
	root["include"] = rawInclude
	return json.Marshal(root)
}

// xaiReasoningIncludes deduplicates reasoning.encrypted_content and leaves
// other include entries in caller order. A non-array value is not rewritten.
func xaiReasoningIncludes(raw json.RawMessage) ([]json.RawMessage, bool) {
	var items []json.RawMessage
	if len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &items) != nil {
			return nil, false
		}
	}
	out := make([]json.RawMessage, 0, len(items)+1)
	seen := false
	for _, item := range items {
		var value string
		if json.Unmarshal(item, &value) == nil && value == "reasoning.encrypted_content" {
			if seen {
				continue
			}
			seen = true
		}
		out = append(out, append(json.RawMessage(nil), item...))
	}
	if !seen {
		out = append(out, json.RawMessage(`"reasoning.encrypted_content"`))
	}
	return out, true
}
