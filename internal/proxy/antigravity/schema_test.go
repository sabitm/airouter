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

func TestMergeAllOf(t *testing.T) {
	t.Run("no allOf unchanged", func(t *testing.T) {
		m := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
		mergeAllOf(m)
		if m["type"] != "object" {
			t.Errorf("type changed")
		}
		if _, ok := m["allOf"]; ok {
			t.Errorf("allOf should not be present")
		}
	})

	t.Run("single allOf merges props and required", func(t *testing.T) {
		m := map[string]any{
			"allOf": []any{
				map[string]any{
					"properties": map[string]any{"name": map[string]any{"type": "string"}},
					"required":   []any{"name"},
				},
			},
		}
		mergeAllOf(m)
		if _, ok := m["allOf"]; ok {
			t.Error("allOf not removed")
		}
		props := m["properties"].(map[string]any)
		if _, ok := props["name"]; !ok {
			t.Error("property name not merged")
		}
		req := m["required"].([]any)
		if len(req) != 1 || req[0] != "name" {
			t.Errorf("required = %+v", req)
		}
	})

	t.Run("multiple allOf unions props and required", func(t *testing.T) {
		m := map[string]any{
			"allOf": []any{
				map[string]any{
					"properties": map[string]any{"a": map[string]any{"type": "string"}},
					"required":   []any{"a"},
				},
				map[string]any{
					"properties": map[string]any{"b": map[string]any{"type": "integer"}},
					"required":   []any{"b"},
				},
			},
		}
		mergeAllOf(m)
		props := m["properties"].(map[string]any)
		if _, ok := props["a"]; !ok {
			t.Error("a not merged")
		}
		if _, ok := props["b"]; !ok {
			t.Error("b not merged")
		}
		req := m["required"].([]any)
		if len(req) != 2 {
			t.Errorf("required len = %d, want 2", len(req))
		}
	})

	t.Run("allOf item without props or required is skipped", func(t *testing.T) {
		m := map[string]any{
			"allOf": []any{
				map[string]any{"description": "no props"},
				map[string]any{"properties": map[string]any{"x": map[string]any{"type": "string"}}},
			},
		}
		mergeAllOf(m)
		props := m["properties"].(map[string]any)
		if _, ok := props["x"]; !ok {
			t.Error("x should be merged from second item")
		}
	})

	t.Run("existing properties preserved and augmented", func(t *testing.T) {
		m := map[string]any{
			"properties": map[string]any{"existing": map[string]any{"type": "boolean"}},
			"allOf": []any{
				map[string]any{"properties": map[string]any{"added": map[string]any{"type": "string"}}},
			},
		}
		mergeAllOf(m)
		props := m["properties"].(map[string]any)
		if _, ok := props["existing"]; !ok {
			t.Error("existing property dropped")
		}
		if _, ok := props["added"]; !ok {
			t.Error("added property missing")
		}
	})

	t.Run("existing required preserved and appended", func(t *testing.T) {
		m := map[string]any{
			"required": []any{"old"},
			"allOf": []any{
				map[string]any{"required": []any{"new"}},
			},
		}
		mergeAllOf(m)
		req := m["required"].([]any)
		if len(req) != 2 {
			t.Fatalf("required len = %d, want 2", len(req))
		}
		if req[0] != "old" || req[1] != "new" {
			t.Errorf("required = %+v, want [old new]", req)
		}
	})

	t.Run("non-map allOf item skipped", func(t *testing.T) {
		m := map[string]any{
			"allOf": []any{
				"not a map",
				map[string]any{"properties": map[string]any{"x": map[string]any{"type": "string"}}},
			},
		}
		mergeAllOf(m)
		props := m["properties"].(map[string]any)
		if _, ok := props["x"]; !ok {
			t.Error("x should be merged despite non-map sibling")
		}
	})

	t.Run("nested allOf inside a property", func(t *testing.T) {
		m := map[string]any{
			"properties": map[string]any{
				"nested": map[string]any{
					"allOf": []any{
						map[string]any{"properties": map[string]any{"deep": map[string]any{"type": "string"}}},
					},
				},
			},
		}
		mergeAllOf(m)
		nested := m["properties"].(map[string]any)["nested"].(map[string]any)
		if _, ok := nested["allOf"]; ok {
			t.Error("nested allOf not removed")
		}
		deepProps := nested["properties"].(map[string]any)
		if _, ok := deepProps["deep"]; !ok {
			t.Error("deep property not merged")
		}
	})

	t.Run("array root recurses into elements", func(t *testing.T) {
		var arr []any
		if err := json.Unmarshal([]byte(`[{"allOf":[{"properties":{"x":{"type":"string"}}}]}]`), &arr); err != nil {
			t.Fatal(err)
		}
		mergeAllOf(arr)
		first := arr[0].(map[string]any)
		if _, ok := first["allOf"]; ok {
			t.Error("allOf not removed in array element")
		}
		if _, ok := first["properties"].(map[string]any)["x"]; !ok {
			t.Error("x not merged in array element")
		}
	})

	t.Run("non-map non-array root is no-op", func(t *testing.T) {
		mergeAllOf("just a string")
		mergeAllOf(42)
		mergeAllOf(nil)
	})
}

func TestSelectBestSchema(t *testing.T) {
	cases := []struct {
		name   string
		items  []map[string]any
		want   int
	}{
		{
			"object wins over scalar",
			[]map[string]any{
				{"type": "string"},
				{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			},
			1,
		},
		{
			"array beats scalar",
			[]map[string]any{
				{"type": "string"},
				{"type": "array", "items": map[string]any{"type": "string"}},
			},
			1,
		},
		{
			"scalar beats null",
			[]map[string]any{
				{"type": "null"},
				{"type": "integer"},
			},
			1,
		},
		{
			"tie keeps first",
			[]map[string]any{
				{"type": "string"},
				{"type": "string"},
			},
			0,
		},
		{
			"properties without type scores as object",
			[]map[string]any{
				{"type": "string"},
				{"properties": map[string]any{"a": map[string]any{}}},
			},
			1,
		},
		{
			"single object",
			[]map[string]any{{"type": "object", "properties": map[string]any{}}},
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectBestSchema(tc.items); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestToString(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string passthrough", "hello", "hello"},
		{"json.Number", json.Number("42.5"), "42.5"},
		{"int", 42, "42"},
		{"float64", 3.14, "3.14"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"nil", nil, "null"},
		{"map", map[string]any{"k": "v"}, `{"k":"v"}`},
		{"empty string", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toString(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeepCopyMap(t *testing.T) {
	t.Run("round-trip preserves values", func(t *testing.T) {
		original := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "string"},
			},
			"required": []any{"a"},
		}
		copy := deepCopyMap(original)
		if copy["type"] != "object" {
			t.Errorf("type lost")
		}
		props := copy["properties"].(map[string]any)
		if _, ok := props["a"]; !ok {
			t.Errorf("nested property lost")
		}
	})

	t.Run("mutating copy does not affect original", func(t *testing.T) {
		original := map[string]any{
			"properties": map[string]any{"a": map[string]any{"type": "string"}},
		}
		copy := deepCopyMap(original)
		cpProps := copy["properties"].(map[string]any)
		cpProps["b"] = map[string]any{"type": "integer"}
		origProps := original["properties"].(map[string]any)
		if _, ok := origProps["b"]; ok {
			t.Error("mutating copy leaked into original (not a deep copy)")
		}
	})

	t.Run("empty map", func(t *testing.T) {
		copy := deepCopyMap(map[string]any{})
		if len(copy) != 0 {
			t.Errorf("got %v, want empty", copy)
		}
	})
}
