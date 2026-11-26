package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"airouter/internal/domain"
	"airouter/internal/xaigrant"
)

type xaiChatRequestState struct {
	scope   string
	comboID int64
	hint    string
}

type xaiChatRequestKey struct{}

func withXAIChatRequest(ctx context.Context, scope string, comboID int64, header http.Header, body []byte) context.Context {
	return context.WithValue(ctx, xaiChatRequestKey{}, xaiChatRequestState{
		scope: scope, comboID: comboID, hint: xaiRequestHint(header, body),
	})
}

func xaiChatRequestFrom(ctx context.Context) (xaiChatRequestState, bool) {
	st, ok := ctx.Value(xaiChatRequestKey{}).(xaiChatRequestState)
	return st, ok && st.scope != "" && st.comboID != 0
}

func hasXAIChatTarget(targets []domain.ComboTarget) bool {
	for _, t := range targets {
		if xaigrant.Official(t.Provider) && t.Provider.Protocol == domain.ProtocolOpenAIResponses {
			return true
		}
	}
	return false
}

// xaiRequestHint selects one stable caller affinity value. Explicit
// prompt_cache_key wins, then X-Grok-Conv-Id, then supported Pi session fields.
// Invalid, empty, and overlong values are ignored. Arbitrary headers and
// volatile per-request ids are never forwarded or hashed. A standalone
// X-Client-Request-Id is not a conversation id and is ignored.
func xaiRequestHint(header http.Header, body []byte) string {
	prompt, session, conversation, metaSession, metaConversation := xaiBodyHints(body)
	if v := xaiNormalizeHint(prompt); v != "" {
		return v
	}
	if v := xaiNormalizeHint(header.Get("X-Grok-Conv-Id")); v != "" {
		return v
	}
	for _, v := range []string{session, conversation, metaSession, metaConversation} {
		if hint := xaiNormalizeHint(v); hint != "" {
			return hint
		}
	}
	for _, name := range []string{"X-Session-Id", "Session-Id", "session_id", "X-Session-Affinity"} {
		if hint := xaiNormalizeHint(header.Get(name)); hint != "" {
			return hint
		}
	}
	return ""
}

func xaiBodyHints(body []byte) (prompt, session, conversation, metaSession, metaConversation string) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return
	}
	read := func(raw json.RawMessage) string {
		var s string
		if len(raw) > 0 && json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	prompt = read(root["prompt_cache_key"])
	session = read(root["session_id"])
	conversation = read(root["conversation_id"])
	var meta map[string]json.RawMessage
	if json.Unmarshal(root["metadata"], &meta) == nil && meta != nil {
		metaSession = read(meta["session_id"])
		metaConversation = read(meta["conversation_id"])
	}
	return
}

func xaiNormalizeHint(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 256 {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return ""
		}
	}
	return v
}
