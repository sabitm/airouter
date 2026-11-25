package grokbuild

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/crypto"
	"airouter/internal/proxy/ir"
)

func testMAC(t *testing.T) (func([]byte) ([]byte, bool), func([]byte, []byte) bool) {
	t.Helper()
	c, err := crypto.New("bridge-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnableGrokBuildChatReplay("bridge-test-secret"); err != nil {
		t.Fatal(err)
	}
	return c.GrokBuildChatMAC, c.VerifyGrokBuildChatMAC
}

func sampleBind() ChatBind {
	return ChatBind{
		Scope: "caller-1", ComboID: 7, ProviderID: 3, Generation: "conn-a",
		Principal: "user-a", Model: "grok-4.7", RouteModel: "grok-4.7(high)",
	}
}

func sampleContent() []ir.ContentBlock {
	enc := "cipher-exact"
	empty := ""
	return []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "think-a", ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_a", Status: "completed", EncryptedContent: &enc,
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "think-a"}, {Type: "summary_text", Text: ""}},
		}},
		{Type: ir.BlockText, Text: "Hello"},
		{Type: ir.BlockToolUse, ToolID: "call_a", ToolName: "read", ToolInput: []byte(`{"path":"a"}`), ResponsesItemID: "fc_a"},
		{Type: ir.BlockToolUse, ToolID: "call_b", ToolName: "list", ToolInput: []byte(`{"path":"b"}`), ResponsesItemID: "fc_b"},
		{Type: ir.BlockReasoning, Text: "", ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_b", EncryptedContent: &empty, Summary: []ir.ReasoningSummaryPart{},
		}},
		{Type: ir.BlockText, Text: "World"},
	}
}

func TestStampParsePreservesExactState(t *testing.T) {
	mac, verify := testMAC(t)
	details, err := StampChatDetails(sampleContent(), sampleBind(), mac)
	if err != nil || len(details) == 0 {
		t.Fatalf("stamp: %v %d", err, len(details))
	}
	body := assistantBody(mustMarshal(t, details))
	decision, err := ParseChatBridge([]byte(body), "caller-1", 7, verify)
	if err != nil || !decision.Pin {
		t.Fatalf("parse: %v pin=%v", err, decision.Pin)
	}
	if decision.Turn.Bind != sampleBind() {
		t.Fatalf("bind = %+v", decision.Turn.Bind)
	}
	got, err := ApplyChatTurn([]ir.ContentBlock{
		{Type: ir.BlockText, Text: "HelloWorld"},
		{Type: ir.BlockToolUse, ToolID: "call_b", ToolName: "list", ToolInput: []byte(`{"path": "b"}`)},
		{Type: ir.BlockToolUse, ToolID: "call_a", ToolName: "read", ToolInput: []byte(`{"path":"a"}`)},
	}, decision.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("blocks = %+v", got)
	}
	if got[0].ResponsesReasoning == nil || got[0].ResponsesReasoning.EncryptedContent == nil || *got[0].ResponsesReasoning.EncryptedContent != "cipher-exact" {
		t.Fatalf("first reasoning = %+v", got[0].ResponsesReasoning)
	}
	if len(got[0].ResponsesReasoning.Summary) != 2 || got[0].ResponsesReasoning.Summary[1].Text != "" || got[0].ResponsesReasoning.Status != "completed" {
		t.Fatalf("summary = %+v", got[0].ResponsesReasoning)
	}
	if got[1].Text != "Hello" || got[2].ToolID != "call_a" || got[2].ResponsesItemID != "fc_a" || string(got[2].ToolInput) != `{"path":"a"}` {
		t.Fatalf("middle = %+v %+v", got[1], got[2])
	}
	if got[3].ResponsesItemID != "fc_b" || string(got[3].ToolInput) != `{"path": "b"}` {
		t.Fatalf("tool restore = %+v", got[3])
	}
	if got[4].ResponsesReasoning == nil || got[4].ResponsesReasoning.EncryptedContent == nil || *got[4].ResponsesReasoning.EncryptedContent != "" {
		t.Fatalf("empty cipher = %+v", got[4].ResponsesReasoning)
	}
	if got[5].Text != "World" {
		t.Fatalf("tail = %+v", got[5])
	}
	raw := string(mustMarshal(t, details))
	if strings.Count(raw, "cipher-exact") != 1 {
		t.Fatalf("ciphertext copied %d times", strings.Count(raw, "cipher-exact"))
	}
}

