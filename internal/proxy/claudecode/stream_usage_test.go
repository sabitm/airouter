package claudecode

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

func usageEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func usageStart(usage string) string {
	if usage == "" {
		return usageEvent("message_start", `{"type":"message_start","message":{"id":"msg_usage","model":"m"}}`)
	}
	return usageEvent("message_start", `{"type":"message_start","message":{"id":"msg_usage","model":"m","usage":`+usage+`}}`)
}

func usageClosed(start string) string {
	return start +
		usageEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		usageEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`) +
		usageEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)
}

func usageDelta(usage, reason string) string {
	if reason == "" {
		reason = "null"
	} else {
		reason = `"` + reason + `"`
	}
	body := `{"type":"message_delta","delta":{"stop_reason":` + reason + `}`
	if usage != "" {
		body += `,"usage":` + usage
	}
	return usageEvent("message_delta", body+`}`)
}

func usageStop() string {
	return usageEvent("message_stop", `{"type":"message_stop"}`)
}

func collectUsage(t *testing.T, body string) ([]ir.StreamEvent, error) {
	t.Helper()
	return collectClaude(t, body)
}

func usageFinish(t *testing.T, events []ir.StreamEvent) ir.StreamEvent {
	t.Helper()
	n := 0
	var finish ir.StreamEvent
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			n++
			finish = ev
		}
	}
	if n != 1 {
		t.Fatalf("finish count = %d, events=%+v", n, events)
	}
	return finish
}

func TestClaudeCodeStreamUsageMerge(t *testing.T) {
	half := fmt.Sprintf("%d", int64(math.MaxInt)/2)
	cases := []struct {
		name        string
		start       string
		deltas      []string
		stop        bool
		in, out     int
		read, write int
		reported    bool
		startIn     int
		startRead   int
		startWrite  int
	}{
		{
			name:  "positive replace and output zero reset",
			start: `{"input_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1,"output_tokens":9}`,
			deltas: []string{
				`{"input_tokens":11,"cache_read_input_tokens":7,"cache_creation_input_tokens":5,"output_tokens":4}`,
				`{"output_tokens":0}`,
			},
			stop: true, in: 23, out: 0, read: 7, write: 5, reported: true,
			startIn: 6, startRead: 2, startWrite: 1,
		},
		{
			name:  "omitted and null retain input partitions",
			start: `{"input_tokens":11,"cache_read_input_tokens":7,"cache_creation_input_tokens":5,"output_tokens":2}`,
			deltas: []string{
				`{"input_tokens":null,"cache_read_input_tokens":null,"output_tokens":8}`,
				`{}`,
			},
			in: 23, out: 8, read: 7, write: 5, reported: true,
			startIn: 23, startRead: 7, startWrite: 5,
		},
		{
			name:   "zero input and cache retain prior values",
			start:  `{"input_tokens":11,"cache_read_input_tokens":7,"cache_creation_input_tokens":5,"output_tokens":1}`,
			deltas: []string{`{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":3}`},
			stop:   true, in: 23, out: 3, read: 7, write: 5, reported: true,
			startIn: 23, startRead: 7, startWrite: 5,
		},
		{
			name:   "explicit start zeros are reported",
			start:  `{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":0}`,
			deltas: []string{`{"output_tokens":null}`},
			in:     0, out: 0, reported: true,
		},
		{
			name:   "output only keeps start input",
			start:  `{"input_tokens":4,"output_tokens":1}`,
			deltas: []string{`{"output_tokens":6}`},
			stop:   true, in: 4, out: 6, reported: true, startIn: 4,
		},
		{
			name:   "input only keeps start output",
			start:  `{"output_tokens":5}`,
			deltas: []string{`{"input_tokens":9}`},
			in:     9, out: 5, reported: true,
		},
		{
			name:   "cache only keeps ordinary and output",
			start:  `{"input_tokens":3,"output_tokens":2}`,
			deltas: []string{`{"cache_read_input_tokens":8}`},
			stop:   true, in: 11, out: 2, read: 8, reported: true, startIn: 3,
		},
		{
			name:   "null usage and omitted usage are unmarked",
			start:  `null`,
			deltas: []string{``},
			in:     0, out: 0, reported: false,
		},
		{
			name:   "unknown fields do not mark usage",
			start:  `{"service_tier":"standard","speed":"fast"}`,
			deltas: []string{`{"iterations":2,"inference_geo":"us"}`},
			stop:   true, reported: false,
		},
		{
			name:   "ttl only sums current frame",
			start:  `{"cache_creation":{"ephemeral_1h_input_tokens":2,"ephemeral_5m_input_tokens":3}}`,
			deltas: []string{`{"cache_creation":{"ephemeral_5m_input_tokens":4}}`},
			in:     4, write: 4, reported: true, startIn: 5, startWrite: 5,
		},
		{
			name:   "positive aggregate wins over ttl split",
			start:  `{"cache_creation_input_tokens":9,"cache_creation":{"ephemeral_1h_input_tokens":2,"ephemeral_5m_input_tokens":3}}`,
			deltas: []string{`{"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_1h_input_tokens":6,"ephemeral_5m_input_tokens":1}}`},
			stop:   true, in: 7, write: 7, reported: true, startIn: 9, startWrite: 9,
		},
		{
			name:  "null aggregate falls through to current ttl",
			start: `{"cache_creation_input_tokens":null,"cache_creation":{"ephemeral_1h_input_tokens":null,"ephemeral_5m_input_tokens":6}}`,
			in:    6, write: 6, reported: true, startIn: 6, startWrite: 6,
		},
		{
			name:   "zero ttl does not erase aggregate",
			start:  `{"cache_creation_input_tokens":5}`,
			deltas: []string{`{"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0}}`},
			stop:   true, in: 5, write: 5, reported: true, startIn: 5, startWrite: 5,
		},
		{
			name:  "multiple deltas keep omitted output",
			start: `{"input_tokens":1,"output_tokens":4}`,
			deltas: []string{
				`{"input_tokens":2}`,
				`{"cache_read_input_tokens":3}`,
				`{"cache_creation_input_tokens":4}`,
			},
			in: 9, out: 4, read: 3, write: 4, reported: true, startIn: 1,
		},
		{
			name:   "max valid partition and total",
			start:  `{"input_tokens":` + half + `,"cache_read_input_tokens":1,"output_tokens":` + half + `}`,
			deltas: []string{`{"cache_creation_input_tokens":1,"output_tokens":` + fmt.Sprintf("%d", int64(math.MaxInt)-int64(math.MaxInt)/2-2) + `}`},
			stop:   true,
			in:     int(int64(math.MaxInt)/2) + 2,
			out:    int(int64(math.MaxInt) - int64(math.MaxInt)/2 - 2),
			read:   1, write: 1, reported: true,
			startIn: int(int64(math.MaxInt)/2) + 1, startRead: 1,
		},
	}
	for _, tc := range cases {
		for _, ending := range []string{"message-stop", "terminal-eof"} {
			if ending == "message-stop" && !tc.stop && len(tc.deltas) == 0 {
				continue
			}
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				body := usageClosed(usageStart(tc.start))
				if len(tc.deltas) == 0 {
					body += usageDelta("", "end_turn")
				}
				for i, delta := range tc.deltas {
					reason := ""
					if i == len(tc.deltas)-1 {
						reason = "end_turn"
					}
					if delta == "null" {
						body += usageEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":`+reasonJSON(reason)+`},"usage":null}`)
						continue
					}
					body += usageDelta(delta, reason)
				}
				if ending == "message-stop" {
					body += usageStop()
				}
				events, err := collectUsage(t, body)
				if err != nil {
					t.Fatal(err)
				}
				start := events[0]
				if start.Kind != ir.EventMessageStart || start.UsageReported {
					t.Fatalf("start = %+v", start)
				}
				if start.InputTokens != tc.startIn || start.CacheReadTokens != tc.startRead || start.CacheWriteTokens != tc.startWrite {
					t.Fatalf("start usage = %+v, want %d/%d/%d", start, tc.startIn, tc.startRead, tc.startWrite)
				}
				finish := usageFinish(t, events)
				if finish.InputTokens != tc.in || finish.OutputTokens != tc.out || finish.CacheReadTokens != tc.read || finish.CacheWriteTokens != tc.write || finish.UsageReported != tc.reported {
					t.Fatalf("finish = %+v, want in %d out %d read %d write %d reported %v", finish, tc.in, tc.out, tc.read, tc.write, tc.reported)
				}
				if finish.InputTokens != finish.CacheReadTokens+finish.CacheWriteTokens+ordinaryOf(finish) {
					t.Fatalf("inclusive input mismatch: %+v", finish)
				}
			})
		}
	}
}

