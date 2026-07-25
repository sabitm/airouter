package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func decodeReq(t *testing.T, body []byte) cwRequest {
	t.Helper()
	var got cwRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	return got
}

func TestEncodeRequestSystemFoldAndHistorySplit(t *testing.T) {
	req := &ir.Request{
		Model:  "claude-sonnet-4.5",
		System: "be brief",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hello"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi there"}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "again"}}},
		},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)

	// System folds into the first user turn (in history since it is not last).
	if len(got.ConversationState.History) != 2 {
		t.Fatalf("history len = %d\n%s", len(got.ConversationState.History), body)
	}
	first := got.ConversationState.History[0].UserInputMessage
	if first == nil || first.Content != "be brief\n\nhello" {
		t.Errorf("first user content = %q", contentOf(first))
	}
	if a := got.ConversationState.History[1].AssistantResponseMessage; a == nil || a.Content != "hi there" {
		t.Errorf("assistant history = %+v", got.ConversationState.History[1])
	}
	// Last user turn is current, carries modelId + origin.
	cur := got.ConversationState.CurrentMessage.UserInputMessage
	if cur == nil || cur.Content != "again" {
		t.Fatalf("current = %+v", cur)
	}
	if cur.ModelID != "claude-sonnet-4.5" || cur.Origin != "AI_EDITOR" {
		t.Errorf("current modelId/origin = %q/%q", cur.ModelID, cur.Origin)
	}
	if got.InferenceConfig.MaxTokens != DefaultMaxTokens {
		t.Errorf("maxTokens = %d, want %d", got.InferenceConfig.MaxTokens, DefaultMaxTokens)
	}
}

func TestEncodeRequestToolsOnCurrentMessage(t *testing.T) {
	req := &ir.Request{
		Model: "claude-sonnet-4.5",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "weather?"}}},
		},
		Tools: []ir.Tool{{Name: "get_weather", Description: "w", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)
	cur := got.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.Tools) != 1 {
		t.Fatalf("tools not attached to current message: %s", body)
	}
	if cur.UserInputMessageContext.Tools[0].ToolSpecification.Name != "get_weather" {
		t.Errorf("tool name = %q", cur.UserInputMessageContext.Tools[0].ToolSpecification.Name)
	}
}

func TestEncodeRequestToolResultAndToolUse(t *testing.T) {
	req := &ir.Request{
		Model: "claude-sonnet-4.5",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "weather?"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockToolUse, ToolID: "call_1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"NYC"}`)},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "call_1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "sunny"}}},
				{Type: ir.BlockText, Text: "thanks"},
			}},
		},
		// Tools present so the guards do not flatten the structured tool blocks.
		Tools: []ir.Tool{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)

	// Assistant tool_use lands in history.
	var sawToolUse bool
	for _, h := range got.ConversationState.History {
		if h.AssistantResponseMessage != nil {
			for _, tu := range h.AssistantResponseMessage.ToolUses {
				if tu.ToolUseID == "call_1" && tu.Name == "get_weather" {
					sawToolUse = true
				}
			}
		}
	}
	if !sawToolUse {
		t.Errorf("tool_use not in history: %s", body)
	}
	// Tool result lands on the current (last user) message context.
	cur := got.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("toolResults missing: %s", body)
	}
	tr := cur.UserInputMessageContext.ToolResults[0]
	if tr.ToolUseID != "call_1" || tr.Status != "success" || len(tr.Content) != 1 || tr.Content[0].Text != "sunny" {
		t.Errorf("toolResult = %+v", tr)
	}
}

func TestInjectProfileArn(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	// Empty arn leaves the field absent.
	if decodeReq(t, InjectProfileArn(body, "")).ProfileArn != "" {
		t.Errorf("empty arn should stay absent")
	}
	arn := "arn:aws:codewhisperer:us-east-1:123:profile/ABC"
	if got := decodeReq(t, InjectProfileArn(body, arn)).ProfileArn; got != arn {
		t.Errorf("profileArn = %q", got)
	}
}