func TestParseRejectsTamperAndWrongScope(t *testing.T) {
	mac, verify := testMAC(t)
	details, err := StampChatDetails(sampleContent(), sampleBind(), mac)
	if err != nil {
		t.Fatal(err)
	}
	body := assistantBody(mustMarshal(t, details))
	flipped := bytes.Replace([]byte(body), []byte("cipher-exact"), []byte("cipher-other"), 1)
	if _, err := ParseChatBridge(flipped, "caller-1", 7, verify); err == nil {
		t.Fatal("tamper accepted")
	}
	if _, err := ParseChatBridge([]byte(body), "other-caller", 7, verify); err == nil {
		t.Fatal("cross-caller accepted")
	}
	if _, err := ParseChatBridge([]byte(body), "caller-1", 8, verify); err == nil {
		t.Fatal("cross-combo accepted")
	}
	mixed := bytes.Replace([]byte(body), []byte(`"reasoning_details":[`), []byte(`"reasoning_details":[{"type":"reasoning.encrypted","format":"xai-responses-v1","data":"foreign"},`), 1)
	if _, err := ParseChatBridge(mixed, "caller-1", 7, verify); err == nil {
		t.Fatal("mixed marker accepted")
	}
	unmatched := bytes.Replace([]byte(body), []byte(`"id":"call_a"`), []byte(`"id":"call_z"`), 1)
	if _, err := ParseChatBridge(unmatched, "caller-1", 7, verify); err == nil {
		t.Fatal("unmatched call accepted")
	}
}

func TestPortableStripsCipherAndKeepsUnknownFields(t *testing.T) {
	mac, verify := testMAC(t)
	details, err := StampChatDetails(sampleContent(), sampleBind(), mac)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"combo","stream_options":{"include_usage":true},"reasoning_effort":"high","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"HelloWorld","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"list","arguments":"{}"}}],"reasoning_details":` + string(mustMarshal(t, details)) + `}]}`)
	decision, err := ParseChatBridge(body, "caller-1", 7, verify)
	if err != nil {
		t.Fatal(err)
	}
	out, err := PortableChatBody(body, decision)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "cipher-exact") || strings.Contains(text, "conn-a") || strings.Contains(text, `"mac"`) {
		t.Fatalf("portable leaked marker: %s", text)
	}
	for _, want := range []string{`"include_usage":true`, `"reasoning_effort":"high"`, "HelloWorld", "call_a", "think-a"} {
		if !strings.Contains(text, want) {
			t.Fatalf("portable missing %s in %s", want, text)
		}
	}
}

func TestPiReserializeStillVerifies(t *testing.T) {
	mac, verify := testMAC(t)
	details, err := StampChatDetails(sampleContent(), sampleBind(), mac)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if json.Unmarshal(details[0], &decoded) != nil {
		t.Fatal("decode")
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var again []json.RawMessage
	again = append(again, raw)
	body := assistantBody(mustMarshal(t, again))
	if _, err := ParseChatBridge([]byte(body), "caller-1", 7, verify); err != nil {
		t.Fatal(err)
	}
}

func TestOlderTurnsStayVerifiable(t *testing.T) {
	mac, verify := testMAC(t)
	first, err := StampChatDetails(sampleContent(), sampleBind(), mac)
	if err != nil {
		t.Fatal(err)
	}
	secondContent := []ir.ContentBlock{{Type: ir.BlockReasoning, Text: "later", ResponsesReasoning: &ir.ResponsesReasoning{ID: "rs_late", EncryptedContent: strPtr("cipher-late"), Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "later"}}}}}
	second, err := StampChatDetails(secondContent, sampleBind(), mac)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"combo","messages":[{"role":"assistant","content":"HelloWorld","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"list","arguments":"{}"}}],"reasoning_details":` + string(mustMarshal(t, first)) + `},{"role":"user","content":"next"},{"role":"assistant","content":"","reasoning_details":` + string(mustMarshal(t, second)) + `}]}`)
	decision, err := ParseChatBridge(body, "caller-1", 7, verify)
	if err != nil || !decision.Pin || len(decision.Assistants) != 2 || decision.Assistants[0].Turn == nil {
		t.Fatalf("history parse: %v %+v", err, decision.Assistants)
	}
	if decision.Turn.Reasoning[0].ID != "rs_late" || decision.Assistants[0].Turn.Reasoning[0].ID != "rs_a" {
		t.Fatalf("pin/history ids = %s %s", decision.Turn.Reasoning[0].ID, decision.Assistants[0].Turn.Reasoning[0].ID)
	}
	other := sampleBind()
	other.ProviderID = 9
	old, err := StampChatDetails(sampleContent(), other, mac)
	if err != nil {
		t.Fatal(err)
	}
	mixed := []byte(`{"model":"combo","messages":[{"role":"assistant","content":"HelloWorld","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"list","arguments":"{}"}}],"reasoning_details":` + string(mustMarshal(t, old)) + `},{"role":"assistant","content":"plain"}]}`)
	decision, err = ParseChatBridge(mixed, "caller-1", 7, verify)
	if err != nil || decision.Pin || decision.Assistants[0].Turn == nil || decision.Assistants[1].Marked {
		t.Fatalf("older marker pinned latest: %v pin=%v %+v", err, decision.Pin, decision.Assistants)
	}
}

