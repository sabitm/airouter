package cursor

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"airouter/internal/proxy/ir"
)

// EncodeRequest maps an IR request to a Connect-RPC-framed
// StreamUnifiedChatRequestWithTools protobuf body. Tool results are flattened
// to XML text inside user messages (the stable path 9router uses: protobuf
// tool_results with partial schemas can loop); tools become mcp_custom_* MCP
// tool definitions.
func EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("cursor: nil request")
	}
	model := strings.TrimPrefix(strings.TrimSpace(req.Model), "cursor/")
	if model == "" {
		return nil, fmt.Errorf("cursor: empty model")
	}

	// Track assistant tool_use ids -> names so tool_result XML can label itself.
	toolNames := map[string]string{}
	for _, m := range req.Messages {
		if m.Role != ir.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolID != "" {
				toolNames[b.ToolID] = b.ToolName
			}
		}
	}

	systemPending := strings.TrimSpace(req.System)
	var messages [][]byte
	var messageIDs [][]byte

	for i, m := range req.Messages {
		role := roleUser
		if m.Role == ir.RoleAssistant {
			role = roleAssistant
		}
		var content strings.Builder
		switch m.Role {
		case ir.RoleUser:
			for _, b := range m.Content {
				switch b.Type {
				case ir.BlockText:
					content.WriteString(b.Text)
				case ir.BlockToolResult:
					name := toolNames[b.ToolUseID]
					if name == "" {
						name = "tool"
					}
					content.WriteString(buildToolResultXML(name, b.ToolUseID, toolResultText(b)))
				}
			}
		case ir.RoleAssistant:
			for _, b := range m.Content {
				if b.Type == ir.BlockText {
					content.WriteString(b.Text)
				}
				// Tool calls are not re-encoded as protobuf (matches 9router):
				// the assistant text alone carries the turn; tool_result XML in
				// the following user message supplies the result context.
			}
		}
		text := content.String()
		// Prepend the system prompt to the first user message (9router maps
		// system -> a user "[System Instructions]" prefix).
		if role == roleUser && systemPending != "" {
			text = "[System Instructions]\n" + systemPending + "\n" + text
			systemPending = ""
		}
		if text == "" && role == roleAssistant {
			// Keep empty assistant turns as empty content so the conversation
			// shape (alternation) is preserved.
		}
		msgID := uuid.NewString()
		isLast := i == len(req.Messages)-1
		messages = append(messages, encodeMessage(text, role, msgID, isLast, len(req.Tools) > 0))
		messageIDs = append(messageIDs, encodeMessageID(msgID, role))
	}
	// System prompt with no user turn to attach to: emit a lone user turn.
	if systemPending != "" {
		msgID := uuid.NewString()
		messages = append(messages, encodeMessage("[System Instructions]\n"+systemPending, roleUser, msgID, true, len(req.Tools) > 0))
		messageIDs = append(messageIDs, encodeMessageID(msgID, roleUser))
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("cursor: empty messages")
	}

	hasTools := len(req.Tools) > 0
	var reqBuf []byte
	for _, mm := range messages {
		reqBuf = append(reqBuf, encodeField(reqMessages, wireLen, mm)...)
	}
	reqBuf = append(reqBuf, encodeField(reqUnknown2, wireVarint, uint64(1))...)
	reqBuf = append(reqBuf, encodeField(reqInstruction, wireLen, encodeInstruction(""))...)
	reqBuf = append(reqBuf, encodeField(reqUnknown4, wireVarint, uint64(1))...)
	reqBuf = append(reqBuf, encodeField(reqModel, wireLen, encodeModel(model))...)
	reqBuf = append(reqBuf, encodeField(reqWebTool, wireLen, "")...)
	reqBuf = append(reqBuf, encodeField(reqUnknown13, wireVarint, uint64(1))...)
	reqBuf = append(reqBuf, encodeField(reqCursorSetting, wireLen, encodeCursorSetting())...)
	reqBuf = append(reqBuf, encodeField(reqUnknown19, wireVarint, uint64(1))...)
	reqBuf = append(reqBuf, encodeField(reqConversationID, wireLen, uuid.NewString())...)
	reqBuf = append(reqBuf, encodeField(reqMetadata, wireLen, encodeMetadata())...)
	reqBuf = append(reqBuf, encodeField(reqIsAgentic, wireVarint, boolVarint(hasTools))...)
	if hasTools {
		reqBuf = append(reqBuf, encodeField(reqSupportedTools, wireLen, encodeVarint(1))...)
	}
	for _, mid := range messageIDs {
		reqBuf = append(reqBuf, encodeField(reqMessageIDs, wireLen, mid)...)
	}
	for _, t := range req.Tools {
		reqBuf = append(reqBuf, encodeField(reqMCPTools, wireLen, encodeMcpTool(t))...)
	}
	reqBuf = append(reqBuf, encodeField(reqLargeContext, wireVarint, uint64(0))...)
	reqBuf = append(reqBuf, encodeField(reqUnknown38, wireVarint, uint64(0))...)
	reqBuf = append(reqBuf, encodeField(reqUnifiedMode, wireVarint, unifiedModeFor(hasTools))...)
	reqBuf = append(reqBuf, encodeField(reqUnknown47, wireLen, "")...)
	reqBuf = append(reqBuf, encodeField(reqShouldDisableTools, wireVarint, boolVarint(!hasTools))...)
	reqBuf = append(reqBuf, encodeField(reqThinkingLevel, wireVarint, uint64(cursorThinkingLevel(req.Thinking)))...)
	reqBuf = append(reqBuf, encodeField(reqUnknown51, wireVarint, uint64(0))...)
	reqBuf = append(reqBuf, encodeField(reqUnknown53, wireVarint, uint64(1))...)
	modeName := "Ask"
	if hasTools {
		modeName = "Agent"
	}
	reqBuf = append(reqBuf, encodeField(reqUnifiedModeName, wireLen, modeName)...)

	top := encodeField(topRequest, wireLen, reqBuf)
	return wrapConnectFrame(top, false), nil
}