func TestFlattenToolInteractionsWhenNoTools(t *testing.T) {
	// A follow-up with tool blocks but no tools declaration must be flattened to
	// text to avoid Kiro's HTTP 400.
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockToolUse, ToolID: "call_1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"NYC"}`)},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "call_1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "sunny"}}},
				{Type: ir.BlockText, Text: "and tomorrow?"},
			}},
		},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)
	// No structured tool state should survive anywhere.
	for _, h := range got.ConversationState.History {
		if h.AssistantResponseMessage != nil && len(h.AssistantResponseMessage.ToolUses) != 0 {
			t.Errorf("tool_use survived flattening: %s", body)
		}
		if h.UserInputMessage != nil && h.UserInputMessage.UserInputMessageContext != nil &&
			len(h.UserInputMessage.UserInputMessageContext.ToolResults) != 0 {
			t.Errorf("toolResult survived flattening: %s", body)
		}
	}
	cur := got.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Errorf("current toolResults survived flattening: %s", body)
	}
	// The result content should be salvaged into text.
	if !strings.Contains(cur.Content, "Tool result") || !strings.Contains(cur.Content, "sunny") {
		t.Errorf("flattened result text missing: %q", cur.Content)
	}
}

func TestReconcileOrphanedToolResults(t *testing.T) {
	// A tool_result whose tool_use was dropped is salvaged to text even when tools
	// are declared (structured orphan would 400).
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "gone", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "orphan"}}},
				{Type: ir.BlockText, Text: "continue"},
			}},
		},
		Tools: []ir.Tool{{Name: "x", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	cur := decodeReq(t, body).ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Errorf("orphan toolResult should have been salvaged: %s", body)
	}
	if !strings.Contains(cur.Content, "orphan") {
		t.Errorf("salvaged orphan text missing: %q", cur.Content)
	}
}

func contentOf(m *cwUserInputMessage) string {
	if m == nil {
		return ""
	}
	return m.Content
}

