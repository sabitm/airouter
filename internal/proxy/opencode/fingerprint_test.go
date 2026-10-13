package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestNeedsFreeTier(t *testing.T) {
	zen := ZenBaseURL
	custom := "https://example.test/v1"
	goURL := GoBaseURL
	cases := []struct {
		base, model string
		want        bool
	}{
		{zen, "big-pickle", true},
		{zen, "opencode/big-pickle", true},
		{zen, "qwen3.6-plus-free(vip)", true},
		{zen, "MINIMAX-M3-FREE", true},
		{zen, "muse-spark-1.3-contributor-free", true},
		{zen, "minimax-m3-free", true},
		{zen, "qwen3.6-plus-free", true},
		{zen, "deepseek-v4-pro", false},
		{zen, "gpt-5", false},
		{custom, "big-pickle", true},
		{custom, "minimax-m3-free", true},
		{goURL, "big-pickle", false},
		{goURL, "ox-alpha-free", false},
		{goURL, "minimax-m3", false},
	}
	for _, tc := range cases {
		if got := NeedsFreeTier(tc.base, tc.model); got != tc.want {
			t.Errorf("NeedsFreeTier(%q, %q) = %v, want %v", tc.base, tc.model, got, tc.want)
		}
	}
}

func TestPrepareFreeTierChatTools(t *testing.T) {
	in := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Bash","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}},{"type":"function","function":{"name":"shell","description":"caller shell","parameters":{"type":"object"}}}]}`)
	out, mapping, err := PrepareFreeTierBody(in, "opencode-chat")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tools []struct {
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice string `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.ToolChoice != "" {
		t.Fatalf("explicit choice invented: %q", got.ToolChoice)
	}
	names := map[string]string{}
	params := map[string]string{}
	for _, tool := range got.Tools {
		names[tool.Function.Name] = tool.Function.Description
		params[tool.Function.Name] = string(tool.Function.Parameters)
	}
	if names["bash"] != "run" || !strings.Contains(params["bash"], `"cmd"`) || !strings.Contains(params["bash"], `"required":["cmd"]`) {
		t.Fatalf("bash schema lost: %+v", got.Tools[0].Function)
	}
	if names["shell"] != "caller shell" {
		t.Fatalf("shell not preserved: %+v", names)
	}
	for _, name := range FingerprintTools {
		if _, ok := names[name]; !ok {
			t.Fatalf("missing %s in %+v", name, names)
		}
	}
	if names["glob"] != decoyToolDescription {
		t.Fatalf("decoy description = %q", names["glob"])
	}
	if mapping.SentToCaller["bash"] != "Bash" || mapping.Decoys["bash"] {
		t.Fatalf("map = %+v", mapping)
	}
	again, _, err := PrepareFreeTierBody(out, "opencode-chat")
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Fatalf("not idempotent:\n%s\n%s", again, out)
	}
}

