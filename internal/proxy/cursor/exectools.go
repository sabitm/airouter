package cursor

import "strings"

import "airouter/internal/proxy/ir"

// ExecServerMessage fields that are metadata or control, not a client-visible
// tool oneof. Field numbers are from agent.v1.ExecServerMessage.
const (
	esmSpanContext                 = 19
	esmMachineID                   = 57
	esmAcceptHookAdditionalContext = 55
)

// execControlFields are protocol/control oneofs and metadata, not tools to
// reject. Any other field on an ExecServerMessage is a Cursor built-in exec.
// Those are rejected and never surfaced as IR tool_use.
var execControlFields = map[int]bool{
	esmID:                          true,
	esmExecID:                      true,
	esmRequestContextArgs:          true,
	esmMCPArgs:                     true,
	esmSpanContext:                 true,
	esmMachineID:                   true,
	esmAcceptHookAdditionalContext: true,
}

// execArgResult maps an ExecServerMessage args field to the matching
// ExecClientMessage result field. The official descriptors do not use the
// same number for every pair: Pi args 45-51 map to results 46-52, and
// mini_swe_agent_bash_args 52 maps to mini_swe_agent_bash_result 55.
// Client field 45 is hook_additional_contexts, not a result.
var execArgResult = map[int]int{
	2:  2,  // shell_args -> shell_result (also shell_stream_args 14)
	3:  3,  // write_args
	4:  4,  // delete_args
	5:  5,  // grep_args
	7:  7,  // read_args (also redacted_read_args 29)
	8:  8,  // ls_args
	9:  9,  // diagnostics_args
	14: 14, // shell_stream_args
	16: 16, // background_shell_spawn_args
	17: 17, // list_mcp_resources_exec_args
	18: 18, // read_mcp_resource_exec_args
	20: 20, // fetch_args
	21: 21, // record_screen_args
	22: 22, // computer_use_args
	23: 23, // write_shell_stdin_args
	27: 27, // execute_hook_args
	28: 28, // subagent_args
	29: 29, // redacted_read_args
	30: 30, // force_background_shell_args
	31: 31, // force_background_subagent_args
	36: 36, // mcp_state_exec_args
	37: 37, // subagent_await_args
	38: 38, // smart_mode_classifier_args
	40: 40, // canvas_diagnostics_args
	41: 41, // shell_allowlist_precheck_args
	42: 42, // mcp_allowlist_precheck_args
	43: 43, // web_fetch_allowlist_precheck_args
	44: 44, // git_diff_request
	45: 46, // pi_read_args
	46: 47, // pi_bash_args
	47: 48, // pi_edit_args
	48: 49, // pi_write_args
	49: 50, // pi_grep_args
	50: 51, // pi_find_args
	51: 52, // pi_ls_args
	52: 55, // mini_swe_agent_bash_args
	53: 53, // conversation_search_args
	54: 54, // agent_store_conflict_args
	56: 56, // adopt_args
}

// execResultSpec is one official failure variant. Outer and inner field
// numbers are message-specific. A recognized exec with no honest failure
// variant uses ExecClientThrow instead of a fabricated success.
type execResultSpec struct {
	outer   int
	inner   int
	path    int // 0 means the inner message has no identity string
	reason  int
	command bool
	url     bool
	enum    bool
	enumVal uint64
}

// execResultSpecs is keyed by ExecClientMessage result field. Shapes that
// share a name still differ by outer field, so they are not one family.
// Field numbers are from the agent.v1 descriptors in the Cursor agent host.
var execResultSpecs = map[int]execResultSpec{
	2:  {outer: 4, inner: 0, path: 1, reason: 3, command: true}, // ShellResult.rejected -> ShellRejected
	3:  {outer: 6, path: 1, reason: 2},                          // WriteResult.rejected -> WriteRejected
	4:  {outer: 6, path: 1, reason: 2},                          // DeleteResult.rejected -> DeleteRejected
	5:  {outer: 2, reason: 1},                                   // GrepResult.error -> GrepError
	7:  {outer: 3, path: 1, reason: 2},                          // ReadResult.rejected -> ReadRejected
	8:  {outer: 3, path: 1, reason: 2},                          // LsResult.rejected -> LsRejected
	9:  {outer: 3, path: 1, reason: 2},                          // DiagnosticsResult.rejected -> DiagnosticsRejected
	14: {outer: 5, inner: 0, path: 1, reason: 3, command: true}, // ShellStream.rejected -> ShellRejected
	16: {outer: 3, inner: 0, path: 1, reason: 3, command: true}, // BackgroundShellSpawnResult.rejected -> ShellRejected
	17: {outer: 3, reason: 1},                                   // ListMcpResourcesExecResult.rejected
	18: {outer: 3, path: 1, reason: 2},                          // ReadMcpResourceExecResult.rejected
	20: {outer: 2, path: 1, reason: 2, url: true},               // FetchResult.error -> FetchError
	21: {outer: 4, reason: 1},                                   // RecordScreenResult.failure
	22: {outer: 2, reason: 1},                                   // ComputerUseResult.error
	23: {outer: 2, reason: 1},                                   // WriteShellStdinResult.error
	28: {outer: 2, reason: 2},                                   // SubagentResult.error; agent_id is optional
	29: {outer: 3, path: 1, reason: 2},                          // redacted_read shares ReadResult
	30: {outer: 1, enum: true, enumVal: 2},                      // ForceBackgroundShellResult.status = NOT_FOUND
	31: {outer: 1, enum: true, enumVal: 2},                      // ForceBackgroundSubagentResult.status = NOT_FOUND
	36: {outer: 3, reason: 1},                                   // McpStateExecResult.rejected
	37: {outer: 4, reason: 2},                                   // SubagentAwaitResult.error
	38: {outer: 2, reason: 1},                                   // SmartModeClassifierResult.error
	40: {outer: 2, path: 1, reason: 2},                          // CanvasDiagnosticsResult.error
	41: {outer: 1, enum: true, enumVal: 0},                      // allowlisted=false
	42: {outer: 1, enum: true, enumVal: 0},
	43: {outer: 1, enum: true, enumVal: 0},
	46: {outer: 2, reason: 1},                                   // PiReadExecResult.error
	47: {outer: 2, reason: 1},                                   // PiBashExecResult.error
	48: {outer: 3, reason: 1},                                   // PiEditExecResult.rejected
	49: {outer: 3, reason: 1},                                   // PiWriteExecResult.rejected
	50: {outer: 2, reason: 1},                                   // PiGrepExecResult.error
	51: {outer: 2, reason: 1},                                   // PiFindExecResult.error
	52: {outer: 2, reason: 1},                                   // PiLsExecResult.error
	53: {outer: 2, reason: 1},                                   // ConversationSearchResult.error
	54: {outer: 2, reason: 1},                                   // AgentStoreConflictResult.error
	55: {outer: 4, inner: 0, path: 1, reason: 3, command: true}, // mini_swe reuses ShellResult
	56: {outer: 5, reason: 0},                                   // AdoptResult.error is a string oneof
}

const execUnavailableReason = "not available; use the declared MCP tools"

// MCPAvailabilityNote tells the model that only the declared MCP tools exist.
// Names come from the current request, not a hardcoded catalog.
func MCPAvailabilityNote(tools []ir.Tool) string {
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
	return "You have no Cursor built-in tools in this session. Your only tools are the MCP tools: " +
		strings.Join(names, ", ") + ". Always call those by name."
}
