package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FingerprintTools are the lowercase names the Zen free-tier gate expects.
// They are decoys when the caller did not declare that member. A caller tool
// named shell stays a caller tool; bash is added beside it when absent.
var FingerprintTools = []string{"bash", "glob", "grep", "read"}

const decoyToolDescription = "This tool is currently unavailable and must not be used."

// decoyToolSchema is an empty object schema. It is not a usable parameter
// schema; the description tells the model not to call the decoy.
var decoyToolSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// ToolMap restores caller tool spellings after a free-tier request rewrite.
// SentToCaller maps the canonical wire name to the original caller spelling.
// Decoys are injected names the caller did not declare. Two distinct caller
// definitions that collapse to one canonical name return an error instead.
type ToolMap struct {
	SentToCaller map[string]string
	Decoys       map[string]bool
}

// Active reports whether response restoration or decoy rejection must run.
func (m ToolMap) Active() bool {
	return len(m.SentToCaller) > 0 || len(m.Decoys) > 0
}

// NeedsFreeTier reports whether a Zen request for model must carry the free
// fingerprint and stream-only transport. The key is not a gate: a real Zen key
// can still call a free model. Go and paid Zen models return false.
func NeedsFreeTier(baseURL, model string) bool {
	if Tier(baseURL) != zenTier {
		return false
	}
	id := NormalizeModelID(model)
	return id == "big-pickle" || strings.HasSuffix(id, "-free")
}

// PrepareFreeTierBody rewrites a free-tier body for one codec family.
// codecID is opencode-chat, opencode-responses, or opencode-messages.
// Colliding caller spellings return an error and do not emit a partial rewrite.
func PrepareFreeTierBody(body []byte, codecID string) ([]byte, ToolMap, error) {
	var m ToolMap
	if len(body) == 0 {
		return body, m, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, m, nil
	}
	switch codecID {
	case "opencode-chat":
		return prepareChatFreeTier(root, body)
	case "opencode-responses":
		return prepareResponsesFreeTier(root, body)
	case "opencode-messages":
		return prepareMessagesFreeTier(root, body)
	default:
		return body, m, nil
	}
}

func prepareChatFreeTier(root map[string]json.RawMessage, original []byte) ([]byte, ToolMap, error) {
	tools, err := decodeRawArray(root["tools"])
	if err != nil {
		return nil, ToolMap{}, err
	}
	hadTools := len(tools) > 0
	rewritten, mapping, changed, err := rewriteNestedTools(tools)
	if err != nil {
		return nil, ToolMap{}, err
	}
	if changed && len(rewritten) > 0 {
		raw, err := json.Marshal(rewritten)
		if err != nil {
			return nil, ToolMap{}, err
		}
		root["tools"] = raw
	}
	if rewriteChatHistory(root, mapping) {
		changed = true
	}
	if retargetNestedChoice(root, mapping) {
		changed = true
	}
	if !hadTools {
		if setStringChoice(root, "none") {
			changed = true
		}
	}
	if !changed {
		return original, mapping, nil
	}
	out, err := json.Marshal(root)
	return out, mapping, err
}

func prepareResponsesFreeTier(root map[string]json.RawMessage, original []byte) ([]byte, ToolMap, error) {
	tools, err := decodeRawArray(root["tools"])
	if err != nil {
		return nil, ToolMap{}, err
	}
	hadTools := len(tools) > 0
	rewritten, mapping, changed, err := rewriteFlatTools(tools, "parameters", true)
	if err != nil {
		return nil, ToolMap{}, err
	}
	if changed && len(rewritten) > 0 {
		raw, err := json.Marshal(rewritten)
		if err != nil {
			return nil, ToolMap{}, err
		}
		root["tools"] = raw
	}
	if rewriteInputNames(root, mapping) {
		changed = true
	}
	if retargetFlatChoice(root, mapping) {
		changed = true
	}
	if !hadTools {
		if setStringChoice(root, "none") {
			changed = true
		}
	}
	if !changed {
		return original, mapping, nil
	}
	out, err := json.Marshal(root)
	return out, mapping, err
}