func TestPrepareFreeTierEmptyToolsSetsNone(t *testing.T) {
	out, mapping, err := PrepareFreeTierBody([]byte(`{"model":"big-pickle","messages":[]}`), "opencode-chat")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ToolChoice string `json:"tool_choice"`
		Tools      []any  `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.ToolChoice != "none" || len(got.Tools) != 4 {
		t.Fatalf("tool_choice=%q tools=%d", got.ToolChoice, len(got.Tools))
	}
	if len(mapping.Decoys) != 4 {
		t.Fatalf("decoys = %+v", mapping.Decoys)
	}
}

func TestPrepareFreeTierPreservesExplicitChoice(t *testing.T) {
	cases := []struct {
		name   string
		codec  string
		body   string
		choice func(t *testing.T, raw json.RawMessage)
	}{
		{
			name:  "chat named",
			codec: "opencode-chat",
			body:  `{"model":"big-pickle","tool_choice":{"type":"function","function":{"name":"Read"}},"tools":[{"type":"function","function":{"name":"Read","description":"file","parameters":{"type":"object"}}}]}`,
			choice: func(t *testing.T, raw json.RawMessage) {
				var c struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if json.Unmarshal(raw, &c) != nil || c.Function.Name != "read" {
					t.Fatalf("chat choice = %s", raw)
				}
			},
		},
		{
			name:  "responses named",
			codec: "opencode-responses",
			body:  `{"model":"muse-spark-1.3-contributor-free","tool_choice":{"type":"function","name":"Grep"},"tools":[{"type":"function","name":"Grep","description":"find","parameters":{"type":"object"}}]}`,
			choice: func(t *testing.T, raw json.RawMessage) {
				var c struct {
					Type string `json:"type"`
					Name string `json:"name"`
				}
				if json.Unmarshal(raw, &c) != nil || c.Type != "function" || c.Name != "grep" {
					t.Fatalf("responses choice = %s", raw)
				}
			},
		},
		{
			name:  "messages named",
			codec: "opencode-messages",
			body:  `{"model":"minimax-m3-free","max_tokens":16,"tool_choice":{"type":"tool","name":"Read","disable_parallel_tool_use":true},"tools":[{"name":"Read","description":"file","input_schema":{"type":"object"}}]}`,
			choice: func(t *testing.T, raw json.RawMessage) {
				var c struct {
					Type string `json:"type"`
					Name string `json:"name"`
					Dis  bool   `json:"disable_parallel_tool_use"`
				}
				if json.Unmarshal(raw, &c) != nil || c.Type != "tool" || c.Name != "read" || !c.Dis {
					t.Fatalf("messages choice = %s", raw)
				}
			},
		},
		{
			name:  "chat required string",
			codec: "opencode-chat",
			body:  `{"model":"big-pickle","tool_choice":"required","tools":[{"type":"function","function":{"name":"Read","description":"file","parameters":{"type":"object"}}}]}`,
			choice: func(t *testing.T, raw json.RawMessage) {
				if string(raw) != `"required"` {
					t.Fatalf("chat string choice = %s", raw)
				}
			},
		},
		{
			name:  "messages auto object",
			codec: "opencode-messages",
			body:  `{"model":"minimax-m3-free","max_tokens":16,"tool_choice":{"type":"auto"},"tools":[{"name":"Read","description":"file","input_schema":{"type":"object"}}]}`,
			choice: func(t *testing.T, raw json.RawMessage) {
				var c struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &c) != nil || c.Type != "auto" {
					t.Fatalf("messages auto choice = %s", raw)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := PrepareFreeTierBody([]byte(tc.body), tc.codec)
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]json.RawMessage
			if err := json.Unmarshal(out, &root); err != nil {
				t.Fatal(err)
			}
			tc.choice(t, root["tool_choice"])
		})
	}
}

func TestPrepareFreeTierCollisionErrors(t *testing.T) {
	cases := []struct {
		name  string
		codec string
		body  string
	}{
		{"chat", "opencode-chat", `{"tools":[{"type":"function","function":{"name":"Bash","description":"a","parameters":{"type":"object"}}},{"type":"function","function":{"name":"bash","description":"b","parameters":{"type":"object"}}}]}`},
		{"responses", "opencode-responses", `{"tools":[{"type":"function","name":"Read","description":"a","parameters":{"type":"object"}},{"type":"function","name":"rEaD","description":"b","parameters":{"type":"object"}}]}`},
		{"messages", "opencode-messages", `{"tools":[{"name":"Grep","description":"a","input_schema":{"type":"object"}},{"name":"grep","description":"b","input_schema":{"type":"object"}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := PrepareFreeTierBody([]byte(tc.body), tc.codec); err == nil {
				t.Fatal("collision accepted")
			}
		})
	}
	if _, _, err := PrepareFreeTierBody([]byte(`{"tools":"nope"}`), "opencode-chat"); err == nil {
		t.Fatal("malformed tools accepted")
	}
	out, _, err := PrepareFreeTierBody([]byte(`not-json`), "opencode-chat")
	if err != nil || string(out) != "not-json" {
		t.Fatalf("malformed body changed: %v %s", err, out)
	}
}

