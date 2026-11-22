package proxy

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
)

func usageHTTPProxy(t *testing.T, targets []selectorTarget, respond func(*http.Request, []byte) *http.Response) (string, string, *Proxy, *http.Client) {
	t.Helper()
	tr := installSelectorTransport(t)
	base, token, px := newSelectorProxy(t, tr, targets...)
	ingress, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	original := tr.next
	tr.next = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == ingress.Host {
			return original.RoundTrip(req)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err := req.Body.Close(); err != nil {
			return nil, err
		}
		resp := respond(req, body)
		resp.Request = req
		return resp, nil
	})
	return base, token, px, selectorClient(tr)
}

func usageSSE(startUsage string, deltas []string, stop bool) string {
	frames := []string{
		completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_usage","type":"message","role":"assistant","model":"claude-sonnet-4-6","usage":`+startUsage+`}}`),
		completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"usage-ok"}}`),
		completionFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
	}
	if len(deltas) == 0 {
		frames = append(frames, completionFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
	}
	for i, usage := range deltas {
		reason := "null"
		if i == len(deltas)-1 {
			reason = `"end_turn"`
		}
		data := `{"type":"message_delta","delta":{"stop_reason":` + reason + `}`
		if usage != "" {
			data += `,"usage":` + usage
		}
		frames = append(frames, completionFrame("message_delta", data+`}`))
	}
	if stop {
		frames = append(frames, completionFrame("message_stop", `{"type":"message_stop"}`))
	}
	return completionSSE(frames...)
}

func usageNumber(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("number %s: %v", raw, err)
	}
	return n
}

func usageFrameData(frame string) string {
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "data:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return ""
}

func assertOptionalUsage(t *testing.T, usage map[string]json.RawMessage, key string, want int) {
	t.Helper()
	raw, ok := usage[key]
	if want == 0 {
		if ok {
			t.Fatalf("%s present in %v", key, usage)
		}
		return
	}
	if !ok || usageNumber(t, raw) != want {
		t.Fatalf("%s = %s, want %d", key, raw, want)
	}
}

func assertChatUsage(t *testing.T, body string, in, out, read, write int) {
	t.Helper()
	var got int
	for _, frame := range strings.Split(body, "\n\n") {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(frame), "data:"))
		if line == "" || line == "[DONE]" || !strings.Contains(line, `"usage"`) {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				TotalTokens         int `json:"total_tokens"`
				PromptTokensDetails *struct {
					CachedTokens     int `json:"cached_tokens"`
					CacheWriteTokens int `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &chunk) != nil || chunk.Usage == nil {
			continue
		}
		got++
		u := chunk.Usage
		if u.PromptTokens != in || u.CompletionTokens != out || u.TotalTokens != in+out {
			t.Fatalf("chat usage = %+v, want %d/%d", u, in, out)
		}
		gotRead, gotWrite := 0, 0
		if u.PromptTokensDetails != nil {
			gotRead, gotWrite = u.PromptTokensDetails.CachedTokens, u.PromptTokensDetails.CacheWriteTokens
		}
		if gotRead != read || gotWrite != write {
			t.Fatalf("chat cache = %d/%d, want %d/%d", gotRead, gotWrite, read, write)
		}
	}
	if got != 1 {
		t.Fatalf("chat usage chunks = %d body=%s", got, body)
	}
}

