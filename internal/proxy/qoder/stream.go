package qoder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
)

// maxFailureMessage bounds the message stored on a structured failure. Terminal
// logging and client envelopes clamp again downstream; raw bodies belong only
// in HAR capture.
const maxFailureMessage = 1000

// DecodeStream unwraps Qoder's {statusCodeValue, body} SSE envelope into plain
// OpenAI chat-completion chunks, then reuses the OpenAI stream decoder.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	return openai.DecodeStream(newUnwrapReader(r), emit)
}

// unwrapReader converts Qoder envelope SSE into OpenAI-shaped SSE bytes.
//
// statusCodeValue may arrive as a number or a numeric string, and body may be
// a JSON string or an embedded object; both forms are normalized into plain
// OpenAI data lines:
//
//   - finish_reason is lifted from choices[].delta.finish_reason or the
//     top-level field into choices[].finish_reason, the only location the
//     shared OpenAI decoder reads.
//   - usage aliases (input_tokens, output_tokens, cache fields) are folded
//     into prompt_tokens / completion_tokens / prompt_tokens_details without
//     double counting; standard fields win over aliases.
//   - after a terminal finish and real usage have both been seen, the reader
//     emits [DONE] itself: Qoder keeps the socket open after the terminal
//     frames (keepalive), and waiting for EOF would hang the client.
//
// Anything else — non-envelope JSON frames, unparseable frames, or a
// recognized envelope with malformed fields — is a structured failure, never a
// silent drop.
type unwrapReader struct {
	sc     *bufio.Scanner
	buf    bytes.Buffer
	err    error
	done   bool
	closed bool
	// Terminal evidence tracking. finishFrame/usageFrame are positions of the
	// frames that carried them, so usage seen on a content frame BEFORE the
	// finish frame stays preliminary and a later trailer can still win.
	frameNo     int
	finishSeen  bool
	finishFrame int
	usageFrame  int
	// usagePositive distinguishes real counts from zero placeholders: a
	// finish frame with zero counters may precede the final usage trailer, so
	// it must not end the stream early. Actual zero usage is preserved on
	// explicit [DONE] or EOF.
	usagePositive bool
}

func newUnwrapReader(r io.Reader) *unwrapReader {
	sc := bufio.NewScanner(r)
	// Upstream chunks can be large (tool args); raise the default 64K limit.
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	return &unwrapReader{sc: sc}
}

