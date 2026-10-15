package responses

import (
	"encoding/json"
	"fmt"
	"testing"

	"airouter/internal/proxy/ir"
)

// TestPromptCacheKeyIngressRoundTrip covers decode -> IR -> encode for the
// explicit key: ordinary, empty, whitespace, and escaped/Unicode strings are
// preserved exactly; absent and null stay absent. Stream false and true must
// not change the field handling. A missing key is never generated.
func TestPromptCacheKeyIngressRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		keyJSON string // raw JSON member, empty means absent
		present bool
		want    string
	}{
		{name: "ordinary", keyJSON: `,"prompt_cache_key":"abc-123"`, present: true, want: "abc-123"},
		{name: "empty", keyJSON: `,"prompt_cache_key":""`, present: true, want: ""},
		{name: "whitespace", keyJSON: `,"prompt_cache_key":"  key  "`, present: true, want: "  key  "},
		{name: "escaped unicode", keyJSON: `,"prompt_cache_key":"k\u00e9y-\u0031"`, present: true, want: "k\u00e9y-1"},
		{name: "missing", keyJSON: "", present: false},
		{name: "null", keyJSON: `,"prompt_cache_key":null`, present: false},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"m","stream":%t,"input":"hi"%s}`, stream, tc.keyJSON)
				req, err := DecodeRequest([]byte(body))
				if err != nil {
					t.Fatal(err)
				}
				if (req.PromptCacheKey != nil) != tc.present {
					t.Fatalf("decoded key presence = %v, want %v", req.PromptCacheKey != nil, tc.present)
				}
				if req.PromptCacheKey != nil && *req.PromptCacheKey != tc.want {
					t.Fatalf("decoded key = %q, want %q", *req.PromptCacheKey, tc.want)
				}
				out, err := EncodeRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]json.RawMessage
				if err := json.Unmarshal(out, &got); err != nil {
					t.Fatal(err)
				}
				rawKey, ok := got["prompt_cache_key"]
				if ok != tc.present {
					t.Fatalf("encoded key presence = %v, want %v; body=%s", ok, tc.present, out)
				}
				if ok {
					var s string
					if err := json.Unmarshal(rawKey, &s); err != nil {
						t.Fatalf("encoded key is not a string: %s", rawKey)
					}
					if s != tc.want {
						t.Fatalf("encoded key = %q, want %q", s, tc.want)
					}
				}
				var gotStream bool
				if err := json.Unmarshal(got["stream"], &gotStream); err != nil {
					t.Fatalf("stream field = %s: %v", got["stream"], err)
				}
				if gotStream != stream {
					t.Fatalf("stream = %v, want %v", gotStream, stream)
				}
			})
		}
	}
}

// Non-string values must fail decoding instead of being coerced or dropped.
func TestPromptCacheKeyInvalidTypesFailDecode(t *testing.T) {
	for _, member := range []string{
		`"prompt_cache_key":123`,
		`"prompt_cache_key":1.5`,
		`"prompt_cache_key":true`,
		`"prompt_cache_key":["a"]`,
		`"prompt_cache_key":{"a":1}`,
	} {
		body := `{"model":"m","input":"hi",` + member + `}`
		if _, err := DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("DecodeRequest(%s) succeeded, want a type error", member)
		}
	}
}

// Direct IR encoding keeps an explicit key, including the empty string, and
// never mutates the request. Nil means omit.
func TestEncodeRequestPromptCacheKeyFromIR(t *testing.T) {
	key := "ir-key-\u00e9"
	req := &ir.Request{Model: "m", PromptCacheKey: &key}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if req.PromptCacheKey == nil || *req.PromptCacheKey != key {
		t.Fatalf("encode mutated the IR key: %v", req.PromptCacheKey)
	}
	assertEncodedKey(t, out, true, key)

	empty := ""
	req = &ir.Request{Model: "m", PromptCacheKey: &empty}
	out, err = EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	assertEncodedKey(t, out, true, "")

	req = &ir.Request{Model: "m"}
	out, err = EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	assertEncodedKey(t, out, false, "")
}

func assertEncodedKey(t *testing.T, body []byte, present bool, want string) {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	raw, ok := got["prompt_cache_key"]
	if ok != present {
		t.Fatalf("key presence = %v, want %v; body=%s", ok, present, body)
	}
	if !ok {
		return
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("key is not a string: %s", raw)
	}
	if s != want {
		t.Fatalf("key = %q, want %q", s, want)
	}
}
