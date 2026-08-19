package cursor

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"airouter/internal/proxy/ir"
)

// ExecServerMessage fields that are not a client-visible tool oneof.
const (
	esmSpanContext                 = 19
	esmAcceptHookAdditionalContext = 55
)

// execControlFields are protocol/control oneofs, not tools to surface.
var execControlFields = map[int]bool{
	esmID:                          true,
	esmExecID:                      true,
	esmRequestContextArgs:          true,
	esmMCPArgs:                     true,
	esmSpanContext:                 true,
	esmAcceptHookAdditionalContext: true,
}

// execToolName is the IR tool name derived from an ExecServerMessage oneof
// (shell_args -> shell). Unknown field numbers fall back to "exec_<n>".
var execToolName = map[int]string{
	2:  "shell",
	3:  "write",
	4:  "delete",
	5:  "grep",
	7:  "read",
	8:  "ls",
	9:  "diagnostics",
	14: "shell_stream",
	16: "background_shell_spawn",
	17: "list_mcp_resources",
	18: "read_mcp_resource",
	20: "fetch",
	21: "record_screen",
	22: "computer_use",
	23: "write_shell_stdin",
	27: "execute_hook",
	28: "subagent",
	29: "redacted_read",
	30: "force_background_shell",
	31: "force_background_subagent",
	36: "mcp_state",
	37: "subagent_await",
	38: "smart_mode_classifier",
	40: "canvas_diagnostics",
	41: "shell_allowlist_precheck",
	42: "mcp_allowlist_precheck",
	43: "web_fetch_allowlist_precheck",
	44: "git_diff",
	45: "pi_read",
	46: "pi_bash",
	47: "pi_edit",
	48: "pi_write",
	49: "pi_grep",
	50: "pi_find",
	51: "pi_ls",
	52: "mini_swe_agent_bash",
	53: "conversation_search",
	54: "agent_store_conflict",
	56: "adopt",
}

// execArgNames maps (ExecServerMessage field, args field number) to the proto
// field name. Seeded from the Cursor CLI agent.v1 bundle. Unknown args fields
// encode as "f<n>" so a new Cursor field still surfaces instead of hanging.
var execArgNames = map[int]map[int]string{
	2: { // ShellArgs
		1: "command", 2: "working_directory", 3: "timeout", 4: "tool_call_id",
		5: "simple_commands", 6: "has_input_redirect", 7: "has_output_redirect",
		10: "file_output_threshold_bytes", 11: "is_background", 12: "skip_approval",
		13: "timeout_behavior", 14: "hard_timeout", 15: "description",
		17: "close_stdin", 21: "conversation_id", 22: "admin_command_denylist",
	},
	3: { // WriteArgs
		1: "path", 2: "file_text", 3: "tool_call_id",
		4: "return_file_content_after_write", 5: "file_bytes", 6: "encoding_hint",
	},
	4: {1: "path", 2: "tool_call_id"}, // DeleteArgs
	5: { // GrepArgs
		1: "pattern", 2: "path", 3: "glob", 4: "output_mode",
		5: "context_before", 6: "context_after", 7: "context",
		8: "case_insensitive", 9: "type", 10: "head_limit",
		11: "multiline", 12: "sort", 13: "sort_ascending",
		14: "tool_call_id", 16: "offset",
	},
	7:  {1: "path", 2: "tool_call_id", 4: "offset", 5: "limit", 6: "encoding_hint"},
	8:  {1: "path", 2: "ignore", 3: "tool_call_id", 5: "timeout_ms"},
	9:  {1: "path", 2: "tool_call_id"},
	16: {1: "command", 2: "working_directory", 3: "tool_call_id", 7: "description", 12: "skip_approval", 13: "conversation_id"},
	17: {1: "server"},
	18: {1: "server", 2: "uri", 3: "download_path", 4: "tool_call_id"},
	20: {1: "url", 2: "tool_call_id"},
	21: {2: "tool_call_id", 3: "save_as_filename"},
	22: {1: "tool_call_id", 3: "description", 4: "bind_unmapped_characters"},
	23: {1: "shell_id", 2: "chars"},
	28: {1: "tool_call_id", 2: "subagent_type", 3: "model_id", 4: "prompt", 5: "readonly", 6: "resume_agent_id", 7: "run_in_background"},
	30: {1: "tool_call_id"},
	31: {1: "tool_call_id"},
	37: {1: "agent_id", 2: "timeout_ms"},
	38: {1: "tool_call_id", 2: "parent_conversation_id"},
	40: {1: "path", 2: "tool_call_id"},
	41: {1: "command", 2: "working_directory", 5: "tool_call_id"},
	42: {1: "provider_identifier", 2: "tool_name", 3: "tool_call_id"},
	43: {1: "url", 2: "tool_call_id"},
	45: {1: "path", 2: "tool_call_id", 4: "offset", 5: "limit"},
	46: {1: "command", 2: "working_directory", 3: "timeout", 4: "tool_call_id"},
	47: {1: "path", 2: "old_string", 3: "new_string", 4: "tool_call_id"},
	48: {1: "path", 2: "file_text", 3: "tool_call_id"},
	49: {1: "pattern", 2: "path", 3: "glob", 14: "tool_call_id"},
	50: {1: "pattern", 2: "path", 3: "tool_call_id"},
	51: {1: "path", 3: "tool_call_id"},
	53: {1: "query", 2: "tool_call_id", 3: "limit"},
	56: {1: "source_agent_id"},
}