func assertResponsesUsage(t *testing.T, body string, in, out, read, write int) {
	t.Helper()
	var found int
	for _, frame := range strings.Split(body, "\n\n") {
		if !strings.Contains(frame, "response.completed") {
			continue
		}
		line := usageFrameData(frame)
		var ev struct {
			Response struct {
				Usage struct {
					InputTokens        int             `json:"input_tokens"`
					OutputTokens       int             `json:"output_tokens"`
					TotalTokens        int             `json:"total_tokens"`
					InputTokensDetails json.RawMessage `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		found++
		u := ev.Response.Usage
		if u.InputTokens != in || u.OutputTokens != out || u.TotalTokens != in+out {
			t.Fatalf("responses usage = %+v", u)
		}
		var details struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		}
		if len(u.InputTokensDetails) > 0 && json.Unmarshal(u.InputTokensDetails, &details) != nil {
			t.Fatalf("details %s", u.InputTokensDetails)
		}
		if details.CachedTokens != read || details.CacheWriteTokens != write {
			t.Fatalf("responses cache = %+v", details)
		}
	}
	if found != 1 {
		t.Fatalf("responses completed count = %d", found)
	}
}

type messagesUsagePartition struct {
	ordinary, read, write int
}

func assertMessagesUsage(t *testing.T, body string, start, finish messagesUsagePartition, out int) {
	t.Helper()
	var starts, deltas int
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.Contains(frame, "event: message_start") {
			var ev struct {
				Message struct {
					Usage map[string]json.RawMessage `json:"usage"`
				} `json:"message"`
			}
			data := usageFrameData(frame)
			if json.Unmarshal([]byte(data), &ev) != nil {
				t.Fatalf("start %s", data)
			}
			if usageNumber(t, ev.Message.Usage["input_tokens"]) != start.ordinary || usageNumber(t, ev.Message.Usage["output_tokens"]) != 0 {
				t.Fatalf("start usage = %s", data)
			}
			assertOptionalUsage(t, ev.Message.Usage, "cache_read_input_tokens", start.read)
			assertOptionalUsage(t, ev.Message.Usage, "cache_creation_input_tokens", start.write)
			starts++
		}
		if strings.Contains(frame, "event: message_delta") {
			var ev struct {
				Usage map[string]json.RawMessage `json:"usage"`
			}
			data := usageFrameData(frame)
			if json.Unmarshal([]byte(data), &ev) != nil {
				t.Fatalf("delta %s", data)
			}
			if usageNumber(t, ev.Usage["output_tokens"]) != out {
				t.Fatalf("delta output = %s", data)
			}
			for _, field := range []struct {
				key           string
				before, after int
			}{
				{"input_tokens", start.ordinary, finish.ordinary},
				{"cache_read_input_tokens", start.read, finish.read},
				{"cache_creation_input_tokens", start.write, finish.write},
			} {
				raw, present := ev.Usage[field.key]
				if field.before == field.after {
					if present {
						t.Fatalf("unchanged terminal %s present: %s", field.key, data)
					}
				} else if !present || usageNumber(t, raw) != field.after {
					t.Fatalf("terminal %s = %s, want %d", field.key, raw, field.after)
				}
			}
			deltas++
		}
	}
	if starts != 1 || deltas != 1 {
		t.Fatalf("messages starts=%d deltas=%d: %s", starts, deltas, body)
	}
}

func assertUsageWire(t *testing.T, ingress, body string, in, out, read, write int) {
	t.Helper()
	switch ingress {
	case "chat":
		assertChatUsage(t, body, in, out, read, write)
	case "responses":
		assertResponsesUsage(t, body, in, out, read, write)
	case "messages":
		part := messagesUsagePartition{in - read - write, read, write}
		assertMessagesUsage(t, body, part, part, out)
	default:
		t.Fatalf("unknown ingress %s", ingress)
	}
}

func TestClaudeCodeStreamUsageHTTP(t *testing.T) {
	cases := []struct {
		name, start string
		deltas      []string
		in, out     int
		read, write int
		estimated   bool
		startPart   *messagesUsagePartition
	}{
		{
			name: "late-partitions", start: `{"input_tokens":12,"cache_read_input_tokens":9,"cache_creation_input_tokens":8,"output_tokens":4}`,
			deltas: []string{
				`{"input_tokens":3,"cache_read_input_tokens":13,"cache_creation":{"ephemeral_5m_input_tokens":15}}`,
				`{"input_tokens":0,"output_tokens":null}`,
			},
			in: 31, out: 4, read: 13, write: 15, startPart: &messagesUsagePartition{12, 9, 8},
		},
		{name: "large-cached", start: `{"input_tokens":200000,"cache_read_input_tokens":2000000,"cache_creation_input_tokens":400000,"output_tokens":1}`, deltas: []string{`{"output_tokens":6}`}, in: 2600000, out: 6, read: 2000000, write: 400000},
		{name: "explicit-zero", start: `{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":0}`, deltas: []string{`{"output_tokens":0}`}},
		{name: "partial-output", start: `{"output_tokens":5}`, deltas: []string{`{"output_tokens":9}`}, out: 9},
		{name: "absent", start: `null`, deltas: []string{``}, estimated: true},
	}
	for _, stop := range []bool{false, true} {
		for _, tc := range cases {
			for _, ingress := range completionIngresses(true) {
				t.Run(tc.name+"/"+ingress.name+"/stop="+strconv.FormatBool(stop), func(t *testing.T) {
					base, token, px, client := usageHTTPProxy(t, []selectorTarget{{
						protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-primary.invalid/v1", model: "claude-sonnet-4-6",
					}}, func(req *http.Request, _ []byte) *http.Response {
						return completionHTTPResponse(req, http.StatusOK, "text/event-stream", usageSSE(tc.start, tc.deltas, stop), nil)
					})
					resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
					body := string(out)
					if resp.StatusCode != http.StatusOK || !strings.Contains(body, "usage-ok") {
						t.Fatalf("status %d: %s", resp.StatusCode, body)
					}
					assertCompletionSuccess(t, ingress.name, body)
					wantIn, wantOut, wantRead, wantWrite := tc.in, tc.out, tc.read, tc.write
					if tc.estimated {
						wantIn, wantOut, wantRead, wantWrite = 0, 0, 0, 0
					}
					if ingress.name == "messages" && tc.startPart != nil {
						finish := messagesUsagePartition{wantIn - wantRead - wantWrite, wantRead, wantWrite}
						assertMessagesUsage(t, body, *tc.startPart, finish, wantOut)
					} else {
						assertUsageWire(t, ingress.name, body, wantIn, wantOut, wantRead, wantWrite)
					}
					row := waitForLogs(t, px.store, 1)[0]
					if tc.estimated {
						if !row.UsageEstimated || row.InputTokens == 0 || row.OutputTokens == 0 {
							t.Fatalf("absent log = %+v", row)
						}
						return
					}
					if row.UsageEstimated || row.InputTokens != tc.in || row.OutputTokens != tc.out {
						t.Fatalf("log = %+v, want %d/%d", row, tc.in, tc.out)
					}
				})
			}
		}
	}
}

func TestClaudeCodeStreamUsageLogAggregateOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("requires 64-bit stream counters")
	}
	var hits atomic.Int32
	base, token, px, client := usageHTTPProxy(t, []selectorTarget{{
		protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-primary.invalid/v1", model: "claude-sonnet-4-6",
	}}, func(req *http.Request, _ []byte) *http.Response {
		usage := `{"input_tokens":1,"output_tokens":2}`
		if hits.Add(1) == 1 {
			usage = `{"input_tokens":` + strconv.Itoa(math.MaxInt) + `}`
		}
		return completionHTTPResponse(req, http.StatusOK, "text/event-stream", usageSSE(usage, nil, true), nil)
	})
	ingress := completionIngresses(true)[0]
	for i, want := range [][2]int{{math.MaxInt, 0}, {1, 2}} {
		resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d: %s", i, resp.StatusCode, out)
		}
		assertCompletionSuccess(t, ingress.name, string(out))
		assertUsageWire(t, ingress.name, string(out), want[0], want[1], 0, 0)
		row := waitForLogs(t, px.store, i+1)[0]
		if row.InputTokens != want[0] || row.OutputTokens != want[1] || row.UsageEstimated || row.ErrMsg != "" {
			t.Fatalf("request %d log = %+v, want %v", i, row, want)
		}
	}
	count, in, out, err := px.store.RequestLogStats(t.Context())
	if err != nil || count != 2 || in != math.MaxInt64 || out != 2 {
		t.Fatalf("stats = (%d,%d,%d), err=%v, want (2,%d,2)", count, in, out, err, int64(math.MaxInt64))
	}
	rows := waitForLogs(t, px.store, 2)
	if rows[1].InputTokens != math.MaxInt || rows[0].InputTokens != 1 || rows[0].OutputTokens != 2 {
		t.Fatalf("aggregate changed stored counts: older=%+v newest=%+v", rows[1], rows[0])
	}
}

func TestClaudeCodeStreamUsageUnaryControl(t *testing.T) {
	for _, ingress := range completionIngresses(false) {
		t.Run(ingress.name, func(t *testing.T) {
			base, token, px, client := usageHTTPProxy(t, []selectorTarget{{
				protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-unary.invalid/v1", model: "claude-sonnet-4-6",
			}}, func(req *http.Request, _ []byte) *http.Response {
				return completionHTTPResponse(req, http.StatusOK, "application/json", completionUnaryBody(), nil)
			})
			resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "unary-ok") {
				t.Fatalf("status %d: %s", resp.StatusCode, out)
			}
			row := waitForLogs(t, px.store, 1)[0]
			if row.Stream || row.UsageEstimated || row.InputTokens != 1 || row.OutputTokens != 1 {
				t.Fatalf("unary log = %+v", row)
			}
		})
	}
}