func TestPrepareFreeTierFlatToolShapes(t *testing.T) {
	t.Run("responses", func(t *testing.T) {
		in := []byte(`{"model":"muse-spark-1.3-contributor-free","input":[{"type":"function_call","name":"Grep","arguments":"{}"},{"type":"function_call","name":"Other","arguments":"{}"}],"tools":[{"type":"web_search_preview"},{"type":"function","name":"Grep","description":"find","parameters":{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}}]}`)
		out, mapping, err := PrepareFreeTierBody(in, "opencode-responses")
		if err != nil {
			t.Fatal(err)
		}
		if mapping.SentToCaller["grep"] != "Grep" {
			t.Fatalf("responses map = %+v", mapping)
		}
		var got struct {
			Tools []map[string]json.RawMessage `json:"tools"`
			Input []struct {
				Name string `json:"name"`
			} `json:"input"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Tools) != 5 {
			t.Fatalf("tools = %d, want 5: %s", len(got.Tools), out)
		}
		byName := map[string]map[string]json.RawMessage{}
		for _, tool := range got.Tools {
			byName[rawString(tool["name"])] = tool
		}
		if string(byName[""]["type"]) != `"web_search_preview"` {
			t.Fatalf("builtin tool not preserved: %s", out)
		}
		grep := byName["grep"]
		if grep == nil || rawString(grep["description"]) != "find" || !strings.Contains(string(grep["parameters"]), `"minimum":1`) {
			t.Fatalf("grep schema lost: %s", out)
		}
		for _, name := range FingerprintTools {
			tool := byName[name]
			if tool == nil {
				t.Fatalf("missing %s: %s", name, out)
			}
			if string(tool["type"]) != `"function"` || len(tool["parameters"]) == 0 {
				t.Fatalf("responses decoy %s shape = %s", name, tool)
			}
		}
		if got.Input[0].Name != "grep" || got.Input[1].Name != "Other" {
			t.Fatalf("history = %+v", got.Input)
		}
	})

	t.Run("messages", func(t *testing.T) {
		in := []byte(`{"model":"minimax-m3-free","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"n":1}},{"type":"tool_use","id":"t2","name":"Other","input":{}},{"type":"text","text":"hi"}]}],"tools":[{"name":"Read","description":"file","input_schema":{"type":"object","properties":{"path":{"type":"string","minimum":3}}}}]}`)
		out, mapping, err := PrepareFreeTierBody(in, "opencode-messages")
		if err != nil {
			t.Fatal(err)
		}
		if mapping.SentToCaller["read"] != "Read" {
			t.Fatalf("messages map = %+v", mapping)
		}
		var got struct {
			Tools    []map[string]json.RawMessage `json:"tools"`
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"messages"`
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		byName := map[string]map[string]json.RawMessage{}
		for _, tool := range got.Tools {
			byName[rawString(tool["name"])] = tool
		}
		read := byName["read"]
		if read == nil || rawString(read["description"]) != "file" || !strings.Contains(string(read["input_schema"]), `"minimum":3`) {
			t.Fatalf("read schema lost: %s", out)
		}
		if _, hasType := read["type"]; hasType {
			t.Fatalf("messages caller tool gained a type: %s", out)
		}
		for _, name := range FingerprintTools {
			tool := byName[name]
			if tool == nil {
				t.Fatalf("missing %s: %s", name, out)
			}
			if _, hasType := tool["type"]; hasType {
				t.Fatalf("messages decoy %s has a type: %s", name, tool)
			}
			if len(tool["input_schema"]) == 0 {
				t.Fatalf("messages decoy %s missing input_schema: %s", name, tool)
			}
		}
		if len(got.ToolChoice) != 0 {
			t.Fatalf("messages choice invented: %s", got.ToolChoice)
		}
		blocks := got.Messages[0].Content
		if len(blocks) != 3 || blocks[0].Name != "read" || blocks[1].Name != "Other" || blocks[2].Type != "text" {
			t.Fatalf("messages history = %+v", blocks)
		}
	})

	t.Run("messages tool-less keeps decoys and no none", func(t *testing.T) {
		out, mapping, err := PrepareFreeTierBody([]byte(`{"model":"minimax-m3-free","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`), "opencode-messages")
		if err != nil {
			t.Fatal(err)
		}
		if len(mapping.Decoys) != 4 {
			t.Fatalf("decoys = %+v", mapping.Decoys)
		}
		var got struct {
			Tools      []any           `json:"tools"`
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Tools) != 4 || len(got.ToolChoice) != 0 {
			t.Fatalf("tools=%d choice=%s", len(got.Tools), got.ToolChoice)
		}
	})
}

func TestPrepareFreeTierChatHistoryExactNames(t *testing.T) {
	in := []byte(`{"model":"big-pickle","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"basH","arguments":"{}"}},{"id":"c3","type":"function","function":{"name":"Other","arguments":"{}"}},{"id":"c4","type":"function","function":{"name":"bash","arguments":"{}"}}]}],"tools":[{"type":"function","function":{"name":"Bash","description":"run","parameters":{"type":"object"}}}]}`)
	out, mapping, err := PrepareFreeTierBody(in, "opencode-chat")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.SentToCaller["bash"] != "Bash" {
		t.Fatalf("map = %+v", mapping)
	}
	var got struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got.Messages[0].ToolCalls))
	for _, call := range got.Messages[0].ToolCalls {
		names = append(names, call.Function.Name)
	}
	want := []string{"bash", "basH", "Other", "bash"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("history names = %v, want %v", names, want)
		}
	}
}

// TestPrepareFreeTierHistoryRangePreserved proves history patching preserves raw
// numeric schema fields rather than round-tripping them through float64.
func TestPrepareFreeTierHistoryRangePreserved(t *testing.T) {
	in := []byte(`{"model":"big-pickle","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"n\":9007199254740993}"}}]}],"tools":[{"type":"function","function":{"name":"Bash","description":"run","parameters":{"type":"object"}}}]}`)
	out, _, err := PrepareFreeTierBody(in, "opencode-chat")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `9007199254740993`) {
		t.Fatalf("large integer lost: %s", out)
	}
}

