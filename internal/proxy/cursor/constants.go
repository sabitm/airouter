// Package cursor implements the Cursor IDE backend codec: Connect-RPC
// protobuf chat (ChatService StreamUnifiedChatWithTools), stream-only. Auth is
// a Cursor OAuth access token plus a stable machine id. Wire constants and
// field numbers are ported from 9router's open-sse/utils/cursorProtobuf.js.
package cursor

const (
	DefaultBaseURL = "https://api2.cursor.sh"
	// UpstreamPath is the ChatService streaming chat endpoint.
	UpstreamPath = "/aiserver.v1.ChatService/StreamUnifiedChatWithTools"
	// AgentBaseURL hosts AgentService; v1 uses it only for model discovery and
	// Check (GetUsableModels), not chat.
	AgentBaseURL = "https://agent.api5.cursor.sh"
	AgentRunPath = "/agent.v1.AgentService/Run"
	ModelsPath   = "/agent.v1.AgentService/GetUsableModels"

	ClientVersion = "3.12.17"
	ClientCommit  = "0fb762053c34788bb7760d5673f8a6d4c8589d50"
	// ConnectContentType is the request Content-Type for framed Connect-RPC.
	ConnectContentType = "application/connect+proto"
	// StreamAccept is the Accept value for the streaming chat endpoint.
	StreamAccept = "application/connect+proto"
	// ProtoContentType is the unframed Content-Type for unary Connect calls
	// (GetUsableModels uses application/proto, not connect+proto).
	ProtoContentType = "application/proto"
	UserAgent        = "connect-es/1.6.1"
	GhostModeDefault = "true"
)

// Protobuf wire types.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireLen     = 2
	wireFixed32 = 5
)

// Roles for ConversationMessage.role.
const (
	roleUser      = 1
	roleAssistant = 2
)

const (
	unifiedModeChat  = 1
	unifiedModeAgent = 2
)

// Thinking levels for the thinking_level field.
const (
	thinkingUnspecified = 0
	thinkingMedium      = 1
	thinkingHigh        = 2
)

// clientSideToolV2MCP is the tool kind varint for MCP tools.
const clientSideToolV2MCP = 19

// Connect-RPC frame flags.
const (
	flagNone        = 0x00
	flagGzip        = 0x01
	flagTrailer     = 0x02
	flagGzipTrailer = 0x03
)

// Field numbers for StreamUnifiedChatRequestWithTools (top level) and nested
// messages. Mirrors 9router's FIELD map exactly.
const (
	// top level
	topRequest = 1
	// StreamUnifiedChatRequest
	reqMessages           = 1
	reqUnknown2           = 2
	reqInstruction        = 3
	reqUnknown4           = 4
	reqModel              = 5
	reqWebTool            = 8
	reqUnknown13          = 13
	reqCursorSetting      = 15
	reqUnknown19          = 19
	reqConversationID     = 23
	reqMetadata           = 26
	reqIsAgentic          = 27
	reqSupportedTools     = 29
	reqMessageIDs         = 30
	reqMCPTools           = 34
	reqLargeContext       = 35
	reqUnknown38          = 38
	reqUnifiedMode        = 46
	reqUnknown47          = 47
	reqShouldDisableTools = 48
	reqThinkingLevel      = 49
	reqUnknown51          = 51
	reqUnknown53          = 53
	reqUnifiedModeName    = 54
	// ConversationMessage
	msgContent        = 1
	msgRole           = 2
	msgID             = 13
	msgIsAgentic      = 29
	msgServerBubbleID = 32
	msgUnifiedMode    = 47
	msgSupportedTools = 51
	// Model
	modelName  = 1
	modelEmpty = 4
	// Instruction
	instructionText = 1
	// CursorSetting
	settingPath     = 1
	settingUnknown3 = 3
	settingUnknown6 = 6
	settingUnknown8 = 8
	settingUnknown9 = 9
	setting6Field1  = 1
	setting6Field2  = 2
	// Metadata
	metaPlatform  = 1
	metaArch      = 2
	metaVersion   = 3
	metaCwd       = 4
	metaTimestamp = 5
	// MessageId
	msgIDID   = 1
	msgIDRole = 3
	// MCPTool
	mcpToolName   = 1
	mcpToolDesc   = 2
	mcpToolParams = 3
	mcpToolServer = 4
	// StreamUnifiedChatResponseWithTools (response)
	respToolCall = 1
	respResponse = 2
	// ClientSideToolV2Call
	toolID        = 3
	toolName      = 9
	toolRawArgs   = 10
	toolIsLast    = 11
	toolMCPParams = 27
	// MCPParams + nested Tool
	mcpToolsList    = 1
	mcpNestedName   = 1
	mcpNestedParams = 3
	// StreamUnifiedChatResponse
	respText     = 1
	respThinking = 25
	// Thinking
	thinkingText = 1
)

// StaticModels is the fallback catalog when live GetUsableModels is unavailable.
// IDs mirror 9router's provider registry cursor.js.
var StaticModels = []string{
	"default",
	"claude-4.5-opus-high-thinking",
	"claude-4.5-opus-high",
	"claude-4.5-sonnet-thinking",
	"claude-4.5-sonnet",
	"claude-4.5-haiku",
	"claude-4.5-opus",
	"gpt-5.2-codex",
	"claude-4.6-opus-max",
	"claude-4.6-sonnet-medium-thinking",
	"kimi-k2.5",
	"gemini-3-flash-preview",
	"gpt-5.2",
	"gpt-5.3-codex",
}
