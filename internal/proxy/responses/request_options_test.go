package responses

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// TestStoreIncludeIngressRoundTrip covers decode -> IR -> encode for the
// explicit store and include intent under both stream flags. Explicit values
// stay exact; absent and null stay absent; a non-nil include always encodes as
// an array, never null.
func TestStoreIncludeIngressRoundTrip(t *testing.T) {
	trueValue, falseValue := true, false
	cases := []struct {
		name           string
		members        string
		wantStore      *bool
		wantInclude    []string
		wantIncludeSet bool
	}{
		{name: "absent", members: ""},
		{name: "store null", members: `,"store":null`},
		{name: "include null", members: `,"include":null`},
		{name: "store false", members: `,"store":false`, wantStore: &falseValue},
		{name: "store true", members: `,"store":true`, wantStore: &trueValue},
		{name: "include empty", members: `,"include":[]`, wantIncludeSet: true, wantInclude: []string{}},
		{name: "include empty string", members: `,"include":[""]`, wantIncludeSet: true, wantInclude: []string{""}},
		{name: "include whitespace", members: `,"include":["  a\tb  ","\n"]`, wantIncludeSet: true, wantInclude: []string{"  a\tb  ", "\n"}},
		{name: "include ordered duplicates", members: `,"include":["a","b","a"]`, wantIncludeSet: true, wantInclude: []string{"a", "b", "a"}},
		{name: "include unicode escapes", members: `,"include":["k\u00e9y","omega\u03a9","sep\u2028x"]`, wantIncludeSet: true, wantInclude: []string{"kéy", "omegaΩ", "sep\u2028x"}},
		{name: "include unknown future", members: `,"include":["reasoning.encrypted_content","x.future.flag"]`, wantIncludeSet: true, wantInclude: []string{"reasoning.encrypted_content", "x.future.flag"}},
		{name: "combined", members: `,"store":true,"include":["one","two","one"]`, wantStore: &trueValue, wantIncludeSet: true, wantInclude: []string{"one", "two", "one"}},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"m","stream":%t,"input":"hi"%s}`, stream, tc.members)
				req, err := DecodeRequest([]byte(body))
				if err != nil {
					t.Fatal(err)
				}
				if (req.Store != nil) != (tc.wantStore != nil) {
					t.Fatalf("decoded store presence = %v, want %v", req.Store != nil, tc.wantStore != nil)
				}
				if req.Store != nil && *req.Store != *tc.wantStore {
					t.Fatalf("decoded store = %v, want %v", *req.Store, *tc.wantStore)
				}
				if (req.Include != nil) != tc.wantIncludeSet {
					t.Fatalf("decoded include presence = %v, want %v", req.Include != nil, tc.wantIncludeSet)
				}
				if req.Include != nil {
					if *req.Include == nil {
						t.Fatal("decoded include is a nil slice")
					}
					if !reflect.DeepEqual(*req.Include, tc.wantInclude) {
						t.Fatalf("decoded include = %#v, want %#v", *req.Include, tc.wantInclude)
					}
				}

				out, err := EncodeRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]json.RawMessage
				if err := json.Unmarshal(out, &got); err != nil {
					t.Fatal(err)
				}
				rawStore, hasStore := got["store"]
				if hasStore != (tc.wantStore != nil) {
					t.Fatalf("encoded store presence = %v, want %v; body=%s", hasStore, tc.wantStore != nil, out)
				}
				if hasStore {
					var b bool
					if err := json.Unmarshal(rawStore, &b); err != nil {
						t.Fatalf("encoded store is not a bool: %s", rawStore)
					}
					if b != *tc.wantStore {
						t.Fatalf("encoded store = %v, want %v", b, *tc.wantStore)
					}
				}
				rawInclude, hasInclude := got["include"]
				if hasInclude != tc.wantIncludeSet {
					t.Fatalf("encoded include presence = %v, want %v; body=%s", hasInclude, tc.wantIncludeSet, out)
				}
				if hasInclude {
					if string(rawInclude) == "null" {
						t.Fatal("encoded include is null")
					}
					var values []string
					if err := json.Unmarshal(rawInclude, &values); err != nil {
						t.Fatalf("encoded include is not a string array: %s", rawInclude)
					}
					if values == nil {
						t.Fatal("encoded include decoded to a nil slice")
					}
					if !reflect.DeepEqual(values, tc.wantInclude) {
						t.Fatalf("encoded include = %#v, want %#v", values, tc.wantInclude)
					}
					if len(tc.wantInclude) == 0 && string(rawInclude) != "[]" {
						t.Fatalf("empty include encoded as %s, want []", rawInclude)
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

// Invalid store/include shapes must fail decode with a wrapped error that names
// the field or element index and never echoes caller content.
func TestStoreIncludeInvalidDecode(t *testing.T) {
	const sentinel = "sentinel-value-must-not-leak"
	cases := []struct {
		name      string
		member    string
		wantField string
		wantIndex int // -1 when no element index applies
	}{
		{name: "store string", member: `"store":"` + sentinel + `"`, wantField: "store", wantIndex: -1},
		{name: "store number", member: `"store":1`, wantField: "store", wantIndex: -1},
		{name: "store array", member: `"store":[]`, wantField: "store", wantIndex: -1},
		{name: "store object", member: `"store":{}`, wantField: "store", wantIndex: -1},
		{name: "include string", member: `"include":"` + sentinel + `"`, wantField: "include", wantIndex: -1},
		{name: "include number", member: `"include":1`, wantField: "include", wantIndex: -1},
		{name: "include bool", member: `"include":true`, wantField: "include", wantIndex: -1},
		{name: "include object", member: `"include":{}`, wantField: "include", wantIndex: -1},
		{name: "include null element", member: `"include":["ok",null]`, wantField: "include", wantIndex: 1},
		{name: "include number element", member: `"include":["ok",1]`, wantField: "include", wantIndex: 1},
		{name: "include bool element", member: `"include":["ok",true]`, wantField: "include", wantIndex: 1},
		{name: "include array element", member: `"include":["ok",["x"]]`, wantField: "include", wantIndex: 1},
		{name: "include object element", member: `"include":["ok",{"x":"` + sentinel + `"}]`, wantField: "include", wantIndex: 1},
		{name: "include mixed ok then invalid", member: `"include":["a",0,"b"]`, wantField: "include", wantIndex: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","input":"hi",` + tc.member + `}`
			_, err := DecodeRequest([]byte(body))
			if err == nil {
				t.Fatalf("DecodeRequest(%s) succeeded, want a decode error", tc.member)
			}
			msg := err.Error()
			if !strings.Contains(msg, "responses: decode request:") {
				t.Fatalf("error lost the decode wrapper: %q", msg)
			}
			if !strings.Contains(msg, tc.wantField) {
				t.Fatalf("error %q does not name field %q", msg, tc.wantField)
			}
			if tc.wantIndex >= 0 && !strings.Contains(msg, fmt.Sprintf("include[%d]", tc.wantIndex)) {
				t.Fatalf("error %q does not name include[%d]", msg, tc.wantIndex)
			}
			if strings.Contains(msg, sentinel) {
				t.Fatalf("error leaked caller content: %q", msg)
			}
		})
	}
}

