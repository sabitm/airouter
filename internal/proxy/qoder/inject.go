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
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("qoder: inject model_config: %w", err)
	}
	var cfgObj map[string]any
	if err := json.Unmarshal(config, &cfgObj); err != nil {
		return nil, fmt.Errorf("qoder: invalid model_config: %w", err)
	}
	m["model_config"] = cfgObj

	isReasoning, _ := cfgObj["is_reasoning"].(bool)
	if cc, ok := m["chat_context"].(map[string]any); ok {
		if extra, ok := cc["extra"].(map[string]any); ok {
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["is_reasoning"] = isReasoning
				extra["modelConfig"] = mc
			}
			cc["extra"] = extra
			m["chat_context"] = cc
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
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	params, _ := m["parameters"].(map[string]any)
	if params == nil {
		return body
	}
	cur, _ := params["max_tokens"].(float64)
	if int(cur) <= 0 || int(cur) > cfg.MaxOutputTokens {
		params["max_tokens"] = cfg.MaxOutputTokens
		m["parameters"] = params
		if out, err := json.Marshal(m); err == nil {
			return out
		}
	}
	return body
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
