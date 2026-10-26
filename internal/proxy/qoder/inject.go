package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"airouter/internal/domain"
)

// Prepared carries the encoded wire body plus the identity fields the proxy
// needs for queue-status polling and X-Model-* headers.
type Prepared struct {
	Wire         []byte
	ModelKey     string
	ModelSource  string
	RequestID    string
	RequestSetID string
	SessionID    string
}

// PrepareWireBody injects the live model_config into the plaintext Qoder JSON,
// sets the account-scoped session id, selects the final effective max_tokens,
// and applies WAF body encoding. The returned Wire bytes are what COSY must
// sign and what the HTTP client posts. requestedMaxTokens is the caller's
// explicit output limit from the IR; it is not written into the plaintext as
// private metadata, only used to select the final effective value.
func PrepareWireBody(ctx context.Context, provider *domain.Provider, requestedMaxTokens int, plainBody []byte) (*Prepared, error) {
	key := ModelKeyFromBody(plainBody)
	if key == "" {
		return nil, fmt.Errorf("qoder: missing model key in request body")
	}
	cfg, err := LookupModelConfig(ctx, provider, key)
	if err != nil {
		return nil, err
	}
	injected, err := InjectModelConfig(plainBody, cfg)
	if err != nil {
		return nil, err
	}
	creds := CredsFromProvider(provider)
	// The encoder cannot see the provider, so prepare owns the account-scoped
	// session id. Overwrite unconditionally, including an empty user id.
	injected, err = withSessionID(injected, creds.UserID, key)
	if err != nil {
		return nil, err
	}
	injected = applyOutputLimit(injected, cfg, requestedMaxTokens)
	// The effective max_tokens can move after the catalog lookup (a catalog
	// limit above the 32768 encoder default, or a caller limit between the
	// two). The record ids must describe the final effective request, so
	// recompute them here; unchanged requests keep their embedded id.
	injected, err = refreshRecordIDs(injected)
	if err != nil {
		return nil, err
	}
	prep := &Prepared{
		Wire:        EncodeBody(injected),
		ModelKey:    key,
		ModelSource: ModelSourceFromConfig(cfg),
	}
	prep.RequestID, prep.RequestSetID, prep.SessionID = RequestIdentity(injected)
	return prep, nil
}

