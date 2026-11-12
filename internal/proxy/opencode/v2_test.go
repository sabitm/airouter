package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func v2TestBody(codecID string, defs []map[string]any) []byte {
	root := map[string]any{"model": "deepseek-v4.1-flash-free", "max_tokens": 65536, "reasoning_effort": "max", "messages": []any{map[string]any{"role": "user", "content": "local"}}}
	tools := []any{}
	for _, def := range defs {
		if codecID == "opencode-chat" {
			tools = append(tools, map[string]any{"type": "function", "function": def})
		} else {
			tool := map[string]any{}
			for k, v := range def {
				tool[k] = v
			}
			if codecID == "opencode-messages" {
				tool["input_schema"] = tool["parameters"]
				delete(tool, "parameters")
			} else {
				tool["type"] = "function"
			}
			tools = append(tools, tool)
		}
	}
	root["tools"] = tools
	if codecID == "opencode-responses" {
		delete(root, "messages")
		root["input"] = []any{map[string]any{"type": "function_call", "name": "read", "call_id": "call_1", "arguments": `{"n":9007199254740993}`}}
		root["tool_choice"] = map[string]any{"type": "function", "name": "read"}
	} else if codecID == "opencode-messages" {
		root["messages"] = []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_1", "name": "read", "input": json.RawMessage(`{"n":9007199254740993}`)}}}}
		root["tool_choice"] = map[string]any{"type": "tool", "name": "read"}
	} else {
		root["messages"] = []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "function": map[string]any{"name": "read", "arguments": `{"n":9007199254740993}`}}}}}
		root["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
	}
	raw, _ := json.Marshal(root)
	return raw
}

func v2TestDefinitions() []map[string]any {
	return []map[string]any{
		{"name": "read", "description": "caller read", "parameters": json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"n":{"minimum":9007199254740993}},"required":["path","n"],"additionalProperties":false}`)},
		{"name": "read_client", "description": "slot", "parameters": json.RawMessage(`{"type":"object"}`)},
		{"name": "shell", "description": "timeout in seconds", "parameters": json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer","description":"seconds"}},"required":["command"]}`)},
		{"name": "OPS_TERMINAL", "description": "terminal", "parameters": json.RawMessage(`{"type":"object"}`)},
		{"name": "run_command", "description": "slot", "parameters": json.RawMessage(`{"type":"object"}`)},
		{"name": "Read", "description": "case distinct", "parameters": json.RawMessage(`{"type":"object"}`)},
	}
}

func TestV2ProfileScope(t *testing.T) {
	for _, endpoint := range []string{"https://opencode.ai/inference/openai/v1/chat/completions", "https://opencode.ai/inference/anthropic/v1/messages", "https://opencode.ai/inference/v1/responses", "https://opencode.ai:443/zen/v1/chat/completions"} {
		if !UseV2Profile(endpoint, "deepseek-v4.1-flash-free") {
			t.Fatal("official free endpoint excluded")
		}
		for _, model := range []string{"big-pickle", "deepseek-v4.1-flash", "x-free-other"} {
			if UseV2Profile(endpoint, model) {
				t.Fatal("non-free row included")
			}
		}
	}
	for _, endpoint := range []string{"https://opencode.ai/inference/go/openai/v1/chat/completions", "https://example.test/inference/v1/chat/completions", "http://opencode.ai/inference/v1/chat/completions", "https://user@opencode.ai/inference/v1/chat/completions", "https://opencode.ai:444/inference/v1/chat/completions", "https://opencode.ai/inference/v1/models", "https://opencode.ai/inference/v1/chat/completions?q=1", "https://opencode.ai/inference/v1/chat/completions#x", "https://opencode.ai/inference/v1/chat/completions/"} {
		if UseV2Profile(endpoint, "x-free") {
			t.Fatal("nonofficial endpoint included")
		}
	}
}

func TestV2Headers(t *testing.T) {
	session := "ses_0123456789abABCDEFGHIJKLMN"
	h := http.Header{"Authorization": {"Bearer private"}, "User-Agent": {"opencode/1.19.5"}}
	h.Set("x-opencode-request", "msg_0123456789abABCDEFGHIJKLMN")
	h.Add("x-session-id", "wrong")
	h.Add("x-session-id", "other")
	FingerprintV2Headers(h, session)
	if h.Get("User-Agent") != V2UserAgent || h.Get("x-opencode-request") != "" || h.Get("Authorization") != "Bearer private" || h.Get("x-opencode-client") != "cli" || h.Get("x-opencode-project") != "global" {
		t.Fatal("v2 identity mismatch")
	}
	for _, key := range []string{"x-opencode-session", SessionIDHeader, "x-session-id", "x-session-affinity"} {
		if h.Get(key) != session || len(h.Values(key)) != 1 {
			t.Fatal("session metadata drift")
		}
	}
}