func prepareMessagesFreeTier(root map[string]json.RawMessage, original []byte) ([]byte, ToolMap, error) {
	tools, err := decodeRawArray(root["tools"])
	if err != nil {
		return nil, ToolMap{}, err
	}
	hadTools := len(tools) > 0
	rewritten, mapping, changed, err := rewriteFlatTools(tools, "input_schema", false)
	if err != nil {
		return nil, ToolMap{}, err
	}
	if changed && len(rewritten) > 0 {
		raw, err := json.Marshal(rewritten)
		if err != nil {
			return nil, ToolMap{}, err
		}
		root["tools"] = raw
	}
	if rewriteMessageToolUses(root, mapping) {
		changed = true
	}
	if retargetAnthropicChoice(root, mapping) {
		changed = true
	}
	// Anthropic has no wire "none". An omitted choice is auto, which can select
	// an injected decoy. Tool-less requests use auto and reject decoy calls on
	// the response path instead of inventing a none mapping.
	if !hadTools {
		mapping.Decoys = fingerprintSet()
	}
	if !changed {
		return original, mapping, nil
	}
	out, err := json.Marshal(root)
	return out, mapping, err
}

// rewriteNestedTools canonicalizes Chat Completions tools in place and appends
// missing fingerprint decoys. Every unrecognized declaration is preserved
// byte-for-byte instead of being dropped. Declarations that share one canonical
// name but differ in spelling return an explicit collision error.
func rewriteNestedTools(tools []json.RawMessage) ([]json.RawMessage, ToolMap, bool, error) {
	mapping := ToolMap{SentToCaller: map[string]string{}, Decoys: map[string]bool{}}
	owners := map[string]string{}
	changed := false
	out := make([]json.RawMessage, 0, len(tools)+len(FingerprintTools))
	for _, raw := range tools {
		var tool map[string]json.RawMessage
		if json.Unmarshal(raw, &tool) != nil || tool == nil {
			out = append(out, raw)
			continue
		}
		fnRaw, hasFn := tool["function"]
		var fn map[string]json.RawMessage
		if !hasFn || json.Unmarshal(fnRaw, &fn) != nil || fn == nil {
			out = append(out, raw)
			continue
		}
		name := rawString(fn["name"])
		key := fingerprintKey(name)
		if key == "" {
			out = append(out, raw)
			continue
		}
		if prev, ok := owners[key]; ok && prev != name {
			return nil, ToolMap{}, false, ambiguousToolError(key, prev, name)
		}
		owners[key] = name
		if name == key {
			out = append(out, raw)
			continue
		}
		fn["name"], _ = json.Marshal(key)
		patchedFn, err := json.Marshal(fn)
		if err != nil {
			return nil, ToolMap{}, false, err
		}
		tool["function"] = patchedFn
		patched, err := json.Marshal(tool)
		if err != nil {
			return nil, ToolMap{}, false, err
		}
		mapping.SentToCaller[key] = name
		changed = true
		out = append(out, patched)
	}
	missing := missingFingerprint(owners)
	if len(missing) == 0 && !changed {
		return nil, mapping, false, nil
	}
	for _, name := range missing {
		decoy, err := decoyNestedTool(name)
		if err != nil {
			return nil, ToolMap{}, false, err
		}
		out = append(out, decoy)
		mapping.Decoys[name] = true
		changed = true
	}
	return out, mapping, true, nil
}

// rewriteFlatTools canonicalizes Responses and Messages tools. includeType adds
// the Responses function discriminator; Anthropic Messages tools omit it.
// Other fields and schemas stay raw so numeric constraints survive.
func rewriteFlatTools(tools []json.RawMessage, schemaKey string, includeType bool) ([]json.RawMessage, ToolMap, bool, error) {
	mapping := ToolMap{SentToCaller: map[string]string{}, Decoys: map[string]bool{}}
	owners := map[string]string{}
	changed := false
	out := make([]json.RawMessage, 0, len(tools)+len(FingerprintTools))
	for _, raw := range tools {
		var tool map[string]json.RawMessage
		if json.Unmarshal(raw, &tool) != nil || tool == nil {
			out = append(out, raw)
			continue
		}
		name := rawString(tool["name"])
		key := fingerprintKey(name)
		if key == "" {
			out = append(out, raw)
			continue
		}
		if prev, ok := owners[key]; ok && prev != name {
			return nil, ToolMap{}, false, ambiguousToolError(key, prev, name)
		}
		owners[key] = name
		if name == key {
			out = append(out, raw)
			continue
		}
		tool["name"], _ = json.Marshal(key)
		patched, err := json.Marshal(tool)
		if err != nil {
			return nil, ToolMap{}, false, err
		}
		mapping.SentToCaller[key] = name
		changed = true
		out = append(out, patched)
	}
	missing := missingFingerprint(owners)
	if len(missing) == 0 && !changed {
		return nil, mapping, false, nil
	}
	for _, name := range missing {
		decoy, err := decoyFlatTool(name, schemaKey, includeType)
		if err != nil {
			return nil, ToolMap{}, false, err
		}
		out = append(out, decoy)
		mapping.Decoys[name] = true
		changed = true
	}
	return out, mapping, true, nil
}

