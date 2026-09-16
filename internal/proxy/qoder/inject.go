package qoder

import (
	"context"
	"encoding/json"
	"fmt"

	"airouter/internal/domain"
)

// PrepareWireBody injects the live model_config into the plaintext Qoder JSON
// and applies WAF body encoding. The returned bytes are what COSY must sign
// and what the HTTP client posts.
func PrepareWireBody(ctx context.Context, provider *domain.Provider, plainBody []byte) ([]byte, error) {
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
	// Clamp max_tokens from model_config when present.
	injected = clampMaxTokens(injected, cfg)
	return EncodeBody(injected), nil
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

func clampMaxTokens(body []byte, config json.RawMessage) []byte {
	var cfg struct {
		MaxOutputTokens int `json:"max_output_tokens"`
	}
	if json.Unmarshal(config, &cfg) != nil || cfg.MaxOutputTokens <= 0 {
		return body
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
	shouldClamp := false
	if mt, ok := params["max_tokens"]; !ok {
		shouldClamp = true
	} else {
		var cur int
		if json.Unmarshal(mt, &cur) != nil {
			return body
		}
		if cur <= 0 || cur > cfg.MaxOutputTokens {
			shouldClamp = true
		}
	}
	if !shouldClamp {
		return body
	}
	raw, err := json.Marshal(cfg.MaxOutputTokens)
	if err != nil {
		return body
	}
	params["max_tokens"] = raw
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
