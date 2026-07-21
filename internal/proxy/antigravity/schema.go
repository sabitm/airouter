package antigravity

import "encoding/json"

// unsupportedSchemaKeys are JSON Schema keywords Gemini/Antigravity reject.
// Ported from 9router translator/formats/gemini.js UNSUPPORTED_SCHEMA_CONSTRAINTS.
var unsupportedSchemaKeys = map[string]bool{
	"minLength": true, "maxLength": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
	"minItems": true, "maxItems": true, "format": true,
	"default": true, "examples": true,
	"$schema": true, "$defs": true, "definitions": true, "const": true, "$ref": true, "$comment": true,
	"deprecated": true, "readOnly": true, "writeOnly": true,
	"additionalProperties": true, "propertyNames": true, "patternProperties": true, "enumDescriptions": true,
	"anyOf": true, "oneOf": true, "allOf": true, "not": true,
	"dependencies": true, "dependentSchemas": true, "dependentRequired": true,
	"title": true, "optional": true, "if": true, "then": true, "else": true,
	"contentMediaType": true, "contentEncoding": true,
	"cornerRadius": true, "fillColor": true, "fontFamily": true, "fontSize": true, "fontWeight": true,
	"gap": true, "padding": true, "strokeColor": true, "strokeThickness": true, "textColor": true,
}

// CleanJSONSchemaForAntigravity mutates a JSON Schema object into a form
// VALIDATED-mode Gemini accepts. Returns a cleaned deep copy of raw (or a
// placeholder object schema when raw is empty/invalid).
func CleanJSONSchemaForAntigravity(raw json.RawMessage) map[string]any {
	var schema map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil || schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	} else {
		schema = deepCopyMap(schema)
	}
	convertConstToEnum(schema)
	convertEnumValuesToStrings(schema)
	mergeAllOf(schema)
	flattenAnyOfOneOf(schema)
	flattenTypeArrays(schema)
	ensureObjectType(schema)
	removeUnsupportedKeywords(schema)
	cleanupRequired(schema)
	addPlaceholders(schema)
	return schema
}

func deepCopyMap(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func convertConstToEnum(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				convertConstToEnum(v)
			}
		}
		return
	}
	if c, has := m["const"]; has {
		if _, hasEnum := m["enum"]; !hasEnum {
			m["enum"] = []any{c}
		}
		delete(m, "const")
	}
	for _, v := range m {
		convertConstToEnum(v)
	}
}

func convertEnumValuesToStrings(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				convertEnumValuesToStrings(v)
			}
		}
		return
	}
	if en, ok := m["enum"].([]any); ok {
		out := make([]any, len(en))
		for i, v := range en {
			out[i] = toString(v)
		}
		m["enum"] = out
		if _, hasType := m["type"]; !hasType {
			m["type"] = "string"
		}
	}
	for _, v := range m {
		convertEnumValuesToStrings(v)
	}
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func mergeAllOf(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				mergeAllOf(v)
			}
		}
		return
	}
	if allOf, ok := m["allOf"].([]any); ok {
		mergedProps := map[string]any{}
		var mergedReq []any
		for _, item := range allOf {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if props, ok := im["properties"].(map[string]any); ok {
				for k, v := range props {
					mergedProps[k] = v
				}
			}
			if req, ok := im["required"].([]any); ok {
				mergedReq = append(mergedReq, req...)
			}
		}
		delete(m, "allOf")
		if len(mergedProps) > 0 {
			if existing, ok := m["properties"].(map[string]any); ok {
				for k, v := range mergedProps {
					existing[k] = v
				}
				m["properties"] = existing
			} else {
				m["properties"] = mergedProps
			}
		}
		if len(mergedReq) > 0 {
			if existing, ok := m["required"].([]any); ok {
				m["required"] = append(existing, mergedReq...)
			} else {
				m["required"] = mergedReq
			}
		}
	}
	for _, v := range m {
		mergeAllOf(v)
	}
}

func flattenAnyOfOneOf(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				flattenAnyOfOneOf(v)
			}
		}
		return
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		items, ok := m[key].([]any)
		if !ok || len(items) == 0 {
			continue
		}
		var nonNull []map[string]any
		for _, it := range items {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := im["type"].(string); t == "null" {
				continue
			}
			nonNull = append(nonNull, im)
		}
		if len(nonNull) == 0 {
			continue
		}
		selected := nonNull[selectBestSchema(nonNull)]
		delete(m, key)
		for k, v := range selected {
			if _, exists := m[k]; !exists {
				m[k] = v
			}
		}
	}
	for _, v := range m {
		flattenAnyOfOneOf(v)
	}
}

func selectBestSchema(items []map[string]any) int {
	bestIdx, bestScore := 0, -1
	for i, item := range items {
		score := 0
		t, _ := item["type"].(string)
		if t == "object" || item["properties"] != nil {
			score = 3
		} else if t == "array" || item["items"] != nil {
			score = 2
		} else if t != "" && t != "null" {
			score = 1
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	return bestIdx
}

func flattenTypeArrays(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				flattenTypeArrays(v)
			}
		}
		return
	}
	if types, ok := m["type"].([]any); ok {
		var nonNull []any
		for _, t := range types {
			if s, ok := t.(string); ok && s != "null" {
				nonNull = append(nonNull, s)
			}
		}
		if len(nonNull) > 0 {
			m["type"] = nonNull[0]
		} else {
			m["type"] = "string"
		}
	}
	for _, v := range m {
		flattenTypeArrays(v)
	}
}

func ensureObjectType(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				ensureObjectType(v)
			}
		}
		return
	}
	if m["properties"] != nil {
		if _, has := m["type"]; !has {
			m["type"] = "object"
		}
	}
	for _, v := range m {
		ensureObjectType(v)
	}
}

func removeUnsupportedKeywords(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				removeUnsupportedKeywords(v)
			}
		}
		return
	}
	for k := range m {
		if unsupportedSchemaKeys[k] || (len(k) > 2 && k[:2] == "x-") {
			delete(m, k)
			continue
		}
		removeUnsupportedKeywords(m[k])
	}
}

func cleanupRequired(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				cleanupRequired(v)
			}
		}
		return
	}
	if req, ok := m["required"].([]any); ok {
		props, _ := m["properties"].(map[string]any)
		if props == nil {
			delete(m, "required")
		} else {
			var valid []any
			for _, r := range req {
				name, _ := r.(string)
				if name != "" {
					if _, ok := props[name]; ok {
						valid = append(valid, name)
					}
				}
			}
			if len(valid) == 0 {
				delete(m, "required")
			} else {
				m["required"] = valid
			}
		}
	}
	for _, v := range m {
		cleanupRequired(v)
	}
}

func addPlaceholders(obj any) {
	m, ok := obj.(map[string]any)
	if !ok {
		if arr, ok := obj.([]any); ok {
			for _, v := range arr {
				addPlaceholders(v)
			}
		}
		return
	}
	if t, _ := m["type"].(string); t == "object" {
		props, _ := m["properties"].(map[string]any)
		if len(props) == 0 {
			m["properties"] = map[string]any{
				"reason": map[string]any{
					"type":        "string",
					"description": "Brief explanation of why you are calling this tool",
				},
			}
			m["required"] = []any{"reason"}
		}
	}
	for _, v := range m {
		addPlaceholders(v)
	}
}