func encodeMessage(content string, role int, id string, isLast, hasTools bool) []byte {
	var b []byte
	b = append(b, encodeField(msgContent, wireLen, content)...)
	b = append(b, encodeField(msgRole, wireVarint, uint64(role))...)
	b = append(b, encodeField(msgID, wireLen, id)...)
	b = append(b, encodeField(msgIsAgentic, wireVarint, boolVarint(hasTools))...)
	b = append(b, encodeField(msgUnifiedMode, wireVarint, unifiedModeFor(hasTools))...)
	if isLast && hasTools {
		b = append(b, encodeField(msgSupportedTools, wireLen, encodeVarint(1))...)
	}
	return b
}

func encodeMessageID(id string, role int) []byte {
	return concatBytes(
		encodeField(msgIDID, wireLen, id),
		encodeField(msgIDRole, wireVarint, uint64(role)),
	)
}

func encodeModel(name string) []byte {
	return concatBytes(
		encodeField(modelName, wireLen, name),
		encodeField(modelEmpty, wireLen, []byte{}),
	)
}

func encodeInstruction(text string) []byte {
	if text == "" {
		return nil
	}
	return encodeField(instructionText, wireLen, text)
}

func encodeCursorSetting() []byte {
	unknown6 := concatBytes(
		encodeField(setting6Field1, wireLen, []byte{}),
		encodeField(setting6Field2, wireLen, []byte{}),
	)
	return concatBytes(
		encodeField(settingPath, wireLen, "cursor\\aisettings"),
		encodeField(settingUnknown3, wireLen, []byte{}),
		encodeField(settingUnknown6, wireLen, unknown6),
		encodeField(settingUnknown8, wireVarint, uint64(1)),
		encodeField(settingUnknown9, wireVarint, uint64(1)),
	)
}

func encodeMetadata() []byte {
	return concatBytes(
		encodeField(metaPlatform, wireLen, osName()),
		encodeField(metaArch, wireLen, archName()),
		encodeField(metaVersion, wireLen, runtimeVersion()),
		encodeField(metaCwd, wireLen, "/"),
		encodeField(metaTimestamp, wireLen, timeNowRFC3339()),
	)
}

