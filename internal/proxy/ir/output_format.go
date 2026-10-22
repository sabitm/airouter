package ir

import (
	"bytes"
	"encoding/json"
	"strings"
)

// OutputFormatType is a portable structured-output kind. Unknown wire tags keep
// their literal value and are marked invalid, so a translated request fails
// instead of silently losing the caller's requirement.
type OutputFormatType string

const (
	OutputFormatJSONSchema OutputFormatType = "json_schema"
	OutputFormatJSONObject OutputFormatType = "json_object"
	OutputFormatText       OutputFormatType = "text"
)

// OutputFormat is the portable subset shared by OpenAI Chat response_format and
// OpenAI Responses text.format. Schema is raw JSON so numeric tokens are never
// coerced through float64. Description and Strict keep omission distinct from an
// explicit empty string or false, and StrictNull keeps an explicit strict:null
// distinct from omission (official OpenAI SDK request types type strict as
// Optional[bool], so callers emit null; it re-encodes as null between OpenAI
// formats and maps like omission onto Anthropic). Source is the ingress codec
// id; a same-id passthrough can forward a shape this IR does not model.
type OutputFormat struct {
	Type        OutputFormatType
	Name        string
	Description *string
	Schema      json.RawMessage
	Strict      *bool
	// StrictNull marks an explicit strict:null member. Strict stays nil in that
	// state; both set at once is an invalid construction rejected by Validate.
	StrictNull bool
	Source     string
	// InvalidReason is a payload-free reason for an unknown tag or malformed
	// recognized field. Decoders retain the intent instead of failing the whole
	// request, because same-codec passthrough must remain unchanged.
	InvalidReason string
}

// DecodeChatResponseFormat parses an OpenAI Chat response_format value. The
// json_schema object is nested under json_schema. Null and absent mean no
// structured-output intent.
func DecodeChatResponseFormat(raw json.RawMessage, source string) *OutputFormat {
	return decodeOpenAIOutputFormat(raw, true, source)
}

// DecodeResponsesFormat parses an OpenAI Responses text.format value. Its
// json_schema fields are flat on the format object. Null and absent mean no
// structured-output intent.
func DecodeResponsesFormat(raw json.RawMessage, source string) *OutputFormat {
	return decodeOpenAIOutputFormat(raw, false, source)
}

func decodeOpenAIOutputFormat(raw json.RawMessage, nested bool, source string) *OutputFormat {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	out := &OutputFormat{Source: source}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		out.InvalidReason = "output format must be an object"
		return out
	}

	var outer struct {
		Type       json.RawMessage `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if json.Unmarshal(trimmed, &outer) != nil {
		out.InvalidReason = "output format must be an object"
		return out
	}
	if len(bytes.TrimSpace(outer.Type)) == 0 {
		out.InvalidReason = "output format type is required"
		return out
	}
	var kind string
	if json.Unmarshal(outer.Type, &kind) != nil || strings.TrimSpace(kind) == "" {
		out.InvalidReason = "output format type must be a non-empty string"
		return out
	}
	out.Type = OutputFormatType(kind)

	format := trimmed
	if nested && out.Type == OutputFormatJSONSchema {
		format = bytes.TrimSpace(outer.JSONSchema)
		if len(format) == 0 || bytes.Equal(format, []byte("null")) {
			out.InvalidReason = "json_schema output format must be an object"
			return out
		}
		if format[0] != '{' || !json.Valid(format) {
			out.InvalidReason = "json_schema output format must be an object"
			return out
		}
	}

	var fields struct {
		Name        json.RawMessage `json:"name"`
		Description json.RawMessage `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Strict      json.RawMessage `json:"strict"`
	}
	if json.Unmarshal(format, &fields) != nil {
		out.InvalidReason = "output format must be an object"
		return out
	}

	switch out.Type {
	case OutputFormatJSONSchema:
		if err := decodeOutputFormatName(fields.Name, out); err != "" {
			out.InvalidReason = err
			return out
		}
		if len(fields.Description) > 0 {
			if bytes.Equal(bytes.TrimSpace(fields.Description), []byte("null")) {
				out.InvalidReason = "output format description must be a string"
				return out
			}
			var description string
			if json.Unmarshal(fields.Description, &description) != nil {
				out.InvalidReason = "output format description must be a string"
				return out
			}
			out.Description = &description
		}
		schema := bytes.TrimSpace(fields.Schema)
		if len(schema) == 0 {
			out.InvalidReason = "json_schema output format schema is required"
			return out
		}
		if bytes.Equal(schema, []byte("null")) || schema[0] != '{' || !json.Valid(schema) {
			out.InvalidReason = "json_schema output format schema must be an object"
			return out
		}
		out.Schema = append(json.RawMessage(nil), schema...)
		if len(fields.Strict) > 0 {
			if bytes.Equal(bytes.TrimSpace(fields.Strict), []byte("null")) {
				// Explicit null is a distinct accepted state, not a malformed bool:
				// OpenAI request types allow Optional[bool] null.
				out.StrictNull = true
			} else {
				var strict bool
				if json.Unmarshal(fields.Strict, &strict) != nil {
					out.InvalidReason = "output format strict must be a boolean"
					return out
				}
				out.Strict = &strict
			}
		}
	case OutputFormatJSONObject, OutputFormatText:
		if nested && len(bytes.TrimSpace(outer.JSONSchema)) > 0 {
			out.InvalidReason = string(out.Type) + " output format cannot carry json_schema fields"
			return out
		}
		if len(fields.Name) > 0 || len(fields.Description) > 0 || len(fields.Schema) > 0 || len(fields.Strict) > 0 {
			out.InvalidReason = string(out.Type) + " output format cannot carry json_schema fields"
			return out
		}
	default:
		out.InvalidReason = "output format type is not supported"
	}
	return out
}

