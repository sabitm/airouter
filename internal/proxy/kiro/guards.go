package kiro

import (
	"bytes"
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
)

const (
	emptyUserContent       = "continue"
	toolResultsPlaceholder = "Tool results provided."
	emptyAssistantContent  = "..."
)

// prepareMessages returns a copy that Kiro can accept: user-first, alternating
// roles, unique tool IDs, and adjacent one-to-one tool pairs. Role repair runs
// even when no usable tool catalog exists. Structured tool state is flattened
// in that case instead of being sent as executable state.
func prepareMessages(msgs []ir.Message, catalog toolCatalog) []ir.Message {
	copied := copyMessages(msgs)
	if catalog.empty() {
		copied = flattenToolInteractions(copied)
	}
	copied = mergeAdjacentRoles(copied)
	if len(copied) > 0 && copied[0].Role == ir.RoleAssistant {
		copied = append([]ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: emptyUserContent}}}}, copied...)
	}
	if catalog.empty() {
		return copied
	}
	return reconcileToolPairs(copied, catalog)
}

func copyMessages(msgs []ir.Message) []ir.Message {
	out := make([]ir.Message, len(msgs))
	for i, m := range msgs {
		out[i] = ir.Message{Role: m.Role, Content: append([]ir.ContentBlock(nil), m.Content...)}
	}
	return out
}

func mergeAdjacentRoles(msgs []ir.Message) []ir.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := []ir.Message{msgs[0]}
	for _, m := range msgs[1:] {
		last := &out[len(out)-1]
		if last.Role == m.Role {
			last.Content = append(last.Content, m.Content...)
			continue
		}
		out = append(out, m)
	}
	return out
}

func reconcileToolPairs(msgs []ir.Message, catalog toolCatalog) []ir.Message {
	usedIDs := map[string]bool{}
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role != ir.RoleAssistant {
			if i == 0 {
				msgs[i].Content = flattenOrphanResults(msgs[i].Content)
			}
			continue
		}
		if i+1 >= len(msgs) || msgs[i+1].Role != ir.RoleUser {
			msgs[i].Content = flattenToolUses(msgs[i].Content)
			continue
		}
		msgs[i].Content, msgs[i+1].Content = pairToolTurn(msgs[i].Content, msgs[i+1].Content, catalog, usedIDs)
	}
	return msgs
}

func pairToolTurn(assistant, user []ir.ContentBlock, catalog toolCatalog, usedIDs map[string]bool) ([]ir.ContentBlock, []ir.ContentBlock) {
	type callRec struct {
		block   ir.ContentBlock
		id      string
		name    string
		input   json.RawMessage
		result  *ir.ContentBlock
		invalid bool
	}
	var calls []*callRec
	queues := map[string][]*callRec{}
	var assistantText []ir.ContentBlock
	for i, b := range assistant {
		if b.Type != ir.BlockToolUse {
			assistantText = append(assistantText, b)
			continue
		}
		wire, declared := catalog.toWire[b.ToolName]
		input, inputOK := normalizeToolInput(b.ToolInput)
		rec := &callRec{
			block:   b,
			id:      b.ToolID,
			name:    wire,
			input:   input,
			invalid: !declared || !inputOK,
		}
		calls = append(calls, rec)
		key := b.ToolID
		if key == "" {
			key = "\x00" + base36(i)
		}
		queues[key] = append(queues[key], rec)
	}

	var userText []ir.ContentBlock
	var orphanText []string
	for _, b := range user {
		if b.Type != ir.BlockToolResult {
			userText = append(userText, b)
			continue
		}
		queue := queues[b.ToolUseID]
		var rec *callRec
		for _, candidate := range queue {
			if candidate.result == nil {
				rec = candidate
				break
			}
		}
		if rec == nil || rec.invalid {
			orphanText = append(orphanText, toolResultLine(b))
			continue
		}
		copied := b
		rec.result = &copied
	}

	var keptCalls []ir.ContentBlock
	var keptResults []ir.ContentBlock
	var flatCalls []string
	var flatResults []string
	for i, rec := range calls {
		if rec.invalid || rec.result == nil {
			flatCalls = append(flatCalls, toolCallLine(rec.block.ToolName, rec.block.ToolInput))
			if rec.result != nil {
				flatResults = append(flatResults, toolResultLine(*rec.result))
			}
			continue
		}
		id := reserveToolID(rec.id, i, rec.name, usedIDs)
		keptCalls = append(keptCalls, ir.ContentBlock{
			Type: ir.BlockToolUse, ToolID: id, ToolName: rec.name, ToolInput: rec.input,
		})
		result := *rec.result
		result.ToolUseID = id
		keptResults = append(keptResults, result)
	}
	if len(flatCalls) > 0 {
		assistantText = append(assistantText, ir.ContentBlock{Type: ir.BlockText, Text: strings.Join(flatCalls, "\n")})
	}
	if len(flatResults)+len(orphanText) > 0 {
		userText = append(userText, ir.ContentBlock{Type: ir.BlockText, Text: strings.Join(append(flatResults, orphanText...), "\n")})
	}
	return append(assistantText, keptCalls...), append(userText, keptResults...)
}

