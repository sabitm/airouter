package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"airouter/internal/domain"
)

const (
	codexCacheKeyVersion  = "codex-cache-v1"
	codexAnchorVersion    = "codex-anchor-v1"
	codexMaxIdentityBytes = 256
	// Quotes plus worst-case \uXXXX expansion of a max-length identity.
	codexMaxIdentityJSONBytes = 2 + 6*codexMaxIdentityBytes
	codexMaxAnchorScanBytes   = 64 << 10
	codexMaxPrefixItems       = 16
	codexMaxScanItems         = 64
	// openTenantScope marks unauthenticated open-mode requests. It is not a
	// shared cache namespace: open-mode still uses request-local fallback
	// material when the client sends no identity and no conversation anchor.
	openTenantScope = "open"
)

type codexReqState struct {
	identity string
	fallback string
	tenant   string
	key      string
}

type codexStateKeyT struct{}

var codexStateKey codexStateKeyT

func withCodexRequest(ctx context.Context, tenant string, h http.Header, body []byte) context.Context {
	if tenant == "" {
		tenant = openTenantScope
	}
	st := &codexReqState{
		identity: captureCodexIdentity(h, body),
		tenant:   tenant,
	}
	if st.identity == "" {
		if anchor := conversationAnchor(body); anchor != "" {
			st.fallback = "conv:" + anchor
		} else {
			// Request-local random material is stable across retries of this
			// request and differs for other unidentified requests.
			st.fallback = "rand:" + randomCodexMaterial()
		}
	}
	return context.WithValue(ctx, codexStateKey, st)
}

func codexStateFrom(ctx context.Context) *codexReqState {
	st, _ := ctx.Value(codexStateKey).(*codexReqState)
	return st
}

func hasCodexTarget(targets []domain.ComboTarget) bool {
	for _, target := range targets {
		if target.Provider != nil && target.Provider.Protocol == domain.ProtocolOpenAICodex {
			return true
		}
	}
	return false
}

type codexIdentityBody struct {
	PromptCacheKey codexIdentityField `json:"prompt_cache_key"`
	SessionID      codexIdentityField `json:"session_id"`
	ConversationID codexIdentityField `json:"conversation_id"`
}

// codexIdentityField keeps a normalized identity or empty. UnmarshalJSON
// never returns an error: a bad value must not discard other body fields.
type codexIdentityField string

func (f *codexIdentityField) UnmarshalJSON(data []byte) error {
	*f = ""
	// Reject before unquoting. encoding/json already copied this token.
	if len(data) == 0 || len(data) > codexMaxIdentityJSONBytes || data[0] != '"' {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	*f = codexIdentityField(normalizeCodexIdentity(s))
	return nil
}

// captureCodexIdentity returns the first valid client identity. Invalid
// candidates are skipped; nothing is truncated or forwarded raw.
func captureCodexIdentity(h http.Header, body []byte) string {
	var fields codexIdentityBody
	if len(body) > 0 {
		_ = json.Unmarshal(body, &fields)
	}
	if fields.PromptCacheKey != "" {
		return string(fields.PromptCacheKey)
	}
	for _, key := range []string{"session_id", "x-session-affinity", "x-session-id", "session-id"} {
		if s := singleNormalizedHeader(h, key); s != "" {
			return s
		}
	}
	if fields.SessionID != "" {
		return string(fields.SessionID)
	}
	if fields.ConversationID != "" {
		return string(fields.ConversationID)
	}
	return singleNormalizedHeader(h, "x-client-request-id")
}

func singleNormalizedHeader(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	values := h.Values(key)
	if len(values) != 1 {
		return ""
	}
	return normalizeCodexIdentity(values[0])
}

func normalizeCodexIdentity(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > codexMaxIdentityBytes {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 || c == 0x7F {
			return ""
		}
	}
	return v
}

func randomCodexMaterial() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func codexProviderScope(p *domain.Provider) string {
	if p == nil {
		return "provider:none"
	}
	idPart := ""
	if p.ID != 0 {
		idPart = "id:" + strconv.FormatInt(p.ID, 10)
	} else {
		idPart = "url:" + strings.ToLower(strings.TrimRight(strings.TrimSpace(p.BaseURL), "/"))
	}
	acct := ""
	if p.OAuthCreds != nil {
		acct = strings.TrimSpace(p.OAuthCreds.AccountID)
	}
	if acct != "" {
		return idPart + "\x00acct:" + acct
	}
	return idPart
}

// hashCodexKey builds the 64-hex upstream key. Client affinity is authoritative.
// Identical initial conversation prefixes without affinity can collide.
func hashCodexKey(tenant, providerScope, identity, fallback string) string {
	client := identity
	if client == "" {
		if fallback != "" {
			client = fallback
		} else {
			client = "rand:" + randomCodexMaterial()
		}
	}
	sum := sha256.Sum256([]byte(
		codexCacheKeyVersion + "\x00" +
			tenant + "\x00" +
			providerScope + "\x00" +
			client,
	))
	return hex.EncodeToString(sum[:])
}

func conversationAnchor(body []byte) string {
	prefix, user, ok := extractConversationPrefix(body)
	if !ok {
		return ""
	}
	canon, err := json.Marshal(struct {
		Prefix []any `json:"p,omitempty"`
		User   any   `json:"u,omitempty"`
	}{Prefix: prefix, User: user})
	if err != nil || len(canon) == 0 || string(canon) == "{}" {
		return ""
	}
	// Hash the complete canonical JSON. Prefix extraction already bounds
	// scan bytes and item counts; truncating here would collide distinct
	// prefixes after JSON escaping expands content.
	sum := sha256.Sum256(append([]byte(codexAnchorVersion+"\x00"), canon...))
	return hex.EncodeToString(sum[:])
}

// extractConversationPrefix scans at most codexMaxAnchorScanBytes of the
// request. It never unmarshals the complete body; a usable messages/input
// prefix returns immediately. Instructions are included only if they appear
// before input.
func extractConversationPrefix(body []byte) (prefix []any, user any, ok bool) {
	if len(body) == 0 {
		return nil, nil, false
	}
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(body), int64(codexMaxAnchorScanBytes)))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	var inst []any
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		key, isString := keyTok.(string)
		if !isString {
			return nil, nil, false
		}
		switch key {
		case "messages":
			p, u, found, fail := readConversationArray(dec)
			if fail {
				return nil, nil, false
			}
			if found {
				return p, u, true
			}
		case "input":
			p, u, found, fail := readConversationInput(dec)
			if fail {
				return nil, nil, false
			}
			if found {
				if len(inst) > 0 {
					p = append(inst, p...)
				}
				return p, u, true
			}
		case "instructions":
			s, fail := readOptionalString(dec)
			if fail {
				return nil, nil, false
			}
			if s != "" {
				inst = []any{map[string]any{"role": "system", "content": s}}
			}
		default:
			if err := skipJSONValue(dec); err != nil {
				return nil, nil, false
			}
		}
	}
	return nil, nil, false
}

