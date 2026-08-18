// Package cursor implements the Cursor backend codec: agent.v1.AgentService
// chat over Connect-RPC protobuf, stream-only and bidi (the upstream asks for
// context/KV replies mid-stream). ChatService, the retired IDE endpoint, is
// gone for non-Pro accounts. Auth is a Cursor OAuth access token plus a stable
// machine id. Wire constants are ported from the Cursor CLI's bundled agent.v1
// proto definitions and cross-checked against 9router's executor.
package cursor

const (
	DefaultBaseURL = "https://api2.cursor.sh"
	// AgentBaseURL hosts AgentService (chat and model discovery).
	AgentBaseURL = "https://agent.api5.cursor.sh"
	AgentRunPath = "/agent.v1.AgentService/Run"
	// AgentRunURL is absolute: chat lives on a different host than the
	// provider's base URL, so the codec carries the full URL.
	AgentRunURL = AgentBaseURL + AgentRunPath
	ModelsPath  = "/agent.v1.AgentService/GetUsableModels"

	// ClientType and ClientVersion must be CLI-shaped. An IDE version string
	// makes AgentService/Run return a false "usage limit" on non-Pro accounts;
	// omitting X-Cursor-Client-Version yields "Update Required".
	ClientType    = "cli"
	ClientVersion = "cli-2026.08.11-e8db854"
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

// Connect-RPC frame flags.
const (
	flagNone        = 0x00
	flagGzip        = 0x01
	flagTrailer     = 0x02
	flagGzipTrailer = 0x03
)

// StaticModels is the fallback catalog when live GetUsableModels is unavailable.
// IDs observed from a current GetUsableModels response; live discovery is the
// source of truth for what an account can actually use.
var StaticModels = []string{
	"default",
	"claude-sonnet-4-6",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-4.6-opus-max",
	"gpt-5.6-sol",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gpt-5.2",
	"gemini-3.6-flash",
	"composer-2.5",
	"kimi-k3",
}