func encodeMcpTool(t ir.Tool) []byte {
	name := formatToolName(t.Name)
	desc := t.Description
	params := t.Parameters
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var b []byte
	b = append(b, encodeField(mcpToolName, wireLen, name)...)
	if desc != "" {
		b = append(b, encodeField(mcpToolDesc, wireLen, desc)...)
	}
	b = append(b, encodeField(mcpToolParams, wireLen, []byte(params))...)
	b = append(b, encodeField(mcpToolServer, wireLen, "custom")...)
	return b
}

// formatToolName maps a tool name to Cursor's mcp_ namespace:
//   - "toolName"           -> "mcp_custom_toolName"
//   - "mcp__server__tool"  -> "mcp_server_tool"
//   - "mcp_foo"            -> unchanged
func formatToolName(name string) string {
	base := strings.TrimSpace(name)
	if base == "" {
		base = "tool"
	}
	if strings.HasPrefix(base, "mcp__") {
		rest := base[len("mcp__"):]
		if idx := strings.Index(rest, "__"); idx >= 0 {
			server := rest[:idx]
			tool := rest[idx+2:]
			if server == "" {
				server = "custom"
			}
			if tool == "" {
				tool = "tool"
			}
			return "mcp_" + server + "_" + tool
		}
		return "mcp_custom_" + rest
	}
	if strings.HasPrefix(base, "mcp_") {
		return base
	}
	return "mcp_custom_" + base
}

// buildToolResultXML renders a tool_result block as structured text the model
// can read, matching 9router's stable representation (protobuf tool_results
// loop on partial schemas). Control chars are stripped so they cannot break the
// XML framing.
func buildToolResultXML(name, callID, result string) string {
	var b strings.Builder
	b.WriteString("<tool_result>\n")
	b.WriteString("<tool_name>")
	b.WriteString(escapeXML(name))
	b.WriteString("</tool_name>\n")
	b.WriteString("<tool_call_id>")
	b.WriteString(escapeXML(callID))
	b.WriteString("</tool_call_id>\n")
	b.WriteString("<result>")
	b.WriteString(escapeXML(sanitizeToolResultText(result)))
	b.WriteString("</result>\n")
	b.WriteString("</tool_result>")
	return b.String()
}

func toolResultText(b ir.ContentBlock) string {
	var parts []string
	for _, c := range b.ToolResult {
		if c.Type == ir.BlockText && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	s := strings.Join(parts, "\n")
	if s == "" && b.IsError {
		return "error"
	}
	return s
}

// cursorThinkingLevel maps IR thinking intent onto Cursor's 3-level enum.
// Lossy by design: minimal/low/medium -> medium; high/xhigh/max -> high.
func cursorThinkingLevel(t *ir.Thinking) int {
	if t == nil {
		return thinkingUnspecified
	}
	switch t.Mode {
	case ir.ThinkingNone, ir.ThinkingAuto:
		return thinkingUnspecified
	case ir.ThinkingBudget:
		switch {
		case t.Budget <= 0:
			return thinkingUnspecified
		case t.Budget <= 16384:
			return thinkingMedium
		default:
			return thinkingHigh
		}
	case ir.ThinkingLevel:
		switch strings.ToLower(t.Level) {
		case "high", "xhigh", "max":
			return thinkingHigh
		case "minimal", "low", "medium":
			return thinkingMedium
		default:
			return thinkingMedium
		}
	}
	return thinkingUnspecified
}

func sanitizeToolResultText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 {
			switch r {
			case '\t', '\n':
				b.WriteRune(r)
			default:
				// drop other control chars
			}
			continue
		}
		if r == 0x7f {
			continue
		}
		if r == utf8.RuneError {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func escapeXML(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func boolVarint(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func unifiedModeFor(hasTools bool) uint64 {
	if hasTools {
		return unifiedModeAgent
	}
	return unifiedModeChat
}
