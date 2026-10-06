package anthropic

import (
	"encoding/json"
	"io"
	"sort"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// --- streaming event wire types ---

type streamMessageStart struct {
	Message struct {
		ID    string    `json:"id"`
		Model string    `json:"model"`
		Usage anthUsage `json:"usage"`
	} `json:"message"`
}

type streamContentBlockStart struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
	} `json:"content_block"`
}

type streamContentBlockDelta struct {
	Index int `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

type streamContentBlockStop struct {
	Index int `json:"index"`
}

type streamMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage streamDeltaUsage `json:"usage"`
}

// streamDeltaUsage is a cumulative Anthropic usage snapshot. Pointers distinguish
// omitted fields (keep prior partition) from an explicit zero (replace).
type streamDeltaUsage struct {
	InputTokens              *int `json:"input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
}

// DecodeStream reads an Anthropic Messages SSE stream and emits IR stream
// events. Used when Anthropic is the backend format. The block index from the
// wire is carried through as StreamEvent.Index so the ingress encoder can
// attribute tool argument fragments correctly.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	reader := sse.NewReader(r)
	var stopReason ir.StopReason = ir.StopEndTurn
	ordinary, cacheRead, cacheWrite := 0, 0, 0
	inputTokens, outputTokens := 0, 0
	finished := false
	started := false
	// thinkingOpen is the set of source indexes whose explicit thinking block
	// has not stopped. Completed indexes are deleted so the map stays bounded
	// by simultaneously open blocks, not stream history. A new explicit thinking
	// block cannot start while one remains open. Text, tool, and redacted
	// indexes may create gaps; those indexes are not required to be contiguous.
	thinkingOpen := map[int]thinkingDecodeState{}
	// stoppedThrough is the highest stopped source index. An index at or below
	// it cannot open again. This avoids retaining every completed block and
	// does not require thinking indexes to be contiguous.
	stoppedThrough := -1
	inclusiveInput := func() int { return ordinary + cacheRead + cacheWrite }

	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch ev.Name {
		case "error":
			var env struct {
				Error *struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(ev.Data, &env) != nil || env.Error == nil {
				return &ir.StreamFailure{Message: "upstream stream failed"}
			}
			sf := &ir.StreamFailure{Type: env.Error.Type, Message: env.Error.Message}
			if sf.Message == "" {
				sf.Message = "upstream stream failed"
			}
			return sf
		case "message_start":
			var m streamMessageStart
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			ordinary = m.Message.Usage.InputTokens
			cacheRead = m.Message.Usage.CacheReadInputTokens
			cacheWrite = m.Message.Usage.CacheCreationInputTokens
			inputTokens = inclusiveInput()
			if err := emit(ir.StreamEvent{
				Kind: ir.EventMessageStart, ID: m.Message.ID, Model: m.Message.Model,
				InputTokens:     inputTokens,
				CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
			}); err != nil {
				return err
			}
			started = true
		case "content_block_start":
			var s streamContentBlockStart
			if json.Unmarshal(ev.Data, &s) != nil {
				continue
			}
			switch s.ContentBlock.Type {
			case "tool_use":
				if len(thinkingOpen) != 0 {
					return ir.ProtocolError("content block started while thinking block is open")
				}
				if err := emit(ir.StreamEvent{
					Kind: ir.EventToolCallStart, Index: s.Index,
					ToolID: s.ContentBlock.ID, ToolName: s.ContentBlock.Name,
				}); err != nil {
					return err
				}
				started = true
			case "thinking":
				if len(thinkingOpen) != 0 {
					return ir.ProtocolError("thinking block started while another is open")
				}
				if s.Index <= stoppedThrough {
					return ir.ProtocolError("thinking block restarted after stop")
				}
				if len(thinkingOpen) >= maxOpenThinkingBlocks {
					return ir.ProtocolError("too many open thinking blocks")
				}
				thinkingOpen[s.Index] = thinkingDecodeState{signed: s.ContentBlock.Signature != ""}
				if err := emit(ir.StreamEvent{
					Kind: ir.EventReasoningStart, Index: s.Index,
					Text: s.ContentBlock.Thinking, Signature: s.ContentBlock.Signature,
				}); err != nil {
					return err
				}
				started = true
			case "redacted_thinking":
				if len(thinkingOpen) != 0 {
					return ir.ProtocolError("content block started while thinking block is open")
				}
				// The data arrives complete on start. Do not invent readable text.
				if err := emit(ir.StreamEvent{Kind: ir.EventRedactedReasoning, Index: s.Index, Data: s.ContentBlock.Data}); err != nil {
					return err
				}
				started = true
			case "text":
				if len(thinkingOpen) != 0 {
					return ir.ProtocolError("content block started while thinking block is open")
				}
			}
		case "content_block_delta":
			var d streamContentBlockDelta
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			switch d.Delta.Type {
			case "thinking_delta":
				st, open := thinkingOpen[d.Index]
				if !open {
					return ir.ProtocolError("thinking delta without an open thinking block")
				}
				// A signature, including one carried on content_block_start, closes
				// the readable portion. Later thinking text is not replayed.
				if st.signed {
					return ir.ProtocolError("thinking delta after signature")
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventReasoningDelta, Index: d.Index, Text: d.Delta.Thinking, Indexed: true}); err != nil {
					return err
				}
				started = true
			case "signature_delta":
				st, open := thinkingOpen[d.Index]
				if !open {
					return ir.ProtocolError("signature delta without an open thinking block")
				}
				st.signed = true
				thinkingOpen[d.Index] = st
				if err := emit(ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: d.Index, Signature: d.Delta.Signature}); err != nil {
					return err
				}
				started = true
			case "text_delta":
				if len(thinkingOpen) != 0 {
					return ir.ProtocolError("text delta while thinking block is open")
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: d.Delta.Text}); err != nil {
					return err
				}
				started = true
			case "input_json_delta":
				if _, open := thinkingOpen[d.Index]; open {
					return ir.ProtocolError("tool delta while thinking block is open")
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: d.Index, ArgsFrag: d.Delta.PartialJSON}); err != nil {
					return err
				}
				started = true
			}
		case "content_block_stop":
			var s streamContentBlockStop
			if json.Unmarshal(ev.Data, &s) != nil {
				continue
			}
			if _, open := thinkingOpen[s.Index]; open {
				delete(thinkingOpen, s.Index)
				if s.Index > stoppedThrough {
					stoppedThrough = s.Index
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventReasoningEnd, Index: s.Index}); err != nil {
					return err
				}
				started = true
			}
		case "message_delta":
			var m streamMessageDelta
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			stopReason = stopReason2(m.Delta.StopReason)
			if m.Usage.InputTokens != nil {
				ordinary = *m.Usage.InputTokens
			}
			if m.Usage.CacheReadInputTokens != nil {
				cacheRead = *m.Usage.CacheReadInputTokens
			}
			if m.Usage.CacheCreationInputTokens != nil {
				cacheWrite = *m.Usage.CacheCreationInputTokens
			}
			inputTokens = inclusiveInput()
			outputTokens = m.Usage.OutputTokens
		case "message_stop":
			if len(thinkingOpen) != 0 {
				return ir.ProtocolError("thinking block closed by stream end")
			}
			if err := emit(ir.StreamEvent{
				Kind: ir.EventFinish, StopReason: stopReason,
				InputTokens: inputTokens, OutputTokens: outputTokens,
				CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
			}); err != nil {
				return err
			}
			finished = true
		}
	}
	if !finished {
		if !started {
			// Empty / comment-only stream: do not fabricate a successful completion.
			return nil
		}
		if len(thinkingOpen) != 0 {
			// An open thinking block has no signature association yet. Do not
			// synthesize a finish that looks like valid signed history.
			return ir.ProtocolError("thinking block closed by stream end")
		}
		return emit(ir.StreamEvent{
			Kind: ir.EventFinish, StopReason: stopReason,
			InputTokens: inputTokens, OutputTokens: outputTokens,
			CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
		})
	}
	if len(thinkingOpen) != 0 {
		return ir.ProtocolError("thinking block closed by stream end")
	}
	return nil
}

