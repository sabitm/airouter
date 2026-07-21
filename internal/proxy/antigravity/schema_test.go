package antigravity

import (
	"encoding/json"
	"testing"
)

func TestCleanJSONSchemaBasic(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"n": {"const": 1, "minLength": 1},
			"x": {"anyOf": [{"type": "string"}, {"type": "null"}]}
		},
		"required": ["n", "missing"],
		"additionalProperties": false
	}`)
	s := CleanJSONSchemaForAntigravity(raw)
	if s["additionalProperties"] != nil {
		t.Fatal("additionalProperties should be stripped")
	}
	props := s["properties"].(map[string]any)
	n := props["n"].(map[string]any)
	if n["const"] != nil {
		t.Fatal("const should become enum")
	}
	en := n["enum"].([]any)
	if len(en) != 1 || en[0] != "1" {
		t.Fatalf("enum: %+v", en)
	}
	if n["minLength"] != nil {
		t.Fatal("minLength stripped")
	}
	x := props["x"].(map[string]any)
	if x["type"] != "string" {
		t.Fatalf("anyOf flattened: %+v", x)
	}
	req := s["required"].([]any)
	if len(req) != 1 || req[0] != "n" {
		t.Fatalf("required cleanup: %+v", req)
	}
}

func TestCleanJSONSchemaEmptyObjectPlaceholder(t *testing.T) {
	s := CleanJSONSchemaForAntigravity(json.RawMessage(`{"type":"object","properties":{}}`))
	props := s["properties"].(map[string]any)
	if props["reason"] == nil {
		t.Fatalf("placeholder missing: %+v", s)
	}
}
