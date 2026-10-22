package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const finding6Schema = `{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993},"f":{"const":1.2300},"e":{"const":1e400}},"required":["n"],"additionalProperties":false}`

func compactRaw(t *testing.T, raw []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatalf("compact JSON: %v", err)
	}
	return out.String()
}

func TestFinding6ChatResponseFormatRoundTrip(t *testing.T) {
	// wantRawStrict asserts the literal raw member in the re-encoded JSON (key
	// presence), because Go *bool decoding collapses explicit null and omission.
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
			body := `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","schema":` + finding6Schema + tc.strict + `}},"messages":[{"role":"user","content":"hi"}]}`
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
			var doc struct {
				ResponseFormat struct {
					Type       string                     `json:"type"`
					JSONSchema map[string]json.RawMessage `json:"json_schema"`
				} `json:"response_format"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			got := doc.ResponseFormat
			if got.Type != "json_schema" || got.JSONSchema["name"] == nil || got.JSONSchema["description"] == nil || string(got.JSONSchema["description"]) != "\"result\"" {
				t.Fatalf("response_format = %+v; body=%s", got, out)
			}
			rawStrict, present := got.JSONSchema["strict"]
			if tc.wantRawStrict == "" {
				if present {
					t.Fatalf("encoded strict member present for omitted input: %s", rawStrict)
				}
			} else {
				if !present || compactRaw(t, rawStrict) != tc.wantRawStrict {
					t.Fatalf("encoded strict = %q (present=%v), want %s; body=%s", rawStrict, present, tc.wantRawStrict, out)
				}
			}
			if compactRaw(t, got.JSONSchema["schema"]) != compactRaw(t, []byte(finding6Schema)) {
				t.Fatalf("schema changed: %s", got.JSONSchema["schema"])
			}
			if tc.wantRawStrict == "" && strings.Contains(string(out), `"strict":`) {
				t.Fatalf("strict written for omitted input: %s", out)
			}
		})
	}
}

func TestFinding6ChatJSONObjectAndUnsupportedIntent(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`))
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
	if !strings.Contains(string(out), `"response_format":{"type":"json_object"}`) {
		t.Fatalf("json_object lost: %s", out)
	}

	for name, raw := range map[string]string{
		"unknown":     `{"type":"regex","pattern":"x"}`,
		"malformed":   `{"type":"json_schema","json_schema":{"name":"Out","schema":null}}`,
		"notobject":   `"json_object"`,
		"strict-str":  `{"type":"json_schema","json_schema":{"name":"Out","schema":{"type":"object"},"strict":"yes"}}`,
		"strict-num":  `{"type":"json_schema","json_schema":{"name":"Out","schema":{"type":"object"},"strict":1}}`,
		"strict-arr":  `{"type":"json_schema","json_schema":{"name":"Out","schema":{"type":"object"},"strict":[]}}`,
		"strict-obj":  `{"type":"json_schema","json_schema":{"name":"Out","schema":{"type":"object"},"strict":{}}}`,
		"jo-strict":   `{"type":"json_object","strict":null}`,
		"text-strict": `{"type":"text","strict":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"m","response_format":` + raw + `,"messages":[{"role":"user","content":"hi"}]}`
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

func TestFinding6FormatAbsentUnchanged(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.OutputFormat != nil {
		t.Fatalf("unexpected format: %+v", req.OutputFormat)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "response_format") {
		t.Fatalf("encoder invented response_format: %s", out)
	}
}
