package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"airouter/internal/codexgrant"
	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/grokbuild"
)

const (
	codexChatRejectedMessage = "invalid codex replay state"
	skipLogCodexReplay       = "codex_replay_unavailable"
	// skipLogCodexModel is a local decline. The prepared body model is not the
	// signed wire model. Ciphertext is not sent and provider health is unchanged.
	skipLogCodexModel = "codex_replay_model_mismatch"
)

// errCodexReplayUnavailable means the pinned grant, account, or endpoint is
// not the one that signed the marker. The body must not be sent.
var errCodexReplayUnavailable = errors.New("codex replay source unavailable")

// errCodexReplayModelMismatch means the final prepared model is not the signed
// wire model. The attempt must use portable history instead of ciphertext.
var errCodexReplayModelMismatch = errors.New("codex replay model mismatch")

// testCodexReplayResolved, when set, runs after token resolution and before
// the replay guard compares the grant. Production leaves it nil.
var testCodexReplayResolved func(*domain.Provider)

type codexReplayGuard struct {
	providerID int64
	generation string
	principal  string
	endpoint   string
	model      string
	routeModel string
}

type codexReplayKey struct{}

func withCodexReplayGuard(ctx context.Context, g codexReplayGuard) context.Context {
	return context.WithValue(ctx, codexReplayKey{}, g)
}

func codexReplayGuardFrom(ctx context.Context) *codexReplayGuard {
	g, ok := ctx.Value(codexReplayKey{}).(codexReplayGuard)
	if !ok || g.providerID == 0 {
		return nil
	}
	return &g
}

func codexReplayUnavailableResult() attemptResult {
	return attemptResult{
		retry:   true,
		status:  http.StatusBadGateway,
		errMsg:  "codex replay source unavailable",
		logErr:  skipLogCodexReplay,
		errType: "api_error",
	}
}

func mapCodexReplayErr(err error) (attemptResult, bool) {
	if errors.Is(err, errCodexReplayModelMismatch) {
		return attemptResult{
			retry:   true,
			status:  http.StatusBadGateway,
			errMsg:  "codex replay source unavailable",
			logErr:  skipLogCodexModel,
			errType: "api_error",
		}, true
	}
	if errors.Is(err, errCodexReplayUnavailable) {
		return codexReplayUnavailableResult(), true
	}
	return attemptResult{}, false
}

func mapChatReplayErr(err error) (attemptResult, bool) {
	if ar, ok := mapGrokReplayErr(err); ok {
		return ar, true
	}
	if ar, ok := mapCodexReplayErr(err); ok {
		return ar, true
	}
	return mapXAIReplayErr(err)
}

// guardCodexReplay refuses a pinned ciphertext send when the resolved grant,
// account, or request URL is not the signed source. A nil guard is an ordinary
// request. The check uses the request-local provider and the URL about to be
// sent, not caller headers.
func guardCodexReplay(ctx context.Context, provider *domain.Provider, requestURL string, body []byte) error {
	g := codexReplayGuardFrom(ctx)
	if g == nil {
		return nil
	}
	if testCodexReplayResolved != nil {
		testCodexReplayResolved(provider)
	}
	if !codexReplayGuardMatches(provider, *g, requestURL) {
		return errCodexReplayUnavailable
	}
	// The signed model is the model in the body about to be sent, not the
	// combo suffix prediction. Check it on the initial send and on auth retry.
	if strings.TrimSpace(reqModelFromBody(body)) != g.model {
		return errCodexReplayModelMismatch
	}
	return nil
}

func codexReplayGuardMatches(provider *domain.Provider, g codexReplayGuard, requestURL string) bool {
	if provider == nil || provider.ID != g.providerID || !codexgrant.Eligible(provider) {
		return false
	}
	if g.model == "" || g.routeModel == "" || g.endpoint == "" || g.generation == "" || g.principal == "" {
		return false
	}
	creds := provider.OAuthCreds
	if creds.CodexGrantGeneration != g.generation || strings.TrimSpace(creds.AccountID) != g.principal {
		return false
	}
	endpoint, ok := codexgrant.NormalizeEndpoint(provider.BaseURL)
	if !ok || endpoint != g.endpoint {
		return false
	}
	if requestURL != "" && !codexgrant.IsStandardResponsesURL(requestURL) {
		return false
	}
	return true
}