func TestMalformedBridgeRejectedAndSummaryOnlyScrubbed(t *testing.T) {
	_, verify := testMAC(t)
	broken := []byte(`{"messages":[{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.encrypted","format":"airouter-grok-build-v1","id":"rs"}]}]}`)
	if _, err := ParseChatBridge(broken, "caller-1", 7, verify); err == nil {
		t.Fatal("malformed bridge accepted")
	}
	summaryOnly := []byte(`{"messages":[{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.summary","format":"airouter-grok-build-v1","id":"rs","summary":"hidden"}]}]}`)
	if _, err := ParseChatBridge(summaryOnly, "caller-1", 7, verify); err == nil {
		t.Fatal("unsigned companion accepted")
	}
	unmarked := []byte(`{"messages":[{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.text","text":"plain"}]}]}`)
	decision, err := ParseChatBridge(unmarked, "caller-1", 7, verify)
	if err != nil || decision.Strip {
		t.Fatalf("unmarked treated as bridge: %v %+v", err, decision)
	}
}

func TestStructuralBridgeRejectsEscapedAndNested(t *testing.T) {
	mac, verify := testMAC(t)
	details, err := StampChatDetails(sampleContent(), sampleBind(), mac)
	if err != nil || len(details) == 0 {
		t.Fatal(err)
	}
	signed := details[0]
	wrapped, err := json.Marshal([]any{map[string]any{
		"type": "reasoning.encrypted", "format": "third-party-format", "unexpected": json.RawMessage(signed),
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"assistant","content":"HelloWorld","reasoning_details":` + string(wrapped) + `}]}`)
	_, parseErr := ParseChatBridge(body, "caller-1", 7, verify)
	if parseErr == nil || parseErr.Error() != ErrChatBridgeRejected.Error() {
		t.Fatalf("wrapped third-party entry: %v", parseErr)
	}
	escaped := []byte(`{"messages":[{"role":"assistant","reasoning_details":{"type":"reasoning.summary","format":"\u0061irouter-grok-build-v1","summary":"test"}}]}`)
	if _, err := ParseChatBridge(escaped, "caller-1", 7, verify); err == nil {
		t.Fatal("escaped format in wrong container accepted")
	}
	escapedKey := []byte(`{"messages":[{"role":"assistant","reasoning_details":{"\u0066ormat":"\u0061irouter-grok-build-v1","type":"reasoning.summary","summary":"test"}}]}`)
	if _, err := ParseChatBridge(escapedKey, "caller-1", 7, verify); err == nil {
		t.Fatal("escaped format key accepted")
	}
	escapedBind := []byte(`{"messages":[{"role":"assistant","content":"ok","note":{"bind":{"protocol":"\u0067rok-build"},"mac":"aa"}}]}`)
	if _, err := ParseChatBridge(escapedBind, "caller-1", 7, verify); err == nil {
		t.Fatal("escaped bind indicator accepted")
	}
	var signedObj map[string]any
	if json.Unmarshal(signed, &signedObj) != nil {
		t.Fatal("signed decode")
	}
	signedObj["extra"] = map[string]any{"format": ChatBridgeFormat}
	extra, err := json.Marshal([]any{signedObj})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseChatBridge([]byte(`{"messages":[{"role":"assistant","content":"HelloWorld","reasoning_details":`+string(extra)+`}]}`), "caller-1", 7, verify); err == nil {
		t.Fatal("nested bridge in signed extra field accepted")
	}
	var companion map[string]any
	if json.Unmarshal(details[len(details)-1], &companion) != nil {
		t.Fatal("companion decode")
	}
	companion["extra"] = map[string]any{"bind": map[string]any{"protocol": chatBridgeProtocol}, "layout": []any{}}
	companionRaw, err := json.Marshal([]any{companion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseChatBridge([]byte(`{"messages":[{"role":"assistant","content":"ok","reasoning_details":`+string(companionRaw)+`}]}`), "caller-1", 7, verify); err == nil {
		t.Fatal("nested bridge in companion extra field accepted")
	}
	old := []byte(`{"messages":[{"role":"assistant","content":"HelloWorld","reasoning_details":` + string(wrapped) + `},{"role":"user","content":"next"},{"role":"assistant","content":"plain"}]}`)
	if _, err := ParseChatBridge(old, "caller-1", 7, verify); err == nil {
		t.Fatal("older nested marker accepted")
	}
	quoted := []byte(`{"messages":[{"role":"user","content":"see \\u0061irouter-grok-build-v1"},{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.text","text":"plain"}]}]}`)
	decision, err := ParseChatBridge(quoted, "caller-1", 7, verify)
	if err != nil || decision.Strip || decision.Pin {
		t.Fatalf("quoted escape rejected: %v %+v", err, decision)
	}
	foreign := []byte(`{"messages":[{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.encrypted","format":"xai-responses-v1","data":"foreign-cipher"}]}]}`)
	decision, err = ParseChatBridge(foreign, "caller-1", 7, verify)
	if err != nil || decision.Strip || decision.Pin {
		t.Fatalf("third-party detail rejected: %v %+v", err, decision)
	}
}

func TestStampAndParseShareBudget(t *testing.T) {
	mac, verify := testMAC(t)
	bind := sampleBind()
	cipher := "cipher"
	build := func(n int) []ir.ContentBlock {
		return []ir.ContentBlock{
			{Type: ir.BlockText, Text: "visible"},
			{Type: ir.BlockToolUse, ToolID: "c", ToolName: "f", ResponsesItemID: "fc_c"},
			{Type: ir.BlockReasoning, ResponsesReasoning: &ir.ResponsesReasoning{
				ID: "rs_test", EncryptedContent: &cipher,
				Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: strings.Repeat("a", n)}},
			}},
		}
	}
	repro, err := StampChatDetails(build(chatBridgeMaxMeta-32), bind, mac)
	if err != nil || len(repro) != 0 {
		t.Fatalf("oversize repro stamped: %v %d", err, len(repro))
	}
	empty, err := turnFromContent(build(0), bind)
	if err != nil || empty == nil {
		t.Fatal(err)
	}
	acc := newBridgeAccount()
	if err := acc.chargeTurn(empty); err != nil {
		t.Fatal(err)
	}
	fit := chatBridgeMaxMeta - acc.meta
	if fit < 1 {
		t.Fatalf("no room under meta cap: used %d", acc.meta)
	}
	details, err := StampChatDetails(build(fit), bind, mac)
	if err != nil || len(details) == 0 {
		t.Fatalf("boundary stamp: %v %d", err, len(details))
	}
	body := []byte(`{"messages":[{"role":"assistant","content":"visible","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}],"reasoning_details":` + string(mustMarshal(t, details)) + `}]}`)
	decision, err := ParseChatBridge(body, "caller-1", 7, verify)
	if err != nil || !decision.Pin {
		t.Fatalf("parser rejected its own stamp: %v", err)
	}
	if decision.Turn.Reasoning[0].ID != "rs_test" || decision.Turn.Layout[1].ID != "c" {
		t.Fatalf("parsed turn lost tool or reasoning")
	}
	over, err := StampChatDetails(build(fit+1), bind, mac)
	if err != nil || len(over) != 0 {
		t.Fatalf("one byte over stamped: %v %d", err, len(over))
	}
}

func TestNestedStampedEntryRejected(t *testing.T) {
	mac, verify := testMAC(t)
	details, err := StampChatDetails(sampleContent(), sampleBind(), mac)
	if err != nil || len(details) == 0 {
		t.Fatal(err)
	}
	wrapped, err := json.Marshal([]any{map[string]json.RawMessage{"unexpected": details[0]}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"assistant","content":"HelloWorld","reasoning_details":` + string(wrapped) + `}]}`)
	if _, err := ParseChatBridge(body, "caller-1", 7, verify); err == nil {
		t.Fatal("nested stamped entry accepted")
	}
	quoted := []byte(`{"messages":[{"role":"user","content":"see airouter-grok-build-v1"},{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.text","text":"plain"}]}]}`)
	decision, err := ParseChatBridge(quoted, "caller-1", 7, verify)
	if err != nil || decision.Strip || decision.Pin {
		t.Fatalf("quoted format rejected: %v %+v", err, decision)
	}
}

func TestChatWireBudgetWithinParser(t *testing.T) {
	if ChatWireBudget > chatBridgeMaxCipher {
		t.Fatalf("wire budget %d exceeds parser cipher cap %d", ChatWireBudget, chatBridgeMaxCipher)
	}
}

func TestNoPrivateKeyRejectsMarkedReplay(t *testing.T) {
	mac, _ := testMAC(t)
	details, err := StampChatDetails(sampleContent(), sampleBind(), mac)
	if err != nil {
		t.Fatal(err)
	}
	none := func([]byte) ([]byte, bool) { return nil, false }
	if got, err := StampChatDetails(sampleContent(), sampleBind(), none); err != nil || got != nil {
		t.Fatalf("unsigned key emitted marker: %v %v", err, got)
	}
	if _, err := ParseChatBridge([]byte(assistantBody(mustMarshal(t, details))), "caller-1", 7, func([]byte, []byte) bool { return false }); err == nil {
		t.Fatal("marked replay accepted without a private key")
	}
}

func strPtr(s string) *string { return &s }

func TestBlankGenerationSkipsStamp(t *testing.T) {
	mac, _ := testMAC(t)
	bind := sampleBind()
	bind.Generation = ""
	details, err := StampChatDetails(sampleContent(), bind, mac)
	if err != nil || details != nil {
		t.Fatalf("blank generation emitted %v %v", err, details)
	}
}

func assistantBody(details []byte) string {
	return `{"model":"combo","messages":[{"role":"assistant","content":"HelloWorld","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"list","arguments":"{}"}}],"reasoning_details":` + string(details) + `}]}`
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