func reserveToolID(raw string, index int, name string, used map[string]bool) string {
	sanitized := sanitizeToolID(raw)
	if sanitized == "" || !validToolID(sanitized) {
		sanitized = "call_" + base36(index) + "_" + sanitizeToolID(name)
	}
	base := trimRunes(sanitized, toolIDLimit)
	if base == "" {
		base = "call_" + base36(index)
	}
	candidate := base
	for suffix := 2; used[candidate]; suffix++ {
		tail := "_" + base36(suffix)
		candidate = trimRunes(base, toolIDLimit-len(tail)) + tail
	}
	used[candidate] = true
	return candidate
}

func sanitizeToolID(value string) string {
	var b strings.Builder
	for _, r := range value {
		if toolNameRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func validToolID(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !toolNameRune(r) {
			return false
		}
	}
	return true
}

func normalizeToolInput(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), true
	}
	if !json.Valid(raw) {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false
	}
	return append(json.RawMessage(nil), raw...), true
}

func flattenOrphanResults(blocks []ir.ContentBlock) []ir.ContentBlock {
	var out []ir.ContentBlock
	var flat []string
	for _, b := range blocks {
		if b.Type == ir.BlockToolResult {
			flat = append(flat, toolResultLine(b))
			continue
		}
		if b.Type == ir.BlockToolUse {
			flat = append(flat, toolCallLine(b.ToolName, b.ToolInput))
			continue
		}
		out = append(out, b)
	}
	if len(flat) > 0 {
		out = append(out, ir.ContentBlock{Type: ir.BlockText, Text: strings.Join(flat, "\n")})
	}
	return out
}

func flattenToolUses(blocks []ir.ContentBlock) []ir.ContentBlock {
	return flattenOrphanResults(blocks)
}

// flattenToolInteractions collapses every tool_use and tool_result block into
// plain text lines, used when the client sends no usable tools. Text blocks
// are preserved. The flattened lines are appended to the same message.
func flattenToolInteractions(msgs []ir.Message) []ir.Message {
	out := make([]ir.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, ir.Message{Role: m.Role, Content: flattenOrphanResults(m.Content)})
	}
	return out
}

func toolCallLine(name string, input json.RawMessage) string {
	if strings.TrimSpace(name) == "" {
		name = "unknown"
	}
	return "[Tool call: " + name + "(" + string(compactJSON(input)) + ")]"
}

// compactJSON returns a compact form of raw JSON without decoding numbers.
// Large integer and float tokens stay unchanged. Empty or invalid input
// becomes "{}".
func compactJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 || !json.Valid(raw) {
		return []byte("{}")
	}
	out := bytes.TrimSpace(raw)
	if len(out) == 0 {
		return []byte("{}")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, out); err != nil {
		return append([]byte(nil), out...)
	}
	return buf.Bytes()
}

func toolResultLine(b ir.ContentBlock) string {
	status := ""
	if b.IsError {
		status = " (error)"
	}
	return "[Tool result" + status + ": " + toolResultText(b) + "]"
}