func decodeOutputFormatName(raw json.RawMessage, out *OutputFormat) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "json_schema output format name is required"
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return "output format name must be a non-empty string"
	}
	if json.Unmarshal(trimmed, &out.Name) != nil || strings.TrimSpace(out.Name) == "" {
		return "output format name must be a non-empty string"
	}
	return ""
}

// Validate reports whether a portable format is representable by an OpenAI
// structured-output wire. It is payload-free so it can drive a client error.
func (f *OutputFormat) Validate() error {
	if f == nil {
		return nil
	}
	if f.InvalidReason != "" {
		return InvalidRequest(f.InvalidReason)
	}
	switch f.Type {
	case OutputFormatJSONSchema:
		if f.Strict != nil && f.StrictNull {
			return InvalidRequest("output format strict cannot be both a boolean and null")
		}
		if strings.TrimSpace(f.Name) == "" {
			return InvalidRequest("json_schema output format name is required")
		}
		schema := bytes.TrimSpace(f.Schema)
		if len(schema) == 0 {
			return InvalidRequest("json_schema output format schema is required")
		}
		if bytes.Equal(schema, []byte("null")) || schema[0] != '{' || !json.Valid(schema) {
			return InvalidRequest("json_schema output format schema must be an object")
		}
	case OutputFormatJSONObject:
		if f.Name != "" || f.Description != nil || len(f.Schema) > 0 || f.Strict != nil || f.StrictNull {
			return InvalidRequest("json_object output format cannot carry json_schema fields")
		}
	case OutputFormatText:
		if f.Name != "" || f.Description != nil || len(f.Schema) > 0 || f.Strict != nil || f.StrictNull {
			return InvalidRequest("text output format cannot carry json_schema fields")
		}
	default:
		return InvalidRequest("output format type is not supported")
	}
	return nil
}

// EffectivePortableOutputFormat returns the portable format and rejects a
// conflicting legacy Anthropic output_config.format. A legacy-only request
// returns nil so the existing Anthropic-owned mapping remains unchanged.
func (r *Request) EffectivePortableOutputFormat() (*OutputFormat, error) {
	if r == nil || r.OutputFormat == nil {
		return nil, nil
	}
	if r.Anthropic != nil && len(r.Anthropic.OutputFormat) > 0 {
		if !outputFormatMatchesLegacy(r.OutputFormat, r.Anthropic.OutputFormat) {
			return nil, InvalidRequest("output format conflicts with legacy output_config.format")
		}
	}
	return r.OutputFormat, nil
}

func outputFormatMatchesLegacy(portable *OutputFormat, legacy json.RawMessage) bool {
	var wire struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	if json.Unmarshal(legacy, &wire) != nil || wire.Type != string(portable.Type) {
		return false
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(legacy, &members) != nil {
		return false
	}
	switch portable.Type {
	case OutputFormatJSONSchema:
		if len(members) != 2 {
			return false
		}
		if portable.Strict != nil && !*portable.Strict {
			// A legacy Anthropic format can only express enforced json_schema.
			return false
		}
		return sameJSONValue(portable.Schema, wire.Schema)
	case OutputFormatJSONObject, OutputFormatText:
		return len(members) == 1
	default:
		return false
	}
}

func sameJSONValue(a, b json.RawMessage) bool {
	ca, err := compactJSON(a)
	if err != nil {
		return false
	}
	cb, err := compactJSON(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

func compactJSON(raw json.RawMessage) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