func (u *unwrapReader) Read(p []byte) (int, error) {
	for u.buf.Len() == 0 {
		if u.err != nil {
			return 0, u.err
		}
		if u.done || u.closed {
			return 0, io.EOF
		}
		if !u.sc.Scan() {
			if err := u.sc.Err(); err != nil {
				u.err = err
				return 0, err
			}
			// Ensure a trailing DONE so the OpenAI decoder finishes cleanly.
			u.buf.WriteString("data: [DONE]\n\n")
			u.done = true
			continue
		}
		line := strings.TrimRight(u.sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		u.processFrame(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
	}
	return u.buf.Read(p)
}

func (u *unwrapReader) fail(err error) {
	u.err = err
}

// processFrame interprets one Qoder data payload and appends the equivalent
// OpenAI data line.
func (u *unwrapReader) processFrame(data string) {
	if data == "" {
		return
	}
	if data == "[DONE]" {
		u.buf.WriteString("data: [DONE]\n\n")
		u.done = true
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &probe); err != nil || probe == nil {
		u.fail(ir.ProtocolError("qoder: unrecognized stream frame"))
		return
	}
	rawStatus, hasStatus := probe["statusCodeValue"]
	rawBody, hasBody := probe["body"]
	if !hasStatus && !hasBody {
		u.fail(ir.ProtocolError("qoder: unrecognized stream frame"))
		return
	}
	u.frameNo++
	// A present-but-uninterpretable status is a recognized malformed envelope,
	// not a chunk to skip. Absent defaults to 200 like the reference client.
	status := 200
	if hasStatus {
		v, err := decodeStatusValue(rawStatus)
		if err != nil {
			u.fail(ir.ProtocolError("qoder: malformed envelope status"))
			return
		}
		status = v
	}
	inner := ""
	if hasBody {
		s, ok := decodeEnvelopeBody(rawBody)
		if !ok {
			u.fail(ir.ProtocolError("qoder: malformed envelope body"))
			return
		}
		inner = s
	}
	if status != 200 {
		u.fail(classifyUpstreamError(status, inner))
		return
	}
	if inner == "" {
		return
	}
	if inner == "[DONE]" {
		u.buf.WriteString("data: [DONE]\n\n")
		u.done = true
		return
	}
	line, finish, counts, ok := normalizeChunk(inner)
	if !ok {
		u.fail(ir.ProtocolError("qoder: envelope body is not a chat chunk"))
		return
	}
	if finish != "" && !u.finishSeen {
		u.finishSeen = true
		u.finishFrame = u.frameNo
	}
	if counts.present {
		u.usageFrame = u.frameNo
		u.usagePositive = counts.prompt > 0 || counts.completion > 0
	}
	u.buf.WriteString("data: ")
	u.buf.WriteString(line)
	u.buf.WriteString("\n\n")
	// Terminal finish plus a positive usage count at or after the finish frame
	// is sufficient evidence to end the stream while the upstream keepalive
	// holds the socket open. Counts on an earlier content frame are
	// preliminary, and zero counters may be a placeholder before the real
	// trailer, so neither may end the stream; explicit [DONE] and EOF still
	// terminate and preserve whatever usage was actually seen.
	if u.finishSeen && u.usagePositive && u.usageFrame >= u.finishFrame {
		u.buf.WriteString("data: [DONE]\n\n")
		u.done = true
	}
}

// decodeStatusValue accepts only a finite integer in the exactly representable
// range, as a JSON number or numeric string. Fractional, non-finite, and
// out-of-range values are malformed: returning them truncated or overflowed
// could mask a failure as a 200 or misreport a 403.
func decodeStatusValue(raw json.RawMessage) (int, error) {
	v, ok := strictJSONInt(raw)
	if !ok || v < math.MinInt || v > math.MaxInt {
		return 0, fmt.Errorf("unsupported status value")
	}
	return int(v), nil
}

// maxExactInt is the largest magnitude float64 that is also an exact integer
// (2^53). Counts beyond it are garbage: int() conversion would overflow or
// lose precision silently.
const maxExactInt float64 = 9007199254740992

// strictJSONInt parses a JSON number or numeric string into a non-negative
// integer. Fractional, non-finite, negative, and out-of-range values are
// rejected rather than truncated: a negative or overflowed count would poison
// accounting, a truncated status could mask a failure as a 200.
func strictJSONInt(raw json.RawMessage) (int64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(trimmed, &f); err != nil {
		var s string
		if json.Unmarshal(trimmed, &s) != nil {
			return 0, false
		}
		var perr error
		f, perr = strconv.ParseFloat(strings.TrimSpace(s), 64)
		if perr != nil {
			return 0, false
		}
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f != math.Trunc(f) || f > maxExactInt {
		return 0, false
	}
	return int64(f), true
}

// decodeEnvelopeBody accepts a JSON string body verbatim and an object or
// array body compacted to text. Null is an empty keepalive body.
func decodeEnvelopeBody(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", true
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", false
		}
		return s, true
	case '{', '[':
		var buf bytes.Buffer
		if err := json.Compact(&buf, trimmed); err != nil {
			return "", false
		}
		return buf.String(), true
	default:
		return "", false
	}
}

// classifyUpstreamError maps a non-200 Qoder envelope to a structured failure.
// Billing blocks (codes 110/112, pricingUrl) become quota errors with a 403;
// a valid embedded status is preserved so pre-commit failover can keep the
// upstream status. Queue code 10605 keeps its code and status: proxy-level
// queue recovery normally intercepts it first, and a refusal that bypasses
// recovery still fails over instead of surfacing as content.
func classifyUpstreamError(status int, inner string) error {
	code := extractErrorCode(inner)
	msg := inner
	if msg == "" {
		msg = fmt.Sprintf("qoder upstream status %d", status)
	}
	if len(msg) > maxFailureMessage {
		msg = msg[:maxFailureMessage]
	}
	if IsBillingBlock(inner) {
		// Billing is always a caller-facing 403 regardless of the embedded
		// status, so quota handling sees a consistent signal.
		return &ir.StreamFailure{Type: "quota_error", Code: code, Message: msg, Status: http.StatusForbidden}
	}
	sf := &ir.StreamFailure{Code: code, Message: msg}
	if status >= 400 && status <= 599 {
		sf.Status = status
	}
	return sf
}

// extractErrorCode reads the top-level (or one-level nested error) code of a
// Qoder error body, accepting numeric or string encodings. Empty when absent.
func extractErrorCode(inner string) string {
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(inner), &probe) != nil || probe == nil {
		return ""
	}
	if code := rawCodeString(probe["code"]); code != "" {
		return code
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(probe["error"], &nested) == nil && nested != nil {
		return rawCodeString(nested["code"])
	}
	return ""
}

func rawCodeString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

// normalizeChunk lifts finish_reason and usage aliases into the locations the
// shared OpenAI decoder reads. It returns the (possibly rewritten) chunk text,
// the normalized finish reason, and the normalized token counts. ok is false
// when the body is not a JSON object chunk.
func normalizeChunk(inner string) (line, finish string, counts usageCounts, ok bool) {
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inner), &chunk); err != nil || chunk == nil {
		return "", "", counts, false
	}
	patched := false
	finish, finPatched := normalizeFinish(chunk)
	patched = patched || finPatched
	counts, usagePatched := normalizeUsage(chunk)
	patched = patched || usagePatched
	line = strings.ReplaceAll(strings.ReplaceAll(inner, "\r", ""), "\n", "")
	if patched {
		out, err := json.Marshal(chunk)
		if err != nil {
			return "", "", counts, false
		}
		line = string(out)
	}
	return line, finish, counts, true
}

