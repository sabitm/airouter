package kirocatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"airouter/internal/proxy/kiro"
)

// Model is one catalog entry. ID is the exact upstream model ID. Capability is
// nil when the schema is missing or does not advertise a supported control.
// Unknown fields are not guessed.
type Model struct {
	ID         string
	Capability *kiro.Capability
}

// Parse reads one ListAvailableModels page. Model order is retained. Duplicate
// IDs on one page are retained so the caller can apply its own dedupe. A
// non-string model ID or nextToken is a shape error. Blank IDs are skipped.
func Parse(body []byte) ([]Model, string, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(body)))
	dec.UseNumber()
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, "", ErrShape
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, "", ErrShape
	}
	modelsRaw, ok := raw["models"]
	if !ok || strings.TrimSpace(string(modelsRaw)) == "null" {
		return nil, "", ErrShape
	}
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(modelsRaw, &models); err != nil || models == nil {
		return nil, "", ErrShape
	}
	out := make([]Model, 0, len(models))
	for _, model := range models {
		idRaw, ok := model["modelId"]
		if !ok || strings.TrimSpace(string(idRaw)) == "null" {
			continue
		}
		var id string
		if err := json.Unmarshal(idRaw, &id); err != nil {
			return nil, "", ErrShape
		}
		if strings.TrimSpace(id) == "" {
			continue
		}
		out = append(out, Model{ID: id, Capability: capabilityFromModel(model)})
	}
	token := ""
	if tokenRaw, ok := raw["nextToken"]; ok && strings.TrimSpace(string(tokenRaw)) != "null" {
		if err := json.Unmarshal(tokenRaw, &token); err != nil {
			return nil, "", ErrShape
		}
	}
	return out, token, nil
}

func capabilityFromModel(model map[string]json.RawMessage) *kiro.Capability {
	raw, ok := model["additionalModelRequestFieldsSchema"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return nil
	}
	props := schemaProperties(schema)
	if props == nil {
		return nil
	}
	cap := &kiro.Capability{}
	if path, levels, def, ok := effortFromSchema(props); ok {
		cap.EffortPath = path
		cap.EffortLevels = levels
		cap.DefaultEffort = def
		cap.HasEffort = true
	}
	if toggle, enabled, present := thinkingFromSchema(props); present {
		cap.HasThinking = true
		cap.ThinkingToggle = toggle
		cap.ThinkingEnabled = enabled
	}
	if !cap.HasEffort && !cap.HasThinking {
		return nil
	}
	return cap
}

// effortFromSchema follows the active client: output_config.effort, then
// reasoning.effort. The first object path with a nonempty string enum wins.
// Enum order is retained. A non-string default is absent. A missing default
// uses the first enum value. A non-object schema is unsupported.
func effortFromSchema(schema map[string]json.RawMessage) (path string, levels []string, def string, ok bool) {
	for _, candidate := range []string{"output_config", "reasoning"} {
		props := objectProperties(schema[candidate])
		if props == nil {
			continue
		}
		effort := jsonObject(props["effort"])
		levels = stringEnum(effort)
		if len(levels) == 0 {
			continue
		}
		path = candidate
		def = stringDefault(effort)
		if def == "" {
			def = levels[0]
		}
		return path, append([]string(nil), levels...), def, true
	}
	return "", nil, "", false
}

// thinkingFromSchema reads properties.thinking.properties.type. The control is
// toggleable only when the enum contains both disabled and adaptive. A missing
// string default is treated as adaptive enabled, matching the active client.
func thinkingFromSchema(schema map[string]json.RawMessage) (toggle, enabled, present bool) {
	props := objectProperties(schema["thinking"])
	if props == nil {
		return false, false, false
	}
	typeProps := jsonObject(props["type"])
	if typeProps == nil {
		return false, false, false
	}
	levels := stringEnum(typeProps)
	if len(levels) == 0 {
		return false, false, false
	}
	hasDisabled, hasAdaptive := false, false
	for _, level := range levels {
		switch level {
		case "disabled":
			hasDisabled = true
		case "adaptive":
			hasAdaptive = true
		}
	}
	toggle = hasDisabled && hasAdaptive
	def, ok := stringDefaultPresent(typeProps)
	if !ok || def != "disabled" {
		enabled = true
	}
	return toggle, enabled, true
}

func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil
	}
	return obj
}

func schemaProperties(schema map[string]json.RawMessage) map[string]json.RawMessage {
	if schema == nil {
		return nil
	}
	raw, ok := schema["properties"]
	if !ok {
		return schema
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(raw, &props); err != nil || props == nil {
		return nil
	}
	return props
}

func objectProperties(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil
	}
	propsRaw, ok := obj["properties"]
	if !ok {
		return map[string]json.RawMessage{}
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(propsRaw, &props); err != nil || props == nil {
		return nil
	}
	return props
}

func stringEnum(props map[string]json.RawMessage) []string {
	if props == nil {
		return nil
	}
	raw, ok := props["enum"]
	if !ok {
		return nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		var s string
		if err := json.Unmarshal(value, &s); err != nil || s == "" {
			return nil
		}
		out = append(out, s)
	}
	return out
}

func stringDefault(props map[string]json.RawMessage) string {
	s, ok := stringDefaultPresent(props)
	if !ok {
		return ""
	}
	return s
}

func stringDefaultPresent(props map[string]json.RawMessage) (string, bool) {
	if props == nil {
		return "", false
	}
	raw, ok := props["default"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
