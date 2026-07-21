package antigravity

// DefaultTools are Antigravity native tool names left unsuffixed when the client
// already uses them. Source: 9router AG_DEFAULT_TOOLS.
var DefaultTools = map[string]bool{
	"browser_subagent":              true,
	"command_status":                true,
	"find_by_name":                  true,
	"generate_image":                true,
	"grep_search":                   true,
	"list_dir":                      true,
	"list_resources":                true,
	"multi_replace_file_content":    true,
	"notify_user":                   true,
	"read_resource":                 true,
	"read_terminal":                 true,
	"read_url_content":              true,
	"replace_file_content":          true,
	"run_command":                   true,
	"search_web":                    true,
	"send_command_input":            true,
	"task_boundary":                 true,
	"view_content_chunk":            true,
	"view_file":                     true,
	"write_to_file":                 true,
}

// DecoyTools are native AG names injected after client tools so the declared
// catalog looks closer to the real IDE (anti-ban). Descriptions mark them
// unavailable. Source: 9router AG_DECOY_TOOLS.
var DecoyTools = []functionDecl{
	decoy("browser_subagent"),
	decoy("command_status"),
	decoy("find_by_name"),
	decoy("generate_image"),
	decoy("grep_search"),
	decoy("list_dir"),
	decoy("list_resources"),
	decoy("mcp_sequential-thinking_sequentialthinking"),
	decoy("multi_replace_file_content"),
	decoy("notify_user"),
	decoy("read_resource"),
	decoy("read_terminal"),
	decoy("read_url_content"),
	decoy("replace_file_content"),
	decoy("run_command"),
	decoy("search_web"),
	decoy("send_command_input"),
	decoy("task_boundary"),
	decoy("view_content_chunk"),
	decoy("view_file"),
	decoy("write_to_file"),
}

func decoy(name string) functionDecl {
	return functionDecl{
		Name:        name,
		Description: "This tool is currently unavailable.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

// CloakTools renames non-native client tools with ToolSuffix, rewrites history
// functionCall/functionResponse names the same way, and appends DecoyTools.
// No-op when tools are empty (decoys are not injected alone). Returns
// map[cloakedName]originalName for callers that want an explicit reverse map;
// DecodeStream uses DecloakName instead.
func CloakTools(req *geminiRequest) map[string]string {
	if req == nil || len(req.Tools) == 0 {
		return nil
	}
	toolNameMap := map[string]string{}
	var clientDecls []functionDecl
	seen := map[string]bool{}

	for _, group := range req.Tools {
		for _, fn := range group.FunctionDeclarations {
			if fn.Name == "" {
				continue
			}
			if DefaultTools[fn.Name] {
				if !seen[fn.Name] {
					seen[fn.Name] = true
					clientDecls = append(clientDecls, fn)
				}
				continue
			}
			suffixed := fn.Name + ToolSuffix
			toolNameMap[suffixed] = fn.Name
			fn.Name = suffixed
			if !seen[fn.Name] {
				seen[fn.Name] = true
				clientDecls = append(clientDecls, fn)
			}
		}
	}

	all := make([]functionDecl, 0, len(clientDecls)+len(DecoyTools))
	all = append(all, clientDecls...)
	for _, d := range DecoyTools {
		if seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		all = append(all, d)
	}
	req.Tools = []geminiToolGroup{{FunctionDeclarations: all}}

	// Rewrite tool names in conversation history.
	for i := range req.Contents {
		for j := range req.Contents[i].Parts {
			p := &req.Contents[i].Parts[j]
			if p.FunctionCall != nil && p.FunctionCall.Name != "" && !DefaultTools[p.FunctionCall.Name] {
				p.FunctionCall.Name = p.FunctionCall.Name + ToolSuffix
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name != "" && !DefaultTools[p.FunctionResponse.Name] {
				p.FunctionResponse.Name = p.FunctionResponse.Name + ToolSuffix
			}
		}
	}
	if len(toolNameMap) == 0 {
		return nil
	}
	return toolNameMap
}

// DecloakName strips one ToolSuffix from a wire tool name so the client sees
// the original. Native/decoy names are left unchanged. Client tools that already
// ended in _ide become _ide_ide outbound; strip once yields the original.
func DecloakName(name string) string {
	if name == "" || DefaultTools[name] {
		return name
	}
	// Do not strip decoy-only names that happen to end with the suffix (none do).
	if len(name) > len(ToolSuffix) && name[len(name)-len(ToolSuffix):] == ToolSuffix {
		base := name[:len(name)-len(ToolSuffix)]
		if base != "" {
			return base
		}
	}
	return name
}
