package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

type claudeRequestParityFixture struct {
	CLIVersion    string                     `json:"cli_version"`
	Method        string                     `json:"method"`
	URL           string                     `json:"url"`
	Headers       map[string]string          `json:"headers"`
	AbsentHeaders []string                   `json:"absent_headers"`
	RequiredBetas []string                   `json:"required_betas"`
	MatchBody     map[string]json.RawMessage `json:"match_body"`
	Cases         []claudeRequestParityCase  `json:"cases"`
}

type claudeRequestParityCase struct {
	Name        string                             `json:"name"`
	Input       json.RawMessage                    `json:"input"`
	MatchBody   map[string]json.RawMessage         `json:"match_body"`
	Differences map[string]claudeRequestParityDiff `json:"differences"`
}

type claudeRequestParityDiff struct {
	CLI      json.RawMessage `json:"cli"`
	Airouter json.RawMessage `json:"airouter"`
}

func TestClaudeCodeRequestParity2318(t *testing.T) {
	data, err := os.ReadFile("testdata/claude_code_request_parity_2_1_318.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture claudeRequestParityFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.CLIVersion != "2.1.318" || len(fixture.Cases) == 0 {
		t.Fatalf("invalid request comparison fixture: version=%q cases=%d", fixture.CLIVersion, len(fixture.Cases))
	}
	const (
		access  = "request-parity-test-token"
		account = "12345678-1111-2222-3333-123456789abc"
		baseURL = "https://api.anthropic.com/v1"
	)
	device := strings.Repeat("ab", 32)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			input := decodeParityJSON(t, tc.Input)
			stream, _ := parityJSONPath(input, "stream")
			streaming := stream == true
			type capturedRequest struct {
				method, url string
				header      http.Header
				body        []byte
				length      int64
			}
			captures := make(chan capturedRequest, 1)
			var calls atomic.Int32
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  baseURL,
				model:    "claude-sonnet-4-6",
				oauth: &domain.OAuthCreds{
					AccessToken: access, ClaudeScopes: "user:inference",
					ClaudeAccountUUID: account, ClaudeDeviceID: device,
					ExpiresAt: time.Now().Add(time.Hour).Unix(),
				},
			}}, func(req *http.Request, body []byte) *http.Response {
				if calls.Add(1) == 1 {
					captures <- capturedRequest{req.Method, req.URL.String(), req.Header.Clone(), body, req.ContentLength}
				}
				if streaming {
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionTextSSE("ok", "end_turn", true), nil)
				}
				return completionHTTPResponse(req, http.StatusOK, "application/json", completionUnaryBody(), nil)
			})
			resp, _ := postSelector(t, client, base+"/v1/messages", token, string(tc.Input), nil)
			if resp.StatusCode != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("status=%d upstream requests=%d, want 200 and one send", resp.StatusCode, calls.Load())
			}
			// The callback publishes before returning the simulated response.
			var wire capturedRequest
			select {
			case wire = <-captures:
			default:
				t.Fatal("missing outbound request")
			}
			if wire.method != fixture.Method || wire.url != fixture.URL {
				t.Errorf("outbound target = %s %s, want %s %s", wire.method, wire.url, fixture.Method, fixture.URL)
			}
			if wire.length != int64(len(wire.body)) {
				t.Errorf("outbound content length = %d, want %d", wire.length, len(wire.body))
			}
			for name, want := range fixture.Headers {
				assertParityHeader(t, wire.header, name, want)
			}
			for _, name := range fixture.AbsentHeaders {
				if len(wire.header.Values(name)) != 0 {
					t.Errorf("unexpected outbound header %s", name)
				}
			}
			assertParityHeader(t, wire.header, "Authorization", "Bearer "+access)
			betas := strings.Split(wire.header.Get("Anthropic-Beta"), ",")
			for _, want := range fixture.RequiredBetas {
				if !slices.ContainsFunc(betas, func(beta string) bool { return strings.TrimSpace(beta) == want }) {
					t.Errorf("missing required beta %q", want)
				}
			}

			assertParityIdentity(t, wire.header, wire.body, account, device)
			assertFinalCCH(t, wire.url, wire.body, true)
			body := decodeParityJSON(t, wire.body)
			for path, value := range fixture.MatchBody {
				assertParityBodyField(t, body, path, value)
			}
			for path, value := range tc.MatchBody {
				assertParityBodyField(t, body, path, value)
			}
			for path, diff := range tc.Differences {
				if parityFieldEqual(decodeParityJSON(t, diff.CLI), diff.CLI != nil, decodeParityJSON(t, diff.Airouter), diff.Airouter != nil) {
					t.Fatalf("fixture difference for %s has equal expectations", path)
				}
				assertParityBodyField(t, body, path, diff.Airouter)
				t.Logf("known request difference: %s: CLI=%s Airouter=%s", path, parityRawValue(diff.CLI), parityRawValue(diff.Airouter))
			}
		})
	}
}

func assertParityHeader(t *testing.T, header http.Header, name, want string) {
	t.Helper()
	values := header.Values(name)
	if len(values) != 1 || values[0] != want {
		t.Errorf("outbound header %s has incorrect value or multiplicity", name)
	}
}

func assertParityIdentity(t *testing.T, header http.Header, body []byte, account, device string) {
	t.Helper()
	var doc struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var user struct {
		Account string `json:"account_uuid"`
		Device  string `json:"device_id"`
		Session string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(doc.Metadata.UserID), &user); err != nil {
		t.Fatalf("metadata.user_id is not a JSON object in a string: %v", err)
	}
	if user.Account != account || user.Device != device {
		t.Error("metadata.user_id account or device does not match the synthetic OAuth identity")
	}
	if !paritySessionUUID.MatchString(user.Session) {
		t.Fatal("metadata.user_id session is not a UUID")
	}
	assertParityHeader(t, header, "X-Claude-Code-Session-Id", user.Session)
}

var paritySessionUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

func decodeParityJSON(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if raw == nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		t.Fatalf("expected one JSON value, got trailing data (error %v)", err)
	}
	return value
}

func parityJSONPath(value any, path string) (any, bool) {
	for key := range strings.SplitSeq(path, ".") {
		switch current := value.(type) {
		case map[string]any:
			next, ok := current[key]
			if !ok {
				return nil, false
			}
			value = next
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(current) {
				return nil, false
			}
			value = current[i]
		default:
			return nil, false
		}
	}
	return value, true
}

func parityFieldEqual(a any, aPresent bool, b any, bPresent bool) bool {
	return aPresent == bPresent && reflect.DeepEqual(a, b)
}

func assertParityBodyField(t *testing.T, body any, path string, raw json.RawMessage) {
	t.Helper()
	got, present := parityJSONPath(body, path)
	if !parityFieldEqual(got, present, decodeParityJSON(t, raw), raw != nil) {
		if !present {
			t.Errorf("body %s = <absent>, want %s", path, parityRawValue(raw))
			return
		}
		value, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		t.Errorf("body %s = %s, want %s", path, value, parityRawValue(raw))
	}
}

func parityRawValue(raw json.RawMessage) string {
	if raw == nil {
		return "<absent>"
	}
	return string(raw)
}
