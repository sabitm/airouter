package qoder

import (
	"io"
	"strings"
	"testing"
)

// TestParseQueueInfoSignalVariants covers nested encodings, numeric or string
// code forms, and the metadata signals the recovery keys off.
func TestParseQueueInfoSignalVariants(t *testing.T) {
	queuedBody := `{"isQueued":true,"modelKey":"auto","queueCount":0,"queueType":"p3","retryAfterSeconds":30,"serviceAvailable":false,"subCode":"RESOURCE_LIMIT","waitTime":30}`
	cases := []struct {
		name   string
		body   string
		status string
		queued bool
	}{
		{"plain string code", `{"code":"10605"}`, "200", true},
		{"numeric code", `{"code":10605}`, "200", true},
		{"nested code inside message string", `{"code":"10605","message":"{\"isQueued\":true}"}`, "200", true},
		{"double-encoded queue object", `{"code":"x","message":"{\"body\":\"{\\\"isQueued\\\":true}\"}"}`, "200", true},
		{"service unavailable false", `{"serviceAvailable":false}`, "200", true},
		{"resource limit subcode", `{"subCode":"RESOURCE_LIMIT"}`, "200", true},
		{"full reference shape", queuedBody, "429", true},
		{"billing 112 is not queue", `{"code":"112","message":"quota exhausted"}`, "403", false},
		{"billing 110 numeric is not queue", `{"code":110}`, "403", false},
		{"pricingurl is not queue", `{"message":"see pricingUrl"}`, "403", false},
		{"random error is not queue", `{"code":"500","message":"boom"}`, "500", false},
		{"isQueued false alone is not a signal", `{"isQueued":false}`, "200", false},
		{"empty", "", "200", false},
		{"non json", "keepalive", "200", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := ParseQueueInfo(tc.body)
			if got := info.HasQueueSignal(); got != tc.queued {
				t.Fatalf("queued = %v, want %v (info=%+v)", got, tc.queued, info)
			}
			if tc.queued && info == nil {
				t.Fatal("nil info for queued body")
			}
		})
	}
}

func TestParseQueueInfoFields(t *testing.T) {
	body := `{"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"auto\",\"queueCount\":0,\"queueType\":\"p3\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"subCode\":\"RESOURCE_LIMIT\",\"waitTime\":45}"}`
	info := ParseQueueInfo(body)
	if info == nil {
		t.Fatal("nil info")
	}
	if info.Code != "10605" || info.SubCode != "RESOURCE_LIMIT" || info.ModelKey != "auto" || info.QueueType != "p3" {
		t.Fatalf("strings = %+v", info)
	}
	if info.IsQueued == nil || !*info.IsQueued || info.ServiceAvailable == nil || *info.ServiceAvailable {
		t.Fatalf("bools = %+v", info)
	}
	if info.QueueCount == nil || *info.QueueCount != 0 {
		t.Fatalf("queueCount = %+v", info)
	}
	if info.RetryAfterSeconds == nil || *info.RetryAfterSeconds != 30 {
		t.Fatalf("retryAfterSeconds = %+v", info)
	}
	if info.WaitTime == nil || *info.WaitTime != 45 {
		t.Fatalf("waitTime = %+v", info)
	}
}

func TestParseQueueInfoNumericCodeField(t *testing.T) {
	info := ParseQueueInfo(`{"code":10605}`)
	if info == nil || info.Code != "10605" {
		t.Fatalf("info = %+v, want numeric 10605 recognized", info)
	}
}

func TestParseQueueStatus(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		st := ParseQueueStatus(`{"isQueued":false,"serviceAvailable":true,"queueType":"p3","retryAfterSeconds":2}`)
		if st == nil || st.IsQueued == nil || *st.IsQueued {
			t.Fatalf("status = %+v", st)
		}
		if st.RetryAfterSeconds == nil || *st.RetryAfterSeconds != 2 {
			t.Fatalf("retry = %+v", st)
		}
	})
	t.Run("still queued", func(t *testing.T) {
		st := ParseQueueStatus(`{"isQueued":true,"queueCount":3}`)
		if st == nil || st.IsQueued == nil || !*st.IsQueued {
			t.Fatalf("status = %+v", st)
		}
	})
	t.Run("unknown shape unusable", func(t *testing.T) {
		for _, body := range []string{`{"foo":1}`, `not json`, ``, `[1]`, `{"isQueued":"yes"}`} {
			if st := ParseQueueStatus(body); st != nil {
				t.Fatalf("body %q parsed as %+v", body, st)
			}
		}
	})
}