func TestImageFormat(t *testing.T) {
	cases := []struct {
		mediaType string
		want      string
	}{
		{"image/png", "png"},
		{"image/jpeg", "jpeg"},
		{"image/svg+xml", "svg+xml"},
		{"image/gif", "gif"},
		{"application/pdf", "pdf"},
		{"noprefix", "png"},
		{"", "png"},
		{"image/", "png"},
		{"/", "png"},
	}
	for _, tc := range cases {
		t.Run(tc.mediaType, func(t *testing.T) {
			if got := imageFormat(tc.mediaType); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToolResultStatus(t *testing.T) {
	if got := toolResultStatus(true); got != "error" {
		t.Errorf("error case: got %q, want error", got)
	}
	if got := toolResultStatus(false); got != "success" {
		t.Errorf("success case: got %q, want success", got)
	}
}

func TestCompactJSON(t *testing.T) {
	t.Run("valid json with whitespace compacted", func(t *testing.T) {
		got := compactJSON(json.RawMessage(`{ "a" : 1 , "b" : [ 2 , 3 ] }`))
		if string(got) != `{"a":1,"b":[2,3]}` {
			t.Errorf("got %s, want compacted", got)
		}
	})

	t.Run("empty input returns empty object", func(t *testing.T) {
		got := compactJSON(json.RawMessage(``))
		if string(got) != `{}` {
			t.Errorf("got %s, want {}", got)
		}
	})

	t.Run("invalid json returns empty object", func(t *testing.T) {
		got := compactJSON(json.RawMessage(`{not valid`))
		if string(got) != `{}` {
			t.Errorf("got %s, want {}", got)
		}
	})

	t.Run("already compact unchanged", func(t *testing.T) {
		got := compactJSON(json.RawMessage(`{"a":1}`))
		if string(got) != `{"a":1}` {
			t.Errorf("got %s, want {\"a\":1}", got)
		}
	})

	t.Run("nested object compacted", func(t *testing.T) {
		got := compactJSON(json.RawMessage(`{ "outer" : { "inner" : "v" } }`))
		if string(got) != `{"outer":{"inner":"v"}}` {
			t.Errorf("got %s, want compacted nested", got)
		}
	})
}

func TestJoinContent(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want string
	}{
		{"both non-empty", "hello", "world", "hello\n\nworld"},
		{"a empty returns b", "", "world", "world"},
		{"b empty returns a", "hello", "", "hello"},
		{"both empty returns empty", "", "", ""},
		{"single char each", "a", "b", "a\n\nb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinContent(tc.a, tc.b); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildUser(t *testing.T) {
	t.Run("text only joined", func(t *testing.T) {
		content, images, toolResults := buildUser([]ir.ContentBlock{
			{Type: ir.BlockText, Text: "hello"},
			{Type: ir.BlockText, Text: "world"},
		})
		if content != "hello\nworld" {
			t.Errorf("content = %q, want hello\\nworld", content)
		}
		if len(images) != 0 || len(toolResults) != 0 {
			t.Errorf("images=%d toolResults=%d, want 0/0", len(images), len(toolResults))
		}
	})

	t.Run("empty text block skipped", func(t *testing.T) {
		content, _, _ := buildUser([]ir.ContentBlock{
			{Type: ir.BlockText, Text: ""},
			{Type: ir.BlockText, Text: "keep"},
		})
		if content != "keep" {
			t.Errorf("content = %q, want keep", content)
		}
	})

	t.Run("single text block", func(t *testing.T) {
		content, _, _ := buildUser([]ir.ContentBlock{{Type: ir.BlockText, Text: "solo"}})
		if content != "solo" {
			t.Errorf("content = %q", content)
		}
	})

	t.Run("inline base64 image emits cwImage with format", func(t *testing.T) {
		content, images, _ := buildUser([]ir.ContentBlock{
			{Type: ir.BlockImage, Image: &ir.Image{Data: "BASE64", MediaType: "image/png"}},
		})
		if content != "" {
			t.Errorf("content = %q, want empty", content)
		}
		if len(images) != 1 {
			t.Fatalf("images len = %d, want 1", len(images))
		}
		if images[0].Format != "png" {
			t.Errorf("format = %q, want png", images[0].Format)
		}
		if images[0].Source.Bytes != "BASE64" {
			t.Errorf("bytes = %q, want BASE64", images[0].Source.Bytes)
		}
	})

	t.Run("remote url image degrades to text marker", func(t *testing.T) {
		content, images, _ := buildUser([]ir.ContentBlock{
			{Type: ir.BlockImage, Image: &ir.Image{URL: "https://e.com/a.png"}},
		})
		if content != "[Image: https://e.com/a.png]" {
			t.Errorf("content = %q, want image marker", content)
		}
		if len(images) != 0 {
			t.Errorf("images len = %d, want 0 (url not inlined)", len(images))
		}
	})

	t.Run("nil image on BlockImage skipped", func(t *testing.T) {
		content, images, _ := buildUser([]ir.ContentBlock{
			{Type: ir.BlockImage, Image: nil},
			{Type: ir.BlockText, Text: "after"},
		})
		if content != "after" {
			t.Errorf("content = %q, want after", content)
		}
		if len(images) != 0 {
			t.Errorf("images len = %d, want 0", len(images))
		}
	})

	t.Run("tool result success status", func(t *testing.T) {
		_, _, toolResults := buildUser([]ir.ContentBlock{
			{Type: ir.BlockToolResult, ToolUseID: "tu1", IsError: false,
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "ok"}}},
		})
		if len(toolResults) != 1 {
			t.Fatalf("toolResults len = %d, want 1", len(toolResults))
		}
		tr := toolResults[0]
		if tr.ToolUseID != "tu1" {
			t.Errorf("ToolUseID = %q", tr.ToolUseID)
		}
		if tr.Status != "success" {
			t.Errorf("Status = %q, want success", tr.Status)
		}
		if len(tr.Content) != 1 || tr.Content[0].Text != "ok" {
			t.Errorf("Content = %+v, want [ok]", tr.Content)
		}
	})

	t.Run("tool result error status", func(t *testing.T) {
		_, _, toolResults := buildUser([]ir.ContentBlock{
			{Type: ir.BlockToolResult, ToolUseID: "tu2", IsError: true,
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "boom"}}},
		})
		if len(toolResults) != 1 {
			t.Fatalf("toolResults len = %d, want 1", len(toolResults))
		}
		if toolResults[0].Status != "error" {
			t.Errorf("Status = %q, want error", toolResults[0].Status)
		}
		if toolResults[0].Content[0].Text != "boom" {
			t.Errorf("Content = %q, want boom", toolResults[0].Content[0].Text)
		}
	})

	t.Run("mixed blocks populate all outputs", func(t *testing.T) {
		content, images, toolResults := buildUser([]ir.ContentBlock{
			{Type: ir.BlockText, Text: "before"},
			{Type: ir.BlockImage, Image: &ir.Image{Data: "B64", MediaType: "image/jpeg"}},
			{Type: ir.BlockToolResult, ToolUseID: "tu3", IsError: false,
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "r"}}},
			{Type: ir.BlockText, Text: "after"},
		})
		if content != "before\nafter" {
			t.Errorf("content = %q, want before\\nafter", content)
		}
		if len(images) != 1 || images[0].Format != "jpeg" {
			t.Errorf("images = %+v", images)
		}
		if len(toolResults) != 1 || toolResults[0].ToolUseID != "tu3" {
			t.Errorf("toolResults = %+v", toolResults)
		}
	})
}