// toolCallName is the IR tool name derived from a ToolCall oneof
// (shell_tool_call -> shell). MCP is handled separately.
var toolCallName = map[int]string{
	1:  "shell",
	3:  "delete",
	4:  "glob",
	5:  "grep",
	8:  "read",
	9:  "update_todos",
	10: "read_todos",
	12: "edit",
	13: "ls",
	14: "read_lints",
	16: "sem_search",
	17: "create_plan",
	18: "web_search",
	19: "task",
	20: "list_mcp_resources",
	21: "read_mcp_resource",
	22: "apply_agent_diff",
	23: "ask_question",
	24: "fetch",
	25: "switch_mode",
	28: "generate_image",
	29: "record_screen",
	30: "computer_use",
	31: "write_shell_stdin",
	32: "reflect",
	33: "setup_vm_environment",
	34: "truncated",
	35: "start_grind_execution",
	36: "start_grind_planning",
	37: "web_fetch",
	38: "report_bugfix_results",
	39: "ai_attribution",
	40: "pr_management",
	41: "mcp_auth",
	42: "await",
	43: "blame_by_file_path",
	44: "get_mcp_tools",
	45: "report_bug",
	46: "set_active_branch",
	48: "communicate_update",
	49: "send_final_summary",
	50: "update_pr_code_tour",
	51: "replace_env",
	52: "edit_pr_labels",
	53: "record_ci_investigation_findings",
	55: "send_message",
	56: "fetch_cloud_agent_data",
	58: "send_to_user",
	61: "pi_read",
	62: "pi_bash",
	63: "pi_edit",
	64: "pi_write",
	65: "pi_grep",
	66: "pi_find",
	67: "pi_ls",
	68: "connect_scm",
	69: "search_conversations",
	70: "create_goal",
	71: "update_goal",
	72: "adopt",
}

const (
	tcToolCallID = 57
	// Most *ToolCall messages wrap typed args at field 1.
	tcInnerArgs = 1
)

// extractExecToolCall maps an ExecServerMessage args oneof onto an IR tool
// call. Control fields (id, request_context, mcp_args) are skipped; the first
// remaining LEN oneof is the tool. Name comes from the oneof, args from the
// field-name table. ok is false when there is no surfacing payload.
func extractExecToolCall(server map[int][]field) (id, name, argsJSON string, ok bool) {
	// Prefer a named oneof; fall back to any non-control LEN field so a new
	// Cursor exec still surfaces instead of hanging.
	pick := func(num int, fs []field) (string, string, string, bool) {
		if execControlFields[num] || len(fs) == 0 || fs[0].wireType != wireLen {
			return "", "", "", false
		}
		n := execToolName[num]
		if n == "" {
			n = "exec_" + strconv.Itoa(num)
		}
		cid, args := encodeNamedArgs(fs[0].value, execArgNames[num])
		if cid == "" {
			cid = ir.NewID("call_")
		}
		return cid, n, args, true
	}
	for num := range execToolName {
		if id, name, argsJSON, ok = pick(num, server[num]); ok {
			return id, name, argsJSON, true
		}
	}
	for num, fs := range server {
		if id, name, argsJSON, ok = pick(num, fs); ok {
			return id, name, argsJSON, true
		}
	}
	return "", "", "", false
}

