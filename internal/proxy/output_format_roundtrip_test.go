package proxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/responses"
)

func TestFinding6OpenAIChatResponsesFormatRoundTrip(t *testing.T) {
	schema := `{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993},"f":{"const":1.2300},"e":{"const":1e400}},"required":["n"],"additionalProperties":false}`
	for _, strict := range []string{"omitted", "null", "false", "true"} {
		t.Run(strict, func(t *testing.T) {
			strictMember := ""
			switch strict {
			case "null":
				strictMember = `,"strict":null`
			case "false":
				strictMember = `,"strict":false`
			case "true":
				strictMember = `,"strict":true`
			}
			chatBody := `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","schema":` + schema + strictMember + `}},"messages":[{"role":"user","content":"hi"}]}`
			req, err := openai.DecodeRequest([]byte(chatBody))
			if err != nil {
				t.Fatal(err)
			}
			responsesBody, err := responses.EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			roundReq, err := responses.DecodeRequest(responsesBody)
			if err != nil {
				t.Fatal(err)
			}
			chatOut, err := openai.EncodeRequest(roundReq)
			if err != nil {
				t.Fatal(err)
			}

			var doc struct {
				ResponseFormat struct {
					Type       string                     `json:"type"`
					JSONSchema map[string]json.RawMessage `json:"json_schema"`
				} `json:"response_format"`
			}
			if err := json.Unmarshal(chatOut, &doc); err != nil {
				t.Fatal(err)
			}
			got := doc.ResponseFormat
			if got.Type != "json_schema" || got.JSONSchema["name"] == nil || got.JSONSchema["description"] == nil || string(got.JSONSchema["description"]) != "\"result\"" {
				t.Fatalf("round-trip format = %+v; body=%s", got, chatOut)
			}
			rawStrict, present := got.JSONSchema["strict"]
			switch strict {
			case "omitted":
				if present {
					t.Fatalf("round-trip strict member appeared for omitted input: %s", rawStrict)
				}
			case "null":
				if !present || string(rawStrict) != "null" {
					t.Fatalf("round-trip strict = %q (present=%v), want explicit null; body=%s", rawStrict, present, chatOut)
				}
			default:
				if !present || string(rawStrict) != strict {
					t.Fatalf("round-trip strict = %q (present=%v), want %s; body=%s", rawStrict, present, strict, chatOut)
				}
			}
			var a, b bytes.Buffer
			if json.Compact(&a, got.JSONSchema["schema"]) != nil || json.Compact(&b, []byte(schema)) != nil {
				t.Fatal("compact schema")
			}
			if a.String() != b.String() {
				t.Fatalf("schema changed across round trip: %s", got.JSONSchema["schema"])
			}
		})
	}
}

func TestFinding6OpenAIJSONObjectRoundTrip(t *testing.T) {
	req, err := openai.DecodeRequest([]byte(`{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	responsesBody, err := responses.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	roundReq, err := responses.DecodeRequest(responsesBody)
	if err != nil {
		t.Fatal(err)
	}
	chatOut, err := openai.EncodeRequest(roundReq)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ResponseFormat struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(chatOut, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ResponseFormat.Type != "json_object" {
		t.Fatalf("json_object changed: %s", chatOut)
	}
}