func TestClaudeCodeStreamUsageFailoverIsolation(t *testing.T) {
	backups := []struct {
		name, usage string
		in, out     int
		read        int
	}{
		{name: "explicit-zero", usage: `{"input_tokens":0,"output_tokens":0}`},
		{name: "present", usage: `{"input_tokens":7,"cache_read_input_tokens":2,"output_tokens":3}`, in: 9, out: 3, read: 2},
	}
	for _, backup := range backups {
		for _, ingress := range completionIngresses(true) {
			t.Run(backup.name+"/"+ingress.name, func(t *testing.T) {
				var primaryHits, backupHits atomic.Int32
				base, token, px, client := usageHTTPProxy(t, []selectorTarget{
					{protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-primary.invalid/v1", model: "claude-sonnet-4-6"},
					{protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-backup.invalid/v1", model: "claude-sonnet-4-6"},
				}, func(req *http.Request, _ []byte) *http.Response {
					if req.URL.Host == "usage-backup.invalid" {
						backupHits.Add(1)
						return completionHTTPResponse(req, http.StatusOK, "text/event-stream", usageSSE(backup.usage, []string{`{"output_tokens":` + strconv.Itoa(backup.out) + `}`}, true), nil)
					}
					primaryHits.Add(1)
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", completionSSE(completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_primary","model":"m","usage":{"input_tokens":11,"output_tokens":5}}}`)), nil)
				})
				resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
				body := string(out)
				if resp.StatusCode != http.StatusOK || primaryHits.Load() != 1 || backupHits.Load() != 1 || strings.Contains(body, "msg_primary") {
					t.Fatalf("status %d hits %d/%d: %s", resp.StatusCode, primaryHits.Load(), backupHits.Load(), body)
				}
				assertUsageWire(t, ingress.name, body, backup.in, backup.out, backup.read, 0)
				logs := waitForLogs(t, px.store, 2)
				failed := findLogByProvider(t, logs, "p0")
				winner := findLogByProvider(t, logs, "p1")
				if failed.InputTokens != 11 || failed.OutputTokens != 0 || failed.UsageEstimated || failed.ErrMsg == "" {
					t.Fatalf("failed log = %+v", failed)
				}
				if winner.InputTokens != backup.in || winner.OutputTokens != backup.out || winner.UsageEstimated || winner.ErrMsg != "" {
					t.Fatalf("winner log = %+v", winner)
				}
			})
		}
	}
}

func TestClaudeCodeStreamUsagePostCommitCorrupt(t *testing.T) {
	for _, ingress := range completionIngresses(true) {
		t.Run(ingress.name, func(t *testing.T) {
			var backupHits atomic.Int32
			primary := completionSSE(
				completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_usage","model":"m","usage":{"input_tokens":4,"output_tokens":1}}}`),
				completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"usage-ok"}}`),
				completionFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
				completionFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":-1,"note":"PAYLOAD_SECRET_USAGE"}}`),
			)
			base, token, px, client := usageHTTPProxy(t, []selectorTarget{
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-primary.invalid/v1", model: "claude-sonnet-4-6"},
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-backup.invalid/v1", model: "claude-sonnet-4-6"},
			}, func(req *http.Request, _ []byte) *http.Response {
				if req.URL.Host == "usage-backup.invalid" {
					backupHits.Add(1)
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", usageSSE(`{"input_tokens":99,"output_tokens":99}`, nil, true), nil)
				}
				return completionHTTPResponse(req, http.StatusOK, "text/event-stream", primary, nil)
			})
			resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
			body := string(out)
			if resp.StatusCode != http.StatusOK || backupHits.Load() != 0 || strings.Contains(body, "PAYLOAD_SECRET_USAGE") || !strings.Contains(body, "invalid usage counter") {
				t.Fatalf("status %d backup %d: %s", resp.StatusCode, backupHits.Load(), body)
			}
			assertNoCompletionTrailer(t, ingress.name, body)
			row := waitForLogs(t, px.store, 1)[0]
			if row.InputTokens != 4 || row.OutputTokens != 0 || row.UsageEstimated || row.ErrMsg == "" {
				t.Fatalf("corrupt log = %+v", row)
			}
		})
	}
}