func TestFilterResponseRestoresAndRejects(t *testing.T) {
	mapping := ToolMap{SentToCaller: map[string]string{"read": "Read"}, Decoys: map[string]bool{"bash": true}}
	resp := &ir.Response{Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "ok"}, {Type: ir.BlockToolUse, ToolName: "read", ToolInput: json.RawMessage(`{"p":1}`)}}}
	got, err := FilterResponse(resp, mapping)
	if err != nil || got.Content[1].ToolName != "Read" || got.Content[0].Text != "ok" {
		t.Fatalf("restore = %+v err=%v", got, err)
	}
	bad := &ir.Response{Content: []ir.ContentBlock{{Type: ir.BlockToolUse, ToolName: "bash"}}}
	if _, err := FilterResponse(bad, mapping); err == nil {
		t.Fatal("decoy accepted")
	}
}

func scriptedDecoder(events ...ir.StreamEvent) func(io.Reader, func(ir.StreamEvent) error) error {
	return func(_ io.Reader, emit func(ir.StreamEvent) error) error {
		for _, ev := range events {
			if err := emit(ev); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestDecodeStreamRejectsUnresolvedAtFinish(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	var events []ir.StreamEvent
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"a":`},
		ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err == nil {
		t.Fatal("unresolved call accepted")
	}
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			t.Fatalf("finish forwarded: %+v", events)
		}
	}
}

func TestDecodeStreamRejectsUnresolvedAtEOF(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1"},
	), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("clean EOF with pending identity accepted")
	}
}

func TestDecodeStreamBoundsHeldArguments(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	events := make([]ir.StreamEvent, 0, maxHeldToolFragments+1)
	for i := 0; i <= maxHeldToolFragments; i++ {
		events = append(events, ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: "x"})
	}
	err := DecodeStream(scriptedDecoder(events...), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeStreamBoundsHeldBytes(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	big := strings.Repeat("x", maxHeldToolBytes+1)
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: big},
	), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("err = %v", err)
	}
}

// TestDecodeStreamChargesHeldIdentityBytes proves held ID bytes count against
// the shared hold budget, not only argument bytes.
func TestDecodeStreamChargesHeldIdentityBytes(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	id := strings.Repeat("a", maxHeldToolBytes-1)
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: id},
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: "xy"},
	), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("identity bytes not charged: %v", err)
	}
}

// TestDecodeStreamIdentityOverwriteBillsDelta proves a shorter replacement ID
// refunds budget instead of stacking, so exactly the net identity plus args fit.
func TestDecodeStreamIdentityOverwriteBillsDelta(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	wide := strings.Repeat("a", maxHeldToolBytes)
	narrow := strings.Repeat("b", maxHeldToolBytes-10)
	fit := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: wide},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: narrow},
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: strings.Repeat("c", 10)},
	), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	if fit == nil || !strings.Contains(fit.Error(), "without a name") {
		t.Fatalf("overwrite did not refund identity budget: %v", fit)
	}
	over := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: wide},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: narrow},
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: strings.Repeat("c", 11)},
	), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	if over == nil || !strings.Contains(over.Error(), "before any tool name") {
		t.Fatalf("budget not exact after overwrite: %v", over)
	}
}

// TestDecodeStreamRetainedAcceptedIdentityStaysBilled proves an accepted
// call's ID stays billed for the stream lifetime, because repeated starts
// re-forward it. Two accepted calls whose retained identity sums past the cap
// fail on the second, and the offending start is never forwarded.
func TestDecodeStreamRetainedAcceptedIdentityStaysBilled(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	big := strings.Repeat("i", maxHeldToolBytes/2+1)
	var starts []ir.StreamEvent
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "c0", ToolName: "shell"},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 1, ToolID: big, ToolName: "shell"},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 2, ToolID: big, ToolName: "shell"},
	), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			starts = append(starts, ev)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("accepted identity escaped the budget: %v", err)
	}
	if len(starts) != 2 || starts[0].Index != 0 || starts[1].Index != 1 ||
		starts[0].ToolID != "c0" || starts[1].ToolID != big {
		t.Fatalf("offending start forwarded: %+v", starts)
	}
}

// TestDecodeStreamRetainedAcceptedNamesStaysBilled proves accepted tool names
// count too, including unknown passthrough names, and the budget error never
// quotes the retained name bytes.
func TestDecodeStreamRetainedAcceptedNamesStaysBilled(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	n1 := strings.Repeat("n", maxHeldToolBytes/2+1)
	n2 := strings.Repeat("m", maxHeldToolBytes/2+1)
	var starts []ir.StreamEvent
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "c0", ToolName: n1},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 1, ToolID: "c1", ToolName: n2},
	), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			starts = append(starts, ev)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("accepted names escaped the budget: %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("n", 16)) || strings.Contains(err.Error(), strings.Repeat("m", 16)) {
		t.Fatalf("error quotes retained name bytes: %q", err.Error())
	}
	if len(starts) != 1 || starts[0].Index != 0 {
		t.Fatalf("offending start forwarded: %+v", starts)
	}
}

// TestDecodeStreamChargesRestoredNameAtRetainedSize proves the restored caller
// name is billed at its retained size, which can exceed the wire name, and
// that a fitting restored name leaves exactly the remaining budget.
func TestDecodeStreamChargesRestoredNameAtRetainedSize(t *testing.T) {
	run := func(caller string, args int) error {
		mapping := ToolMap{SentToCaller: map[string]string{"t": caller}, Decoys: map[string]bool{"bash": true}}
		var starts []ir.StreamEvent
		err := DecodeStream(scriptedDecoder(
			ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "c0", ToolName: "t"},
			ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: strings.Repeat("c", args)},
		), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
			if ev.Kind == ir.EventToolCallStart {
				starts = append(starts, ev)
			}
			return nil
		})
		if err == nil && len(starts) != 1 {
			t.Fatalf("accepted start missing: %+v", starts)
		}
		return err
	}
	huge := strings.Repeat("R", maxHeldToolBytes+1)
	err := run(huge, 0)
	if err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("restored name not charged at retained size: %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("R", 16)) {
		t.Fatalf("error quotes restored name: %q", err.Error())
	}
	// Retained identity is 2 + (cap-10); exactly 8 argument bytes still fit.
	if err := run(strings.Repeat("R", maxHeldToolBytes-10), 8); err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("restored name overcharged: %v", err)
	}
	if err := run(strings.Repeat("R", maxHeldToolBytes-10), 9); err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("restored name undercharged: %v", err)
	}
}

// TestDecodeStreamReleasedArgumentBudget proves argument bytes held before a
// name are unbilled on acceptance, so a later call can hold them again.
func TestDecodeStreamReleasedArgumentBudget(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	big := strings.Repeat("a", maxHeldToolBytes/2+1)
	var args []string
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: big},
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ToolName: "shell"},
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: big},
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 1, ToolName: "shell"},
		ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallDelta {
			args = append(args, ev.ArgsFrag)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("released args still billed: %v", err)
	}
	if len(args) != 2 || args[0] != big || args[1] != big {
		t.Fatalf("args = %d fragments", len(args))
	}
}

// TestDecodeStreamAcceptedIdentityOverwriteRefundsDelta proves replacing an
// accepted ID with a shorter one refunds exactly the size delta, the shared
// total never goes negative, and pending arguments fit only up to the cap
// minus the retained identity.
func TestDecodeStreamAcceptedIdentityOverwriteRefundsDelta(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	wide := strings.Repeat("a", maxHeldToolBytes/2)
	run := func(args string) error {
		return DecodeStream(scriptedDecoder(
			ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: wide, ToolName: "shell"},
			ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "b"},
			ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: args},
		), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	}
	// Retained identity after the overwrite is 1 + 5; exactly cap-6 argument
	// bytes fit, and one more fails the budget instead of going negative.
	if err := run(strings.Repeat("c", maxHeldToolBytes-6)); err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("overwrite refund wrong: %v", err)
	}
	if err := run(strings.Repeat("c", maxHeldToolBytes-5)); err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("negative charge inflated the budget: %v", err)
	}
}

// TestDecodeStreamAcceptedNameOverwriteRefundsDelta proves replacing an
// accepted call's restored name refunds exactly the retained-name delta.
func TestDecodeStreamAcceptedNameOverwriteRefundsDelta(t *testing.T) {
	wide := strings.Repeat("w", maxHeldToolBytes/2+10)
	narrow := strings.Repeat("n", 10)
	mapping := ToolMap{SentToCaller: map[string]string{"ww": wide, "nn": narrow}, Decoys: map[string]bool{"bash": true}}
	run := func(args string) error {
		return DecodeStream(scriptedDecoder(
			ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "c0", ToolName: "ww"},
			ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolName: "nn"},
			ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: args},
		), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	}
	// Retained identity after the name overwrite is 2 + 10.
	if err := run(strings.Repeat("c", maxHeldToolBytes-12)); err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("name overwrite refund wrong: %v", err)
	}
	if err := run(strings.Repeat("c", maxHeldToolBytes-11)); err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("stale name bytes still billed: %v", err)
	}
}

// TestDecodeStreamHugeRepeatedIdentityDoesNotBypassBudget proves retained
// identity accumulates across many accepted calls instead of resetting per
// call, for both IDs and names.
func TestDecodeStreamHugeRepeatedIdentityDoesNotBypassBudget(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	big := strings.Repeat("x", maxHeldToolBytes/4+1)
	for _, mode := range []string{"id", "name"} {
		t.Run(mode, func(t *testing.T) {
			events := make([]ir.StreamEvent, 0, 6)
			for i := 0; i < 5; i++ {
				ev := ir.StreamEvent{Kind: ir.EventToolCallStart, Index: i, ToolName: "shell"}
				if mode == "id" {
					ev.ToolID = big
				} else {
					ev.ToolName = big
				}
				events = append(events, ev)
			}
			var starts []ir.StreamEvent
			err := DecodeStream(scriptedDecoder(events...), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
				if ev.Kind == ir.EventToolCallStart {
					starts = append(starts, ev)
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "before any tool name") {
				t.Fatalf("repeated huge %ss bypassed the budget: %v", mode, err)
			}
			// Three quarter-cap identities fit; the fourth does not.
			if len(starts) != 3 {
				t.Fatalf("starts = %d, want 3: %+v", len(starts), starts)
			}
		})
	}
}

// TestDecodeStreamArgBudgetReuseRespectsRetainedIdentity proves released
// argument bytes are reusable after acceptance and the reusable amount is
// exactly the cap minus the identity accepted calls retain.
func TestDecodeStreamArgBudgetReuseRespectsRetainedIdentity(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	run := func(args string) error {
		return DecodeStream(scriptedDecoder(
			ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: strings.Repeat("a", maxHeldToolBytes/2+1)},
			ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ToolName: "shell"},
			ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 1, ToolID: "c1", ToolName: "shell"},
			ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 2, ArgsFrag: args},
		), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	}
	// Retained identity is 5 + (2+5) = 12; exactly cap-12 argument bytes fit
	// again, and one more fails the budget.
	if err := run(strings.Repeat("c", maxHeldToolBytes-12)); err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("arg budget not reusable minus retained identity: %v", err)
	}
	if err := run(strings.Repeat("c", maxHeldToolBytes-11)); err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("retained identity not billed against arg budget: %v", err)
	}
}

// TestDecodeStreamKnownCallLateIDUpdatesRetained proves a late ID-only start
// on an accepted call replaces the retained fallback ID, is billed, and a
// repeated name-only start re-forwards the newest ID instead of a stale one.
func TestDecodeStreamKnownCallLateIDUpdatesRetained(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	late := strings.Repeat("L", 100)
	var starts []ir.StreamEvent
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "c0", ToolName: "shell"},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: late},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolName: "shell"},
	), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			starts = append(starts, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 3 {
		t.Fatalf("starts = %+v", starts)
	}
	if last := starts[len(starts)-1]; last.ToolID != late || last.ToolName != "shell" {
		t.Fatalf("repeated start used stale identity: %+v", last)
	}
	// The late ID is retained and billed: exactly cap-(100+5) argument bytes
	// fit behind it.
	run := func(args int) error {
		return DecodeStream(scriptedDecoder(
			ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "c0", ToolName: "shell"},
			ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: late},
			ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: strings.Repeat("c", args)},
		), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	}
	if err := run(maxHeldToolBytes - 105); err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("late ID not billed: %v", err)
	}
	if err := run(maxHeldToolBytes - 104); err == nil || !strings.Contains(err.Error(), "before any tool name") {
		t.Fatalf("late ID escaped the budget: %v", err)
	}
}

// TestToolHoldIdentityChargeMatchesRetainedBytes drives the filter directly
// and asserts after every step that each hold's identity charge equals the
// size of the id and name it retains, argument charges equal held fragment
// bytes, and the shared total is their sum and never negative.
func TestToolHoldIdentityChargeMatchesRetainedBytes(t *testing.T) {
	f := newToolFilter(ToolMap{Decoys: map[string]bool{"bash": true}})
	emit := func(ir.StreamEvent) error { return nil }
	check := func(step string) {
		t.Helper()
		total := 0
		for index, h := range f.holds {
			if h.identCharged != len(h.id)+len(h.name) {
				t.Fatalf("%s: hold %d identity charge %d != retained id %d + name %d", step, index, h.identCharged, len(h.id), len(h.name))
			}
			argBytes := 0
			for _, frag := range h.frags {
				argBytes += len(frag)
			}
			if h.argCharged != argBytes {
				t.Fatalf("%s: hold %d arg charge %d != held %d", step, index, h.argCharged, argBytes)
			}
			total += h.identCharged + h.argCharged
		}
		if f.heldBytes != total || f.heldBytes < 0 {
			t.Fatalf("%s: heldBytes %d, want sum %d and non-negative", step, f.heldBytes, total)
		}
	}
	steps := []ir.StreamEvent{
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: strings.Repeat("a", 300)},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"p":`},
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: strings.Repeat("b", 40)},
		{Kind: ir.EventToolCallStart, Index: 0, ToolName: "shell"},
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: "c1", ToolName: "shell"},
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: strings.Repeat("c", 900)},
		{Kind: ir.EventToolCallStart, Index: 2, ToolID: "c2", ToolName: "shell"},
		{Kind: ir.EventToolCallStart, Index: 2, ToolName: "shell"},
		{Kind: ir.EventToolCallDelta, Index: 3, ArgsFrag: "held"},
	}
	for i, ev := range steps {
		if err := f.handle(ev, emit); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		check(fmt.Sprintf("step %d kind %v", i, ev.Kind))
	}
}