func decoyNestedTool(name string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": decoyToolDescription,
			"parameters":  json.RawMessage(decoyToolSchema),
		},
	})
}

func decoyFlatTool(name, schemaKey string, includeType bool) (json.RawMessage, error) {
	tool := map[string]any{
		"name":        name,
		"description": decoyToolDescription,
		schemaKey:     json.RawMessage(decoyToolSchema),
	}
	if includeType {
		tool["type"] = "function"
	}
	return json.Marshal(tool)
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func missingFingerprint(owners map[string]string) []string {
	var missing []string
	for _, name := range FingerprintTools {
		if _, ok := owners[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// rewriteChatHistory canonicalizes only history tool-call names that exactly
// match the original caller spelling recorded in mapping. An already-canonical
// or unrelated name stays unchanged. Numeric schema constraints inside history
// are preserved because only the name field is patched.
func rewriteChatHistory(root map[string]json.RawMessage, mapping ToolMap) bool {
	if len(mapping.SentToCaller) == 0 {
		return false
	}
	msgs, err := decodeRawArray(root["messages"])
	if err != nil || len(msgs) == 0 {
		return false
	}
	changed := false
	out := make([]json.RawMessage, len(msgs))
	for i, raw := range msgs {
		patched, did := rewriteChatToolCalls(raw, mapping)
		out[i] = patched
		changed = changed || did
	}
	if !changed {
		return false
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return false
	}
	root["messages"] = raw
	return true
}

func rewriteChatToolCalls(raw json.RawMessage, mapping ToolMap) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil || msg == nil {
		return raw, false
	}
	callsRaw, ok := msg["tool_calls"]
	if !ok {
		return raw, false
	}
	var calls []json.RawMessage
	if json.Unmarshal(callsRaw, &calls) != nil {
		return raw, false
	}
	changed := false
	for i, callRaw := range calls {
		var call map[string]json.RawMessage
		if json.Unmarshal(callRaw, &call) != nil || call == nil {
			continue
		}
		fnRaw, ok := call["function"]
		if !ok {
			continue
		}
		var fn map[string]json.RawMessage
		if json.Unmarshal(fnRaw, &fn) != nil || fn == nil {
			continue
		}
		name := rawString(fn["name"])
		key := fingerprintKey(name)
		caller, ok := mapping.SentToCaller[key]
		if !ok || caller != name {
			continue
		}
		fn["name"], _ = json.Marshal(key)
		patchedFn, err := json.Marshal(fn)
		if err != nil {
			continue
		}
		call["function"] = patchedFn
		patchedCall, err := json.Marshal(call)
		if err != nil {
			continue
		}
		calls[i] = patchedCall
		changed = true
	}
	if !changed {
		return raw, false
	}
	patchedCalls, err := json.Marshal(calls)
	if err != nil {
		return raw, false
	}
	msg["tool_calls"] = patchedCalls
	patched, err := json.Marshal(msg)
	if err != nil {
		return raw, false
	}
	return patched, true
}

func rewriteInputNames(root map[string]json.RawMessage, mapping ToolMap) bool {
	if len(mapping.SentToCaller) == 0 {
		return false
	}
	items, err := decodeRawArray(root["input"])
	if err != nil || len(items) == 0 {
		return false
	}
	changed := false
	out := make([]json.RawMessage, len(items))
	for i, raw := range items {
		patched, did := rewriteFunctionCallItem(raw, mapping)
		out[i] = patched
		changed = changed || did
	}
	if !changed {
		return false
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return false
	}
	root["input"] = raw
	return true
}

func rewriteFunctionCallItem(raw json.RawMessage, mapping ToolMap) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil || item == nil {
		return raw, false
	}
	if rawString(item["type"]) != "function_call" {
		return raw, false
	}
	name := rawString(item["name"])
	key := fingerprintKey(name)
	caller, ok := mapping.SentToCaller[key]
	if !ok || caller != name {
		return raw, false
	}
	item["name"], _ = json.Marshal(key)
	patched, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return patched, true
}

func rewriteMessageToolUses(root map[string]json.RawMessage, mapping ToolMap) bool {
	if len(mapping.SentToCaller) == 0 {
		return false
	}
	msgs, err := decodeRawArray(root["messages"])
	if err != nil || len(msgs) == 0 {
		return false
	}
	changed := false
	out := make([]json.RawMessage, len(msgs))
	for i, raw := range msgs {
		var msg map[string]json.RawMessage
		if json.Unmarshal(raw, &msg) != nil || msg == nil {
			out[i] = raw
			continue
		}
		contentRaw, ok := msg["content"]
		if !ok {
			out[i] = raw
			continue
		}
		patched, did := rewriteContentToolUses(contentRaw, mapping)
		if !did {
			out[i] = raw
			continue
		}
		msg["content"] = patched
		patchedMsg, err := json.Marshal(msg)
		if err != nil {
			out[i] = raw
			continue
		}
		out[i] = patchedMsg
		changed = true
	}
	if !changed {
		return false
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return false
	}
	root["messages"] = raw
	return true
}

// rewriteContentToolUses patches tool_use names in one content value. A string
// content (plain user text) cannot contain tool_use blocks and is returned as is.
func rewriteContentToolUses(content json.RawMessage, mapping ToolMap) (json.RawMessage, bool) {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return content, false
	}
	changed := false
	for i, raw := range blocks {
		var block map[string]json.RawMessage
		if json.Unmarshal(raw, &block) != nil || block == nil {
			continue
		}
		if rawString(block["type"]) != "tool_use" {
			continue
		}
		name := rawString(block["name"])
		key := fingerprintKey(name)
		caller, ok := mapping.SentToCaller[key]
		if !ok || caller != name {
			continue
		}
		block["name"], _ = json.Marshal(key)
		patched, err := json.Marshal(block)
		if err != nil {
			continue
		}
		blocks[i] = patched
		changed = true
	}
	if !changed {
		return content, false
	}
	patched, err := json.Marshal(blocks)
	if err != nil {
		return content, false
	}
	return patched, true
}

func retargetNestedChoice(root map[string]json.RawMessage, mapping ToolMap) bool {
	raw, ok := root["tool_choice"]
	if !ok || len(mapping.SentToCaller) == 0 || len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var choice map[string]any
	if json.Unmarshal(raw, &choice) != nil || choice == nil {
		return false
	}
	fn, _ := choice["function"].(map[string]any)
	if fn == nil {
		return false
	}
	name, _ := fn["name"].(string)
	key := fingerprintKey(name)
	if _, ok := mapping.SentToCaller[key]; !ok || name == key {
		return false
	}
	fn["name"] = key
	choice["function"] = fn
	patched, err := json.Marshal(choice)
	if err != nil {
		return false
	}
	root["tool_choice"] = patched
	return true
}

func retargetFlatChoice(root map[string]json.RawMessage, mapping ToolMap) bool {
	raw, ok := root["tool_choice"]
	if !ok || len(mapping.SentToCaller) == 0 || len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var choice map[string]any
	if json.Unmarshal(raw, &choice) != nil || choice == nil {
		return false
	}
	name, _ := choice["name"].(string)
	key := fingerprintKey(name)
	if _, ok := mapping.SentToCaller[key]; !ok || name == key {
		return false
	}
	choice["name"] = key
	patched, err := json.Marshal(choice)
	if err != nil {
		return false
	}
	root["tool_choice"] = patched
	return true
}

func retargetAnthropicChoice(root map[string]json.RawMessage, mapping ToolMap) bool {
	return retargetFlatChoice(root, mapping)
}

func setStringChoice(root map[string]json.RawMessage, value string) bool {
	if _, ok := root["tool_choice"]; ok {
		return false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	root["tool_choice"] = raw
	return true
}

func decodeRawArray(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("opencode: tools must be an array")
	}
	return items, nil
}

// fingerprintKey canonicalizes only case variants of the four fingerprint
// tools. Spaces and other decorations stay caller-owned names.
func fingerprintKey(name string) string {
	key := strings.ToLower(name)
	switch key {
	case "bash", "glob", "grep", "read":
		return key
	default:
		return ""
	}
}

func fingerprintSet() map[string]bool {
	out := make(map[string]bool, len(FingerprintTools))
	for _, name := range FingerprintTools {
		out[name] = true
	}
	return out
}

func ambiguousToolError(key, a, b string) error {
	return fmt.Errorf("opencode: tool names %q and %q both map to %q", a, b, key)
}

// RestoreCallerName maps a wire tool name back to the caller spelling.
// Decoy calls return an error so the caller never sees an injected tool.
// Unknown names pass through.
func RestoreCallerName(name string, mapping ToolMap) (string, error) {
	key := fingerprintKey(name)
	if key == "" {
		key = strings.TrimSpace(name)
	}
	if mapping.Decoys[key] || mapping.Decoys[name] {
		return "", fmt.Errorf("opencode: model called unavailable tool %q", canonicalOr(name, key))
	}
	if caller, ok := mapping.SentToCaller[key]; ok {
		return caller, nil
	}
	if caller, ok := mapping.SentToCaller[name]; ok {
		return caller, nil
	}
	return name, nil
}

func canonicalOr(name, key string) string {
	if key != "" {
		return key
	}
	return name
}
