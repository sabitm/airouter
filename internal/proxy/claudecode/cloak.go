package claudecode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultTools are the Claude Code native tool names. They are injected unsuffixed
// as decoys and used as the decloak skip set: a returned tool_use whose name is
// native is left alone, since the model called a decoy by its real name. Source:
// 9router CC_DEFAULT_TOOLS / CC_DECOY_TOOLS.
var DefaultTools = map[string]bool{
	"Task":            true,
	"TaskOutput":      true,
	"TaskStop":        true,
	"TaskCreate":      true,
	"TaskGet":         true,
	"TaskUpdate":      true,
	"TaskList":        true,
	"Bash":            true,
	"Glob":            true,
	"Grep":            true,
	"Read":            true,
	"Edit":            true,
	"Write":           true,
	"NotebookEdit":    true,
	"WebFetch":        true,
	"WebSearch":       true,
	"AskUserQuestion": true,
	"Skill":           true,
	"EnterPlanMode":   true,
	"ExitPlanMode":    true,
}

// DecoyTools are Claude Code native tool declarations appended after the
// (suffixed) client tools so the declared catalog looks closer to a real Claude
// Code session. They are marked unavailable so the model does not act on them.
var DecoyTools = []wireTool{
	decoy("Task"),
	decoy("TaskOutput"),
	decoy("TaskStop"),
	decoy("TaskCreate"),
	decoy("TaskGet"),
	decoy("TaskUpdate"),
	decoy("TaskList"),
	decoy("Bash"),
	decoy("Glob"),
	decoy("Grep"),
	decoy("Read"),
	decoy("Edit"),
	decoy("Write"),
	decoy("NotebookEdit"),
	decoy("WebFetch"),
	decoy("WebSearch"),
	decoy("AskUserQuestion"),
	decoy("Skill"),
	decoy("EnterPlanMode"),
	decoy("ExitPlanMode"),
}

var decoySchema = json.RawMessage(`{"type":"object","properties":{}}`)

func decoy(name string) wireTool {
	return wireTool{
		Name:        name,
		Description: "This tool is currently unavailable.",
		InputSchema: decoySchema,
	}
}

// CloakTools renames non-server client tools with ToolSuffix, appends DecoyTools,
// rewrites tool_use names in message history, and rewrites a forced
// tool_choice{name} to the suffixed name. No-op when tools are empty (decoys are
// not injected alone), matching 9router. Server tools (carrying a wire Type) are
// left unsuffixed since they require an exact reserved name. Every non-typed
// tool is suffixed regardless of its name, matching the executable reference.
func CloakTools(body *messagesBody) {
	if body == nil || len(body.Tools) == 0 {
		return
	}
	clientNames := map[string]bool{}
	decls := make([]wireTool, 0, len(body.Tools)+len(DecoyTools))
	for _, t := range body.Tools {
		if t.Type != "" || t.Name == "" {
			decls = append(decls, t)
			continue
		}
		suffixed := t.Name + ToolSuffix
		clientNames[t.Name] = true
		t.Name = suffixed
		decls = append(decls, t)
	}
	// Append decoys the client did not already declare under their native name.
	// Client tools are all suffixed above, so a native-named client tool and its
	// decoy coexist (e.g. client "Bash" -> "Bash_ide" plus decoy "Bash").
	for _, d := range DecoyTools {
		decls = append(decls, d)
	}
	body.Tools = decls

	for i := range body.Messages {
		cloakMessageContent(&body.Messages[i], clientNames)
	}

	if body.ToolChoice != nil && body.ToolChoice.Type == "tool" && clientNames[body.ToolChoice.Name] {
		body.ToolChoice.Name = body.ToolChoice.Name + ToolSuffix
	}
}

// cloakMessageContent rewrites tool_use block names with ToolSuffix inside one
// message's content. String content and non-array content pass through. Only
// tool_use blocks are renamed; tool_result blocks reference tool_use_id, not the
// name, so they need no rewrite.
func cloakMessageContent(msg *wireMessage, clientNames map[string]bool) {
	raw := msg.Content
	if len(raw) == 0 || raw[0] != '[' {
		return
	}
	var blocks []wireBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	changed := false
	for i := range blocks {
		if blocks[i].Type == "tool_use" && blocks[i].Name != "" {
			blocks[i].Name = blocks[i].Name + ToolSuffix
			changed = true
		}
	}
	if !changed {
		return
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return
	}
	msg.Content = out
}

// DecloakName strips one ToolSuffix from a wire tool name so the client sees the
// original. Native/decoy names (DefaultTools) are returned unchanged. A client
// tool that already ended in _ide becomes _ide_ide outbound; stripping once
// yields the original _ide. Mirrors the Antigravity decloak invariant.
func DecloakName(name string) string {
	if name == "" || DefaultTools[name] {
		return name
	}
	if len(name) > len(ToolSuffix) && strings.HasSuffix(name, ToolSuffix) {
		return name[:len(name)-len(ToolSuffix)]
	}
	return name
}

// IsOAuthToken reports whether the resolved upstream token is a Claude OAuth
// access token, the gate for cloaking. Matches 9router's sk-ant-oat check.
func IsOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, OAuthTokenMarker)
}

// GenerateBillingHeader builds the x-anthropic-billing-header telemetry marker
// injected as system[0]. cch is the first 5 hex of sha256(body) over the
// pre-injection body; buildHash is 3 random hex. body is the exact request body
// before the billing block is added.
func GenerateBillingHeader(body []byte) string {
	sum := sha256.Sum256(body)
	cch := hex.EncodeToString(sum[:])[:5]
	var b [2]byte
	_, _ = rand.Read(b[:])
	buildHash := hex.EncodeToString(b[:])[:3]
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=%s;",
		CLIVersion, buildHash, CCEntrypoint, cch)
}

// deriveUUID returns a deterministic UUID-v4-shaped string from a seed, matching
// 9router's deriveUuid: the version nibble is forced to 4 and the variant
// nibble to 8/9/a/b.
func deriveUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:])
	// h[12] replaced with '4' (version); h[16] masked to the variant.
	return fmt.Sprintf("%s-%s-4%s-%s%s-%s",
		h[0:8], h[8:12], h[13:16],
		string(variantNibble(h[16])), h[17:20],
		h[20:32],
	)
}

func variantNibble(c byte) byte {
	v := c - '0'
	if v > 9 {
		v = c - 'a' + 10
	}
	v = (v & 0x3) | 0x8
	if v < 10 {
		return '0' + v
	}
	return 'a' + (v - 10)
}

// GenerateFakeUserID builds the metadata.user_id JSON string
// {"device_id","account_uuid","session_id"}. device_id and account_uuid derive
// from seed (stable per account); sessionID is the per-request session id
// aligned with X-Claude-Code-Session-Id. An empty seed falls back to random
// values so the field is always populated when cloaking.
func GenerateFakeUserID(sessionID, seed string) string {
	var deviceID, accountUUID string
	if seed == "" {
		var b [32]byte
		_, _ = rand.Read(b[:])
		deviceID = hex.EncodeToString(b[:])
		accountUUID = randomUUID()
	} else {
		ds := sha256.Sum256([]byte("device:" + seed))
		deviceID = hex.EncodeToString(ds[:])
		accountUUID = deriveUUID("account:" + seed)
	}
	sess := sessionID
	if sess == "" {
		sess = randomUUID()
	}
	return fmt.Sprintf(`{"device_id":"%s","account_uuid":"%s","session_id":"%s"}`,
		deviceID, accountUUID, sess)
}

// randomUUID returns a random RFC 4122 version-4 UUID string.
func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