func TestDecodeStreamBareStartRejectsAtFinish(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	var events []ir.StreamEvent
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 3},
		ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("bare start accepted: %v", err)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			t.Fatalf("finish forwarded: %+v", events)
		}
	}
}

func TestDecodeStreamBareStartRejectsAtEOF(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0},
	), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("bare start accepted at EOF: %v", err)
	}
}

// TestDecodeStreamBareStartThenIdentitySucceeds proves a bare start does not
// poison an index that later receives a real ID and name.
func TestDecodeStreamBareStartThenIdentitySucceeds(t *testing.T) {
	mapping := ToolMap{SentToCaller: map[string]string{"read": "Read"}, Decoys: map[string]bool{"bash": true}}
	var events []ir.StreamEvent
	err := DecodeStream(scriptedDecoder(
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0},
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"p":`},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1"},
		ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolName: "read"},
		ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `1}`},
		ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	), mapping)(strings.NewReader(""), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var starts []ir.StreamEvent
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			starts = append(starts, ev)
		}
	}
	if len(starts) != 1 || starts[0].ToolID != "call_1" || starts[0].ToolName != "Read" {
		t.Fatalf("starts = %+v", starts)
	}
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallDelta && ev.Index == 0 {
			b.WriteString(ev.ArgsFrag)
		}
	}
	if got := b.String(); got != `{"p":1}` {
		t.Fatalf("args = %q", got)
	}
}

func TestDecodeStreamBareStartsHitIndexCap(t *testing.T) {
	mapping := ToolMap{Decoys: map[string]bool{"bash": true}}
	events := make([]ir.StreamEvent, 0, maxHeldToolIndices+1)
	for i := 0; i <= maxHeldToolIndices; i++ {
		events = append(events, ir.StreamEvent{Kind: ir.EventToolCallStart, Index: i})
	}
	err := DecodeStream(scriptedDecoder(events...), mapping)(strings.NewReader(""), func(ir.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "too many tool calls") {
		t.Fatalf("bare start flood not bounded: %v", err)
	}
}
