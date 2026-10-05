package kiro

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"airouter/internal/proxy/ir"
)

// DecodeStream reads a Kiro binary AWS EventStream and emits IR stream events.
// It does not restore cloaked tool names. Production response paths use
// DecodeStreamTools with the original client declarations.
//
// Event mapping:
//   - assistantResponseEvent / codeEvent -> text delta (<thinking> tags stripped)
//   - reasoningContentEvent              -> reasoning delta
//   - toolUseEvent                       -> tool call start + argument delta
//   - metricsEvent / metadataEvent       -> usage, carried onto the finish event
//   - messageStopEvent / metadataEvent   -> terminal stop
//
// A selected transport must provide a terminal event. Clean EOF without one is
// an incomplete stream, not a successful end turn.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	return decodeStream(nil, r, emit, false, false)
}

// DecodeStreamTools reads a Kiro EventStream and restores wire tool names to
// the exact client names from clientTools. A nil or empty list is authoritative:
// decoy and unknown tool calls fail the stream instead of being forwarded.
// The catalog is rebuilt from the same declarations used at encode time.
func DecodeStreamTools(clientTools []ir.Tool, r io.Reader, emit func(ir.StreamEvent) error) error {
	return decodeStream(clientTools, r, emit, true, true)
}

// DecodeStreamToolsTransport is DecodeStreamTools with an explicit terminal
// requirement. Legacy CodeWhisperer keeps requireTerminal false so an older
// stream that ends at EOF still finishes. Runtime sets it true.
func DecodeStreamToolsTransport(clientTools []ir.Tool, r io.Reader, emit func(ir.StreamEvent) error, requireTerminal bool) error {
	return decodeStream(clientTools, r, emit, true, requireTerminal)
}