func TestClaudeCodeStreamUsagePreCommitRetry(t *testing.T) {
	for _, ingress := range completionIngresses(true) {
		t.Run(ingress.name, func(t *testing.T) {
			var primaryHits, backupHits atomic.Int32
			base, token, px, client := usageHTTPProxy(t, []selectorTarget{
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-primary.invalid/v1", model: "claude-sonnet-4-6"},
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-backup.invalid/v1", model: "claude-sonnet-4-6"},
			}, func(req *http.Request, _ []byte) *http.Response {
				if req.URL.Host == "usage-backup.invalid" {
					backupHits.Add(1)
					return completionHTTPResponse(req, http.StatusOK, "text/event-stream", usageSSE(`{"input_tokens":0,"output_tokens":2}`, []string{`{"output_tokens":2}`}, false), nil)
				}
				primaryHits.Add(1)
				return completionHTTPResponse(req, http.StatusOK, "text/event-stream", usageSSE(`{"input_tokens":-3}`, nil, true), nil)
			})
			resp, out := postSelector(t, client, base+ingress.path, token, ingress.body, nil)
			body := string(out)
			if resp.StatusCode != http.StatusOK || primaryHits.Load() != 1 || backupHits.Load() != 1 || strings.Contains(body, "invalid usage counter") {
				t.Fatalf("status %d hits %d/%d: %s", resp.StatusCode, primaryHits.Load(), backupHits.Load(), body)
			}
			assertUsageWire(t, ingress.name, body, 0, 2, 0, 0)
			logs := waitForLogs(t, px.store, 2)
			failed := findLogByProvider(t, logs, "p0")
			winner := findLogByProvider(t, logs, "p1")
			if failed.InputTokens != 0 || failed.OutputTokens != 0 || failed.ErrMsg == "" || winner.InputTokens != 0 || winner.OutputTokens != 2 || winner.UsageEstimated {
				t.Fatalf("logs failed=%+v winner=%+v", failed, winner)
			}
		})
	}
}