// normalizeFinish returns the chunk's finish reason, reading the choice-level,
// delta-level, and top-level locations. When only a delta-level or top-level
// value exists it is lifted into choices[].finish_reason so the shared decoder
// sees the terminal reason; content and tool-call fragments in the same chunk
// are preserved.
func normalizeFinish(chunk map[string]json.RawMessage) (finish string, patched bool) {
	var choices []json.RawMessage
	if raw, ok := chunk["choices"]; ok {
		if err := json.Unmarshal(raw, &choices); err != nil {
			return "", false
		}
	}
	top := rawFinishField(chunk["finish_reason"])
	choiceFinish := ""
	deltaFinish := ""
	var choice0 map[string]json.RawMessage
	if len(choices) > 0 {
		if err := json.Unmarshal(choices[0], &choice0); err == nil && choice0 != nil {
			choiceFinish = rawFinishField(choice0["finish_reason"])
			var delta map[string]json.RawMessage
			if raw, ok := choice0["delta"]; ok {
				if json.Unmarshal(raw, &delta) == nil && delta != nil {
					deltaFinish = rawFinishField(delta["finish_reason"])
				}
			}
		} else {
			choice0 = nil
		}
	}
	finish = firstNonEmpty(choiceFinish, deltaFinish, top)
	if finish == "" || choiceFinish != "" {
		return finish, false
	}
	if choice0 == nil {
		// No usable choice object: synthesize a terminal choice so the shared
		// decoder observes the reason at its standard location.
		choice0 = map[string]json.RawMessage{"index": json.RawMessage("0"), "delta": json.RawMessage("{}")}
	}
	enc, err := json.Marshal(finish)
	if err != nil {
		return finish, false
	}
	choice0["finish_reason"] = enc
	newChoice, err := json.Marshal(choice0)
	if err != nil {
		return finish, false
	}
	if len(choices) == 0 {
		choices = []json.RawMessage{newChoice}
	} else {
		choices[0] = newChoice
	}
	arr, err := json.Marshal(choices)
	if err != nil {
		return finish, false
	}
	chunk["choices"] = arr
	return finish, true
}