func decodeStream(clientTools []ir.Tool, r io.Reader, emit func(ir.StreamEvent) error, cloak, requireTerminal bool) error {
	var catalog toolCatalog
	if cloak {
		catalog = buildToolCatalog(clientTools)
	}
	started := false
	sawTool := false
	sawTerminal := false
	stop := ir.StopEndTurn
	inputTokens, outputTokens := 0, 0
	cacheRead, cacheWrite := 0, 0
	sawUsage := false

	// Tool calls are keyed by toolUseId. Each distinct id gets a monotonic index
	// so argument fragments attribute to the right call; a start event is emitted
	// the first time an id is seen. Request-aware decoding binds each id to its
	// validated wire name before accepting nameless continuation fragments.
	toolIndex := map[string]int{}
	toolWireNames := map[string]string{}
	nextIndex := 0

	ensureStarted := func() error {
		if started {
			return nil
		}
		started = true
		return emit(ir.StreamEvent{Kind: ir.EventMessageStart})
	}

	for {
		msg, err := readEventStreamMessage(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Smithy exception / error frames use :message-type rather than a content
		// :event-type. Surface them as failures without fabricating Finish.
		switch msg.headers[":message-type"] {
		case "exception", "error":
			return kiroStreamFailure(msg)
		}
		eventType := msg.headers[":event-type"]
		if failure, ok := typedEventFailure(eventType, msg.payload); ok {
			return failure
		}
		switch eventType {
		case "contextUsageEvent", "meteringEvent":
			// Context percentage and credit metering are not token usage.
			continue

		case "assistantResponseEvent", "codeEvent":
			var p struct {
				Content string `json:"content"`
			}
			if json.Unmarshal(msg.payload, &p) != nil || p.Content == "" {
				continue
			}
			text := stripThinkingTags(p.Content)
			if text == "" {
				continue
			}
			if err := ensureStarted(); err != nil {
				return err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: text}); err != nil {
				return err
			}

		case "reasoningContentEvent":
			var p struct {
				Text    string `json:"text"`
				Content string `json:"content"`
			}
			if json.Unmarshal(msg.payload, &p) != nil {
				continue
			}
			text := p.Text
			if text == "" {
				text = p.Content
			}
			if text == "" {
				continue
			}
			if err := ensureStarted(); err != nil {
				return err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EventReasoningDelta, Text: text}); err != nil {
				return err
			}

		case "toolUseEvent":
			var p struct {
				ToolUseID string          `json:"toolUseId"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				Stop      bool            `json:"stop"`
			}
			if json.Unmarshal(msg.payload, &p) != nil {
				continue
			}
			name := p.Name
			// Kiro must declare the tool name on the first frame for an id. Later
			// frames may omit or repeat that name, but cannot change it. Validate
			// before emitting identity or arguments; failures carry no wire data.
			if cloak {
				wireName, bound := toolWireNames[p.ToolUseID]
				if p.ToolUseID == "" || (!bound && name == "") || (bound && name != "" && name != wireName) {
					return &ir.StreamFailure{Type: "invalid_request_error", Message: "upstream tool call is not available"}
				}
				if !bound {
					restored, ok := catalog.resolveWireName(name)
					if !ok {
						return &ir.StreamFailure{Type: "invalid_request_error", Message: "upstream tool call is not available"}
					}
					toolWireNames[p.ToolUseID] = name
					name = restored
				}
			}
			if err := ensureStarted(); err != nil {
				return err
			}
			sawTool = true
			idx, ok := toolIndex[p.ToolUseID]
			if !ok {
				idx = nextIndex
				nextIndex++
				toolIndex[p.ToolUseID] = idx
				if err := emit(ir.StreamEvent{
					Kind: ir.EventToolCallStart, Index: idx, ToolID: p.ToolUseID, ToolName: name,
				}); err != nil {
					return err
				}
			}
			if frag := toolInputFragment(p.Input); frag != "" {
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: idx, ArgsFrag: frag}); err != nil {
					return err
				}
			}

		case "metricsEvent":
			// Base input excludes separately reported cache fields; fold cache-read
			// and cache-creation into the input total. Accept camelCase and snake_case
			// aliases; camel takes precedence when both are present. Missing fields stay 0.
			if applyUsage(&inputTokens, &outputTokens, &cacheRead, &cacheWrite, msg.payload) {
				sawUsage = true
				if err := ensureStarted(); err != nil {
					return err
				}
			}

		case "metadataEvent", "MetadataEvent":
			meta, ok := parseMetadataEvent(msg.payload)
			if !ok {
				return &ir.StreamFailure{Type: "api_error", Code: "invalid_metadata", Message: "upstream metadata is malformed"}
			}
			if applyUsage(&inputTokens, &outputTokens, &cacheRead, &cacheWrite, meta.usage) {
				sawUsage = true
			}
			if meta.stopPresent {
				mapped, known := mapStopReason(meta.stopReason)
				if !known {
					return unknownStopFailure()
				} else {
					stop = mapped
					sawTerminal = true
				}
				if err := ensureStarted(); err != nil {
					return err
				}
			} else if sawUsage {
				if err := ensureStarted(); err != nil {
					return err
				}
			}

		case "messageStopEvent":
			mapped, terminal, malformed := messageStop(msg.payload)
			if malformed {
				return unknownStopFailure()
			} else if terminal {
				stop = mapped
				sawTerminal = true
			}
			// A valid empty response may contain only a stop marker. Start a minimal
			// response so callers still receive the terminal event.
			if err := ensureStarted(); err != nil {
				return err
			}
		}
	}

	if !started {
		if requireTerminal {
			return &ir.StreamFailure{Type: "api_error", Message: "upstream stream ended without a terminal event"}
		}
		return nil
	}
	if requireTerminal && !sawTerminal {
		return &ir.StreamFailure{Type: "api_error", Message: "upstream stream ended without a terminal event"}
	}
	if sawTool && stop == ir.StopEndTurn {
		stop = ir.StopToolUse
	}
	return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stop, InputTokens: inputTokens, OutputTokens: outputTokens, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite})
}

func applyUsage(input, output, cacheRead, cacheWrite *int, raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var p struct {
		InputTokens                   int `json:"inputTokens"`
		OutputTokens                  int `json:"outputTokens"`
		UncachedInputTokens           int `json:"uncachedInputTokens"`
		CacheReadInputTokens          int `json:"cacheReadInputTokens"`
		CacheReadInputTokensSnake     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens      int `json:"cacheCreationInputTokens"`
		CacheCreationInputTokensSnake int `json:"cache_creation_input_tokens"`
		CacheWriteInputTokens         int `json:"cacheWriteInputTokens"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	read := p.CacheReadInputTokens
	if read == 0 {
		read = p.CacheReadInputTokensSnake
	}
	write := p.CacheCreationInputTokens
	if write == 0 {
		write = p.CacheCreationInputTokensSnake
	}
	if write == 0 {
		write = p.CacheWriteInputTokens
	}
	base := p.InputTokens
	if base == 0 && p.UncachedInputTokens > 0 {
		base = p.UncachedInputTokens
	}
	*cacheRead = read
	*cacheWrite = write
	*input = base + read + write
	*output = p.OutputTokens
	return true
}

type metadataEvent struct {
	usage       json.RawMessage
	stopReason  string
	stopPresent bool
}

// parseMetadataEvent keeps stopReason and token usage when stopDetails is an
// object, null, missing, or an unknown nested shape. stopDetails is not a stop
// reason and does not replace an explicit stop reason. An explicit unknown stop
// stays present so the caller can fail instead of treating the frame as success.
func parseMetadataEvent(payload []byte) (metadataEvent, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil || raw == nil {
		return metadataEvent{}, false
	}
	out := metadataEvent{usage: firstRaw(raw, "tokenUsage", "usage")}
	if stopRaw, ok := firstPresent(raw, "stopReason", "stop_reason"); ok {
		out.stopPresent = true
		out.stopReason = jsonString(stopRaw)
		if out.stopReason == "" && !isJSONNull(stopRaw) {
			// A non-string explicit stop is not a recognized terminal.
			out.stopReason = "unrecognized_stop"
		}
	}
	return out, true
}

func firstPresent(raw map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if v, ok := raw[key]; ok {
			return v, true
		}
	}
	return nil, false
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// messageStop accepts the legacy empty stop marker. An explicit stop must be a
// recognized value. Malformed JSON and unknown nonempty values are unsafe and
// must not become a successful finish, including when a later empty marker or
// EOF follows.
func messageStop(payload []byte) (ir.StopReason, bool, bool) {
	trimmed := bytes.TrimSpace(payload)
	if string(trimmed) == "{}" {
		return ir.StopEndTurn, true, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw == nil {
		return "", false, true
	}
	stopRaw, present := firstPresent(raw, "stopReason", "stop_reason")
	if !present {
		return ir.StopEndTurn, true, false
	}
	if isJSONNull(stopRaw) {
		return "", false, true
	}
	reason := jsonString(stopRaw)
	if reason == "" {
		return "", false, true
	}
	mapped, known := mapStopReason(reason)
	if !known {
		return "", false, true
	}
	return mapped, true, false
}

func unknownStopFailure() *ir.StreamFailure {
	return &ir.StreamFailure{Type: "api_error", Code: "unknown_stop", Message: "upstream stop reason is not recognized"}
}

// typedEventFailure maps the Runtime event types that the official parser
// throws. The payload is reduced to a known message; the raw body is not
// forwarded. Header exceptions are handled before this path.
func typedEventFailure(eventType string, payload []byte) (*ir.StreamFailure, bool) {
	var kind, code, fallback string
	switch eventType {
	case "error", "InternalServerException":
		kind, code, fallback = "api_error", "internal_server_error", "Internal server error"
	case "throttlingError", "ThrottlingException":
		kind, code, fallback = "rate_limit_error", "throttling", "Too many requests"
	case "validationError", "ValidationException":
		kind, code, fallback = "invalid_request_error", "validation", "Invalid request"
	case "serviceUnavailableError", "ServiceUnavailableException":
		kind, code, fallback = "service_unavailable_error", "service_unavailable", "Service unavailable"
	default:
		return nil, false
	}
	msg := jsonMessage(payload)
	if msg == "" {
		msg = fallback
	}
	return &ir.StreamFailure{Type: kind, Code: code, Message: msg}, true
}

func jsonMessage(payload []byte) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal(payload, &raw) != nil {
		return ""
	}
	msg := jsonString(firstRaw(raw, "message", "Message"))
	if msg == "" {
		return ""
	}
	return truncateUTF8Bytes(msg, 240)
}

// truncateUTF8Bytes keeps the 240-byte message budget and moves the cut to a
// UTF-8 boundary. A 240-rune cut would expand that budget.
func truncateUTF8Bytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func firstRaw(raw map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, key := range keys {
		if v, ok := raw[key]; ok && len(bytes.TrimSpace(v)) > 0 && string(bytes.TrimSpace(v)) != "null" {
			return v
		}
	}
	return nil
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// mapStopReason maps a known Kiro stop value. Camel-case and compact forms are
// normalized before matching. Unknown values and unsafe terminal states are not
// treated as a successful completion.
func mapStopReason(reason string) (ir.StopReason, bool) {
	switch normalizeStopReason(reason) {
	case "":
		return "", false
	case "end_turn", "stop", "complete":
		return ir.StopEndTurn, true
	case "tool_use", "tool_calls":
		return ir.StopToolUse, true
	case "max_tokens", "max_output_tokens", "length":
		return ir.StopMaxTokens, true
	case "stop_sequence":
		return ir.StopStopSequence, true
	case "content_filtered", "refusal", "guardrail_intervened":
		// IR has no refusal stop. Max tokens is the existing non-success terminal
		// that does not claim a clean end turn or a completed tool call. The
		// refusal category, explanation, and recommended model are not forwarded.
		return ir.StopMaxTokens, true
	default:
		return "", false
	}
}

func normalizeStopReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	var b strings.Builder
	var prev rune
	for i, r := range reason {
		if r == '-' || r == ' ' || r == '\t' {
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
			prev = r
			continue
		}
		if r >= 'A' && r <= 'Z' {
			if i > 0 && prev >= 'a' && prev <= 'z' && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// kiroStreamFailure parses known safe fields from an EventStream exception frame.
func kiroStreamFailure(msg *esMessage) *ir.StreamFailure {
	sf := &ir.StreamFailure{
		Type: msg.headers[":exception-type"],
		Code: msg.headers[":error-code"],
	}
	if sf.Type == "" {
		sf.Type = msg.headers[":event-type"]
	}
	var p struct {
		Message string `json:"message"`
		Msg     string `json:"Message"`
	}
	if json.Unmarshal(msg.payload, &p) == nil {
		sf.Message = p.Message
		if sf.Message == "" {
			sf.Message = p.Msg
		}
	}
	if sf.Message == "" {
		sf.Message = "upstream stream failed"
	}
	return sf
}

// toolInputFragment renders a toolUseEvent input to a JSON argument fragment.
// The upstream may send input as either a JSON string fragment (already a piece
// of the arguments) or a JSON value; a string is used verbatim, a value is
// marshaled.
func toolInputFragment(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

// thinkingTag matches a leaked <thinking>...</thinking> span (or a stray opening
// or closing tag) that Kiro sometimes embeds in assistantResponseEvent content.
var thinkingTag = regexp.MustCompile(`(?s)</?thinking>`)

// stripThinkingTags removes leaked <thinking> tags from assistant text. Only the
// tags are removed, not the enclosed text, to avoid dropping content when a span
// is split across streamed fragments.
func stripThinkingTags(s string) string {
	if !strings.Contains(s, "thinking>") {
		return s
	}
	return thinkingTag.ReplaceAllString(s, "")
}