func TestV2BodyContractsAndHistory(t *testing.T) {
	for _, codecID := range []string{"opencode-chat", "opencode-responses", "opencode-messages"} {
		t.Run(codecID, func(t *testing.T) {
			in := v2TestBody(codecID, v2TestDefinitions())
			before := append([]byte(nil), in...)
			out, mapping, err := PrepareV2Body(in, codecID)
			if err != nil || !bytes.Equal(in, before) {
				t.Fatal("preparation failed or mutated input")
			}
			if mapping.SentToCaller["read_client2"] != "read" || mapping.SentToCaller["shell_client"] != "shell" || mapping.SentToCaller["run_command2"] != "OPS_TERMINAL" || !mapping.Allowed["Read"] || !mapping.Decoys["read"] || !mapping.Decoys["shell"] {
				t.Fatal("alias or allowlist mismatch")
			}
			var root map[string]json.RawMessage
			_ = json.Unmarshal(out, &root)
			if string(root["max_tokens"]) != "65536" || rawString(root["reasoning_effort"]) != "max" || !bytes.Contains(out, []byte("9007199254740993")) {
				t.Fatal("caps reasoning or precision changed")
			}
			var tools []map[string]json.RawMessage
			_ = json.Unmarshal(root["tools"], &tools)
			byName := map[string]map[string]json.RawMessage{}
			for _, tool := range tools {
				if codecID == "opencode-chat" {
					var fn map[string]json.RawMessage
					_ = json.Unmarshal(tool["function"], &fn)
					tool = fn
				}
				name := rawString(tool["name"])
				if byName[name] != nil {
					t.Fatal("duplicate wire tool")
				}
				byName[name] = tool
			}
			key := "parameters"
			if codecID == "opencode-messages" {
				key = "input_schema"
			}
			for _, def := range v2TestDefinitions() {
				name := def["name"].(string)
				wire := callerWireName(name, mapping)
				if wire == "" {
					wire = name
				}
				if rawString(byName[wire]["description"]) != def["description"] || !v2JSONEqual(byName[wire][key], def["parameters"].(json.RawMessage)) {
					t.Fatal("caller contract replaced")
				}
			}
			for _, name := range []string{"read", "shell"} {
				description, schema := v2Definition(name)
				if rawString(byName[name]["description"]) != description || !v2JSONEqual(byName[name][key], schema) {
					t.Fatal("pinned definition differs")
				}
			}
			if !bytes.Contains(root["tool_choice"], []byte("read_client2")) {
				t.Fatal("choice not retargeted")
			}
			history := root["messages"]
			if codecID == "opencode-responses" {
				history = root["input"]
			}
			if !bytes.Contains(history, []byte("read_client2")) {
				t.Fatal("history not retargeted")
			}
			again, _, err := PrepareV2Body(out, codecID)
			if err != nil || !bytes.Equal(out, again) {
				t.Fatal("body preparation not idempotent")
			}
			if name, err := RestoreCallerName("Read", mapping); err != nil || name != "Read" {
				t.Fatal("case-distinct caller blocked")
			}
		})
	}
}

func TestV2ExactDefinitionsAndNone(t *testing.T) {
	for _, codecID := range []string{"opencode-chat", "opencode-responses", "opencode-messages"} {
		defs := []map[string]any{}
		for _, name := range []string{"read", "shell"} {
			desc, schema := v2Definition(name)
			defs = append(defs, map[string]any{"name": name, "description": desc, "parameters": schema})
		}
		body := v2TestBody(codecID, defs)
		_, mapping, err := PrepareV2Body(body, codecID)
		if err != nil || len(mapping.Decoys) != 0 || len(mapping.SentToCaller) != 0 || !mapping.Allowed["read"] || !mapping.Allowed["shell"] {
			t.Fatal("exact caller tools changed")
		}
		var root map[string]json.RawMessage
		_ = json.Unmarshal(body, &root)
		root["tool_choice"] = json.RawMessage(`"none"`)
		if codecID == "opencode-messages" {
			root["tool_choice"] = json.RawMessage(`{"type":"none"}`)
		}
		body, _ = json.Marshal(root)
		out, mapping, err := PrepareV2Body(body, codecID)
		var after map[string]json.RawMessage
		_ = json.Unmarshal(out, &after)
		if err != nil || !bytes.Equal(root["tool_choice"], after["tool_choice"]) {
			t.Fatal("explicit none changed")
		}
		if _, err := RestoreCallerName("read", mapping); err == nil {
			t.Fatal("explicit none authorized tool")
		}
		_, mapping, err = PrepareV2Body(v2TestBody(codecID, nil), codecID)
		if err != nil || !mapping.NoTools || len(mapping.Allowed) != 0 {
			t.Fatal("tool-less authorization changed")
		}
	}
	for _, defs := range [][]map[string]any{{{"name": ""}}, {{"name": "lookup"}, {"name": "lookup"}}, {{"name": " read "}}} {
		if _, _, err := PrepareV2Body(v2TestBody("opencode-chat", defs), "opencode-chat"); err == nil {
			t.Fatal("ambiguous tool name accepted")
		}
	}
}

