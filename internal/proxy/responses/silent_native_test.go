package responses

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// silentNativeSSE is the parent regression: a silent reasoning placeholder,
// then a tool at output_index 0, then a later reasoning snapshot at index 1,
// with a final array that lists the tool first. Native decode must accept it.
func silentNativeSSE() string {
	return sseData(
		`{"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_silent"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_silent","status":"completed","encrypted_content":"cipher","summary":[]}}`,
		`{"type":"response.completed","response":{"id":"resp_silent","status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}"},{"type":"reasoning","id":"rs_silent","encrypted_content":"cipher","summary":[]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
}

func TestSilentReasoningDoesNotChangeNativeOrder(t *testing.T) {
	body := silentNativeSSE()
	native, err := collectKinds(body, nil)
	if err != nil {
		t.Fatalf("native decode: %v", err)
	}
	if containsKind(native, ir.EventReasoningReserve) {
		t.Fatal("native decode emitted EventReasoningReserve")
	}
	if indexKind(native, ir.EventToolCallStart) < 0 || indexKind(native, ir.EventReasoningItemDone) < indexKind(native, ir.EventToolCallStart) {
		t.Fatalf("native order = %v", native)
	}
	var reserved int
	observed, err := collectKinds(body, func(ev ir.StreamEvent) {
		if ev.Kind == ir.EventReasoningReserve {
			reserved++
		}
	})
	if err != nil {
		t.Fatalf("observed decode: %v", err)
	}
	if reserved == 0 {
		t.Fatal("Chat observer saw no reasoning reserve")
	}
	if !sameKinds(native, observed) {
		t.Fatalf("observer changed emit order\nnative=%v\nobserved=%v", native, observed)
	}
}

func collectKinds(body string, observe func(ir.StreamEvent)) ([]ir.StreamEventKind, error) {
	var kinds []ir.StreamEventKind
	emit := func(ev ir.StreamEvent) error {
		kinds = append(kinds, ev.Kind)
		return nil
	}
	var err error
	if observe == nil {
		err = DecodeStreamAwaitingItemID(strings.NewReader(body), emit)
	} else {
		err = DecodeStreamAwaitingItemIDObserved(strings.NewReader(body), emit, observe)
	}
	return kinds, err
}

func containsKind(kinds []ir.StreamEventKind, want ir.StreamEventKind) bool {
	return indexKind(kinds, want) >= 0
}

func indexKind(kinds []ir.StreamEventKind, want ir.StreamEventKind) int {
	for i, kind := range kinds {
		if kind == want {
			return i
		}
	}
	return -1
}

func sameKinds(a, b []ir.StreamEventKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
