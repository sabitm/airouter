package ir

import (
	"encoding/json"
	"testing"
)

// Direct decode coverage for the strict member's three states: omission,
// explicit false/true, and explicit null. Explicit null must be accepted as a
// distinct state (StrictNull) and never collapsed into omission or false.
func TestDecodeChatResponseFormatStrictNull(t *testing.T) {
	cases := []struct {
		name           string
		strictMember   string
		wantStrict     *bool
		wantStrictNull bool
		wantInvalid    bool
	}{
		{name: "omitted", strictMember: ""},
		{name: "false", strictMember: `,"strict":false`, wantStrict: boolPtrIR(false)},
		{name: "true", strictMember: `,"strict":true`, wantStrict: boolPtrIR(true)},
		{name: "null", strictMember: `,"strict":null`, wantStrictNull: true},
		{name: "string", strictMember: `,"strict":"yes"`, wantInvalid: true},
		{name: "number", strictMember: `,"strict":1`, wantInvalid: true},
		{name: "array", strictMember: `,"strict":[]`, wantInvalid: true},
		{name: "object", strictMember: `,"strict":{}`, wantInvalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"type":"json_schema","json_schema":{"name":"Out","schema":{"type":"object"}` + tc.strictMember + `}}`
			f := DecodeChatResponseFormat(json.RawMessage(raw), "test")
			if f == nil {
				t.Fatal("format dropped")
			}
			if tc.wantInvalid {
				if f.InvalidReason == "" {
					t.Fatalf("expected invalid intent, got %+v", f)
				}
				return
			}
			if f.InvalidReason != "" {
				t.Fatalf("unexpected invalid reason: %s", f.InvalidReason)
			}
			if (f.Strict == nil) != (tc.wantStrict == nil) {
				t.Fatalf("Strict = %v, want %v", f.Strict, tc.wantStrict)
			}
			if tc.wantStrict != nil && *f.Strict != *tc.wantStrict {
				t.Fatalf("Strict = %v, want %v", *f.Strict, *tc.wantStrict)
			}
			if f.StrictNull != tc.wantStrictNull {
				t.Fatalf("StrictNull = %v, want %v", f.StrictNull, tc.wantStrictNull)
			}
			if err := f.Validate(); err != nil {
				t.Fatalf("Validate rejected accepted state: %v", err)
			}
		})
	}
}

// Responses flat shape must behave identically for strict null.
func TestDecodeResponsesFormatStrictNull(t *testing.T) {
	f := DecodeResponsesFormat(json.RawMessage(`{"type":"json_schema","name":"Out","schema":{"type":"object"},"strict":null}`), "test")
	if f == nil || f.InvalidReason != "" {
		t.Fatalf("strict null rejected: %+v", f)
	}
	if f.Strict != nil || !f.StrictNull {
		t.Fatalf("Strict = %v StrictNull = %v, want nil/true", f.Strict, f.StrictNull)
	}
}

// json_object and text keep rejecting any extraneous strict member, including
// an explicit null.
func TestDecodeNonSchemaStrictNullRejected(t *testing.T) {
	for _, kind := range []OutputFormatType{OutputFormatJSONObject, OutputFormatText} {
		t.Run(string(kind), func(t *testing.T) {
			f := DecodeChatResponseFormat(json.RawMessage(`{"type":"`+string(kind)+`","strict":null}`), "test")
			if f == nil || f.InvalidReason == "" {
				t.Fatalf("%s accepted extraneous strict null: %+v", kind, f)
			}
		})
	}
}

// A hand-built format carrying both states is inconsistent and must fail
// validation so the wire encoders never guess.
func TestValidateStrictStateConsistency(t *testing.T) {
	tr := true
	f := &OutputFormat{Type: OutputFormatJSONSchema, Name: "Out", Schema: json.RawMessage(`{"type":"object"}`), Strict: &tr, StrictNull: true}
	if err := f.Validate(); err == nil {
		t.Fatal("Validate accepted Strict and StrictNull together")
	}
}

// Legacy matching treats explicit null strict as unspecified, same as omission.
func TestLegacyConflictStrictNullEquivalentAbsent(t *testing.T) {
	f := &OutputFormat{Type: OutputFormatJSONSchema, Name: "Out", Schema: json.RawMessage(`{"type":"object","title":"Same"}`), StrictNull: true}
	legacy := json.RawMessage(`{"type":"json_schema","schema":{"type":"object","title":"Same"}}`)
	if !outputFormatMatchesLegacy(f, legacy) {
		t.Fatal("strict null legacy match failed; explicit null must be equivalent to absent")
	}
	falseValue := false
	f2 := &OutputFormat{Type: OutputFormatJSONSchema, Name: "Out", Schema: json.RawMessage(`{"type":"object","title":"Same"}`), Strict: &falseValue}
	if outputFormatMatchesLegacy(f2, legacy) {
		t.Fatal("strict false must stay rejected by the enforced legacy slot")
	}
}

func boolPtrIR(v bool) *bool { return &v }
