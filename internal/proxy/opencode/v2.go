package opencode

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"airouter/internal/proxy/ir"
)

const V2UserAgent = "opencode/latest/2.0.26/cli"

// Public tool definitions from 9router v0.5.116 (1339107), not verified
// statements about upstream admission policy.
//
// MIT License
// Copyright (c) 2024-2026 decolua and contributors
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
const (
	v2ReadDescription  = "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files."
	v2ReadSchema       = `{"type":"object","properties":{"path":{"type":"string","description":"File or directory to read"},"offset":{"type":"integer","minimum":0,"description":"The line or directory entry to start reading from (1-based)"},"limit":{"type":"integer","minimum":0,"description":"The maximum number of lines or directory entries to read (defaults to and capped at 2000)"}},"required":["path"],"additionalProperties":false}`
	v2ShellDescription = "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete."
	v2ShellSchema      = `{"type":"object","properties":{"command":{"type":"string","description":"Shell command string to execute"},"workdir":{"type":"string","description":"Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},"timeout":{"type":"integer","minimum":0,"description":"Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},"background":{"type":"boolean","description":"Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."}},"required":["command"],"additionalProperties":false}`
)

// UseV2Profile excludes paid, Go, custom URLs and big-pickle. endpoint is the
// resolved inference URL, not a discovery base or a caller-supplied identity.
func UseV2Profile(endpoint, model string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" && !strings.HasSuffix(u.Path, "/") &&
		IsOfficialFreeTierURL(endpoint) && strings.HasSuffix(NormalizeModelID(model), "-free")
}

func FingerprintV2Headers(h http.Header, session string) {
	if h == nil {
		return
	}
	h.Set("User-Agent", V2UserAgent)
	if h.Get("x-opencode-client") == "" {
		h.Set("x-opencode-client", FreeClient)
	}
	if h.Get("x-opencode-project") == "" {
		h.Set("x-opencode-project", "global")
	}
	h.Del("x-opencode-request")
	for _, key := range []string{"x-opencode-session", SessionIDHeader, "x-session-affinity", "x-session-id"} {
		h.Del(key)
		if IsNativeSessionID(session) {
			h.Set(key, session)
		}
	}
}

func v2Definition(name string) (string, json.RawMessage) {
	if name == "read" {
		return v2ReadDescription, json.RawMessage(v2ReadSchema)
	}
	return v2ShellDescription, json.RawMessage(v2ShellSchema)
}

func v2JSONEqual(a, b []byte) bool {
	decode := func(raw []byte) (any, error) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		err := dec.Decode(&v)
		return v, err
	}
	x, e1 := decode(a)
	y, e2 := decode(b)
	return e1 == nil && e2 == nil && reflect.DeepEqual(x, y)
}