// extractAnyToolCall maps ToolCallStartedUpdate / PartialToolCallUpdate onto
// an IR tool call. MCP stays on extractMCPToolCall; every other ToolCall oneof
// is named from the proto and encoded from its inner args (field 1).
func extractAnyToolCall(update []byte) (id, name, argsJSON string, mcp, ok bool) {
	if id, name, argsJSON, ok = extractMCPToolCall(update); ok {
		return id, name, argsJSON, true, true
	}
	m := decodeOrEmpty(update)
	callID, _ := stringField(m, tcsCallID)
	tcs, have := m[tcsToolCall]
	if !have || len(tcs) == 0 {
		return "", "", "", false, false
	}
	tc := decodeOrEmpty(tcs[0].value)
	if v, ok := stringField(tc, tcToolCallID); ok && v != "" {
		callID = v
	}
	for num, fs := range tc {
		// MCP (15) and ToolCall identity/timing fields are not named tools.
		// Incomplete MCP at tool_call_started must not become "tool_15".
		if num == tcMCPTOolCall || num == tcToolCallID || num == 54 || num == 59 || num == 60 {
			continue
		}
		if len(fs) == 0 || fs[0].wireType != wireLen {
			continue
		}
		name = toolCallName[num]
		if name == "" {
			name = "tool_" + strconv.Itoa(num)
		}
		inner := decodeOrEmpty(fs[0].value)
		argsRaw := fs[0].value
		if as, ok := inner[tcInnerArgs]; ok && len(as) > 0 {
			argsRaw = as[0].value
		}
		// Reuse exec arg names when the ToolCall oneof matches a known exec
		// tool (shell/read/...). Unknown nested fields become "f<n>".
		id2, argsJSON := encodeNamedArgs(argsRaw, argNamesForTool(name))
		if callID == "" {
			callID = id2
		}
		if callID == "" {
			callID = ir.NewID("call_")
		}
		return callID, name, argsJSON, false, true
	}
	return "", "", "", false, false
}

func argNamesForTool(name string) map[int]string {
	for num, n := range execToolName {
		if n == name {
			return execArgNames[num]
		}
	}
	// ToolCall-only names that share an exec schema under a different field.
	switch name {
	case "edit":
		return map[int]string{1: "path", 6: "stream_content"}
	case "web_search":
		return map[int]string{1: "search_term", 2: "tool_call_id"}
	case "web_fetch", "fetch":
		return map[int]string{1: "url", 2: "tool_call_id"}
	}
	return nil
}