func TestBuildTurns(t *testing.T) {
	t.Run("single user message with system prepended", func(t *testing.T) {
		msgs := []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}}
		turns := buildTurns(msgs, "sys")
		if len(turns) != 1 || turns[0].user == nil {
			t.Fatalf("turns = %+v, want single user turn", turns)
		}
		if turns[0].user.Content != "sys\n\nhi" {
			t.Errorf("content = %q, want sys joined with hi", turns[0].user.Content)
		}
	})

	t.Run("consecutive assistant messages merge content and tool uses", func(t *testing.T) {
		msgs := []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "first"},
				{Type: ir.BlockToolUse, ToolID: "t1", ToolName: "f"},
			}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "second"},
				{Type: ir.BlockToolUse, ToolID: "t2", ToolName: "g"},
			}},
		}
		turns := buildTurns(msgs, "")
		if len(turns) != 1 || turns[0].assistant == nil {
			t.Fatalf("turns = %+v, want single merged assistant turn", turns)
		}
		a := turns[0].assistant
		if a.Content != "first\n\nsecond" {
			t.Errorf("content = %q, want first joined with second", a.Content)
		}
		if len(a.ToolUses) != 2 || a.ToolUses[0].ToolUseID != "t1" || a.ToolUses[1].ToolUseID != "t2" {
			t.Errorf("toolUses = %+v, want [t1 t2]", a.ToolUses)
		}
	})

	t.Run("consecutive user messages merge content and images", func(t *testing.T) {
		msgs := []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "a"},
				{Type: ir.BlockImage, Image: &ir.Image{Data: "B1", MediaType: "image/png"}},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "b"},
				{Type: ir.BlockImage, Image: &ir.Image{Data: "B2", MediaType: "image/jpeg"}},
			}},
		}
		turns := buildTurns(msgs, "")
		if len(turns) != 1 || turns[0].user == nil {
			t.Fatalf("turns = %+v, want single merged user turn", turns)
		}
		u := turns[0].user
		if u.Content != "a\n\nb" {
			t.Errorf("content = %q, want a joined with b", u.Content)
		}
		if len(u.Images) != 2 || u.Images[0].Source.Bytes != "B1" || u.Images[1].Source.Bytes != "B2" {
			t.Errorf("images = %+v, want [B1 B2]", u.Images)
		}
	})

	t.Run("consecutive user messages with tool results merge into existing context", func(t *testing.T) {
		msgs := []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "tu1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "r1"}}},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "tu2", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "r2"}}},
			}},
		}
		turns := buildTurns(msgs, "")
		if len(turns) != 1 || turns[0].user == nil {
			t.Fatalf("turns = %+v, want single merged user turn", turns)
		}
		u := turns[0].user
		if u.UserInputMessageContext == nil || len(u.UserInputMessageContext.ToolResults) != 2 {
			t.Fatalf("toolResults = %+v, want 2 merged", u.UserInputMessageContext)
		}
		if u.UserInputMessageContext.ToolResults[0].ToolUseID != "tu1" || u.UserInputMessageContext.ToolResults[1].ToolUseID != "tu2" {
			t.Errorf("toolResults order = %+v", u.UserInputMessageContext.ToolResults)
		}
	})

	t.Run("assistant first ordering no system attached", func(t *testing.T) {
		msgs := []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "reply"}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "q"}}},
		}
		turns := buildTurns(msgs, "sys")
		if len(turns) != 2 {
			t.Fatalf("turns = %d, want 2", len(turns))
		}
		if turns[0].assistant == nil || turns[0].assistant.Content != "reply" {
			t.Errorf("first turn = %+v, want assistant reply", turns[0])
		}
		// System attaches to the first user turn, which is the second turn here.
		if turns[1].user == nil || turns[1].user.Content != "sys\n\nq" {
			t.Errorf("second turn = %+v, want user with system prepended", turns[1])
		}
	})

	t.Run("system with no user messages emits lone user turn", func(t *testing.T) {
		turns := buildTurns(nil, "sys")
		if len(turns) != 1 || turns[0].user == nil {
			t.Fatalf("turns = %+v, want single lone user turn", turns)
		}
		if turns[0].user.Content != "sys" {
			t.Errorf("content = %q, want sys", turns[0].user.Content)
		}
	})

	t.Run("system with only assistant messages emits lone user turn", func(t *testing.T) {
		msgs := []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "reply"}}},
		}
		turns := buildTurns(msgs, "sys")
		if len(turns) != 2 {
			t.Fatalf("turns = %d, want 2 (assistant + lone system user)", len(turns))
		}
		if turns[1].user == nil || turns[1].user.Content != "sys" {
			t.Errorf("second turn = %+v, want lone system user turn", turns[1])
		}
	})

	t.Run("single assistant message no merge", func(t *testing.T) {
		msgs := []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "solo"},
				{Type: ir.BlockToolUse, ToolID: "t1", ToolName: "f"},
			}},
		}
		turns := buildTurns(msgs, "")
		if len(turns) != 1 || turns[0].assistant == nil {
			t.Fatalf("turns = %+v, want single assistant turn", turns)
		}
		if turns[0].assistant.Content != "solo" || len(turns[0].assistant.ToolUses) != 1 {
			t.Errorf("assistant = %+v", turns[0].assistant)
		}
	})
}