func rawFinishField(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// usageCounts is the normalized prompt/completion pair after alias folding.
// present is true when any count field carried a supported value; the shared
// decoder then sees real (possibly zero) numbers instead of an empty object.
type usageCounts struct {
	present    bool
	prompt     int
	completion int
}

// Usage count fields the shared OpenAI decoder parses as integers. Any other
// JSON type in these positions makes the decoder silently drop the whole
// chunk, content included, so every one of them is normalized or removed.
const (
	usageKeyPrompt     = "prompt_tokens"
	usageKeyCompletion = "completion_tokens"
	usageKeyTotal      = "total_tokens"
	usageKeyDetails    = "prompt_tokens_details"
)

// normalizeUsage folds Qoder usage aliases into the standard OpenAI fields the
// shared decoder reads:
//
//   - input_tokens/output_tokens map to prompt_tokens/completion_tokens;
//   - cache aliases at usage level and nested inside prompt_tokens_details map
//     into prompt_tokens_details.cached_tokens / .cache_write_tokens;
//   - numeric-string counts are rewritten as integer JSON;
//   - fractional, non-finite, negative, and out-of-range values are removed,
//     never truncated or overflowed, and a consumed alias is deleted so no
//     counter can be double counted;
//   - standard fields win over aliases; an invalid standard field falls back
//     to aliases like an absent one.
//
// A usage value that is not a usable object is removed instead of left in
// place, because the shared decoder would otherwise drop the entire chunk —
// content included. It reports the normalized counts and whether anything was
// rewritten.
func normalizeUsage(chunk map[string]json.RawMessage) (counts usageCounts, patched bool) {
	raw, ok := chunk["usage"]
	if !ok {
		return counts, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return counts, false
	}
	var u map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &u); err != nil || u == nil {
		// A JSON-encoded usage object inside a string is still usable.
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			if json.Unmarshal([]byte(strings.TrimSpace(s)), &u) != nil || u == nil {
				u = nil
			}
		}
		if u == nil {
			// An uninterpretable usage value would make the shared decoder drop
			// the whole response-bearing chunk; remove it instead.
			delete(chunk, "usage")
			return counts, true
		}
		patched = true
	}

	prompt, promptSet, changed := sanitizeCount(u, usageKeyPrompt)
	patched = patched || changed
	if !promptSet {
		if v, ok := consumeAlias(u, "input_tokens"); ok {
			u[usageKeyPrompt] = marshalInt(v)
			patched = true
			prompt, promptSet = v, true
		}
	}
	completion, completionSet, changed := sanitizeCount(u, usageKeyCompletion)
	patched = patched || changed
	if !completionSet {
		if v, ok := consumeAlias(u, "output_tokens"); ok {
			u[usageKeyCompletion] = marshalInt(v)
			patched = true
			completion, completionSet = v, true
		}
	}
	// total_tokens is parsed by the shared decoder too; sanitize it even though
	// the IR usage does not carry it.
	if _, _, totalChanged := sanitizeCount(u, usageKeyTotal); totalChanged {
		patched = true
	}

	detailsPatched := normalizeDetails(u)
	patched = patched || detailsPatched

	if patched {
		enc, err := json.Marshal(u)
		if err != nil {
			return counts, true
		}
		chunk["usage"] = enc
	}
	counts.present = promptSet || completionSet
	counts.prompt = prompt
	counts.completion = completion
	return counts, patched
}

// sanitizeCount normalizes one integer count field in place: unsupported
// values are removed, supported non-canonical forms (numeric strings, floats)
// are rewritten as plain integer JSON. Reports the value, whether a supported
// count is present, and whether the map changed.
func sanitizeCount(u map[string]json.RawMessage, key string) (int, bool, bool) {
	raw, ok := u[key]
	if !ok {
		return 0, false, false
	}
	v, valid := strictJSONInt(raw)
	if !valid {
		delete(u, key)
		return 0, false, true
	}
	if !isCanonicalJSONInt(raw) {
		u[key] = marshalInt64(v)
		return int(v), true, true
	}
	return int(v), true, false
}

// normalizeDetails sanitizes prompt_tokens_details and folds nested cache
// aliases into the two fields the shared decoder reads. A nil details map must
// never be written to: a JSON null previously panicked here. It reports
// whether the usage object needs re-encoding, and only rewrites the details
// field itself when its content actually changed.
func normalizeDetails(u map[string]json.RawMessage) bool {
	draw, hasDetails := u[usageKeyDetails]
	patched := false
	details := map[string]json.RawMessage{}
	detailsChanged := false
	if hasDetails {
		trimmed := bytes.TrimSpace(draw)
		if len(trimmed) > 0 && string(trimmed) != "null" {
			if err := json.Unmarshal(trimmed, &details); err != nil || details == nil {
				details = map[string]json.RawMessage{}
				var s string
				if json.Unmarshal(trimmed, &s) == nil {
					_ = json.Unmarshal([]byte(strings.TrimSpace(s)), &details)
				}
				if len(details) == 0 {
					// Uninterpretable details would make the shared decoder drop
					// the chunk; remove rather than keep.
					delete(u, usageKeyDetails)
					return true
				}
				detailsChanged = true // string form is rewritten as an object
			}
		}
	}

	sanitize := func(key string) (int, bool) {
		v, valid := strictJSONInt(details[key])
		if !valid {
			if _, exists := details[key]; exists {
				delete(details, key)
				detailsChanged = true
			}
			return 0, false
		}
		if !isCanonicalJSONInt(details[key]) {
			details[key] = marshalInt64(v)
			detailsChanged = true
		}
		return int(v), true
	}

	_, cachedSet := sanitize("cached_tokens")
	if !cachedSet {
		// Nested aliases first, then usage-level ones; first match wins.
		for _, alias := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens"} {
			if v, ok := consumeAlias(details, alias); ok {
				details["cached_tokens"] = marshalInt(v)
				detailsChanged = true
				cachedSet = true
				break
			}
		}
		if !cachedSet {
			for _, alias := range []string{"cached_tokens", "prompt_cache_hit_tokens", "cache_read_input_tokens"} {
				if v, ok := consumeAlias(u, alias); ok {
					details["cached_tokens"] = marshalInt(v)
					patched = true
					detailsChanged = true
					cachedSet = true
					break
				}
			}
		}
	}
	_, writeSet := sanitize("cache_write_tokens")
	if !writeSet {
		for _, alias := range []string{"cache_creation_input_tokens", "cache_creation_tokens"} {
			if v, ok := consumeAlias(details, alias); ok {
				details["cache_write_tokens"] = marshalInt(v)
				detailsChanged = true
				writeSet = true
				break
			}
		}
		if !writeSet {
			for _, alias := range []string{"cache_write_tokens", "cache_creation_input_tokens", "cache_creation_tokens"} {
				if v, ok := consumeAlias(u, alias); ok {
					details["cache_write_tokens"] = marshalInt(v)
					patched = true
					detailsChanged = true
					writeSet = true
					break
				}
			}
		}
	}

	if detailsChanged {
		enc, err := json.Marshal(details)
		if err != nil {
			return patched
		}
		u[usageKeyDetails] = enc
		patched = true
	}
	return patched
}

