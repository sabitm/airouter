package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/thinking"
)

// CodexCLIVersion is the Codex CLI version the upstream identifies us as; the
// proxy sends it as the User-Agent and originator.
const CodexCLIVersion = "0.136.0"

// codexDefaultModel is the model the encoder falls back to when the request
// carries none.
const codexDefaultModel = "gpt-5.3-codex"

// defaultCodexInstructions is injected when a request carries no system prompt:
// the Codex backend rejects empty instructions. A minimal, behavior-neutral
// prompt keeps the request valid without imposing intent the caller did not
// express.
const defaultCodexInstructions = "You are Codex, a coding assistant."

// codexEffortSuffix maps a model-name effort suffix to the reasoning.effort
// value the Codex backend expects. An unrecognized suffix (or none) defaults to
// "low", matching the official CLI.
var codexEffortSuffix = map[string]string{
	"none":   "none",
	"low":    "low",
	"medium": "medium",
	"high":   "high",
	"xhigh":  "xhigh",
}

// codexEffortForModel splits a model id into its base and reasoning effort. A
// trailing token the backend recognizes as an effort level (e.g.
// gpt-5.3-codex-high -> base gpt-5.3-codex, effort high) is stripped; any other
// suffix (e.g. -spark) leaves the model id intact and defaults effort to "low".
func codexEffortForModel(model string) (base, effort string) {
	base = model
	effort = "low"
	idx := strings.LastIndex(model, "-")
	if idx < 0 {
		return
	}
	if e, ok := codexEffortSuffix[model[idx+1:]]; ok {
		base = model[:idx]
		effort = e
	}
	return
}

// EncodeCodexRequest renders the IR as a Codex (ChatGPT-backend Responses)
// request body. It reuses the shared Responses input/tool encoding but enforces
// the Codex envelope the upstream requires: store=false, non-empty
// instructions, a model-name effort suffix mapped to reasoning.effort, and the
// reasoning-encrypted-content include when effort != none. Fields the Codex
// backend rejects (temperature, top_p, max_output_tokens) are omitted. The
// prompt_cache_key is injected by the proxy (it must equal the session_id header).
func EncodeCodexRequest(req *ir.Request) ([]byte, error) {
	// Prefer IR thinking (body intent or upstream model(level) suffix). Else the
	// Codex-native hyphen suffix on the model id. Else default low.
	base, hyphenEffort := codexEffortForModel(req.Model)
	if base == "" {
		base = codexDefaultModel
	}
	effort := resolveCodexEffort(req.Thinking, hyphenEffort, base)

	out := map[string]any{
		"model":  base,
		"input":  encodeInput(req),
		"stream": req.Stream,
		"store":  false,
	}
	instructions := req.System
	if instructions == "" {
		instructions = defaultCodexInstructions
	}
	out["instructions"] = instructions

	if effort != "none" {
		out["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
		out["include"] = []string{"reasoning.encrypted_content"}
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := json.RawMessage(t.Parameters)
			if len(params) == 0 {
				params = json.RawMessage("{}")
			}
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			})
		}
		out["tools"] = tools
	}
	if req.ToolChoice != nil {
		out["tool_choice"] = encodeToolChoice(req.ToolChoice)
	}
	return json.Marshal(out)
}

// resolveCodexEffort picks the wire effort: IR thinking first, then the hyphen
// suffix result from codexEffortForModel (already defaulted to low when absent).
// Explicit none is honored (omits reasoning upstream). Level strings are
// forwarded verbatim; callers own per-model validity. CanDisable clamps still
// live in thinking.Effective when used before IR is set.
func resolveCodexEffort(t *ir.Thinking, hyphenEffort, base string) string {
	if t != nil {
		cfg := thinking.FromIR(t)
		switch cfg.Mode {
		case thinking.ModeNone:
			return "none"
		case thinking.ModeAuto:
			return "medium"
		case thinking.ModeBudget:
			if lvl := thinking.BudgetToLevel(cfg.Budget); lvl != "" {
				return lvl
			}
			return "medium"
		case thinking.ModeLevel:
			return cfg.Level
		}
	}
	_ = base
	return hyphenEffort
}

// SyncCodexReasoningInclude keeps encrypted reasoning continuity aligned with
// the effective effort after provider-aware finalization. Explicit none removes
// only the Codex-required include; other include entries are preserved.
// Nested values stay json.RawMessage so number tokens are not coerced to float64.
func SyncCodexReasoningInclude(body []byte) ([]byte, error) {
	m, err := unmarshalObjectRaw(body)
	if err != nil {
		return nil, err
	}
	effort := ""
	if raw, ok := m["reasoning"]; ok {
		var reasoning map[string]json.RawMessage
		if json.Unmarshal(raw, &reasoning) == nil && reasoning != nil {
			if eRaw, ok := reasoning["effort"]; ok {
				_ = json.Unmarshal(eRaw, &effort)
			}
		}
	}
	var includes []json.RawMessage
	if raw, ok := m["include"]; ok {
		_ = json.Unmarshal(raw, &includes)
	}
	filtered := make([]json.RawMessage, 0, len(includes)+1)
	for _, item := range includes {
		var value string
		if json.Unmarshal(item, &value) == nil && value == "reasoning.encrypted_content" {
			continue
		}
		filtered = append(filtered, item)
	}
	if effort != "" && effort != "none" {
		filtered = append(filtered, json.RawMessage(`"reasoning.encrypted_content"`))
	}
	if len(filtered) == 0 {
		delete(m, "include")
	} else {
		raw, err := json.Marshal(filtered)
		if err != nil {
			return nil, err
		}
		m["include"] = raw
	}
	return json.Marshal(m)
}

// InjectCodexRequestKey sets prompt_cache_key on an already-encoded Codex
// request body. Nested values stay json.RawMessage so number tokens are not
// coerced to float64. Invalid JSON, a non-object, or top-level null fails closed.
func InjectCodexRequestKey(body []byte, key string) ([]byte, error) {
	m, err := unmarshalObjectRaw(body)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	m["prompt_cache_key"] = raw
	return json.Marshal(m)
}

func unmarshalObjectRaw(body []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return m, nil
}
