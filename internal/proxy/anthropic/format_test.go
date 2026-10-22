package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

const finding6AnthropicSchema = `{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993},"f":{"const":1.2300},"e":{"const":1e400}},"required":["n"],"additionalProperties":false}`

func compactAnthropicJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatalf("compact JSON: %v", err)
	}
	return out.String()
}

func finding6Request(format *ir.OutputFormat) *ir.Request {
	return &ir.Request{
		Model:        "m",
		MaxTokens:    8,
		Messages:     []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		OutputFormat: format,
	}
}

func TestFinding6PortableJSONSchemaToAnthropicWire(t *testing.T) {
	strict := true
	description := "result"
	cases := []struct {
		name   string
		format *ir.OutputFormat
	}{
		{name: "strict true", format: &ir.OutputFormat{Type: ir.OutputFormatJSONSchema, Name: "Out", Description: &description, Schema: json.RawMessage(finding6AnthropicSchema), Strict: &strict}},
		// Explicit null strict is unspecified, mapped like omission onto the
		// enforced slot with no strict member on the wire.
		{name: "strict null", format: &ir.OutputFormat{Type: ir.OutputFormatJSONSchema, Name: "Out", Description: &description, Schema: json.RawMessage(finding6AnthropicSchema), StrictNull: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := EncodeRequest(finding6Request(tc.format))
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				OutputConfig struct {
					Format map[string]json.RawMessage `json:"format"`
				} `json:"output_config"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			format := doc.OutputConfig.Format
			if string(format["type"]) != `"json_schema"` {
				t.Fatalf("format type = %s; body=%s", format["type"], out)
			}
			if compactAnthropicJSON(t, format["schema"]) != compactAnthropicJSON(t, []byte(finding6AnthropicSchema)) {
				t.Fatalf("schema changed: %s", format["schema"])
			}
			if format["name"] != nil || format["description"] != nil || format["strict"] != nil {
				t.Fatalf("invented Claude envelope fields: %s", out)
			}
		})
	}
}

func TestFinding6PortableFormatRejectedOnAnthropicWire(t *testing.T) {
	falseValue := false
	for name, format := range map[string]*ir.OutputFormat{
		"strict false": {Type: ir.OutputFormatJSONSchema, Name: "Out", Schema: json.RawMessage(`{"type":"object"}`), Strict: &falseValue},
		"json object":  {Type: ir.OutputFormatJSONObject},
		"unknown":      {Type: "regex", InvalidReason: "output format type is not supported"},
		"malformed":    {Type: ir.OutputFormatJSONSchema, Name: "Out", Schema: json.RawMessage(`null`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EncodeRequest(finding6Request(format)); err == nil {
				t.Fatal("unsupported portable format was accepted")
			}
		})
	}
}

func TestFinding6AnthropicLegacyOutputFormatUnchanged(t *testing.T) {
	legacy := json.RawMessage(`{"type":"json_schema","schema":{"type":"object","title":"Out"}}`)
	req := finding6Request(nil)
	req.Anthropic = &ir.AnthropicRequest{OutputFormat: legacy}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OutputConfig struct {
			Format json.RawMessage `json:"format"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc.OutputConfig.Format), `"title":"Out"`) {
		t.Fatalf("legacy format changed: %s", out)
	}
}

func TestFinding6PortableAndLegacyConflict(t *testing.T) {
	portable := &ir.OutputFormat{Type: ir.OutputFormatJSONSchema, Name: "Out", Schema: json.RawMessage(`{"type":"object","title":"New"}`)}
	req := finding6Request(portable)
	req.Anthropic = &ir.AnthropicRequest{OutputFormat: json.RawMessage(`{"type":"json_schema","schema":{"type":"object","title":"Old"}}`)}
	if _, err := EncodeRequest(req); err == nil {
		t.Fatal("conflicting portable and legacy formats were accepted")
	}
}