// Direct IR encoding honors presence, emits [] for a non-nil nil or empty
// include, and never mutates the caller's request.
func TestEncodeRequestStoreIncludeFromIR(t *testing.T) {
	trueValue, falseValue := true, false
	tests := []struct {
		name        string
		store       *bool
		include     *[]string
		wantStore   string // raw expected, "" means absent
		wantInclude string // raw expected, "" means absent
	}{
		{name: "nil", wantStore: "", wantInclude: ""},
		{name: "store true", store: &trueValue, wantStore: "true"},
		{name: "store false", store: &falseValue, wantStore: "false"},
		{name: "include empty", include: &[]string{}, wantInclude: "[]"},
		{name: "include values", include: &[]string{"a", "b", "a"}, wantInclude: `["a","b","a"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &ir.Request{Model: "m", Store: tc.store, Include: tc.include}
			out, err := EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			assertRawField(t, out, "store", tc.wantStore)
			assertRawField(t, out, "include", tc.wantInclude)
		})
	}

	// A non-nil pointer to a nil slice is an explicit empty list. Encoding must
	// normalize only a local copy and leave the IR untouched.
	var nilSlice []string
	req := &ir.Request{Model: "m", Include: &nilSlice}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	assertRawField(t, out, "include", "[]")
	if req.Include == nil || *req.Include != nil {
		t.Fatalf("encoding changed the nil include slice: %#v", req.Include)
	}

	// Encoding reads the live pointer values at call time; a later mutation is
	// reflected only by a later encode.
	include := []string{"kept"}
	store := true
	req = &ir.Request{Model: "m", Store: &store, Include: &include}
	first, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	include[0] = "changed"
	store = false
	var firstGot, secondGot map[string]json.RawMessage
	if err := json.Unmarshal(first, &firstGot); err != nil {
		t.Fatal(err)
	}
	if string(firstGot["include"]) != `["kept"]` || string(firstGot["store"]) != "true" {
		t.Fatalf("first encode = %s, want kept/true", first)
	}
	second, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &secondGot); err != nil {
		t.Fatal(err)
	}
	if string(secondGot["include"]) != `["changed"]` || string(secondGot["store"]) != "false" {
		t.Fatalf("second encode = %s, want changed/false", second)
	}
}

func assertRawField(t *testing.T, body []byte, key, want string) {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	raw, ok := got[key]
	if want == "" {
		if ok {
			t.Fatalf("%s present as %s, want absent; body=%s", key, raw, body)
		}
		return
	}
	if !ok {
		t.Fatalf("%s absent, want %s; body=%s", key, want, body)
	}
	if string(raw) != want {
		t.Fatalf("%s = %s, want %s", key, raw, want)
	}
}