// thinkingDecodeState tracks one open source thinking block. signed means a
// signature has already arrived, so a later thinking_delta is invalid.
type thinkingDecodeState struct {
	signed bool
}

func stopReason2(s string) ir.StopReason {
	if s == "" {
		return ir.StopEndTurn
	}
	return stopReason(s)
}

// open block kinds tracked by the encoder.
const (
	blockNone = iota
	blockText
	blockReasoning
	blockTool
)

// StreamEncoder renders IR stream events as an Anthropic Messages SSE stream.
// It manages content-block indices and start/stop framing, which the flat
// OpenAI delta stream does not carry. Used when Anthropic is the ingress format.
//
// Explicit reasoning events keep the source block index. Adjacent thinking
// blocks are not merged. A signature is emitted before the matching stop and
// is not buffered across later blocks.
type StreamEncoder struct {
	id            string
	model         string
	inputTokens   int
	cacheRead     int
	cacheWrite    int
	started       bool
	startWire     anthUsage // partition actually sent on message_start
	openKind      int
	openIndex     int
	nextIndex     int
	tools         map[int]*streamTool
	openToolIdx   []int // anthropic block indices started and not yet stopped
	pendingBytes  int
	finishEmitted bool

	// thinking holds only open explicit thinking blocks, keyed by source index.
	// A stopped block is removed. stoppedThrough is the highest stopped source
	// index and rejects reuse. Indexes may have gaps; this is not a sequential
	// index check and does not retain completed history.
	thinking       map[int]*thinkingBlock
	stoppedThrough int
	legacyOpen     bool
	legacyBlock    int
}