// consumeAlias deletes and returns a supported integer alias. Deleting the
// consumed source keeps the rewritten usage unambiguous: no field that could
// be counted twice survives normalization.
func consumeAlias(u map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := u[key]
	if !ok {
		return 0, false
	}
	v, valid := strictJSONInt(raw)
	if !valid {
		return 0, false
	}
	delete(u, key)
	return int(v), true
}

// isCanonicalJSONInt reports whether raw already parses directly as an int64
// literal (plain JSON integer, not a quoted string or float form).
func isCanonicalJSONInt(raw json.RawMessage) bool {
	var i int64
	return json.Unmarshal(bytes.TrimSpace(raw), &i) == nil
}

func marshalInt64(v int64) json.RawMessage {
	return json.RawMessage(strconv.FormatInt(v, 10))
}

func marshalInt(n int) json.RawMessage {
	return json.RawMessage(strconv.Itoa(n))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// maxPeekBytes bounds the memory PeekFirstSSEFrame may invest while hunting
// the first data line. It matches the unwrap reader's 4MiB stream line
// ceiling: a first frame larger than this is never a queue refusal.
const maxPeekBytes = 4 << 20

// PeekFirstSSEFrame reads from r up to and including the first data line and
// returns every byte consumed plus that line's payload. It reads one byte at
// a time so nothing beyond the consumed frame is buffered away from the live
// stream: the caller replays the consumed prefix before handing the reader
// back.
//
// A data line terminated by EOF (no trailing newline) is still parsed. A real
// read error is returned together with any payload completed by the same
// bytes, so callers must inspect the payload even when err != nil. A payload
// of "" with nil err means the stream ended (or hit the size cap) before a
// data line completed.
func PeekFirstSSEFrame(r io.Reader) (consumed []byte, payload string, err error) {
	var one [1]byte
	var line []byte
	for {
		if len(consumed) >= maxPeekBytes {
			// The consumed prefix is replayed and the live reader re-reads the
			// remainder, so no bytes are lost; the cap only stops unbounded
			// memory growth during the peek.
			return consumed, "", nil
		}
		n, rerr := r.Read(one[:])
		var completed string
		if n > 0 {
			b := one[0]
			consumed = append(consumed, b)
			if b == '\n' {
				if pl, ok := dataLinePayload(line); ok {
					completed = pl
				}
				line = line[:0]
			} else {
				line = append(line, b)
			}
		}
		if completed != "" {
			// A read error that arrived in the same call as the frame's final
			// byte must not be swallowed by the successful parse.
			return consumed, completed, rerr
		}
		if rerr != nil {
			if rerr == io.EOF {
				// The last data line may lack its newline.
				if pl, ok := dataLinePayload(line); ok {
					return consumed, pl, nil
				}
				return consumed, "", nil
			}
			return consumed, "", rerr
		}
	}
}

// dataLinePayload extracts the payload of a complete SSE data line, ignoring
// comment, field, and blank lines. A data line without a payload is a
// keepalive, not a frame.
func dataLinePayload(line []byte) (string, bool) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" {
		return "", false
	}
	return payload, true
}