func TestV2ResponseIsolation(t *testing.T) {
	_, mapping, err := PrepareV2Body(v2TestBody("opencode-chat", v2TestDefinitions()), "opencode-chat")
	if err != nil {
		t.Fatal(err)
	}
	in := &ir.Response{Content: []ir.ContentBlock{{Type: ir.BlockToolUse, ToolName: "read_client2", ToolInput: json.RawMessage(`{"n":9007199254740993}`)}}}
	out, err := FilterResponse(in, mapping)
	if err != nil || out == in || out.Content[0].ToolName != "read" || in.Content[0].ToolName != "read_client2" {
		t.Fatal("response restoration mutated decoder result")
	}
	for _, name := range []string{"read", "shell", "madeup"} {
		in.Content[0].ToolName = name
		out, err := FilterResponse(in, mapping)
		if err == nil || out != nil || strings.Contains(err.Error(), "madeup") {
			t.Fatal("invalid tool not safely rejected")
		}
	}
}

func TestV2StreamFragments(t *testing.T) {
	_, mapping, _ := PrepareV2Body(v2TestBody("opencode-chat", v2TestDefinitions()), "opencode-chat")
	decode := func(_ io.Reader, emit func(ir.StreamEvent) error) error {
		for _, ev := range []ir.StreamEvent{
			{Kind: ir.EventTextDelta, Text: "text"},
			{Kind: ir.EventToolCallStart, ToolID: "call_1", ToolName: "read_"},
			{Kind: ir.EventToolCallDelta, ArgsFrag: `{"n":`},
			{Kind: ir.EventToolCallStart, ToolName: "client2"},
			{Kind: ir.EventToolCallDelta, ArgsFrag: `9007199254740993}`},
			{Kind: ir.EventFinish, InputTokens: 7, OutputTokens: 2},
		} {
			if err := emit(ev); err != nil {
				return err
			}
		}
		return nil
	}
	var events []ir.StreamEvent
	err := DecodeStream(decode, mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var name, args, text string
	var finish ir.StreamEvent
	for _, ev := range events {
		switch ev.Kind {
		case ir.EventToolCallStart:
			name += ev.ToolName
		case ir.EventToolCallDelta:
			args += ev.ArgsFrag
		case ir.EventTextDelta:
			text += ev.Text
		case ir.EventFinish:
			finish = ev
		}
	}
	if name != "read" || args != `{"n":9007199254740993}` || text != "text" || finish.InputTokens != 7 || finish.OutputTokens != 2 {
		t.Fatal("fragment text or usage restoration failed")
	}
}

func TestV2StreamSafety(t *testing.T) {
	_, mapping, _ := PrepareV2Body(v2TestBody("opencode-chat", v2TestDefinitions()), "opencode-chat")
	for _, mode := range []string{"decoy", "unknown", "incomplete", "error", "fragments", "indices", "bytes", "atomic", "identity"} {
		t.Run(mode, func(t *testing.T) {
			decode := func(_ io.Reader, emit func(ir.StreamEvent) error) error {
				if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: "text"}); err != nil {
					return err
				}
				n := 1
				if mode == "fragments" {
					n = maxHeldToolFragments + 1
				}
				if mode == "indices" {
					n = maxHeldToolIndices + 1
				}
				for i := 0; i < n; i++ {
					ev := ir.StreamEvent{Kind: ir.EventToolCallDelta, ArgsFrag: "secret"}
					if mode == "indices" {
						ev.Index = i
					}
					if mode == "bytes" {
						ev.ArgsFrag = strings.Repeat("x", maxHeldToolBytes+1)
					}
					if err := emit(ev); err != nil {
						return err
					}
				}
				name := "shell"
				if mode == "unknown" {
					name = "madeup"
				}
				if mode == "atomic" || mode == "identity" {
					name = "read_client2"
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallStart, ToolName: name, ToolID: "secret-id"}); err != nil {
					return err
				}
				if mode == "incomplete" {
					return nil
				}
				if mode == "error" {
					return errors.New("upstream failure")
				}
				if mode == "identity" {
					return emit(ir.StreamEvent{Kind: ir.EventToolCallStart, ToolID: "different"})
				}
				if mode == "atomic" {
					if err := emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 1, ToolName: "shell"}); err != nil {
						return err
					}
				}
				return emit(ir.StreamEvent{Kind: ir.EventFinish})
			}
			kinds := []ir.StreamEventKind{}
			err := DecodeStream(decode, mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error { kinds = append(kinds, ev.Kind); return nil })
			if err == nil || !reflect.DeepEqual(kinds, []ir.StreamEventKind{ir.EventTextDelta}) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "madeup") {
				t.Fatal("held tool leaked or safety bound missing")
			}
		})
	}
}