// maxOpenThinkingBlocks bounds simultaneously open source thinking blocks.
// Completed blocks are released, so this is not a history limit.
const maxOpenThinkingBlocks = 64

// thinkingBlock is one open source-indexed thinking block. block is its wire
// index after content_block_start. A signature before the start is a protocol
// error; signatures are not queued out of order.
type thinkingBlock struct {
	block  int
	signed bool
}

// streamTool is one IR-index tool call. block is -1 until content_block_start
// has been written; identity may arrive across repeated Starts and argument
// fragments may precede the first Start.
type streamTool struct {
	block   int
	id      string
	name    string
	pending string
}

func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{
		model: model, openKind: blockNone,
		tools: map[int]*streamTool{}, thinking: map[int]*thinkingBlock{},
		stoppedThrough: -1,
	}
}

// maxPendingArgs bounds fragments buffered for an index whose Start has not
// arrived yet, so a stream that never resolves identity cannot retain
// unbounded bytes.
const maxPendingArgs = 1 << 20

func (e *StreamEncoder) applyUsage(ev ir.StreamEvent) {
	if ev.InputTokens != 0 {
		e.inputTokens = ev.InputTokens
	}
	if ev.CacheReadTokens != 0 {
		e.cacheRead = ev.CacheReadTokens
	}
	if ev.CacheWriteTokens != 0 {
		e.cacheWrite = ev.CacheWriteTokens
	}
}

func anthUsageMap(u ir.Usage, output int) map[string]any {
	a := usageToAnth(u)
	m := map[string]any{"input_tokens": a.InputTokens, "output_tokens": output}
	if a.CacheReadInputTokens != 0 {
		m["cache_read_input_tokens"] = a.CacheReadInputTokens
	}
	if a.CacheCreationInputTokens != 0 {
		m["cache_creation_input_tokens"] = a.CacheCreationInputTokens
	}
	return m
}

func (e *StreamEncoder) event(w *sse.Writer, name string, payload map[string]any) error {
	payload["type"] = name
	raw, _ := json.Marshal(payload)
	return w.WriteEvent(name, raw)
}

func (e *StreamEncoder) ensureStart(w *sse.Writer) error {
	if e.started {
		return nil
	}
	e.started = true
	id := e.id
	if id == "" {
		id = ir.NewID("msg_")
	}
	startUsage := ir.Usage{
		InputTokens: e.inputTokens, CacheReadTokens: e.cacheRead, CacheWriteTokens: e.cacheWrite,
	}
	e.startWire = usageToAnth(startUsage)
	return e.event(w, "message_start", map[string]any{
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": e.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthUsageMap(startUsage, 0),
		},
	})
}

func (e *StreamEncoder) explicitThinkingOpen() bool {
	return len(e.thinking) != 0
}