func reasonJSON(reason string) string {
	if reason == "" {
		return "null"
	}
	return `"` + reason + `"`
}

func ordinaryOf(ev ir.StreamEvent) int {
	return ev.InputTokens - ev.CacheReadTokens - ev.CacheWriteTokens
}

func TestClaudeCodeStreamUsageRejectsUnsafeCounts(t *testing.T) {
	max := fmt.Sprintf("%d", int64(math.MaxInt))
	over := strconv.FormatUint(uint64(math.MaxInt)+1, 10)
	secret := "PAYLOAD_SECRET_USAGE"
	cases := []struct {
		name, start, delta, want string
	}{
		{"negative input", `{"input_tokens":-1}`, ``, "invalid usage counter"},
		{"negative cache read", `{"cache_read_input_tokens":-2}`, ``, "invalid usage counter"},
		{"negative cache write", `{"cache_creation_input_tokens":-3}`, ``, "invalid usage counter"},
		{"negative output", `{"output_tokens":-4}`, ``, "invalid usage counter"},
		{"negative ttl", `{"cache_creation":{"ephemeral_1h_input_tokens":-1}}`, ``, "invalid usage counter"},
		{"string counter", `{"input_tokens":"12"}`, ``, "invalid usage counter"},
		{"fraction", `{"output_tokens":1.5}`, ``, "invalid usage counter"},
		{"exponent", `{"cache_read_input_tokens":1e2}`, ``, "invalid usage counter"},
		{"usage array", `[]`, ``, "invalid usage object"},
		{"usage string", `"12"`, ``, "invalid usage object"},
		{"ttl overflow", `{"cache_creation":{"ephemeral_1h_input_tokens":` + max + `,"ephemeral_5m_input_tokens":1}}`, ``, "usage total overflow"},
		{"input overflow", `{"input_tokens":` + max + `,"cache_read_input_tokens":1}`, ``, "usage total overflow"},
		{"total overflow", `{"input_tokens":` + max + `,"output_tokens":1}`, ``, "usage total overflow"},
		{"counter above int", `{"input_tokens":` + over + `}`, ``, "usage total overflow"},
		{"delta keeps no partial", `{"input_tokens":3,"output_tokens":1}`, `{"cache_creation":{"ephemeral_1h_input_tokens":` + max + `,"ephemeral_5m_input_tokens":1},"output_tokens":9,` + `"note":"` + secret + `"}`, "usage total overflow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := usageStart(tc.start)
			if tc.delta != "" {
				body = usageClosed(body) + usageDelta(tc.delta, "end_turn") + usageStop()
			}
			events, err := collectUsage(t, body)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), max) {
				t.Fatalf("err=%v events=%+v", err, events)
			}
			if claudeFinishCount(events) != 0 {
				t.Fatalf("unsafe usage emitted finish: %+v", events)
			}
			for _, ev := range events {
				if strings.Contains(ev.Text, secret) || strings.Contains(ev.Data, secret) {
					t.Fatalf("payload leak: %+v", ev)
				}
			}
		})
	}
}