// PrepareV2Body preserves caller contracts. A shared property name cannot
// establish compatible units, required fields, constraints, or descriptions.
func PrepareV2Body(body []byte, codecID string) ([]byte, ToolMap, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, ToolMap{}, ir.ProtocolError("opencode: invalid request object")
	}
	if codecID != "opencode-chat" && codecID != "opencode-responses" && codecID != "opencode-messages" {
		return body, ToolMap{}, nil
	}
	tools, err := decodeRawArray(root["tools"])
	if err != nil {
		return nil, ToolMap{}, err
	}
	mapping := ToolMap{SentToCaller: map[string]string{}, Decoys: map[string]bool{}, Allowed: map[string]bool{}, FragmentNames: codecID == "opencode-chat"}
	defs := make([]map[string]json.RawMessage, len(tools))
	wrappers := make([]map[string]json.RawMessage, len(tools))
	taken, present := map[string]bool{}, map[string]bool{}
	schemaKey := "parameters"
	if codecID == "opencode-messages" {
		schemaKey = "input_schema"
	}
	for i, raw := range tools {
		var tool map[string]json.RawMessage
		if json.Unmarshal(raw, &tool) != nil || tool == nil {
			return nil, ToolMap{}, ir.ProtocolError("opencode: invalid tool definition")
		}
		wrappers[i], defs[i] = tool, tool
		if codecID == "opencode-chat" {
			defs[i] = nil
			if json.Unmarshal(tool["function"], &defs[i]) != nil || defs[i] == nil {
				return nil, ToolMap{}, ir.ProtocolError("opencode: invalid function definition")
			}
		}
		name := rawString(defs[i]["name"])
		if name == "" || name != strings.TrimSpace(name) || taken[name] {
			return nil, ToolMap{}, ir.ProtocolError("opencode: duplicate or invalid tool name")
		}
		taken[name] = true
	}
	for i, def := range defs {
		name := rawString(def["name"])
		base := ""
		if strings.Contains(strings.ToLower(name), "terminal") {
			base = "run_command"
		} else if name == "read" || name == "shell" {
			description, schema := v2Definition(name)
			if rawString(def["description"]) == description && v2JSONEqual(def[schemaKey], schema) {
				present[name] = true
			} else {
				base = name + "_client"
			}
		}
		wire := name
		if base != "" {
			wire = base
			for suffix := 2; taken[wire]; suffix++ {
				wire = base + strconv.Itoa(suffix)
			}
			taken[wire] = true
			mapping.SentToCaller[wire] = name
			def["name"], _ = json.Marshal(wire)
			if codecID == "opencode-chat" {
				wrappers[i]["function"], _ = json.Marshal(def)
			}
			tools[i], _ = json.Marshal(wrappers[i])
		}
		mapping.Allowed[wire] = true
	}
	missing := []string{}
	for _, name := range []string{"read", "shell"} {
		if present[name] {
			continue
		}
		description, schema := v2Definition(name)
		var tool any = map[string]any{"name": name, "description": description, schemaKey: schema}
		if codecID == "opencode-chat" {
			tool = map[string]any{"type": "function", "function": tool}
		} else if codecID == "opencode-responses" {
			tool = map[string]any{"type": "function", "name": name, "description": description, "parameters": schema, "strict": false}
		}
		raw, _ := json.Marshal(tool)
		tools = append(tools, raw)
		missing = append(missing, name)
		mapping.Decoys[name] = true
	}
	root["tools"], _ = json.Marshal(tools)
	switch codecID {
	case "opencode-chat":
		rewriteChatHistory(root, mapping)
	case "opencode-responses":
		rewriteInputNames(root, mapping)
	case "opencode-messages":
		rewriteMessageToolUses(root, mapping)
	}
	var choice map[string]json.RawMessage
	if json.Unmarshal(root["tool_choice"], &choice) == nil && choice != nil {
		def := choice
		if codecID == "opencode-chat" {
			def = nil
			_ = json.Unmarshal(choice["function"], &def)
		}
		if wire := callerWireName(rawString(def["name"]), mapping); wire != "" {
			def["name"], _ = json.Marshal(wire)
			if codecID == "opencode-chat" {
				choice["function"], _ = json.Marshal(def)
			}
			root["tool_choice"], _ = json.Marshal(choice)
		}
	}
	if len(mapping.Allowed) == 0 && len(root["tool_choice"]) == 0 {
		if codecID == "opencode-messages" {
			root["tool_choice"] = json.RawMessage(`{"type":"none"}`)
		} else {
			root["tool_choice"] = json.RawMessage(`"none"`)
		}
	} else if codecID == "opencode-responses" && len(root["tool_choice"]) == 0 {
		root["tool_choice"] = json.RawMessage(`"auto"`)
	}
	mapping.NoTools = rawString(root["tool_choice"]) == "none" || rawString(choice["type"]) == "none" || len(mapping.Allowed) == 0
	if len(missing) > 0 {
		quoted := make([]string, len(missing))
		for i, name := range missing {
			quoted[i] = "`" + name + "`"
		}
		note := "Do not call the " + strings.Join(quoted, " or ") + " tools; they are placeholders that do nothing. Use the other tools you have been given instead."
		appendV2Note(root, codecID, note)
	}
	out, err := json.Marshal(root)
	return AppendEnvironment(out, codecID), mapping, err
}

func appendV2Note(root map[string]json.RawMessage, codecID, note string) {
	if codecID == "opencode-responses" {
		root["instructions"], _ = json.Marshal(joinSystemText(rawString(root["instructions"]), note))
		return
	}
	if codecID == "opencode-messages" {
		root["system"] = appendV2Text(root["system"], note)
		return
	}
	messages, _ := decodeRawArray(root["messages"])
	for i, raw := range messages {
		var msg map[string]json.RawMessage
		if json.Unmarshal(raw, &msg) == nil && rawString(msg["role"]) == "system" {
			msg["content"] = appendV2Text(msg["content"], note)
			messages[i], _ = json.Marshal(msg)
			root["messages"], _ = json.Marshal(messages)
			return
		}
	}
	raw, _ := json.Marshal(map[string]string{"role": "system", "content": note})
	root["messages"], _ = json.Marshal(append([]json.RawMessage{raw}, messages...))
}

func appendV2Text(raw json.RawMessage, note string) json.RawMessage {
	if text, ok := asJSONString(raw); ok {
		out, _ := json.Marshal(joinSystemText(text, note))
		return out
	}
	blocks, err := decodeRawArray(raw)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		out, _ := json.Marshal(note)
		return out
	}
	out, _ := json.Marshal(appendTextBlock(blocks, note))
	return out
}