func (e *StreamEncoder) closeBlock(w *sse.Writer) error {
	if e.openKind != blockText && e.openKind != blockReasoning {
		return nil
	}
	// An explicit thinking stop must come from EventReasoningEnd. Closing it
	// here would fabricate a successful stop for an invalid transition.
	if e.explicitThinkingOpen() && e.openKind == blockReasoning {
		return ir.ProtocolError("thinking block closed by invalid transition")
	}
	idx := e.openIndex
	e.openKind = blockNone
	// Any automatic close must clear the matching legacy record. A later text
	// or reasoning segment otherwise writes into a block that already stopped.
	// Explicit thinking is never released here; its stop is EventReasoningEnd.
	if e.legacyOpen && e.legacyBlock == idx {
		e.legacyOpen = false
	}
	return e.event(w, "content_block_stop", map[string]any{"index": idx})
}

func (e *StreamEncoder) tool(irIndex int) *streamTool {
	if t, ok := e.tools[irIndex]; ok {
		return t
	}
	t := &streamTool{block: -1}
	e.tools[irIndex] = t
	return t
}

func (e *StreamEncoder) bufferPending(t *streamTool, frag string) {
	if frag == "" || e.pendingBytes+len(frag) > maxPendingArgs {
		return
	}
	t.pending += frag
	e.pendingBytes += len(frag)
}

