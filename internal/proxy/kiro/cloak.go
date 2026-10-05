package kiro

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"airouter/internal/proxy/ir"
)

const (
	toolNameLimit        = 64
	toolDescriptionLimit = 10237
	toolIDLimit          = 64
	emptySchema          = `{"type":"object","properties":{}}`
)

// toolCatalog is the request-local wire-name map built from one declaration
// list. The same declarations in the same order always produce the same maps.
// Nothing here is shared across requests.
type toolCatalog struct {
	tools    []cwTool
	toWire   map[string]string
	toClient map[string]string
}

func (c toolCatalog) empty() bool {
	return len(c.tools) == 0
}

// buildToolCatalog normalizes usable client declarations. Empty, whitespace-only,
// and exact-duplicate names are omitted. The first usable declaration of a
// repeated name wins. Already valid names stay unchanged. Invalid characters
// become underscores, names are limited to 64 characters, and collisions get a
// deterministic numeric suffix.
func buildToolCatalog(tools []ir.Tool) toolCatalog {
	usable := make([]ir.Tool, 0, len(tools))
	seenOriginal := map[string]bool{}
	for _, tool := range tools {
		name := tool.Name
		if strings.TrimSpace(name) == "" || seenOriginal[name] {
			continue
		}
		seenOriginal[name] = true
		usable = append(usable, tool)
	}
	if len(usable) == 0 {
		return toolCatalog{}
	}

	cat := toolCatalog{
		toWire:   make(map[string]string, len(usable)),
		toClient: make(map[string]string, len(usable)),
	}
	used := map[string]bool{}
	for i, tool := range usable {
		wire := uniqueToolName(tool.Name, i, used)
		cat.toWire[tool.Name] = wire
		cat.toClient[wire] = tool.Name
		used[wire] = true
		cat.tools = append(cat.tools, cwTool{ToolSpecification: cwToolSpecification{
			Name:        wire,
			Description: normalizeToolDescription(tool.Description, tool.Name),
			InputSchema: cwInputSchema{JSON: normalizeToolSchema(tool.Parameters)},
		}})
	}
	return cat
}

// uniqueToolName returns one unused wire name. used is updated by the caller.
func uniqueToolName(original string, index int, used map[string]bool) string {
	base := sanitizeToolName(original)
	if base == "" {
		base = "tool_" + base36(index+1)
	}
	base = trimRunes(base, toolNameLimit)
	candidate := base
	for suffix := 2; used[candidate]; suffix++ {
		tail := "_" + base36(suffix)
		candidate = trimRunes(base, toolNameLimit-len(tail)) + tail
	}
	return candidate
}

func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if toolNameRune(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return strings.Trim(b.String(), "_")
}

func toolNameRune(r rune) bool {
	return r <= unicodeMaxASCII && (r == '_' || r == '-' ||
		(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
}

const unicodeMaxASCII = 127

func normalizeToolDescription(description, original string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		description = "Tool: " + strings.TrimSpace(original)
	}
	if description == "" || description == "Tool:" {
		description = "Tool"
	}
	return trimRunes(description, toolDescriptionLimit)
}

// normalizeToolSchema repairs only the root object shape. Nested values stay
// raw so large numbers and non-standard tokens are not rewritten. Nested
// additionalProperties is kept because no local evidence requires stripping it.
func normalizeToolSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(emptySchema)
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return json.RawMessage(emptySchema)
	}
	typeRaw, _ := json.Marshal("object")
	schema["type"] = typeRaw
	propsRaw, ok := schema["properties"]
	props := map[string]json.RawMessage{}
	if !ok || json.Unmarshal(propsRaw, &props) != nil || props == nil {
		props = map[string]json.RawMessage{}
		empty, _ := json.Marshal(props)
		schema["properties"] = empty
	}
	if requiredRaw, ok := schema["required"]; ok {
		var required []json.RawMessage
		if json.Unmarshal(requiredRaw, &required) == nil {
			seen := map[string]bool{}
			filtered := make([]json.RawMessage, 0, len(required))
			for _, item := range required {
				var name string
				if json.Unmarshal(item, &name) != nil || seen[name] {
					continue
				}
				if _, exists := props[name]; !exists {
					continue
				}
				seen[name] = true
				filtered = append(filtered, item)
			}
			if len(filtered) == 0 {
				delete(schema, "required")
			} else {
				out, err := json.Marshal(filtered)
				if err == nil {
					schema["required"] = out
				}
			}
		}
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(emptySchema)
	}
	return out
}

func trimRunes(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	count := 0
	for i := range value {
		if count == limit {
			return value[:i]
		}
		count++
	}
	return value
}

func base36(n int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n < 0 {
		n = 0
	}
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%len(digits)]
		n /= len(digits)
	}
	return string(b[i:])
}

// resolveWireName maps one upstream tool name back to the exact client name.
// ok is false for names that are not in this request's catalog.
func (c toolCatalog) resolveWireName(wire string) (string, bool) {
	original, ok := c.toClient[wire]
	return original, ok
}
