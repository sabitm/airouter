package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
)

type grokBuildReqState struct {
	tenant   string
	identity string
	reqID    string
}

type grokBuildStateKeyT struct{}

var grokBuildStateKey grokBuildStateKeyT

func withGrokBuildRequest(ctx context.Context, tenant string, h http.Header, body []byte) context.Context {
	st := &grokBuildReqState{
		tenant:   tenant,
		identity: grokBuildIdentity(h, body),
		reqID:    grokbuild.NewReqID(),
	}
	return context.WithValue(ctx, grokBuildStateKey, st)
}

func grokBuildStateFrom(ctx context.Context) *grokBuildReqState {
	st, _ := ctx.Value(grokBuildStateKey).(*grokBuildReqState)
	return st
}

func hasGrokBuildTarget(targets []domain.ComboTarget) bool {
	for _, target := range targets {
		if target.Provider != nil && (target.Provider.Protocol == domain.ProtocolGrokBuild ||
			target.Provider.OAuthCreds != nil && target.Provider.OAuthCreds.GrokBuildAuth) {
			return true
		}
	}
	return false
}

func grokBuildIdentity(h http.Header, body []byte) string {
	if h != nil {
		if v := grokbuild.NormalizeIdentity(h.Get("X-Grok-Session-Id")); v != "" {
			return v
		}
		if v := grokbuild.NormalizeIdentity(h.Get("X-Grok-Conv-Id")); v != "" {
			return v
		}
	}
	if len(body) == 0 || body[0] != '{' {
		return ""
	}
	var doc struct {
		Metadata struct {
			SessionID string `json:"session_id"`
			ConvID    string `json:"conversation_id"`
		} `json:"metadata"`
		SessionID string `json:"session_id"`
		ConvID    string `json:"conversation_id"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	if v := grokbuild.NormalizeIdentity(doc.Metadata.SessionID); v != "" {
		return v
	}
	if v := grokbuild.NormalizeIdentity(doc.SessionID); v != "" {
		return v
	}
	if v := grokbuild.NormalizeIdentity(doc.Metadata.ConvID); v != "" {
		return v
	}
	return grokbuild.NormalizeIdentity(doc.ConvID)
}

func applyGrokBuildHeaders(req *http.Request, provider *domain.Provider, ctx context.Context, body []byte) {
	if req == nil || provider == nil {
		return
	}
	st := grokBuildStateFrom(ctx)
	tenant := ""
	identity := ""
	reqID := ""
	if st != nil {
		tenant = st.tenant
		identity = st.identity
		reqID = st.reqID
	}
	if reqID == "" {
		reqID = grokbuild.NewReqID()
	}
	model := grokBuildModel(body)
	grokbuild.ApplyHeaders(req.Header, body, model, grokBuildAccountID(provider), grokbuild.ScopedIDs(tenant, provider.ID, identity, reqID))
	req.Header.Del("X-Grok-User-Id")
	req.Header.Del("X-Email")
}

func grokBuildAccountID(provider *domain.Provider) string {
	if provider == nil || provider.OAuthCreds == nil {
		return ""
	}
	return grokbuild.NormalizeIdentity(provider.OAuthCreds.UserID)
}

func grokBuildModel(body []byte) string {
	var doc struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.Model)
}