func readOptionalString(dec *json.Decoder) (s string, fail bool) {
	tok, err := dec.Token()
	if err != nil {
		return "", true
	}
	if v, ok := tok.(string); ok {
		return strings.TrimSpace(v), false
	}
	if d, ok := tok.(json.Delim); ok {
		return "", skipJSONRest(dec, d) != nil
	}
	return "", false
}

func readConversationInput(dec *json.Decoder) (prefix []any, user any, found, fail bool) {
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, false, true
	}
	if v, ok := tok.(string); ok {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, nil, false, false
		}
		return nil, v, true, false
	}
	if tok != json.Delim('[') {
		if d, ok := tok.(json.Delim); ok {
			return nil, nil, false, skipJSONRest(dec, d) != nil
		}
		return nil, nil, false, false
	}
	return readConversationArrayElems(dec)
}

func readConversationArray(dec *json.Decoder) (prefix []any, user any, found, fail bool) {
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, false, true
	}
	if tok != json.Delim('[') {
		if d, ok := tok.(json.Delim); ok {
			return nil, nil, false, skipJSONRest(dec, d) != nil
		}
		return nil, nil, false, false
	}
	return readConversationArrayElems(dec)
}

func readConversationArrayElems(dec *json.Decoder) (prefix []any, user any, found, fail bool) {
	n := 0
	for dec.More() {
		n++
		if n > codexMaxScanItems {
			return nil, nil, false, true
		}
		var item any
		if err := dec.Decode(&item); err != nil {
			return nil, nil, false, true
		}
		switch itemRole(item) {
		case "system", "developer":
			if len(prefix) < codexMaxPrefixItems {
				prefix = append(prefix, item)
			}
		case "user":
			return prefix, item, true, false
		default:
			if isUserTypedItem(item) {
				return prefix, item, true, false
			}
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return nil, nil, false, true
	}
	return prefix, nil, false, false
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	return skipJSONRest(dec, d)
}

func skipJSONRest(dec *json.Decoder, open json.Delim) error {
	if open != '{' && open != '[' {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			continue
		}
		switch d {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return nil
}

func itemRole(item any) string {
	m, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	r, _ := m["role"].(string)
	return strings.ToLower(strings.TrimSpace(r))
}

func isUserTypedItem(item any) bool {
	switch v := item.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case map[string]any:
		if _, hasRole := v["role"].(string); hasRole {
			return false
		}
		t, _ := v["type"].(string)
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "input_text", "input_image", "input_file", "message":
			return true
		}
	}
	return false
}

func codexSessionID(ctx context.Context) string {
	if t := traceInfoFrom(ctx); t != nil && t.CodexSessionID != "" {
		return t.CodexSessionID
	}
	if st := codexStateFrom(ctx); st != nil {
		return st.key
	}
	return ""
}

// resolveCodexCacheKey derives the 64-character hex cache key for this
// request and provider. Without captured request state (narrow unit tests),
// a nonempty random fallback is hashed so the upstream still receives a key.
func resolveCodexCacheKey(ctx context.Context, provider *domain.Provider) string {
	st := codexStateFrom(ctx)
	var id string
	if st == nil {
		id = hashCodexKey(openTenantScope, codexProviderScope(provider), "", randomCodexMaterial())
	} else {
		id = hashCodexKey(st.tenant, codexProviderScope(provider), st.identity, st.fallback)
		st.key = id
	}
	return id
}