func TestClaudeCodeUsageOnlyDeltaDoesNotFinish(t *testing.T) {
	body := usageClosed(usageStart(`{"input_tokens":2,"output_tokens":1}`)) +
		usageDelta(`{"output_tokens":4}`, "")
	events, err := collectUsage(t, body)
	if err == nil || !strings.Contains(err.Error(), "stream ended before completion") || claudeFinishCount(events) != 0 {
		t.Fatalf("err=%v events=%+v", err, events)
	}
}

func TestClaudeCodeUsageTruncationDoesNotPublishSuccess(t *testing.T) {
	body := usageClosed(usageStart(`{"input_tokens":9,"output_tokens":8}`))
	events, err := collectUsage(t, body)
	if err == nil || !strings.Contains(err.Error(), "stream ended before completion") || claudeFinishCount(events) != 0 {
		t.Fatalf("err=%v events=%+v", err, events)
	}
	if events[0].InputTokens != 9 || events[0].UsageReported {
		t.Fatalf("start = %+v", events[0])
	}
}

func TestClaudeCodeDirectCollectionHonorsReportedSnapshot(t *testing.T) {
	body := usageClosed(usageStart(`{"input_tokens":4,"cache_read_input_tokens":2,"output_tokens":7}`)) +
		usageDelta(`{"input_tokens":0,"cache_creation_input_tokens":3,"output_tokens":0}`, "end_turn")
	var events []ir.StreamEvent
	if err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finish := usageFinish(t, events)
	if !finish.UsageReported || finish.InputTokens != 9 || finish.OutputTokens != 0 || finish.CacheReadTokens != 2 || finish.CacheWriteTokens != 3 {
		t.Fatalf("finish = %+v", finish)
	}
	collected := ir.Usage{}
	if finish.UsageReported {
		collected = ir.Usage{
			InputTokens: finish.InputTokens, OutputTokens: finish.OutputTokens,
			CacheReadTokens: finish.CacheReadTokens, CacheWriteTokens: finish.CacheWriteTokens,
			UsageReported: true,
		}.Clamped()
	}
	if !collected.UsageReported || collected.InputTokens != 9 || collected.OutputTokens != 0 || collected.CacheReadTokens != 2 || collected.CacheWriteTokens != 3 {
		t.Fatalf("collected snapshot = %+v", collected)
	}
}

func TestGenericAnthropicUsagePolicyUnchanged(t *testing.T) {
	body := usageClosed(usageStart(`{"input_tokens":5,"cache_read_input_tokens":2,"cache_creation_input_tokens":1,"output_tokens":9}`)) +
		usageDelta(`{"input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`, "end_turn") + usageStop()
	var events []ir.StreamEvent
	err := anthropic.DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	finish := usageFinish(t, events)
	if finish.UsageReported || finish.InputTokens != 1 || finish.OutputTokens != 0 || finish.CacheReadTokens != 0 || finish.CacheWriteTokens != 1 {
		t.Fatalf("generic finish = %+v", finish)
	}
	ttl := usageClosed(usageStart(`{"cache_creation":{"ephemeral_1h_input_tokens":2,"ephemeral_5m_input_tokens":3},"output_tokens":1}`)) +
		usageDelta(`{"output_tokens":1}`, "end_turn") + usageStop()
	events = nil
	err = anthropic.DecodeStream(strings.NewReader(ttl), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	finish = usageFinish(t, events)
	if finish.UsageReported || finish.InputTokens != 0 || finish.CacheWriteTokens != 0 || finish.OutputTokens != 1 {
		t.Fatalf("generic ttl finish = %+v", finish)
	}
}
