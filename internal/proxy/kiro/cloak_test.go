package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestBuildToolCatalogNames(t *testing.T) {
	longName := "lookup_" + strings.Repeat("x", 70)
	tools := []ir.Tool{
		{Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		{Name: "fs_read", Description: "client native"},
		{Name: "already_ide", Description: "already suffixed"},
		{Name: longName, Description: "long"},
		{Name: "bad name", Description: "invalid"},
		{Name: "bad-name", Description: "hyphen"},
		{Name: " ", Description: "blank"},
		{Name: "get_weather", Description: "duplicate ignored"},
		{Name: "", Description: "empty"},
	}
	first := buildToolCatalog(tools)
	second := buildToolCatalog(tools)
	if first.empty() {
		t.Fatal("catalog empty")
	}
	if len(first.tools) != 6 {
		t.Fatalf("wire tools = %d, want 6", len(first.tools))
	}

	want := map[string]string{
		"get_weather": "get_weather",
		"fs_read":     "fs_read",
		"already_ide": "already_ide",
		"bad-name":    "bad-name",
		"bad name":    "bad_name",
	}
	for original, wire := range want {
		if first.toWire[original] != wire {
			t.Errorf("%s wire = %q, want %q", original, first.toWire[original], wire)
		}
		if got := first.toClient[wire]; got != original {
			t.Errorf("reverse %s = %q, want %q", wire, got, original)
		}
	}
	longWire := first.toWire[longName]
	if longWire == "" || len(longWire) > toolNameLimit || !validToolName(longWire) {
		t.Fatalf("long wire = %q", longWire)
	}
	if first.toClient[longWire] != longName {
		t.Fatalf("reverse long = %q", first.toClient[longWire])
	}
	if _, ok := first.toWire[" "]; ok {
		t.Error("blank name was mapped")
	}
	if _, ok := first.toWire[""]; ok {
		t.Error("empty name was mapped")
	}
	if first.tools[0].ToolSpecification.Description != "weather" {
		t.Errorf("duplicate replaced the first description: %q", first.tools[0].ToolSpecification.Description)
	}
	for _, spec := range first.tools {
		for _, decoy := range []string{"execute_bash", "fs_write", "glob", "grep", "web_search", "web_fetch"} {
			if spec.ToolSpecification.Name == decoy {
				t.Fatalf("decoy %s was advertised", decoy)
			}
		}
	}
	if !catalogEqual(first, second) {
		t.Fatal("catalog is not deterministic")
	}
}

func TestBuildToolCatalogCollisions(t *testing.T) {
	base := strings.Repeat("a", toolNameLimit)
	tools := []ir.Tool{
		{Name: base},
		{Name: base + "extra"},
		{Name: "Tool"},
		{Name: "tool"},
		{Name: "123bad"},
		{Name: "123bad!"},
	}
	cat := buildToolCatalog(tools)
	seen := map[string]bool{}
	for _, tool := range tools {
		wire := cat.toWire[tool.Name]
		if wire == "" || seen[wire] || cat.toClient[wire] != tool.Name {
			t.Fatalf("%s collapsed to %q", tool.Name, wire)
		}
		if len(wire) > toolNameLimit || !validToolName(wire) {
			t.Fatalf("%s wire %q is not a valid unique name", tool.Name, wire)
		}
		seen[wire] = true
	}
	if cat.toWire[base] != base {
		t.Errorf("already valid long name changed to %q", cat.toWire[base])
	}
	if cat.toWire["Tool"] != "Tool" || cat.toWire["tool"] != "tool" {
		t.Fatalf("case-distinct names changed: %+v", cat.toWire)
	}
	if cat.toWire["123bad"] != "123bad" || cat.toWire["123bad!"] == "123bad" {
		t.Fatalf("invalid collision = %+v", cat.toWire)
	}
	again := buildToolCatalog(tools)
	if !catalogEqual(cat, again) {
		t.Fatal("collision catalog is not deterministic")
	}
}

func TestBuildToolCatalogEmptyAndNoDecoys(t *testing.T) {
	for _, tools := range [][]ir.Tool{nil, {}, {{Name: ""}}, {{Name: "  "}}} {
		cat := buildToolCatalog(tools)
		if !cat.empty() {
			t.Fatalf("unusable declarations produced catalog %+v", cat.toWire)
		}
	}
	cat := buildToolCatalog([]ir.Tool{{Name: "x"}, {Name: "x"}})
	if cat.toWire["x"] != "x" || len(cat.tools) != 1 {
		t.Fatalf("duplicate usable catalog = %+v", cat.toWire)
	}
}

func TestBuildToolCatalogSchemaAndDescription(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"id":{"maximum":9223372036854775807,"additionalProperties":false}},"required":["id","missing"]}`)
	tool := ir.Tool{Name: "lookup", Description: "", Parameters: schema}
	cat := buildToolCatalog([]ir.Tool{tool})
	got := cat.tools[0].ToolSpecification
	if got.Name != "lookup" || got.Description != "Tool: lookup" {
		t.Fatalf("spec = %+v", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got.InputSchema.JSON, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["type"] != "object" {
		t.Fatalf("root type = %#v", parsed["type"])
	}
	props := parsed["properties"].(map[string]any)
	id := props["id"].(map[string]any)
	if _, ok := id["additionalProperties"]; !ok {
		t.Fatal("nested additionalProperties was stripped")
	}
	required := parsed["required"].([]any)
	if len(required) != 1 || required[0] != "id" {
		t.Fatalf("required = %#v", required)
	}
	schema[0] = 'X'
	if string(cat.tools[0].ToolSpecification.InputSchema.JSON) == string(schema) {
		t.Fatal("catalog kept a live alias of the caller schema")
	}
}

func catalogEqual(a, b toolCatalog) bool {
	if len(a.tools) != len(b.tools) || len(a.toWire) != len(b.toWire) || len(a.toClient) != len(b.toClient) {
		return false
	}
	for i := range a.tools {
		as := a.tools[i].ToolSpecification
		bs := b.tools[i].ToolSpecification
		if as.Name != bs.Name || as.Description != bs.Description || string(as.InputSchema.JSON) != string(bs.InputSchema.JSON) {
			return false
		}
	}
	for k, v := range a.toWire {
		if b.toWire[k] != v {
			return false
		}
	}
	for k, v := range a.toClient {
		if b.toClient[k] != v {
			return false
		}
	}
	return true
}

func validToolName(name string) bool {
	if name == "" || len(name) > toolNameLimit {
		return false
	}
	for _, r := range name {
		if !toolNameRune(r) {
			return false
		}
	}
	return true
}
