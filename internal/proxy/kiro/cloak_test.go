package kiro

import (
	"crypto/sha256"
	"encoding/hex"
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
	if len(first.tools) != 6+len(decoyNames) {
		t.Fatalf("wire tools = %d, want %d", len(first.tools), 6+len(decoyNames))
	}

	want := map[string]string{
		"get_weather": "get_weather_ide",
		"fs_read":     "fs_read_ide",
		"already_ide": "already_ide_ide",
	}
	for original, wire := range want {
		if first.toWire[original] != wire {
			t.Errorf("%s wire = %q, want %q", original, first.toWire[original], wire)
		}
		if got := first.toClient[wire]; got != original {
			t.Errorf("reverse %s = %q, want %q", wire, got, original)
		}
	}
	for _, original := range []string{longName, "bad name", "bad-name"} {
		wire := first.toWire[original]
		if wire == "" || len(wire) > toolNameLimit || !strings.HasSuffix(wire, toolSuffix) {
			t.Errorf("%s alias = %q", original, wire)
		}
		if !validAlias(wire) {
			t.Errorf("%s alias %q is outside the conservative alphabet", original, wire)
		}
		if first.toClient[wire] != original {
			t.Errorf("reverse alias %s = %q", wire, first.toClient[wire])
		}
	}
	if _, ok := first.toWire[" "]; ok {
		t.Error("blank name was cloaked")
	}
	if _, ok := first.toWire[""]; ok {
		t.Error("empty name was cloaked")
	}
	if first.tools[0].ToolSpecification.Description != "weather" {
		t.Errorf("duplicate replaced the first description: %q", first.tools[0].ToolSpecification.Description)
	}

	wires := map[string]string{}
	for original, wire := range first.toWire {
		if prev, ok := wires[wire]; ok {
			t.Errorf("wire %q maps both %q and %q", wire, prev, original)
		}
		wires[wire] = original
		if first.toClient[wire] != original {
			t.Errorf("reverse mismatch for %q", original)
		}
	}
	for i, name := range decoyNames {
		spec := first.tools[len(first.tools)-len(decoyNames)+i].ToolSpecification
		if spec.Name != name || spec.Description != decoyDescription || string(spec.InputSchema.JSON) != string(decoySchema) {
			t.Errorf("decoy %d = %+v", i, spec)
		}
		if !first.decoys[name] {
			t.Errorf("decoy %s missing from reject set", name)
		}
	}
	if !catalogEqual(first, second) {
		t.Fatal("catalog is not deterministic")
	}
}

func TestBuildToolCatalogCollisions(t *testing.T) {
	base := strings.Repeat("a", toolNameLimit-len(toolSuffix))
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
		if len(wire) > toolNameLimit || !strings.HasSuffix(wire, toolSuffix) || !validAlias(wire) {
			t.Fatalf("%s wire %q is not a valid unique alias", tool.Name, wire)
		}
		seen[wire] = true
	}
	if cat.toWire[base] != base+toolSuffix {
		t.Errorf("ordinary long-enough name wire = %q", cat.toWire[base])
	}

	// Truncation of an invalid name must not land on another client's ordinary wire name.
	ordinary := "lookup"
	forced := "lookup_" + strings.Repeat("x", toolNameLimit)
	collision := buildToolCatalog([]ir.Tool{{Name: ordinary}, {Name: forced}})
	if collision.toWire[ordinary] == collision.toWire[forced] || collision.toClient[collision.toWire[forced]] != forced {
		t.Fatalf("truncated alias collapsed: %+v", collision.toWire)
	}
	again := buildToolCatalog(tools)
	if !catalogEqual(cat, again) {
		t.Fatal("collision catalog is not deterministic")
	}
}

func TestBuildToolCatalogOccupiedAliasesStayWithinLimit(t *testing.T) {
	const original = "bad name"
	sum := sha256.Sum256([]byte(original))
	digest := hex.EncodeToString(sum[:])
	for _, counters := range []int{0, 3} {
		var tools []ir.Tool
		for n := 8; n <= 56; n += 4 {
			prefix := "bad_name"
			room := toolNameLimit - len(toolSuffix) - 1 - n
			if len(prefix) > room {
				prefix = prefix[:room]
			}
			tools = append(tools, ir.Tool{Name: prefix + "_" + digest[:n]})
		}
		for n := 2; n < 2+counters; n++ {
			tools = append(tools, ir.Tool{Name: "bad_name_" + digest[:12] + "_" + base36(n)})
		}
		tools = append(tools, ir.Tool{Name: original})

		cat := buildToolCatalog(tools)
		seen := map[string]bool{}
		for _, tool := range tools {
			wire := cat.toWire[tool.Name]
			if !validAlias(wire) || !strings.HasSuffix(wire, toolSuffix) || seen[wire] {
				t.Fatalf("%d occupied counters: %q produced invalid or repeated wire name %q (%d bytes)", counters, tool.Name, wire, len(wire))
			}
			if cat.toClient[wire] != tool.Name {
				t.Fatalf("%q restored to %q", tool.Name, cat.toClient[wire])
			}
			if tool.Name != original && wire != tool.Name+toolSuffix {
				t.Fatalf("fixture did not occupy the expected alias: %q became %q", tool.Name, wire)
			}
			seen[wire] = true
		}
		if !catalogEqual(cat, buildToolCatalog(tools)) {
			t.Fatal("occupied-alias catalog is not deterministic")
		}
	}
}

func TestFitAliasBoundsCompleteName(t *testing.T) {
	for _, prefix := range []string{"", "bad_name", strings.Repeat("a", 100)} {
		for _, tailLength := range []int{8, 56, 58, 60, 64, 256} {
			wire := fitAlias(prefix, strings.Repeat("b", tailLength))
			if !validAlias(wire) || !strings.HasSuffix(wire, toolSuffix) {
				t.Fatalf("prefix %q with %d-byte tail produced invalid alias %q (%d bytes)", prefix, tailLength, wire, len(wire))
			}
		}
	}
}

func TestBuildToolCatalogEmptyAndNoDecoys(t *testing.T) {
	for _, tools := range [][]ir.Tool{nil, {}, {{Name: ""}}, {{Name: "  "}}, {{Name: "x"}, {Name: "x"}}} {
		cat := buildToolCatalog(tools)
		if len(tools) > 0 && tools[0].Name == "x" {
			if cat.toWire["x"] != "x_ide" || len(cat.tools) != 1+len(decoyNames) {
				t.Fatalf("duplicate usable catalog = %+v", cat.toWire)
			}
			continue
		}
		if !cat.empty() || len(cat.decoys) != 0 {
			t.Fatalf("unusable declarations produced catalog %+v", cat)
		}
	}
}

func TestBuildToolCatalogPreservesRawSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"id":{"maximum":9223372036854775807},"n":{"maximum":1e400}}}`)
	tool := ir.Tool{Name: "lookup", Description: "keep", Parameters: schema}
	cat := buildToolCatalog([]ir.Tool{tool})
	got := cat.tools[0].ToolSpecification
	if got.Name != "lookup_ide" || got.Description != "keep" || string(got.InputSchema.JSON) != string(schema) {
		t.Fatalf("spec = %+v", got)
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

func validAlias(name string) bool {
	if name == "" || len(name) > toolNameLimit {
		return false
	}
	for i, r := range name {
		if !aliasRune(r, i == 0) {
			return false
		}
	}
	return true
}