func TestQueueStatusURL(t *testing.T) {
	got := QueueStatusURL("https://api3.qoder.sh/algo", "set-1", "auto", "p3")
	want := "https://api3.qoder.sh/algo/api/v2/service/ask/queue/status?modelKey=auto&queueType=p3&requestSetId=set-1"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
	if !strings.HasPrefix(QueueStatusURL("http://127.0.0.1:1/algo/", "s", "", ""), "http://127.0.0.1:1/algo/api/v2/service/ask/queue/status") {
		t.Fatal("httptest base not supported")
	}
	if got := QueueStatusURL("https://h/algo", "", "", ""); !strings.HasSuffix(got, QueueStatusPath+"?") {
		t.Fatalf("url = %q", got)
	}
}

func TestIsBillingBlock(t *testing.T) {
	billing := []string{
		`{"code":"110","message":"daily count exceeded"}`,
		`{"code":110}`,
		`{"code":"112"}`,
		`{"code":112,"message":"x"}`,
		`{"error":{"code":112}}`,
		`visit pricingUrl for details`,
		`{"message":"pricingUrl here"}`,
		`{"x":1,"code":"112"}`,
	}
	for _, b := range billing {
		if !IsBillingBlock(b) {
			t.Errorf("IsBillingBlock(%q) = false, want true", b)
		}
	}
	notBilling := []string{
		`{"code":"10605"}`,
		`{"code":10605}`,
		`{"code":"500","message":"boom"}`,
		``,
		`plain overload`,
		`{"code":"111"}`,
	}
	for _, b := range notBilling {
		if IsBillingBlock(b) {
			t.Errorf("IsBillingBlock(%q) = true, want false", b)
		}
	}
}

