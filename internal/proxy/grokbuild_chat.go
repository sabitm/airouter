package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/sse"
	"airouter/internal/proxy/thinking"
)

const (
	grokChatRejectedMessage = "invalid grok build replay state"
	// skipLogGrokReplay is a local guard failure. It is not an upstream health
	// signal and must not be logged with marker bytes.
	skipLogGrokReplay = "grok_build_replay_unavailable"
)

// errGrokBuildReplayUnavailable means the pinned connection changed or is not
// the connection that signed the marker. The body must not be sent.
var errGrokBuildReplayUnavailable = errors.New("grok build replay source unavailable")

// testGrokBuildReplayResolved, when set, runs after token resolution and
// before the replay guard compares the connection. Tests use it to simulate a
// reconnect that lands between route selection and send. Production leaves it nil.
var testGrokBuildReplayResolved func(*domain.Provider)

// testGrokChatBeforeFresh, when set, runs after combo hydration and before the
// signed source is trusted for a pin. Tests use it to change the stored
// connection in that window. Production leaves it nil.
var testGrokChatBeforeFresh func()

type grokChatEmitState struct {
	scope   string
	comboID int64
}

type grokBuildReplayGuard struct {
	providerID int64
	generation string
	principal  string
	model      string
}

type grokChatEmitKey struct{}
type grokBuildReplayGuardKey struct{}

func withGrokChatEmit(ctx context.Context, scope string, comboID int64) context.Context {
	return context.WithValue(ctx, grokChatEmitKey{}, grokChatEmitState{scope: scope, comboID: comboID})
}

func grokChatEmitFrom(ctx context.Context) (grokChatEmitState, bool) {
	st, ok := ctx.Value(grokChatEmitKey{}).(grokChatEmitState)
	return st, ok && st.comboID != 0 && st.scope != ""
}

func withGrokBuildReplayGuard(ctx context.Context, guard grokBuildReplayGuard) context.Context {
	return context.WithValue(ctx, grokBuildReplayGuardKey{}, guard)
}

func grokBuildReplayGuardFrom(ctx context.Context) *grokBuildReplayGuard {
	guard, ok := ctx.Value(grokBuildReplayGuardKey{}).(grokBuildReplayGuard)
	if !ok {
		return nil
	}
	return &guard
}

func replayUnavailableResult() attemptResult {
	return attemptResult{
		retry:   true,
		status:  http.StatusBadGateway,
		errMsg:  "grok build replay source unavailable",
		logErr:  skipLogGrokReplay,
		errType: "api_error",
	}
}

func mapGrokReplayErr(err error) (attemptResult, bool) {
	if errors.Is(err, errGrokBuildReplayUnavailable) {
		return replayUnavailableResult(), true
	}
	return attemptResult{}, false
}

// guardGrokBuildReplay checks the resolved connection and prepared wire model.
// Ordinary refresh keeps the generation and is allowed.
func guardGrokBuildReplay(ctx context.Context, provider *domain.Provider, body []byte) error {
	g := grokBuildReplayGuardFrom(ctx)
	if g == nil {
		return nil
	}
	if testGrokBuildReplayResolved != nil {
		testGrokBuildReplayResolved(provider)
	}
	if !grokReplayGuardMatches(provider, *g) || strings.TrimSpace(reqModelFromBody(body)) != g.model {
		return errGrokBuildReplayUnavailable
	}
	return nil
}

func grokReplayGuardMatches(provider *domain.Provider, g grokBuildReplayGuard) bool {
	if provider == nil || provider.ID != g.providerID || provider.Protocol != domain.ProtocolGrokBuild || !grokbuild.Involved(provider) {
		return false
	}
	creds := provider.OAuthCreds
	if creds == nil || !creds.GrokBuildAuth || creds.GrokBuildConnection == "" || creds.GrokBuildConnection != g.generation {
		return false
	}
	return strings.TrimSpace(creds.UserID) == g.principal
}

func grokChatBind(provider *domain.Provider, upstreamModel string, scope string, comboID int64) (grokbuild.ChatBind, bool) {
	if provider == nil || provider.Protocol != domain.ProtocolGrokBuild || !grokbuild.Involved(provider) || provider.OAuthCreds == nil {
		return grokbuild.ChatBind{}, false
	}
	creds := provider.OAuthCreds
	if !creds.GrokBuildAuth || strings.TrimSpace(creds.GrokBuildConnection) == "" || provider.ID == 0 || comboID == 0 || scope == "" {
		return grokbuild.ChatBind{}, false
	}
	return grokbuild.ChatBind{
		Scope:      scope,
		ComboID:    comboID,
		ProviderID: provider.ID,
		Generation: creds.GrokBuildConnection,
		Principal:  strings.TrimSpace(creds.UserID),
		Model:      thinking.StripSuffix(upstreamModel),
		RouteModel: upstreamModel,
	}, true
}

// freshGrokChatTarget reloads the signed provider before a pin is trusted.
// A reconnect or deletion that landed after combo hydration must not keep
// the ciphertext on that route. A store miss fails closed to the portable path.
func (p *Proxy) freshGrokChatTarget(ctx context.Context, t domain.ComboTarget, bind grokbuild.ChatBind) (domain.ComboTarget, bool) {
	if p == nil || p.store == nil || t.Provider == nil {
		return t, false
	}
	live, err := p.store.GetProvider(ctx, bind.ProviderID)
	if err != nil || live == nil || live.Archived {
		return t, false
	}
	t.Provider = live
	t.ProviderID = live.ID
	return t, grokChatSourceAvailable(t, bind)
}

