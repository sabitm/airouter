package responses

import (
	"encoding/json"
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
// prompt_cache_key is injected by the proxy (it must be a stable per-request id
// shared with the session_id header).
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
// Explicit none is honored (omits reasoning upstream). Codex cannot fully disable
// on some accounts; callers that need clamp use thinking.Effective before IR.
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
				if lvl == "max" {
					return "xhigh"
				}
				return lvl
			}
			return "medium"
		case thinking.ModeLevel:
			lvl := cfg.Level
			if lvl == "max" {
				return "xhigh"
			}
			if lvl == "minimal" {
				return "low"
			}
			return lvl
		}
	}
	_ = base
	return hyphenEffort
}

// InjectCodexRequestKey sets prompt_cache_key on an already-encoded Codex
// request body. It is a no-op parse/patch so the encoder stays free of the
// per-request id, which the proxy generates alongside the session_id header.
// Returns the body unchanged (and the error) if the body is not a JSON object.
func InjectCodexRequestKey(body []byte, key string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["prompt_cache_key"] = key
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
