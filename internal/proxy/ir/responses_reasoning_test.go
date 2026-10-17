package ir

import "testing"

func TestResponsesReasoningProviderState(t *testing.T) {
	if (&ResponsesReasoning{}).ProviderState() {
		t.Fatal("empty reasoning has provider state")
	}
	if !(&ResponsesReasoning{ID: "rs_1"}).ProviderState() {
		t.Fatal("item id must count as provider state")
	}
	empty := ""
	if !(&ResponsesReasoning{EncryptedContent: &empty}).ProviderState() {
		t.Fatal("explicit empty encrypted_content must count as provider state")
	}
	if (&ResponsesReasoning{Summary: []ReasoningSummaryPart{{Type: "summary_text", Text: "x"}}}).ProviderState() {
		t.Fatal("summary text must not count as provider state")
	}
}

func TestResponsesReasoningCloneIsDeep(t *testing.T) {
	enc := "cipher"
	rr := &ResponsesReasoning{
		ID: "rs_1", Status: "completed", EncryptedContent: &enc,
		Summary: []ReasoningSummaryPart{{Type: "summary_text", Text: "a"}},
	}
	cp := rr.Clone()
	if cp == rr {
		t.Fatal("clone returned same pointer")
	}
	rr.ID = "changed"
	rr.Status = "changed"
	*rr.EncryptedContent = "changed"
	rr.Summary[0].Text = "changed"
	if cp.ID != "rs_1" || cp.Status != "completed" || *cp.EncryptedContent != "cipher" || cp.Summary[0].Text != "a" {
		t.Fatalf("clone aliased: %+v", cp)
	}
	var nilRR *ResponsesReasoning
	if nilRR.Clone() != nil {
		t.Fatal("nil clone must stay nil")
	}
}

func TestResponsesReasoningEqualDistinguishesNilEncrypted(t *testing.T) {
	empty := ""
	other := ""
	a := &ResponsesReasoning{ID: "rs_1"}
	b := &ResponsesReasoning{ID: "rs_1", EncryptedContent: &empty}
	if a.Equal(b) {
		t.Fatal("nil and explicit empty encrypted must differ")
	}
	c := &ResponsesReasoning{ID: "rs_1", EncryptedContent: &other}
	if !b.Equal(c) {
		t.Fatal("equal explicit empty encrypted must compare equal")
	}
	d := &ResponsesReasoning{ID: "rs_1", Summary: []ReasoningSummaryPart{{Type: "summary_text", Text: "a"}}}
	e := &ResponsesReasoning{ID: "rs_1", Summary: []ReasoningSummaryPart{{Type: "summary_text", Text: "b"}}}
	if d.Equal(e) {
		t.Fatal("different summaries must differ")
	}
}