// withSessionID overwrites the top-level session_id with the reference value
// stableHash("qoder-session", userID, modelKey). It rejects malformed JSON and
// non-object roots instead of panicking.
func withSessionID(body []byte, userID, modelKey string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("qoder: set session_id: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("qoder: set session_id: not a JSON object")
	}
	raw, err := json.Marshal(stableHash("qoder-session", userID, modelKey))
	if err != nil {
		return nil, err
	}
	m["session_id"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// InjectModelConfig sets model_config on a plaintext Qoder JSON body and
// updates chat_context.extra.modelConfig.is_reasoning from the catalog entry.
func InjectModelConfig(body []byte, config json.RawMessage) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("qoder: inject model_config: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("qoder: inject model_config: not a JSON object")
	}
	var cfgObj map[string]json.RawMessage
	if err := json.Unmarshal(config, &cfgObj); err != nil {
		return nil, fmt.Errorf("qoder: invalid model_config: %w", err)
	}
	if cfgObj == nil {
		return nil, fmt.Errorf("qoder: invalid model_config: not a JSON object")
	}
	copied := make(json.RawMessage, len(config))
	copy(copied, config)
	m["model_config"] = copied

	isReasoning := false
	if raw, ok := cfgObj["is_reasoning"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			isReasoning = b
		}
	}
	if ccRaw, ok := m["chat_context"]; ok {
		var cc map[string]json.RawMessage
		if json.Unmarshal(ccRaw, &cc) == nil && cc != nil {
			if extraRaw, ok := cc["extra"]; ok {
				var extra map[string]json.RawMessage
				if json.Unmarshal(extraRaw, &extra) == nil && extra != nil {
					if mcRaw, ok := extra["modelConfig"]; ok {
						var mc map[string]json.RawMessage
						if json.Unmarshal(mcRaw, &mc) == nil && mc != nil {
							iraw, err := json.Marshal(isReasoning)
							if err != nil {
								return nil, err
							}
							mc["is_reasoning"] = iraw
							mcBytes, err := json.Marshal(mc)
							if err != nil {
								return nil, err
							}
							extra["modelConfig"] = mcBytes
							extraBytes, err := json.Marshal(extra)
							if err != nil {
								return nil, err
							}
							cc["extra"] = extraBytes
							ccBytes, err := json.Marshal(cc)
							if err != nil {
								return nil, err
							}
							m["chat_context"] = ccBytes
						}
					}
				}
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// applyOutputLimit writes the final effective max_tokens: the catalog output
// limit when positive, else the encoder's 32768 default, then the caller's
// lower explicit limit wins. A catalog limit above 32768 raises the default; a
// caller limit above the catalog is clamped down. An unparsable current value
// keeps the body unchanged rather than guessing.
func applyOutputLimit(body []byte, config json.RawMessage, requested int) []byte {
	ceiling := DefaultMaxTokens
	var cfg struct {
		MaxOutputTokens int `json:"max_output_tokens"`
	}
	if json.Unmarshal(config, &cfg) == nil && cfg.MaxOutputTokens > 0 {
		ceiling = cfg.MaxOutputTokens
	}
	final := ceiling
	if requested > 0 && requested < final {
		final = requested
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	paramsRaw, ok := m["parameters"]
	if !ok {
		return body
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(paramsRaw, &params) != nil || params == nil {
		return body
	}
	raw, present := params["max_tokens"]
	if present {
		cur, ok := rawJSONInt(raw)
		if !ok {
			// A value this code did not write and cannot interpret: leave the
			// body unchanged rather than guessing.
			return body
		}
		if cur > 0 && cur == final {
			return body
		}
	}
	params["max_tokens"] = json.RawMessage(strconv.Itoa(final))
	patched, err := json.Marshal(params)
	if err != nil {
		return body
	}
	m["parameters"] = patched
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// rawJSONInt decodes a strict JSON integer. Fractions, strings, and null are
// rejected so a value this code did not write is never silently reinterpreted.
func rawJSONInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n int
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

// refreshRecordIDs recomputes chat_record_id and request_set_id from the
// final prepared body. The recompute is a no-op when the inputs still hash to
// the embedded id, so an unchanged effective request never changes identity.
// Identity values are re-extracted with raw-message preservation so large
// schema numbers keep their exact bytes instead of surviving a float64
// round trip.
func refreshRecordIDs(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("qoder: refresh record ids: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("qoder: refresh record ids: not a JSON object")
	}
	maxTokens, maxOK := 0, false
	if paramsRaw, ok := m["parameters"]; ok {
		var params map[string]json.RawMessage
		if json.Unmarshal(paramsRaw, &params) == nil && params != nil {
			if mt, ok := rawJSONInt(params["max_tokens"]); ok {
				maxTokens, maxOK = mt, true
			}
		}
	}
	if !maxOK {
		// No parsable effective max_tokens: keep the encoder's ids rather than
		// hashing an identity that cannot be reconstructed from the body.
		return body, nil
	}
	msgs, err := rawPreservingList(m["messages"])
	if err != nil {
		return nil, fmt.Errorf("qoder: refresh record ids: %w", err)
	}
	tools, err := rawPreservingList(m["tools"])
	if err != nil {
		return nil, fmt.Errorf("qoder: refresh record ids: %w", err)
	}
	system := ""
	if raw, ok := m["system"]; ok {
		_ = json.Unmarshal(raw, &system)
	}
	id, err := stableChatRecordID(ModelKeyFromBody(body), system, msgs, tools, maxTokens)
	if err != nil {
		return nil, err
	}
	var current struct {
		Chat string `json:"chat_record_id"`
		Set  string `json:"request_set_id"`
	}
	_ = json.Unmarshal(body, &current)
	if id == current.Chat && id == current.Set {
		return body, nil
	}
	enc, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	m["request_set_id"] = enc
	m["chat_record_id"] = enc
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// rawPreservingList converts a JSON array of objects into the encoder's
// []map[string]any shape with every leaf kept as json.RawMessage, so
// re-marshaling reproduces the original bytes: encoding/json sorts map keys
// and writes RawMessage values verbatim.
func rawPreservingList(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, err
		}
		if fields == nil {
			fields = map[string]json.RawMessage{}
		}
		m := make(map[string]any, len(fields))
		for k, v := range fields {
			m[k] = json.RawMessage(v)
		}
		out = append(out, m)
	}
	return out, nil
}

// RequestIdentity reads the identity fields the proxy needs for queue-status
// polling from a plaintext body.
func RequestIdentity(body []byte) (requestID, requestSetID, sessionID string) {
	var f struct {
		RequestID    string `json:"request_id"`
		RequestSetID string `json:"request_set_id"`
		SessionID    string `json:"session_id"`
	}
	if json.Unmarshal(body, &f) != nil {
		return "", "", ""
	}
	return f.RequestID, f.RequestSetID, f.SessionID
}

// CredsFromProvider builds COSY identity from a hydrated provider.
// Auth token prefers APIKey (post-Resolve) then OAuthCreds.AccessToken.
func CredsFromProvider(provider *domain.Provider) Creds {
	c := Creds{}
	if provider == nil {
		return c
	}
	if provider.APIKey != "" {
		c.AuthToken = provider.APIKey
	}
	if provider.OAuthCreds != nil {
		oc := provider.OAuthCreds
		c.UserID = oc.UserID
		c.MachineID = oc.MachineID
		c.Name = oc.DisplayName
		c.Email = oc.Email
		if c.AuthToken == "" {
			c.AuthToken = oc.AccessToken
		}
	}
	return c
}

// ModelSourceFromConfig returns X-Model-Source from a catalog entry.
func ModelSourceFromConfig(config json.RawMessage) string {
	var cfg struct {
		Source string `json:"source"`
	}
	if json.Unmarshal(config, &cfg) == nil && cfg.Source != "" {
		return cfg.Source
	}
	return "system"
}

// ModelKeyAndSourceFromWire is used after inject to set X-Model-* headers.
// It reads from the *plaintext* body before encode; callers should extract
// before EncodeBody or from the plain inject step.
func ModelKeyAndSource(plainInjected []byte) (key, source string) {
	key = ModelKeyFromBody(plainInjected)
	var m struct {
		ModelConfig json.RawMessage `json:"model_config"`
	}
	if json.Unmarshal(plainInjected, &m) == nil {
		source = ModelSourceFromConfig(m.ModelConfig)
	}
	if source == "" {
		source = "system"
	}
	return key, source
}
