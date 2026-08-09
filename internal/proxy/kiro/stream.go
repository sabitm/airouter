package kiro

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"airouter/internal/proxy/ir"
)

// DecodeStream reads a Kiro binary AWS EventStream and emits IR stream events.
// Kiro is stream-only, so this is the sole response direction (a unary client
// request is collected from these same events upstream in the proxy).
//
// Event mapping (see KIRO.md 5.4):
//   - assistantResponseEvent / codeEvent -> text delta (<thinking> tags stripped)
//   - toolUseEvent                       -> tool call start + argument delta
//   - metricsEvent                       -> usage, carried onto the finish event
//   - messageStopEvent / end-of-stream   -> finish
//
// The IR has no reasoning field, so reasoningContentEvent folds into text.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	started := false
	sawTool := false
	stop := ir.StopEndTurn
	inputTokens, outputTokens := 0, 0

	// Tool calls are keyed by toolUseId. Each distinct id gets a monotonic index
	// so argument fragments attribute to the right call; a start event is emitted
	// the first time an id is seen.
	toolIndex := map[string]int{}
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
		eventType := msg.headers[":event-type"]
		switch eventType {
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
			if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: text}); err != nil {
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
					Kind: ir.EventToolCallStart, Index: idx, ToolID: p.ToolUseID, ToolName: p.Name,
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
			var p struct {
				InputTokens                   int `json:"inputTokens"`
				OutputTokens                  int `json:"outputTokens"`
				CacheReadInputTokens          int `json:"cacheReadInputTokens"`
				CacheReadInputTokensSnake     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens      int `json:"cacheCreationInputTokens"`
				CacheCreationInputTokensSnake int `json:"cache_creation_input_tokens"`
			}
			if json.Unmarshal(msg.payload, &p) == nil {
				if err := ensureStarted(); err != nil {
					return err
				}
				cacheRead := p.CacheReadInputTokens
				if cacheRead == 0 {
					cacheRead = p.CacheReadInputTokensSnake
				}
				cacheCreation := p.CacheCreationInputTokens
				if cacheCreation == 0 {
					cacheCreation = p.CacheCreationInputTokensSnake
				}
				inputTokens = p.InputTokens + cacheRead + cacheCreation
				outputTokens = p.OutputTokens
			}

		case "messageStopEvent":
			// A valid empty response may contain only a stop marker. Start a minimal
			// response so callers still receive the terminal event.
			if err := ensureStarted(); err != nil {
				return err
			}
		}
	}

	if !started {
		return nil
	}
	if sawTool {
		stop = ir.StopToolUse
	}
	return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stop, InputTokens: inputTokens, OutputTokens: outputTokens})
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