func TestLastUserTurn(t *testing.T) {
	t.Run("single user turn returns 0", func(t *testing.T) {
		turns := []turn{{user: &cwUserInputMessage{}}}
		if got := lastUserTurn(turns); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})

	t.Run("user then assistant skips assistant", func(t *testing.T) {
		turns := []turn{
			{user: &cwUserInputMessage{}},
			{assistant: &cwAssistantResponseMessage{}},
		}
		if got := lastUserTurn(turns); got != 0 {
			t.Errorf("got %d, want 0 (skip trailing assistant)", got)
		}
	})

	t.Run("assistant then user returns 1", func(t *testing.T) {
		turns := []turn{
			{assistant: &cwAssistantResponseMessage{}},
			{user: &cwUserInputMessage{}},
		}
		if got := lastUserTurn(turns); got != 1 {
			t.Errorf("got %d, want 1", got)
		}
	})

	t.Run("no user turns returns -1", func(t *testing.T) {
		turns := []turn{
			{assistant: &cwAssistantResponseMessage{}},
			{assistant: &cwAssistantResponseMessage{}},
		}
		if got := lastUserTurn(turns); got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})

	t.Run("empty slice returns -1", func(t *testing.T) {
		if got := lastUserTurn(nil); got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})

	t.Run("multiple user turns returns last", func(t *testing.T) {
		turns := []turn{
			{user: &cwUserInputMessage{Content: "first"}},
			{assistant: &cwAssistantResponseMessage{}},
			{user: &cwUserInputMessage{Content: "last"}},
		}
		if got := lastUserTurn(turns); got != 2 {
			t.Errorf("got %d, want 2 (last user)", got)
		}
	})
}

func TestInjectProfileArnErrorPaths(t *testing.T) {
	t.Run("empty arn returns body unchanged byte-for-byte", func(t *testing.T) {
		body := []byte(`{"content":"hi"}`)
		got := InjectProfileArn(body, "")
		if string(got) != string(body) {
			t.Errorf("got %s, want body unchanged", got)
		}
	})

	t.Run("invalid json body returns unchanged", func(t *testing.T) {
		body := []byte(`not valid json`)
		got := InjectProfileArn(body, "arn:aws:codewhisperer:us-east-1:123:profile/ABC")
		if string(got) != string(body) {
			t.Errorf("got %s, want body unchanged for invalid JSON", got)
		}
	})
}