func TestClaudeCodeStreamUsageDirectCollection(t *testing.T) {
	body := usageSSE(`{"input_tokens":4,"cache_read_input_tokens":2,"output_tokens":7}`, []string{`{"input_tokens":0,"cache_creation_input_tokens":3,"output_tokens":0}`}, false)
	resp, err := collectStreamResponse(strings.NewReader(body), claudeCodeCodec, nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Usage.UsageReported || resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 0 || resp.Usage.CacheReadTokens != 2 || resp.Usage.CacheWriteTokens != 3 {
		t.Fatalf("collected = %+v", resp.Usage)
	}
	generic, err := collectStreamResponse(strings.NewReader(body+completionFrame("message_stop", `{"type":"message_stop"}`)+"\n\n"), anthropicCodec, nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if generic.Usage.UsageReported || generic.Usage.InputTokens != 5 || generic.Usage.OutputTokens != 0 || generic.Usage.CacheReadTokens != 2 || generic.Usage.CacheWriteTokens != 3 {
		t.Fatalf("generic collection = %+v", generic.Usage)
	}
	truncated := completionSSE(
		completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_usage","model":"m","usage":{"input_tokens":8,"output_tokens":3}}}`),
		completionFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		completionFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"usage-ok"}}`),
		completionFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
	if _, err := collectStreamResponse(strings.NewReader(truncated), claudeCodeCodec, nil, "m", nil); err == nil || !strings.Contains(err.Error(), "stream ended before completion") {
		t.Fatalf("truncated err=%v", err)
	}
}

func TestClaudeCodeStreamUsageStrictLifecycleStillRequired(t *testing.T) {
	body := completionSSE(
		completionFrame("message_start", `{"type":"message_start","message":{"id":"msg_usage","model":"m","usage":{"input_tokens":8,"output_tokens":3}}}`),
		completionFrame("message_delta", `{"type":"message_delta","delta":{},"usage":{"output_tokens":9}}`),
	)
	base, token, px, client := usageHTTPProxy(t, []selectorTarget{{
		protocol: domain.ProtocolClaudeCode, baseURL: "https://usage-primary.invalid/v1", model: "claude-sonnet-4-6",
	}}, func(req *http.Request, _ []byte) *http.Response {
		return completionHTTPResponse(req, http.StatusOK, "text/event-stream", body, nil)
	})
	resp, out := postSelector(t, client, base+"/v1/messages", token, completionIngresses(true)[2].body, nil)
	if resp.StatusCode == http.StatusOK || !strings.Contains(string(out), "stream ended before completion") {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	row := waitForLogs(t, px.store, 1)[0]
	if row.InputTokens != 8 || row.OutputTokens != 0 || row.UsageEstimated || row.Status == http.StatusOK {
		t.Fatalf("truncated log = %+v", row)
	}
}