func TestPeekFirstSSEFrame(t *testing.T) {
	t.Run("first data line with comment noise", func(t *testing.T) {
		stream := ": ping\n\n event: x\ndata: {\"a\":1}\ndata: second\n\nmore"
		consumed, payload, err := PeekFirstSSEFrame(strings.NewReader(stream))
		if err != nil {
			t.Fatal(err)
		}
		if payload != `{"a":1}` {
			t.Fatalf("payload = %q", payload)
		}
		if !strings.HasPrefix(string(consumed), ": ping") {
			t.Fatalf("consumed = %q", consumed)
		}
		// The consumed prefix must include the peeked data line's newline.
		if !strings.HasSuffix(string(consumed), "\n") {
			t.Fatalf("consumed = %q, want trailing newline", consumed)
		}
	})
	t.Run("eof before data", func(t *testing.T) {
		consumed, payload, err := PeekFirstSSEFrame(strings.NewReader(": only comments\n"))
		if err != nil || payload != "" {
			t.Fatalf("payload=%q err=%v", payload, err)
		}
		if string(consumed) != ": only comments\n" {
			t.Fatalf("consumed = %q", consumed)
		}
	})
	t.Run("read error surfaces with consumed bytes", func(t *testing.T) {
		consumed, payload, err := PeekFirstSSEFrame(&failReader{data: "data: {\"a", err: errFail})
		if payload != "" {
			t.Fatalf("payload = %q", payload)
		}
		if err == nil {
			t.Fatal("want error")
		}
		if string(consumed) != "data: {\"a" {
			t.Fatalf("consumed = %q", consumed)
		}
	})
	t.Run("byte-at-a-time means nothing buffered away", func(t *testing.T) {
		// After the peek, the remaining stream must start exactly where the
		// consumed prefix ended.
		stream := "data: {\"a\":1}\n\nREST"
		consumed, _, err := PeekFirstSSEFrame(strings.NewReader(stream))
		if err != nil {
			t.Fatal(err)
		}
		rest := stream[len(consumed):]
		if rest != "\nREST" {
			t.Fatalf("rest = %q, consumed = %q", rest, consumed)
		}
	})
	t.Run("heartbeat comment prefixes do not hide the frame", func(t *testing.T) {
		stream := ": ka\n\n: ka2\n\ndata: {\"code\":\"10605\"}\n\n"
		consumed, payload, err := PeekFirstSSEFrame(strings.NewReader(stream))
		if err != nil {
			t.Fatal(err)
		}
		if payload != `{"code":"10605"}` {
			t.Fatalf("payload = %q", payload)
		}
		if !strings.HasPrefix(string(consumed), ": ka") || !strings.HasSuffix(string(consumed), "\n") {
			t.Fatalf("consumed = %q", consumed)
		}
	})
	t.Run("fragmented frames across reads", func(t *testing.T) {
		consumed, payload, err := PeekFirstSSEFrame(&chunkReader{chunks: []string{"dat", "a: {\"a\"", ":1}\n"}, tail: "REST"})
		if err != nil {
			t.Fatal(err)
		}
		if payload != `{"a":1}` {
			t.Fatalf("payload = %q", payload)
		}
		if string(consumed) != "data: {\"a\":1}\n" {
			t.Fatalf("consumed = %q", consumed)
		}
	})
	t.Run("final data line without newline is parsed on eof", func(t *testing.T) {
		// A queue refusal terminated by EOF instead of a newline must still be
		// detected.
		consumed, payload, err := PeekFirstSSEFrame(strings.NewReader("data: {\"code\":\"10605\"}"))
		if err != nil {
			t.Fatal(err)
		}
		if payload != `{"code":"10605"}` {
			t.Fatalf("payload = %q", payload)
		}
		if string(consumed) != "data: {\"code\":\"10605\"}" {
			t.Fatalf("consumed = %q", consumed)
		}
	})
	t.Run("n greater than zero with eof in one read keeps payload and error", func(t *testing.T) {
		// The last byte and io.EOF may arrive in the same Read call; the
		// completed payload must survive alongside the error signal.
		consumed, payload, err := PeekFirstSSEFrame(&chunkReader{chunks: []string{"data: ok\n"}, eofWithLastByte: true})
		if payload != "ok" {
			t.Fatalf("payload = %q", payload)
		}
		if err != io.EOF {
			t.Fatalf("err = %v, want io.EOF", err)
		}
		if string(consumed) != "data: ok\n" {
			t.Fatalf("consumed = %q", consumed)
		}
	})
	t.Run("eof on a separate read keeps payload without error", func(t *testing.T) {
		consumed, payload, err := PeekFirstSSEFrame(&chunkReader{chunks: []string{"data: ok\n"}})
		if err != nil || payload != "ok" {
			t.Fatalf("payload=%q err=%v", payload, err)
		}
		if string(consumed) != "data: ok\n" {
			t.Fatalf("consumed = %q", consumed)
		}
	})
	t.Run("n greater than zero with read error keeps consumed and error", func(t *testing.T) {
		consumed, payload, err := PeekFirstSSEFrame(&chunkReader{chunks: []string{"data: {\"a"}, tailErr: errFail})
		if payload != "" {
			t.Fatalf("payload = %q", payload)
		}
		if err != errFail {
			t.Fatalf("err = %v, want the read error", err)
		}
		if string(consumed) != "data: {\"a" {
			t.Fatalf("consumed = %q", consumed)
		}
	})
	t.Run("oversized first frame stops at the cap without error", func(t *testing.T) {
		// Larger than the peek budget and never a data line: the peek must
		// stop bounded, and the caller's replay+live reconstruction preserves
		// the bytes for the decoder.
		big := strings.Repeat("x", maxPeekBytes+16)
		consumed, payload, err := PeekFirstSSEFrame(strings.NewReader(big))
		if err != nil {
			t.Fatal(err)
		}
		if payload != "" {
			t.Fatalf("payload = %q", payload)
		}
		if len(consumed) != maxPeekBytes {
			t.Fatalf("consumed = %d, want cap %d", len(consumed), maxPeekBytes)
		}
	})
	t.Run("comments-only eof stays an empty payload", func(t *testing.T) {
		consumed, payload, err := PeekFirstSSEFrame(strings.NewReader(": c\n\n: c2\n"))
		if err != nil || payload != "" {
			t.Fatalf("payload=%q err=%v", payload, err)
		}
		if string(consumed) != ": c\n\n: c2\n" {
			t.Fatalf("consumed = %q", consumed)
		}
	})
}

// chunkReader serves its chunks in order, at most one chunk's remaining
// bytes per Read, so data lines fragment across call boundaries. After the
// chunks it returns the optional tail once, then EOF or tailErr.
// eofWithLastByte delivers the final byte of the last chunk together with
// io.EOF in a single call (an n>0-plus-error return).
type chunkReader struct {
	chunks          []string
	tail            string
	tailErr         error
	eofWithLastByte bool
	cur             int
	pos             int
	tailDone        bool
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.cur < len(r.chunks) {
		c := r.chunks[r.cur]
		if r.pos >= len(c) {
			r.cur++
			r.pos = 0
		} else {
			n := copy(p, c[r.pos:])
			r.pos += n
			if r.pos == len(c) {
				r.cur++
				r.pos = 0
				if r.eofWithLastByte && r.cur == len(r.chunks) {
					return n, io.EOF
				}
			}
			return n, nil
		}
	}
	if !r.tailDone && r.tail != "" {
		r.tailDone = true
		return copy(p, r.tail), nil
	}
	if r.tailErr != nil {
		return 0, r.tailErr
	}
	return 0, io.EOF
}

var errFail = &peekErr{}

type peekErr struct{}

func (e *peekErr) Error() string { return "boom" }

type failReader struct {
	data string
	err  error
	off  int
}

func (r *failReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		r.off++
		return copy(p, r.data[r.off-1:r.off]), nil
	}
	return 0, r.err
}