// openToolBlock writes content_block_start once the tool name is known.
// Fragments buffered before that are flushed immediately after so the wire
// stays message/block-before-delta. Id-only Starts wait for the name so a
// split identity does not open a nameless block.
func (e *StreamEncoder) openToolBlock(w *sse.Writer, t *streamTool) error {
	if t.block >= 0 || t.name == "" {
		return nil
	}
	if e.openKind != blockNone && e.openKind != blockTool {
		if err := e.closeBlock(w); err != nil {
			return err
		}
	}
	e.openKind = blockTool
	t.block = e.nextIndex
	e.nextIndex++
	e.openIndex = t.block
	e.openToolIdx = append(e.openToolIdx, t.block)
	if t.id == "" {
		t.id = ir.NewID("toolu_")
	}
	if err := e.event(w, "content_block_start", map[string]any{
		"index": t.block,
		"content_block": map[string]any{
			"type": "tool_use", "id": t.id, "name": t.name, "input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	return e.flushToolPending(w, t)
}

func (e *StreamEncoder) flushToolPending(w *sse.Writer, t *streamTool) error {
	if t.block < 0 || t.pending == "" {
		return nil
	}
	frag := t.pending
	t.pending = ""
	e.pendingBytes -= len(frag)
	return e.event(w, "content_block_delta", map[string]any{
		"index": t.block, "delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
	})
}

func (e *StreamEncoder) sourceStopped(index int) bool {
	return index <= e.stoppedThrough
}

func (e *StreamEncoder) releaseThinking(index int) {
	delete(e.thinking, index)
	if index > e.stoppedThrough {
		e.stoppedThrough = index
	}
}

func (e *StreamEncoder) closeLegacyReasoning(w *sse.Writer) error {
	if !e.legacyOpen {
		return nil
	}
	e.legacyOpen = false
	if e.openKind == blockReasoning && e.openIndex == e.legacyBlock {
		e.openKind = blockNone
	}
	return e.event(w, "content_block_stop", map[string]any{"index": e.legacyBlock})
}

func (e *StreamEncoder) openThinking(w *sse.Writer, index int, initial, signature string) (*thinkingBlock, error) {
	if e.explicitThinkingOpen() {
		return nil, ir.ProtocolError("thinking block started while another is open")
	}
	if e.legacyOpen {
		if err := e.closeLegacyReasoning(w); err != nil {
			return nil, err
		}
	}
	if e.openKind == blockText || e.openKind == blockReasoning {
		if err := e.closeBlock(w); err != nil {
			return nil, err
		}
	}
	block := map[string]any{"type": "thinking", "thinking": initial}
	if signature != "" {
		block["signature"] = signature
	}
	tb := &thinkingBlock{block: e.nextIndex, signed: signature != ""}
	e.nextIndex++
	e.thinking[index] = tb
	e.openKind = blockReasoning
	e.openIndex = tb.block
	if err := e.event(w, "content_block_start", map[string]any{
		"index": tb.block, "content_block": block,
	}); err != nil {
		return nil, err
	}
	return tb, nil
}

func (e *StreamEncoder) stopThinking(w *sse.Writer, index int, tb *thinkingBlock) error {
	wire := tb.block
	if e.openKind == blockReasoning && e.openIndex == wire {
		e.openKind = blockNone
	}
	e.releaseThinking(index)
	return e.event(w, "content_block_stop", map[string]any{"index": wire})
}

func (e *StreamEncoder) closeOpenTools(w *sse.Writer) error {
	if len(e.openToolIdx) == 0 {
		return nil
	}
	idxs := append([]int(nil), e.openToolIdx...)
	sort.Ints(idxs)
	e.openToolIdx = nil
	e.openKind = blockNone
	for _, idx := range idxs {
		if err := e.event(w, "content_block_stop", map[string]any{"index": idx}); err != nil {
			return err
		}
	}
	return nil
}

func (e *StreamEncoder) Encode(ev ir.StreamEvent, w *sse.Writer) error {
	switch ev.Kind {
	case ir.EventMessageStart:
		e.id = ev.ID
		if ev.Model != "" {
			e.model = ev.Model
		}
		e.applyUsage(ev)
		return e.ensureStart(w)

	case ir.EventTextDelta:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("text delta while thinking block is open")
		}
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if e.openKind != blockText {
			if err := e.closeBlock(w); err != nil {
				return err
			}
			e.openIndex = e.nextIndex
			e.nextIndex++
			e.openKind = blockText
			if err := e.event(w, "content_block_start", map[string]any{
				"index": e.openIndex, "content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": e.openIndex, "delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})

	case ir.EventReasoningStart:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("thinking block started while another is open")
		}
		if _, open := e.thinking[ev.Index]; open {
			return ir.ProtocolError("thinking block started twice")
		}
		if e.sourceStopped(ev.Index) {
			return ir.ProtocolError("thinking block restarted after stop")
		}
		if len(e.thinking) >= maxOpenThinkingBlocks {
			return ir.ProtocolError("too many open thinking blocks")
		}
		if err := e.ensureStart(w); err != nil {
			return err
		}
		_, err := e.openThinking(w, ev.Index, ev.Text, ev.Signature)
		return err

	case ir.EventReasoningDelta:
		if ev.Indexed {
			if err := e.ensureStart(w); err != nil {
				return err
			}
			tb, ok := e.thinking[ev.Index]
			if !ok {
				return ir.ProtocolError("thinking delta without an open thinking block")
			}
			if tb.signed {
				return ir.ProtocolError("thinking delta after signature")
			}
			return e.event(w, "content_block_delta", map[string]any{
				"index": tb.block, "delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
			})
		}
		// Legacy unindexed reasoning stays one open block. It is not mixed into
		// an explicit source-indexed thinking block. A later text segment closes
		// it; the next unindexed delta opens a new block.
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("reasoning delta while thinking block is open")
		}
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if !e.legacyOpen {
			if e.openKind == blockText || e.openKind == blockReasoning {
				if err := e.closeBlock(w); err != nil {
					return err
				}
			}
			e.legacyBlock = e.nextIndex
			e.nextIndex++
			e.legacyOpen = true
			e.openKind = blockReasoning
			e.openIndex = e.legacyBlock
			if err := e.event(w, "content_block_start", map[string]any{
				"index": e.legacyBlock, "content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); err != nil {
				return err
			}
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": e.legacyBlock, "delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
		})

	case ir.EventReasoningSignature:
		if err := e.ensureStart(w); err != nil {
			return err
		}
		tb, ok := e.thinking[ev.Index]
		if !ok {
			return ir.ProtocolError("signature delta without an open thinking block")
		}
		tb.signed = true
		return e.event(w, "content_block_delta", map[string]any{
			"index": tb.block, "delta": map[string]any{"type": "signature_delta", "signature": ev.Signature},
		})

	case ir.EventReasoningEnd:
		tb, ok := e.thinking[ev.Index]
		if !ok {
			return ir.ProtocolError("thinking stop without an open thinking block")
		}
		return e.stopThinking(w, ev.Index, tb)

	case ir.EventRedactedReasoning:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("redacted block while thinking block is open")
		}
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if e.legacyOpen {
			if err := e.closeLegacyReasoning(w); err != nil {
				return err
			}
		}
		if e.openKind == blockText || e.openKind == blockReasoning {
			if err := e.closeBlock(w); err != nil {
				return err
			}
		}
		idx := e.nextIndex
		e.nextIndex++
		if err := e.event(w, "content_block_start", map[string]any{
			"index":         idx,
			"content_block": map[string]any{"type": "redacted_thinking", "data": ev.Data},
		}); err != nil {
			return err
		}
		return e.event(w, "content_block_stop", map[string]any{"index": idx})

	case ir.EventToolCallStart:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("tool start while thinking block is open")
		}
		if err := e.ensureStart(w); err != nil {
			return err
		}
		t := e.tool(ev.Index)
		if ev.ToolID != "" {
			t.id = ev.ToolID
		}
		if ev.ToolName != "" {
			t.name = ev.ToolName
		}
		return e.openToolBlock(w, t)

	case ir.EventToolCallDelta:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("tool delta while thinking block is open")
		}
		t := e.tool(ev.Index)
		if t.block < 0 {
			// No content_block_start yet: buffer instead of emitting a delta
			// outside any content block (or onto whichever block happens to be
			// open, which would merge another call's arguments into it).
			e.bufferPending(t, ev.ArgsFrag)
			return nil
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": t.block, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ArgsFrag},
		})

	case ir.EventFinish:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("thinking block closed by stream end")
		}
		startEmitted := e.started
		// OpenAI-family backends report input at Finish. Capture it before
		// ensureStart so a Finish-first stream puts the count on message_start.
		// Do not reset a nonzero start count with absent later fields.
		e.applyUsage(ev)
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if err := e.closeLegacyReasoning(w); err != nil {
			return err
		}
		if err := e.closeBlock(w); err != nil {
			return err
		}
		if err := e.closeOpenTools(w); err != nil {
			return err
		}
		usage := map[string]any{"output_tokens": ev.OutputTokens}
		// message_start already went out: expose a late/changed wire partition
		// on the terminal delta. Compare ordinary input against the start frame,
		// not inclusive totals, so a cache-only finish still corrects input_tokens.
		if startEmitted {
			part := usageToAnth(ir.Usage{
				InputTokens: e.inputTokens, CacheReadTokens: e.cacheRead, CacheWriteTokens: e.cacheWrite,
			})
			if part.InputTokens != e.startWire.InputTokens {
				usage["input_tokens"] = part.InputTokens
			}
			if part.CacheReadInputTokens != 0 && part.CacheReadInputTokens != e.startWire.CacheReadInputTokens {
				usage["cache_read_input_tokens"] = part.CacheReadInputTokens
			}
			if part.CacheCreationInputTokens != 0 && part.CacheCreationInputTokens != e.startWire.CacheCreationInputTokens {
				usage["cache_creation_input_tokens"] = part.CacheCreationInputTokens
			}
		}
		if err := e.event(w, "message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": stopReasonWire(ev.StopReason), "stop_sequence": nil},
			"usage": usage,
		}); err != nil {
			return err
		}
		e.finishEmitted = true
		return e.event(w, "message_stop", map[string]any{})
	}
	return nil
}

// EncodeError writes a terminal Anthropic error event. No message_stop follows.
func (e *StreamEncoder) EncodeError(w *sse.Writer, message, errType string) error {
	if errType == "" {
		errType = "api_error"
	}
	raw := EncodeError(message, errType)
	return w.WriteEvent("error", raw)
}

func (e *StreamEncoder) Close(w *sse.Writer) error {
	if !e.started || e.finishEmitted {
		return nil
	}
	// A missing explicit stop is a sequence error. Do not invent message_stop.
	if e.explicitThinkingOpen() {
		return ir.ProtocolError("thinking block closed by stream end")
	}
	if err := e.closeLegacyReasoning(w); err != nil {
		return err
	}
	if err := e.closeBlock(w); err != nil {
		return err
	}
	if err := e.closeOpenTools(w); err != nil {
		return err
	}
	if err := e.event(w, "message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0},
	}); err != nil {
		return err
	}
	return e.event(w, "message_stop", map[string]any{})
}
