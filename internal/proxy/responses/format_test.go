package responses

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const finding6ResponsesSchema = `{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993},"f":{"const":1.2300},"e":{"const":1e400}},"required":["n"],"additionalProperties":false}`

func compactFormatJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatalf("compact JSON: %v", err)
	}
	return out.String()
}

func TestFinding6ResponsesTextFormatRoundTrip(t *testing.T) {
	// wantRawStrict asserts the literal raw member (key presence) because *bool
	// decoding collapses explicit null and omission.
	cases := []struct {
		name          string
		strict        string
		wantRawStrict string // "" = member must be absent
	}{
		{name: "omitted"},
		{name: "false", strict: `,"strict":false`, wantRawStrict: "false"},
		{name: "true", strict: `,"strict":true`, wantRawStrict: "true"},
		{name: "null", strict: `,"strict":null`, wantRawStrict: "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","text":{"format":{"type":"json_schema","name":"Out","description":"result","schema":` + finding6ResponsesSchema + tc.strict + `},"verbosity":"low"},"input":"hi"}`
			req, err := DecodeRequest([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if req.OutputFormat == nil || req.OutputFormat.Type != "json_schema" || req.OutputFormat.Name != "Out" {
				t.Fatalf("format = %+v", req.OutputFormat)
			}
			if req.OutputFormat.Description == nil || *req.OutputFormat.Description != "result" {
				t.Fatalf("description = %+v", req.OutputFormat.Description)
			}
			switch tc.wantRawStrict {
			case "":
				if req.OutputFormat.Strict != nil || req.OutputFormat.StrictNull {
					t.Fatalf("omitted strict decoded as %v/null=%v", req.OutputFormat.Strict, req.OutputFormat.StrictNull)
				}
			case "null":
				if req.OutputFormat.Strict != nil || !req.OutputFormat.StrictNull {
					t.Fatalf("null strict decoded as %v/null=%v", req.OutputFormat.Strict, req.OutputFormat.StrictNull)
				}
			default:
				if req.OutputFormat.Strict == nil || *req.OutputFormat.Strict != (tc.wantRawStrict == "true") {
					t.Fatalf("strict = %v, want %s", req.OutputFormat.Strict, tc.wantRawStrict)
				}
			}

			out, err := EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			// Re-extract the format object as a raw member map so explicit null
			// survives as a present key with a null value.
			var textRaw struct {
				Text struct {
					Format map[string]json.RawMessage `json:"format"`
				} `json:"text"`
			}
			if err := json.Unmarshal(out, &textRaw); err != nil {
				t.Fatal(err)
			}
			got := textRaw.Text.Format
			if string(got["type"]) != "\"json_schema\"" || got["name"] == nil || got["description"] == nil || string(got["description"]) != "\"result\"" {
				t.Fatalf("text.format = %+v; body=%s", got, out)
			}
			rawStrict, present := got["strict"]
			if tc.wantRawStrict == "" {
				if present {
					t.Fatalf("encoded strict member present for omitted input: %s", rawStrict)
				}
			} else {
				if !present || compactFormatJSON(t, rawStrict) != tc.wantRawStrict {
					t.Fatalf("encoded strict = %q (present=%v), want %s; body=%s", rawStrict, present, tc.wantRawStrict, out)
				}
			}
			if compactFormatJSON(t, got["schema"]) != compactFormatJSON(t, []byte(finding6ResponsesSchema)) {
				t.Fatalf("schema changed: %s", got["schema"])
			}
			if tc.wantRawStrict == "" && strings.Contains(string(out), `"strict":`) {
				t.Fatalf("strict written for omitted input: %s", out)
			}
		})
	}
}

func TestFinding6ResponsesJSONObjectAndInvalidIntent(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","text":{"format":{"type":"json_object"}},"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.OutputFormat == nil || req.OutputFormat.Type != "json_object" {
		t.Fatalf("format = %+v", req.OutputFormat)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"text":{"format":{"type":"json_object"}}`) {
		t.Fatalf("json_object lost: %s", out)
	}

	for name, text := range map[string]string{
		"unknown":          `{"format":{"type":"regex"}}`,
		"malformed schema": `{"format":{"type":"json_schema","name":"Out","schema":[]}}`,
		"malformed text":   `"not-an-object"`,
		"strict-string":    `{"format":{"type":"json_schema","name":"Out","schema":{"type":"object"},"strict":"yes"}}`,
		"strict-number":    `{"format":{"type":"json_schema","name":"Out","schema":{"type":"object"},"strict":0}}`,
		"strict-array":     `{"format":{"type":"json_schema","name":"Out","schema":{"type":"object"},"strict":[]}}`,
		"strict-object":    `{"format":{"type":"json_schema","name":"Out","schema":{"type":"object"},"strict":{}}}`,
		"jo-null-strict":   `{"format":{"type":"json_object","strict":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"m","text":` + text + `,"input":"hi"}`
			req, err := DecodeRequest([]byte(body))
			if err != nil {
				t.Fatalf("decode rejected natively forwarded shape: %v", err)
			}
			if req.OutputFormat == nil || req.OutputFormat.InvalidReason == "" {
				t.Fatalf("invalid intent vanished: %+v", req.OutputFormat)
			}
			if _, err := EncodeRequest(req); err == nil {
				t.Fatal("translated encoder accepted invalid format intent")
			}
		})
	}
}

func TestFinding6ResponsesTextWithoutFormatUnchanged(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","text":{"verbosity":"low"},"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.OutputFormat != nil {
		t.Fatalf("verbosity became format intent: %+v", req.OutputFormat)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["text"]; ok {
		t.Fatalf("encoder invented text config: %s", out)
	}
}