// encodeNamedArgs renders a protobuf args message as JSON. Scalar/enum/varint
// fields use table names when known, else "f<n>". tool_call_id is lifted out
// as the IR id and omitted from the object. Nested messages are skipped
// (sandbox policy, classifier blobs) so the client sees the actionable scalars.
func encodeNamedArgs(raw []byte, names map[int]string) (callID, argsJSON string) {
	m := decodeOrEmpty(raw)
	out := map[string]any{}
	for num, fs := range m {
		if len(fs) == 0 {
			continue
		}
		key := ""
		if names != nil {
			key = names[num]
		}
		if key == "" {
			key = "f" + strconv.Itoa(num)
		}
		f := fs[0]
		switch f.wireType {
		case wireLen:
			if !utf8.Valid(f.value) {
				continue
			}
			if key == "tool_call_id" {
				callID = string(f.value)
				continue
			}
			out[key] = string(f.value)
		case wireVarint:
			v, _, err := decodeVarint(f.value, 0)
			if err != nil {
				continue
			}
			out[key] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return callID, "{}"
	}
	return callID, string(b)
}

// resolveClientTool maps a Cursor-native tool name onto a declared ingress
// tool. Exact name keeps Cursor args. Canonical match (lowercase, strip
// non-alphanumeric) remaps the name and binds the first string scalar onto
// the declared schema's first required string property. ok is false when
// nothing matches — the caller must not invent a tool_use.
func resolveClientTool(tools []ir.Tool, cursorName, argsJSON string) (name, bound string, ok bool) {
	if cursorName == "" || len(tools) == 0 {
		return "", argsJSON, false
	}
	for _, t := range tools {
		if t.Name == cursorName {
			return t.Name, argsJSON, true
		}
	}
	want := canonToolName(cursorName)
	for _, t := range tools {
		if canonToolName(t.Name) != want {
			continue
		}
		return t.Name, bindPrimaryArg(t.Parameters, argsJSON), true
	}
	return "", argsJSON, false
}

func canonToolName(s string) string {
	s = strings.ToLower(s)
	// Cursor's pi_* namespace is the same capability without the prefix.
	s = strings.TrimPrefix(s, "pi_")
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// bindPrimaryArg copies the first string value in Cursor args onto the
// declared schema's first required string property. If the schema has none,
// the original JSON is kept.
func bindPrimaryArg(schema json.RawMessage, argsJSON string) string {
	key := firstRequiredString(schema)
	if key == "" {
		return argsJSON
	}
	var src map[string]any
	if json.Unmarshal([]byte(argsJSON), &src) != nil {
		return argsJSON
	}
	primary := firstStringArg(src)
	if primary == "" {
		return argsJSON
	}
	out, err := json.Marshal(map[string]string{key: primary})
	if err != nil {
		return argsJSON
	}
	return string(out)
}

func firstStringArg(src map[string]any) string {
	for _, k := range []string{"search_term", "url", "query", "command", "path"} {
		s, _ := src[k].(string)
		if s != "" {
			return s
		}
	}
	for _, v := range src {
		s, ok := v.(string)
		if ok && s != "" {
			return s
		}
	}
	return ""
}

// UnmatchedBuiltinError means Cursor asked for a built-in that is not among
// the ingress-declared tools. Decode does not emit Finish so the caller can
// start one fresh AgentService turn that tells the model to use MCP tools.
type UnmatchedBuiltinError struct {
	Name string
}

func (e *UnmatchedBuiltinError) Error() string {
	if e == nil || e.Name == "" {
		return "cursor: unmatched built-in tool"
	}
	return "cursor: unmatched built-in tool " + e.Name
}

// AsUnmatchedBuiltin reports whether err is an unmatched built-in.
func AsUnmatchedBuiltin(err error) (*UnmatchedBuiltinError, bool) {
	e, ok := err.(*UnmatchedBuiltinError)
	return e, ok && e != nil
}

// MCPAvailabilityNote tells the model that only the declared MCP tools exist.
// Names come from the current request, not a hardcoded catalog.
func MCPAvailabilityNote(tools []ir.Tool, rejected string) string {
	if len(tools) == 0 {
		return ""
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	list := strings.Join(names, ", ")
	if rejected != "" {
		return "Tool \"" + rejected + "\" is not available. Available MCP tools: " +
			list + ". Call one of those by name; do not use Cursor built-in tools."
	}
	return "You have no Cursor built-in tools in this session. Your only tools are the MCP tools: " +
		list + ". Always call those by name."
}

// WithBuiltinRejection returns a shallow copy of req whose last user turn
// includes the unmatched-tool instruction. Used for the single follow-up
// AgentService run; the original request is not mutated.
func WithBuiltinRejection(req *ir.Request, rejected string) *ir.Request {
	if req == nil {
		return nil
	}
	note := MCPAvailabilityNote(req.Tools, rejected)
	if note == "" {
		return req
	}
	out := *req
	out.Messages = append(append([]ir.Message(nil), req.Messages...), ir.Message{
		Role:    ir.RoleUser,
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: note}},
	})
	return &out
}

func firstRequiredString(schema json.RawMessage) string {
	var spec struct {
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if json.Unmarshal(schema, &spec) != nil {
		return ""
	}
	for _, name := range spec.Required {
		p := spec.Properties[name]
		if p == nil {
			return name
		}
		if t, _ := p["type"].(string); t == "" || t == "string" {
			return name
		}
	}
	return ""
}