func grokChatSourceAvailable(t domain.ComboTarget, bind grokbuild.ChatBind) bool {
	if t.Provider == nil || !t.Enabled || t.Provider.Archived {
		return false
	}
	if t.ProviderID != bind.ProviderID && t.Provider.ID != bind.ProviderID {
		return false
	}
	if t.UpstreamModel != bind.RouteModel || thinking.StripSuffix(t.UpstreamModel) != bind.Model {
		return false
	}
	return grokReplayGuardMatches(t.Provider, grokBuildReplayGuard{
		providerID: bind.ProviderID,
		generation: bind.Generation,
		principal:  bind.Principal,
		model:      bind.Model,
	})
}

// pinGrokChatTarget moves the exact signed route to the front. Other routes
// keep their relative order. The signed route is not appended again.
func pinGrokChatTarget(candidates []domain.ComboTarget, bind grokbuild.ChatBind) ([]domain.ComboTarget, bool) {
	idx := -1
	for i, t := range candidates {
		if grokChatSourceAvailable(t, bind) {
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

func (p *Proxy) bridgeMAC(payload []byte) ([]byte, bool) {
	if p == nil || p.store == nil {
		return nil, false
	}
	return p.store.GrokBuildChatMAC(payload)
}

func (p *Proxy) verifyBridgeMAC(payload, mac []byte) bool {
	if p == nil || p.store == nil {
		return false
	}
	return p.store.VerifyGrokBuildChatMAC(payload, mac)
}

type grokChatEncoder struct {
	inner   *openai.StreamEncoder
	order   *grokChatOrder
	flushed bool
}

// testGrokChatUnaryCap, when positive, is the stamp-only output cap. It does
// not change the real unary response limit. Production leaves it zero.
var testGrokChatUnaryCap int64

func grokChatUnaryCap() int64 {
	if testGrokChatUnaryCap > 0 {
		return testGrokChatUnaryCap
	}
	return maxUnaryUpstreamResponseBytes
}

func (p *Proxy) grokChatCanSign() bool {
	_, ok := p.bridgeMAC(nil)
	return ok
}

func (p *Proxy) grokChatOrder(ctx context.Context, ingress codec, backend codec, provider *domain.Provider, upstreamModel string, res *reqResult) *grokChatOrder {
	if !p.grokChatCanSign() || ingress.id != "oai-chat" || backend.id != "grok-build" {
		return nil
	}
	emit, ok := grokChatEmitFrom(ctx)
	if !ok {
		return nil
	}
	bind, ok := grokChatBind(provider, upstreamModel, emit.scope, emit.comboID)
	if !ok {
		return nil
	}
	return newGrokChatOrder(bind, p.bridgeMAC, res, observability.Logger(ctx, p.logger), ingress.id)
}

func wrapGrokChatEncoder(enc streamEncoder, order *grokChatOrder) streamEncoder {
	if order == nil {
		return enc
	}
	inner, ok := enc.(*openai.StreamEncoder)
	if !ok {
		return enc
	}
	return &grokChatEncoder{inner: inner, order: order}
}

func (e *grokChatEncoder) Encode(ev ir.StreamEvent, w *sse.Writer) error {
	switch ev.Kind {
	case ir.EventReasoningReserve, ir.EventReasoningItemDone:
		// Snapshots arrive through the observer, not this emit path.
		return nil
	case ir.EventTextStart, ir.EventTextDelta, ir.EventToolCallStart, ir.EventToolCallDelta:
		e.order.noteVisible(ev)
		return e.inner.Encode(ev, w)
	case ir.EventFinish:
		if err := e.flush(w); err != nil {
			return err
		}
		return e.inner.Encode(ev, w)
	default:
		return e.inner.Encode(ev, w)
	}
}

func (e *grokChatEncoder) EncodeError(w *sse.Writer, message, errType string) error {
	return e.inner.EncodeError(w, message, errType)
}

func (e *grokChatEncoder) Close(w *sse.Writer) error {
	if err := e.flush(w); err != nil {
		return err
	}
	return e.inner.Close(w)
}

func (e *grokChatEncoder) flush(w *sse.Writer) error {
	if e == nil || e.order == nil || e.flushed {
		return nil
	}
	e.flushed = true
	details, ok := e.order.details()
	// A bridge-only retention or budget miss omits the marker. Readable output,
	// usage, finish, and [DONE] still proceed.
	if !ok {
		if e.order.sawOpaque {
			noteResponsesOmission(e.order.res, e.order.logger, e.order.ingressID)
		}
		return nil
	}
	if err := e.inner.EmitReasoningDetails(w, details); err != nil {
		// The marker is optional. A frame that does not fit is skipped. Visible
		// text, finish, usage, and [DONE] still proceed.
		if errors.Is(err, openai.ErrReasoningFrameOmitted) {
			noteResponsesOmission(e.order.res, e.order.logger, e.order.ingressID)
			return nil
		}
		return err
	}
	return nil
}