func codexChatBind(provider *domain.Provider, upstreamModel, wireModel, scope string, comboID int64) (grokbuild.ChatBind, bool) {
	if !codexgrant.Eligible(provider) || comboID == 0 || scope == "" {
		return grokbuild.ChatBind{}, false
	}
	endpoint, ok := codexgrant.NormalizeEndpoint(provider.BaseURL)
	if !ok {
		return grokbuild.ChatBind{}, false
	}
	// Model is the exact model in the prepared body. A suffix prediction can
	// differ once body effort rewrites the wire model, so an empty reading does
	// not get a guessed signature.
	model := strings.TrimSpace(wireModel)
	if model == "" {
		return grokbuild.ChatBind{}, false
	}
	return grokbuild.ChatBind{
		Scope:      scope,
		ComboID:    comboID,
		ProviderID: provider.ID,
		Generation: provider.OAuthCreds.CodexGrantGeneration,
		Principal:  strings.TrimSpace(provider.OAuthCreds.AccountID),
		Model:      model,
		RouteModel: upstreamModel,
		Endpoint:   endpoint,
	}, true
}

func (p *Proxy) freshCodexChatTarget(ctx context.Context, t domain.ComboTarget, bind grokbuild.ChatBind) (domain.ComboTarget, bool) {
	if p == nil || p.store == nil || t.Provider == nil {
		return t, false
	}
	live, err := p.store.GetProvider(ctx, bind.ProviderID)
	if err != nil || live == nil || live.Archived {
		return t, false
	}
	t.Provider = live
	t.ProviderID = live.ID
	return t, codexChatSourceAvailable(t, bind)
}

func codexChatSourceAvailable(t domain.ComboTarget, bind grokbuild.ChatBind) bool {
	if t.Provider == nil || !t.Enabled || t.Provider.Archived {
		return false
	}
	if t.ProviderID != bind.ProviderID && t.Provider.ID != bind.ProviderID {
		return false
	}
	if t.UpstreamModel != bind.RouteModel {
		return false
	}
	// Route affinity is the combo target. The prepared body model is checked
	// at send time so a legitimate signed wire model is not dropped just because
	// the suffix prediction differs.
	return codexReplayGuardMatches(t.Provider, codexReplayGuard{
		providerID: bind.ProviderID,
		generation: bind.Generation,
		principal:  bind.Principal,
		endpoint:   bind.Endpoint,
		model:      bind.Model,
		routeModel: bind.RouteModel,
	}, codexgrant.ResponsesURL)
}

func pinCodexChatTarget(candidates []domain.ComboTarget, bind grokbuild.ChatBind) ([]domain.ComboTarget, bool) {
	idx := -1
	for i, t := range candidates {
		if codexChatSourceAvailable(t, bind) {
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

func (p *Proxy) codexChatCanSign() bool {
	if p == nil || p.store == nil {
		return false
	}
	_, ok := p.store.CodexChatMAC(nil)
	return ok
}

func (p *Proxy) verifyChatReplay(protocol string, payload, mac []byte) bool {
	switch protocol {
	case "grok-build":
		return p.verifyBridgeMAC(payload, mac)
	case "codex":
		if p == nil || p.store == nil {
			return false
		}
		return p.store.VerifyCodexChatMAC(payload, mac)
	case grokbuild.XAIReplayProfile().Protocol:
		return p.verifyXAIReplay(payload, mac)
	default:
		return false
	}
}

func (p *Proxy) codexChatOrder(ctx context.Context, ingress codec, backend codec, provider *domain.Provider, upstreamModel, wireModel string, res *reqResult) *grokChatOrder {
	if !p.codexChatCanSign() || ingress.id != "oai-chat" || backend.id != "oai-codex" {
		return nil
	}
	emit, ok := grokChatEmitFrom(ctx)
	if !ok {
		return nil
	}
	bind, ok := codexChatBind(provider, upstreamModel, wireModel, emit.scope, emit.comboID)
	if !ok {
		return nil
	}
	return newChatOrder(grokbuild.CodexReplayProfile(), bind, func(payload []byte) ([]byte, bool) {
		if p == nil || p.store == nil {
			return nil, false
		}
		return p.store.CodexChatMAC(payload)
	}, res, observability.Logger(ctx, p.logger), ingress.id)
}

func (p *Proxy) chatOrder(ctx context.Context, ingress codec, backend codec, provider *domain.Provider, upstreamModel, wireModel string, res *reqResult) *grokChatOrder {
	if order := p.grokChatOrder(ctx, ingress, backend, provider, upstreamModel, res); order != nil {
		return order
	}
	if order := p.codexChatOrder(ctx, ingress, backend, provider, upstreamModel, wireModel, res); order != nil {
		return order
	}
	return p.xaiChatOrder(ctx, ingress, backend, provider, upstreamModel, wireModel, res)
}

func codexClientNoRedirect(base *http.Client) *http.Client {
	if base == nil {
		return nil
	}
	clone := *base
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errCodexReplayUnavailable
	}
	return &clone
}

func chatReplayRejectedMessage(err error) string {
	switch {
	case errors.Is(err, grokbuild.ErrCodexReplayRejected):
		return codexChatRejectedMessage
	case errors.Is(err, grokbuild.ErrXAIReplayRejected):
		return xaiChatRejectedMessage
	default:
		return grokChatRejectedMessage
	}
}
