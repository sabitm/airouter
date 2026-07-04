package kiro

import (
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
)

// Kiro rejects requests with inconsistent tool state (HTTP 400 "Improperly
// formed request"). Two guards salvage such requests by rewriting the offending
// structured blocks into plain text rather than sending them as tool blocks.

// flattenToolInteractions collapses every tool_use and tool_result block into
// plain text lines, used when the client sends no tools on a follow-up turn:
// without a tools declaration Kiro rejects structured tool blocks. Text blocks
// are preserved as-is; the flattened lines are appended to the same message.
func flattenToolInteractions(msgs []ir.Message) []ir.Message {
	out := make([]ir.Message, 0, len(msgs))
	for _, m := range msgs {
		var blocks []ir.ContentBlock
		var flat []string
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockToolUse:
				flat = append(flat, "[Tool call: "+b.ToolName+"("+string(compactJSON(b.ToolInput))+")]")
			case ir.BlockToolResult:
				flat = append(flat, "[Tool result: "+toolResultText(b)+"]")
			default:
				blocks = append(blocks, b)
			}
		}
		if len(flat) > 0 {
			blocks = append(blocks, ir.ContentBlock{Type: ir.BlockText, Text: strings.Join(flat, "\n")})
		}
		out = append(out, ir.Message{Role: m.Role, Content: blocks})
	}
	return out
}

// reconcileOrphanedToolResults salvages tool_result blocks whose matching
// tool_use was dropped (e.g. by client-side history compaction). A dangling
// structured tool_result triggers a 400, so an orphan is rewritten to a text
// block carrying its content. A tool_result is an orphan when no earlier
// assistant tool_use shares its ToolUseID.
func reconcileOrphanedToolResults(msgs []ir.Message) []ir.Message {
	toolUseIDs := map[string]bool{}
	for _, m := range msgs {
		if m.Role != ir.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolID != "" {
				toolUseIDs[b.ToolID] = true
			}
		}
	}

	out := make([]ir.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != ir.RoleUser {
			out = append(out, m)
			continue
		}
		var blocks []ir.ContentBlock
		var salvaged []string
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult && !toolUseIDs[b.ToolUseID] {
				salvaged = append(salvaged, "[Tool result: "+toolResultText(b)+"]")
				continue
			}
			blocks = append(blocks, b)
		}
		if len(salvaged) > 0 {
			blocks = append(blocks, ir.ContentBlock{Type: ir.BlockText, Text: strings.Join(salvaged, "\n")})
		}
		out = append(out, ir.Message{Role: m.Role, Content: blocks})
	}
	return out
}

// compactJSON returns a compact form of raw JSON, or "{}" when empty/invalid, so
// a flattened tool call reads on one line.
func compactJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []byte("{}")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
