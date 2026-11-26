package proxy

import (
	"context"
	"net/http"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/proxy/ir"
)

type chatReplayAttempt struct {
	notes       []grokbuild.AssistantNote
	pinnedModel string
}

type chatReplayAttemptKey struct{}

func withChatReplayAttempt(ctx context.Context, notes []grokbuild.AssistantNote, pinnedModel string) context.Context {
	return context.WithValue(ctx, chatReplayAttemptKey{}, chatReplayAttempt{notes: notes, pinnedModel: pinnedModel})
}

// Reload only a normally selected target with a potential match. This must not
// change the order of attempts, and the resolved credentials still need the
// send-time check.
func (p *Proxy) freshChatReplayTarget(ctx context.Context, t domain.ComboTarget, notes []grokbuild.AssistantNote) (domain.ComboTarget, bool) {
	for _, note := range notes {
		if note.Turn == nil {
			continue
		}
		bind := note.Turn.Bind
		switch note.Protocol {
		case grokbuild.GrokReplayProfile().Protocol:
			if grokChatSourceAvailable(t, bind) {
				return p.freshGrokChatTarget(ctx, t, bind)
			}
		case grokbuild.CodexReplayProfile().Protocol:
			if codexChatSourceAvailable(t, bind) {
				return p.freshCodexChatTarget(ctx, t, bind)
			}
		case grokbuild.XAIReplayProfile().Protocol:
			if xaiChatSourceAvailable(t, bind) {
				return p.freshXAIChatTarget(ctx, t, bind)
			}
		}
	}
	return t, false
}

// Use the finalized readable request to identify the wire model. A Codex
// route can have more than one wire model depending on effort. The send guard
// checks the body again after any replay encoding and preparation.
func restoreChatReplay(ctx context.Context, req *ir.Request, backend codec, provider *domain.Provider, routeModel string, body []byte) (context.Context, []byte, error) {
	attempt, ok := ctx.Value(chatReplayAttemptKey{}).(chatReplayAttempt)
	if !ok {
		return ctx, body, nil
	}
	emit, ok := grokChatEmitFrom(ctx)
	if !ok {
		return ctx, body, nil
	}
	model := strings.TrimSpace(reqModelFromBody(body))
	var bind grokbuild.ChatBind
	var profile grokbuild.ReplayProfile
	switch backend.id {
	case codexCodec.id:
		if attempt.pinnedModel != "" && attempt.pinnedModel != model {
			return ctx, nil, errCodexReplayModelMismatch
		}
		profile = grokbuild.CodexReplayProfile()
		bind, ok = codexChatBind(provider, routeModel, model, emit.scope, emit.comboID)
	case grokBuildCodec.id:
		profile = grokbuild.GrokReplayProfile()
		bind, ok = grokChatBind(provider, routeModel, emit.scope, emit.comboID)
		ok = ok && model == bind.Model
	case responsesCodec.id:
		if attempt.pinnedModel != "" && attempt.pinnedModel != model {
			return ctx, nil, errXAIReplayModelMismatch
		}
		profile = grokbuild.XAIReplayProfile()
		bind, ok = xaiChatBind(provider, routeModel, model, emit.scope, emit.comboID)
	default:
		return ctx, body, nil
	}
	if !ok {
		return ctx, body, nil
	}
	applied, err := applyChatReplay(req, attempt.notes, profile.Protocol, bind)
	if err != nil || !applied {
		return ctx, body, err
	}
	body, err = backend.encodeRequest(req)
	if err != nil {
		return ctx, nil, err
	}
	body, err = finalizeEncodedBody(body, req, backend, provider)
	if err != nil {
		return ctx, nil, err
	}
	switch backend.id {
	case codexCodec.id:
		ctx = withCodexReplayGuard(ctx, codexReplayGuard{
			providerID: bind.ProviderID, generation: bind.Generation, principal: bind.Principal,
			endpoint: bind.Endpoint, model: bind.Model, routeModel: bind.RouteModel,
		})
	case grokBuildCodec.id:
		ctx = withGrokBuildReplayGuard(ctx, grokBuildReplayGuard{
			providerID: bind.ProviderID, generation: bind.Generation, principal: bind.Principal, model: bind.Model,
		})
	case responsesCodec.id:
		ctx = withXAIReplayGuard(ctx, xaiReplayGuard{
			providerID: bind.ProviderID, generation: bind.Generation, principal: bind.Principal,
			endpoint: bind.Endpoint, model: bind.Model, routeModel: bind.RouteModel,
		})
	}
	return ctx, body, nil
}

func isChatReplayDecline(logErr string) bool {
	switch logErr {
	case skipLogGrokReplay, skipLogCodexReplay, skipLogCodexModel, skipLogXAIReplay, skipLogXAIModel:
		return true
	default:
		return false
	}
}

func chatReplayBuildError(err error) attemptResult {
	if ar, ok := mapChatReplayErr(err); ok {
		return ar
	}
	if err == grokbuild.ErrChatBridgeRejected || err == grokbuild.ErrCodexReplayRejected {
		return terminal(http.StatusBadRequest, chatReplayRejectedMessage(err), "invalid_request_error")
	}
	return terminalEncodeError(err)
}

func applyChatReplay(req *ir.Request, notes []grokbuild.AssistantNote, protocol string, bind grokbuild.ChatBind) (bool, error) {
	rejected := grokbuild.ErrChatBridgeRejected
	if protocol == grokbuild.CodexReplayProfile().Protocol {
		rejected = grokbuild.ErrCodexReplayRejected
	}
	applied := false
	n := 0
	for i := range req.Messages {
		msg := &req.Messages[i]
		if msg.Role != ir.RoleAssistant {
			continue
		}
		if n >= len(notes) {
			return false, rejected
		}
		note := notes[n]
		n++
		if note.Turn == nil || note.Protocol != protocol || note.Turn.Bind != bind {
			continue
		}
		next, err := grokbuild.ApplyChatTurn(msg.Content, *note.Turn)
		if err != nil {
			return false, rejected
		}
		msg.Content = next
		applied = true
	}
	if n != len(notes) {
		return false, rejected
	}
	return applied, nil
}
