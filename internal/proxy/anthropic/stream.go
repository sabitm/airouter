package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// --- streaming event wire types ---

type streamMessageStart struct {
	Message struct {
		ID                string          `json:"id"`
		Model             string          `json:"model"`
		Usage             anthUsage       `json:"usage"`
		Container         json.RawMessage `json:"container"`
		ContextManagement json.RawMessage `json:"context_management"`
	} `json:"message"`
}

type streamContentBlockStart struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type      string            `json:"type"`
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Text      string            `json:"text"`
		Thinking  string            `json:"thinking"`
		Signature string            `json:"signature"`
		Data      string            `json:"data"`
		Citations []json.RawMessage `json:"citations"`
	} `json:"content_block"`
}

type streamContentBlockDelta struct {
	Index int `json:"index"`
	Delta struct {
		Type             string          `json:"type"`
		Text             string          `json:"text"`
		Thinking         string          `json:"thinking"`
		Signature        string          `json:"signature"`
		PartialJSON      string          `json:"partial_json"`
		Citation         json.RawMessage `json:"citation"`
		Content          json.RawMessage `json:"content"`
		EncryptedContent json.RawMessage `json:"encrypted_content"`
	} `json:"delta"`
}

type streamContentBlockStop struct {
	Index int `json:"index"`
}

type streamMessageDelta struct {
	Delta struct {
		StopReason   string          `json:"stop_reason"`
		StopSequence *string         `json:"stop_sequence"`
		StopDetails  json.RawMessage `json:"stop_details"`
		Container    json.RawMessage `json:"container"`
	} `json:"delta"`
	ContextManagement    json.RawMessage  `json:"context_management"`
	InputTransformations json.RawMessage  `json:"input_transformations"`
	Usage                streamDeltaUsage `json:"usage"`
}

// streamDeltaUsage is a cumulative Anthropic usage snapshot. Pointers distinguish
// omitted fields (keep prior partition) from an explicit zero (replace).
type streamDeltaUsage struct {
	InputTokens              *int `json:"input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
}

func quoteJSON(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(raw)
}

func rawContentBlock(data []byte) json.RawMessage {
	var wrap struct {
		ContentBlock json.RawMessage `json:"content_block"`
	}
	if json.Unmarshal(data, &wrap) != nil {
		return nil
	}
	return wrap.ContentBlock
}

// DecodeStreamOptions selects optional Anthropic stream completion rules.
// The zero value preserves generic Anthropic and OpenCode Messages behavior.
type DecodeStreamOptions struct {
	// StrictCompletion requires a valid message_start and a terminal condition
	// before a successful finish. Claude Code opts in. It is a bounded proxy
	// rule, not full CLI or provider conformance.
	StrictCompletion bool
	// ReportedUsage merges message_start and message_delta usage with the
	// pinned Claude Code aggregate rules and marks a successful finish when a
	// supported counter was present. It is independent of StrictCompletion.
	// Generic Anthropic and OpenCode leave it false.
	ReportedUsage bool
}

// DecodeStream reads an Anthropic Messages SSE stream and emits IR stream
// events. Used when Anthropic is the backend format. The block index from the
// wire is carried through as StreamEvent.Index so the ingress encoder can
// attribute tool argument fragments correctly. A clean EOF after any started
// content still synthesizes EventFinish.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	return DecodeStreamWithOptions(r, emit, DecodeStreamOptions{})
}

// DecodeStreamWithOptions is DecodeStream with optional completion rules.
func DecodeStreamWithOptions(r io.Reader, emit func(ir.StreamEvent) error, opt DecodeStreamOptions) error {
	reader := sse.NewReader(r)
	var stopReason ir.StopReason = ir.StopEndTurn
	ordinary, cacheRead, cacheWrite := 0, 0, 0
	inputTokens, outputTokens := 0, 0
	// usageSeen is attempt-local. A successful ReportedUsage finish sets
	// UsageReported only when this attempt saw a supported counter.
	var usageSeen bool
	var stopSequence *string
	var container, contextManagement, stopDetails, inputTransformations json.RawMessage
	finished := false
	started := false
	strict := opt.StrictCompletion
	mergeUsage := opt.ReportedUsage
	// messageStarted is a valid message_start only. started is also set by
	// content events and must not decide strict completion.
	messageStarted := false
	// terminal is set when message_delta carries a JSON string stop_reason.
	// A later content start, delta, or stop clears it. Clean EOF can finish
	// only while it remains set and every tracked block is closed. message_stop
	// does not consult this flag. An empty string is still a string.
	terminal := false
	// thinkingOpen is the set of source indexes whose explicit thinking block
	// has not stopped. Completed indexes are deleted so the map stays bounded
	// by simultaneously open blocks, not stream history. A new explicit thinking
	// block cannot start while one remains open. Text, tool, and redacted
	// indexes may create gaps; those indexes are not required to be contiguous.
	thinkingOpen := map[int]thinkingDecodeState{}
	// Redacted data is complete on start, but strict completion still requires
	// the source stop. Retain only open indexes under the thinking-block cap.
	redactedOpen := map[int]bool{}
	// textOpen tracks source text blocks so a delta and its citations bind to
	// the source block rather than the latest merged text buffer. Completed
	// indexes are deleted, so the map is bounded by simultaneous open blocks.
	textOpen := map[int]bool{}
	// toolOpen tracks source indexes of started ordinary tool_use blocks.
	// Completed indexes are deleted, so the map stays bounded by simultaneously
	// open tools. Closing a tool advances stoppedThrough, which rejects late
	// argument fragments and restarts of indexes spent by any block kind. The
	// open set is consulted before stoppedThrough so a higher index's stop
	// cannot close a still-open lower-index tool.
	toolOpen := map[int]bool{}
	// explicitText reports that the source used a content_block_start text
	// lifecycle. Legacy streams without one may start at a text delta; after an
	// explicit start, a missing or closed index is a protocol error and must not
	// degrade to the legacy unindexed stream.
	explicitText := false
	// serverOpen tracks provider-owned blocks that may still receive input.
	// Completed indexes are deleted and their bytes are released, so both the
	// open-block count and retained bytes describe simultaneous state rather
	// than total stream history.
	type openServer struct {
		typ   string
		bytes int
	}
	serverOpen := map[int]*openServer{}
	serverOpenBytes := 0
	// stoppedThrough is the highest stopped indexed block. An index at or below
	// it cannot open again. This avoids retaining every completed block and
	// does not require thinking indexes to be contiguous.
	stoppedThrough := -1
	inclusiveInput := func() int { return ordinary + cacheRead + cacheWrite }
	closeServer := func(index int) *openServer {
		sb := serverOpen[index]
		if sb == nil {
			return nil
		}
		delete(serverOpen, index)
		serverOpenBytes -= sb.bytes
		if index > stoppedThrough {
			stoppedThrough = index
		}
		return sb
	}
	applyReported := func(data []byte, fromMessage bool) error {
		raw, present, err := reportedUsageRaw(data, fromMessage)
		if err != nil {
			return err
		}
		if err := mergeReportedUsage(&ordinary, &cacheRead, &cacheWrite, &outputTokens, &usageSeen, raw, present); err != nil {
			return err
		}
		sum, err := reportedInclusive(ordinary, cacheRead, cacheWrite)
		if err != nil {
			return err
		}
		inputTokens = sum
		return nil
	}
	emitFinish := func() error {
		reported := false
		if mergeUsage {
			sum, err := reportedInclusive(ordinary, cacheRead, cacheWrite)
			if err != nil {
				return err
			}
			if err := reportedTotal(sum, outputTokens); err != nil {
				return err
			}
			inputTokens = sum
			reported = usageSeen
		}
		return emit(ir.StreamEvent{
			Kind: ir.EventFinish, StopReason: stopReason, StopSequence: stopSequence,
			InputTokens: inputTokens, OutputTokens: outputTokens,
			CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
			Container: container, ContextManagement: contextManagement,
			StopDetails: stopDetails, InputTransformations: inputTransformations,
			UsageReported: reported,
		})
	}
	beforeStart := func() error {
		if strict && !messageStarted {
			return ir.ProtocolError("stream event before message_start")
		}
		return nil
	}
	// Content events invalidate a prior terminal delta. The received stop
	// reason itself stays available for an explicit message_stop.
	invalidateTerminal := func() {
		if strict {
			terminal = false
		}
	}

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
			if strict && messageStarted {
				return ir.ProtocolError("message_start before message_stop")
			}
			if strict {
				if err := strictMessageStartObject(ev.Data); err != nil {
					return err
				}
			}
			var m streamMessageStart
			// Reported usage is parsed from raw JSON. The shared start struct uses
			// plain integers, so an invalid counter would otherwise hide the usage
			// error behind a generic decode failure.
			if mergeUsage {
				var ident struct {
					Message struct {
						ID                string          `json:"id"`
						Model             string          `json:"model"`
						Container         json.RawMessage `json:"container"`
						ContextManagement json.RawMessage `json:"context_management"`
					} `json:"message"`
				}
				if json.Unmarshal(ev.Data, &ident) != nil {
					if strict {
						return ir.ProtocolError("malformed stream event")
					}
					continue
				}
				m.Message.ID = ident.Message.ID
				m.Message.Model = ident.Message.Model
				m.Message.Container = ident.Message.Container
				m.Message.ContextManagement = ident.Message.ContextManagement
			} else if json.Unmarshal(ev.Data, &m) != nil {
				if strict {
					return ir.ProtocolError("malformed stream event")
				}
				continue
			}
			if m.Message.Container != nil {
				v, err := validateOptionalObject(m.Message.Container, "container")
				if err != nil {
					return ir.ProtocolError("message_start container is malformed")
				}
				container = v
			}
			if m.Message.ContextManagement != nil {
				v, err := validateOptionalObject(m.Message.ContextManagement, "context_management")
				if err != nil {
					return ir.ProtocolError("message_start context_management is malformed")
				}
				contextManagement = v
			}
			if mergeUsage {
				if err := applyReported(ev.Data, true); err != nil {
					return err
				}
			} else {
				ordinary = m.Message.Usage.InputTokens
				cacheRead = m.Message.Usage.CacheReadInputTokens
				cacheWrite = m.Message.Usage.CacheCreationInputTokens
				inputTokens = inclusiveInput()
			}
			if err := emit(ir.StreamEvent{
				Kind: ir.EventMessageStart, ID: m.Message.ID, Model: m.Message.Model,
				InputTokens:     inputTokens,
				CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
				Container: container, ContextManagement: contextManagement,
			}); err != nil {
				return err
			}
			started = true
			messageStarted = true
		case "content_block_start":
			if err := beforeStart(); err != nil {
				return err
			}
			if strict {
				if err := strictContentStartObject(ev.Data); err != nil {
					return err
				}
			}
			var s streamContentBlockStart
			if json.Unmarshal(ev.Data, &s) != nil {
				if strict {
					return ir.ProtocolError("malformed stream event")
				}
				continue
			}
			invalidateTerminal()
			if s.Index < 0 {
				return ir.ProtocolError("negative content block index")
			}
			if redactedOpen[s.Index] {
				return ir.ProtocolError("content block started on an open redacted thinking block")
			}
			switch s.ContentBlock.Type {
			case "tool_use":
				if len(thinkingOpen) != 0 {
					return ir.ProtocolError("content block started while thinking block is open")
				}
				if serverOpen[s.Index] != nil {
					return ir.ProtocolError("tool block started on an open server block")
				}
				// A repeated start on a still-open index is tolerated identity backfill
				// and adds no open state, so the restart and capacity checks apply only
				// when opening a new tool index. A still-open index can sit at or below
				// stoppedThrough once a higher index of any kind has stopped; only a
				// spent index is a restart.
				if !toolOpen[s.Index] {
					if s.Index <= stoppedThrough {
						return ir.ProtocolError("tool block restarted after stop")
					}
					if len(toolOpen) >= maxOpenToolBlocks {
						return ir.ProtocolError("too many open tool blocks")
					}
					toolOpen[s.Index] = true
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
				if strict {
					if s.Index <= stoppedThrough {
						return ir.ProtocolError("redacted thinking block restarted after stop")
					}
					if textOpen[s.Index] || toolOpen[s.Index] || serverOpen[s.Index] != nil {
						return ir.ProtocolError("redacted thinking block started on an open content block")
					}
					if len(redactedOpen) >= maxOpenThinkingBlocks {
						return ir.ProtocolError("too many open redacted thinking blocks")
					}
					redactedOpen[s.Index] = true
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
				if textOpen[s.Index] {
					return ir.ProtocolError("text block started twice")
				}
				if s.Index <= stoppedThrough {
					return ir.ProtocolError("text block restarted after stop")
				}
				if serverOpen[s.Index] != nil {
					return ir.ProtocolError("text block started on an open server block")
				}
				if len(textOpen) >= maxOpenTextBlocks {
					return ir.ProtocolError("too many open text blocks")
				}
				citations, err := decodeCitationItems(s.ContentBlock.Citations)
				if err != nil {
					return ir.ProtocolError("text block citations are malformed")
				}
				explicitText = true
				textOpen[s.Index] = true
				if err := emit(ir.StreamEvent{Kind: ir.EventTextStart, Index: s.Index, Text: s.ContentBlock.Text, TextIndexed: true}); err != nil {
					return err
				}
				for _, c := range citations {
					if err := emit(ir.StreamEvent{Kind: ir.EventTextCitation, Index: s.Index, Data: string(c.Raw), TextIndexed: true}); err != nil {
						return err
					}
				}
				started = true
			default:
				if s.ContentBlock.Type == "" {
					break
				}
				if len(thinkingOpen) != 0 {
					return ir.ProtocolError("content block started while thinking block is open")
				}
				if !knownServerBlockType(s.ContentBlock.Type) {
					return ir.ProtocolError("unsupported server content block")
				}
				if s.Index <= stoppedThrough {
					return ir.ProtocolError("server block restarted after stop")
				}
				if serverOpen[s.Index] != nil {
					return ir.ProtocolError("server block started twice")
				}
				if len(serverOpen) >= maxOpenServerBlocks {
					return ir.ProtocolError("too many open server blocks")
				}
				raw := rawContentBlock(ev.Data)
				if len(raw) == 0 {
					return ir.ProtocolError("server block is malformed")
				}
				if len(raw) > maxServerStartBytes {
					return ir.ProtocolError("server start exceeded limit")
				}
				if _, err := serverStartObject(ir.StreamEvent{ServerType: s.ContentBlock.Type, Data: string(raw)}); err != nil {
					return err
				}
				serverOpen[s.Index] = &openServer{typ: s.ContentBlock.Type, bytes: len(raw)}
				serverOpenBytes += len(raw)
				if serverOpenBytes > maxServerInputBytes {
					return ir.ProtocolError("server input exceeded limit")
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventServerStart, Index: s.Index, ServerType: s.ContentBlock.Type, Data: string(raw)}); err != nil {
					return err
				}
				started = true
			}
		case "content_block_delta":
			if err := beforeStart(); err != nil {
				return err
			}
			if strict {
				if err := strictContentDeltaObject(ev.Data); err != nil {
					return err
				}
			}
			var d streamContentBlockDelta
			if json.Unmarshal(ev.Data, &d) != nil {
				if strict {
					return ir.ProtocolError("malformed stream event")
				}
				continue
			}
			invalidateTerminal()
			if d.Index < 0 {
				return ir.ProtocolError("negative content block index")
			}
			if redactedOpen[d.Index] {
				return ir.ProtocolError("delta on a redacted thinking block")
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
				if serverOpen[d.Index] != nil {
					return ir.ProtocolError("text delta on a server block")
				}
				if textOpen[d.Index] {
					// Still open: a higher index's stop advanced stoppedThrough past
					// this index, but its deltas keep flowing. Once this block stops,
					// the watermark check below rejects its late deltas again.
					if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Index: d.Index, Text: d.Delta.Text, TextIndexed: true}); err != nil {
						return err
					}
					started = true
					break
				}
				if d.Index <= stoppedThrough {
					return ir.ProtocolError("text delta after block stop")
				}
				if explicitText || strict {
					return ir.ProtocolError("text delta without an open text block")
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: d.Delta.Text}); err != nil {
					return err
				}
				started = true
			case "citations_delta":
				if !textOpen[d.Index] {
					return ir.ProtocolError("citation delta without an open text block")
				}
				citations, err := decodeCitationItems([]json.RawMessage{d.Delta.Citation})
				if err != nil {
					return ir.ProtocolError("citation delta is malformed")
				}
				ev := ir.StreamEvent{Kind: ir.EventTextCitation, Index: d.Index, Data: string(citations[0].Raw), TextIndexed: true}
				if err := emit(ev); err != nil {
					return err
				}
				started = true
			case "input_json_delta":
				if _, open := thinkingOpen[d.Index]; open {
					return ir.ProtocolError("tool delta while thinking block is open")
				}
				if sb := serverOpen[d.Index]; sb != nil {
					if sb.typ == "compaction" {
						return ir.ProtocolError("compaction block received an input delta")
					}
					if serverOpenBytes+len(d.Delta.PartialJSON) > maxServerInputBytes {
						return ir.ProtocolError("server input exceeded limit")
					}
					sb.bytes += len(d.Delta.PartialJSON)
					serverOpenBytes += len(d.Delta.PartialJSON)
					if err := emit(ir.StreamEvent{Kind: ir.EventServerDelta, Index: d.Index, ArgsFrag: d.Delta.PartialJSON}); err != nil {
						return err
					}
					started = true
					break
				}
				if toolOpen[d.Index] {
					// Still open: fragments keep flowing even when a higher index's
					// stop advanced stoppedThrough past this index.
					if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: d.Index, ArgsFrag: d.Delta.PartialJSON}); err != nil {
						return err
					}
					started = true
					break
				}
				if d.Index <= stoppedThrough {
					return ir.ProtocolError("input delta after block stop")
				}
				if strict {
					return ir.ProtocolError("input delta without an open tool block")
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: d.Index, ArgsFrag: d.Delta.PartialJSON}); err != nil {
					return err
				}
				started = true
			case "compaction_delta":
				sb := serverOpen[d.Index]
				if sb == nil || sb.typ != "compaction" {
					return ir.ProtocolError("compaction delta without an open compaction block")
				}
				fields, err := compactionDeltaFields(ev.Data)
				if err != nil {
					return ir.ProtocolError("compaction delta is malformed")
				}
				if serverOpenBytes+len(fields) > maxServerInputBytes {
					return ir.ProtocolError("server input exceeded limit")
				}
				sb.bytes += len(fields)
				serverOpenBytes += len(fields)
				if err := emit(ir.StreamEvent{Kind: ir.EventServerUpdate, Index: d.Index, ServerType: "compaction", Data: string(fields)}); err != nil {
					return err
				}
				started = true
			default:
				if d.Delta.Type != "" {
					return ir.ProtocolError("unsupported content delta")
				}
			}
		case "content_block_stop":
			if err := beforeStart(); err != nil {
				return err
			}
			if strict {
				if _, err := strictContentObject(ev.Data); err != nil {
					return err
				}
			}
			var s streamContentBlockStop
			if json.Unmarshal(ev.Data, &s) != nil {
				if strict {
					return ir.ProtocolError("malformed stream event")
				}
				continue
			}
			invalidateTerminal()
			if s.Index < 0 {
				return ir.ProtocolError("negative content block index")
			}
			if redactedOpen[s.Index] {
				delete(redactedOpen, s.Index)
				if s.Index > stoppedThrough {
					stoppedThrough = s.Index
				}
				// EventRedactedReasoning already contains the complete IR block.
				break
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
				break
			}
			if textOpen[s.Index] {
				delete(textOpen, s.Index)
				if s.Index > stoppedThrough {
					stoppedThrough = s.Index
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventTextEnd, Index: s.Index, TextIndexed: true}); err != nil {
					return err
				}
				started = true
				break
			}
			if toolOpen[s.Index] {
				delete(toolOpen, s.Index)
				if s.Index > stoppedThrough {
					stoppedThrough = s.Index
				}
				// Ordinary tool blocks carry no decoder-side stop event; the
				// ingress encoder closes its open tool blocks at Finish.
				break
			}
			if closeServer(s.Index) != nil {
				if err := emit(ir.StreamEvent{Kind: ir.EventServerEnd, Index: s.Index}); err != nil {
					return err
				}
				started = true
			}
		case "message_delta":
			if err := beforeStart(); err != nil {
				return err
			}
			var received string
			var gotReason bool
			if strict {
				reason, ok, err := strictReceivedStopReason(ev.Data)
				if err != nil {
					return err
				}
				received, gotReason = reason, ok
			}
			if mergeUsage {
				// Validate usage before the shared integer decode. A bad counter must
				// not be reported as a generic malformed event, and it must not update
				// a later field after an earlier field already failed.
				if err := applyReported(ev.Data, false); err != nil {
					return err
				}
			}
			var m streamMessageDelta
			if json.Unmarshal(ev.Data, &m) != nil {
				if strict {
					return ir.ProtocolError("malformed stream event")
				}
				continue
			}
			if strict {
				if gotReason {
					stopReason = stopReason2(received)
					terminal = true
				}
			} else {
				stopReason = stopReason2(m.Delta.StopReason)
			}
			stopSequence = m.Delta.StopSequence
			if m.Delta.Container != nil {
				v, err := validateOptionalObject(m.Delta.Container, "container")
				if err != nil {
					return ir.ProtocolError("message_delta container is malformed")
				}
				container = v
			}
			if m.Delta.StopDetails != nil {
				v, err := validateOptionalObject(m.Delta.StopDetails, "stop_details")
				if err != nil {
					return ir.ProtocolError("message_delta stop_details is malformed")
				}
				stopDetails = v
			}
			if m.ContextManagement != nil {
				v, err := validateOptionalObject(m.ContextManagement, "context_management")
				if err != nil {
					return ir.ProtocolError("message_delta context_management is malformed")
				}
				contextManagement = v
			}
			if m.InputTransformations != nil {
				v, err := validateOptionalArray(m.InputTransformations, "input_transformations")
				if err != nil {
					return ir.ProtocolError("message_delta input_transformations is malformed")
				}
				inputTransformations = v
			}
			// A usage-only delta updates counters but does not set terminal.
			// Completion still requires a string stop_reason or message_stop.
			if !mergeUsage {
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
			}
		case "message_stop":
			if err := beforeStart(); err != nil {
				return err
			}
			if strict {
				if err := strictEventObject(ev.Data); err != nil {
					return err
				}
				if err := strictOpenBlocks(len(thinkingOpen), len(textOpen), len(toolOpen), len(serverOpen), len(redactedOpen)); err != nil {
					return err
				}
			} else {
				if len(thinkingOpen) != 0 {
					return ir.ProtocolError("thinking block closed by stream end")
				}
				if len(serverOpen) != 0 {
					return ir.ProtocolError("server block closed by stream end")
				}
			}
			if err := emitFinish(); err != nil {
				return err
			}
			finished = true
			// A valid explicit message_stop ends strict decoding. Later events,
			// including a provider error, must not replace that completion.
			if strict {
				return nil
			}
		}
	}
	if !finished {
		if strict {
			if !messageStarted {
				// Empty, comment, ping, and unknown events do not fabricate success.
				return nil
			}
			if err := strictOpenBlocks(len(thinkingOpen), len(textOpen), len(toolOpen), len(serverOpen), len(redactedOpen)); err != nil {
				return err
			}
			if !terminal {
				return ir.ProtocolError("stream ended before completion")
			}
			return emitFinish()
		}
		if !started {
			// Empty / comment-only stream: do not fabricate a successful completion.
			return nil
		}
		if len(thinkingOpen) != 0 {
			// An open thinking block has no signature association yet. Do not
			// synthesize a finish that looks like valid signed history.
			return ir.ProtocolError("thinking block closed by stream end")
		}
		if len(serverOpen) != 0 {
			return ir.ProtocolError("server block closed by stream end")
		}
		return emitFinish()
	}
	if len(thinkingOpen) != 0 {
		return ir.ProtocolError("thinking block closed by stream end")
	}
	return nil
}

// mergeReportedUsage applies one Claude Code usage snapshot. Positive ordinary
// input, cache read, and aggregate cache write replace. Omitted, null, and zero
// values keep those partitions. A positive aggregate cache write wins over the
// current frame's TTL split. Otherwise a positive sum of this frame's
// ephemeral_1h_input_tokens and ephemeral_5m_input_tokens replaces cache write.
// Stale TTL members from earlier frames are not added. Output uses nullish
// coalescing, so an explicit zero replaces and an omission does not. Rich usage
// fields are ignored. This is not fallback-credit accounting.
//
// The four input/output sums are checked before the state is stored. Chat and
// Responses encode total as input plus output, so an overflowing snapshot is
// rejected before it can be emitted.
func mergeReportedUsage(ordinary, cacheRead, cacheWrite, output *int, seen *bool, raw json.RawMessage, present bool) error {
	if !present || jsonNull(raw) {
		return nil
	}
	obj, err := reportedUsageObject(raw)
	if err != nil {
		return err
	}
	nextOrdinary, nextRead, nextWrite, nextOutput := *ordinary, *cacheRead, *cacheWrite, *output
	nextSeen := *seen
	if err := applyPositiveUsage(&nextOrdinary, &nextSeen, obj, "input_tokens"); err != nil {
		return err
	}
	if err := applyPositiveUsage(&nextRead, &nextSeen, obj, "cache_read_input_tokens"); err != nil {
		return err
	}
	if err := applyOutputUsage(&nextOutput, &nextSeen, obj, "output_tokens"); err != nil {
		return err
	}
	if err := applyCacheWriteUsage(&nextWrite, &nextSeen, obj); err != nil {
		return err
	}
	sum, err := reportedInclusive(nextOrdinary, nextRead, nextWrite)
	if err != nil {
		return err
	}
	if err := reportedTotal(sum, nextOutput); err != nil {
		return err
	}
	*ordinary, *cacheRead, *cacheWrite, *output, *seen = nextOrdinary, nextRead, nextWrite, nextOutput, nextSeen
	return nil
}

func applyPositiveUsage(dst *int, seen *bool, obj map[string]json.RawMessage, key string) error {
	raw, ok := obj[key]
	if !ok {
		return nil
	}
	value, present, err := parseReportedCounter(raw)
	if err != nil || !present {
		return err
	}
	*seen = true
	// Zero is a supported counter, but it is not a positive replacement.
	if value > 0 {
		*dst = value
	}
	return nil
}

func applyOutputUsage(dst *int, seen *bool, obj map[string]json.RawMessage, key string) error {
	raw, ok := obj[key]
	if !ok {
		return nil
	}
	value, present, err := parseReportedCounter(raw)
	if err != nil || !present {
		return err
	}
	*seen = true
	*dst = value
	return nil
}

func applyCacheWriteUsage(dst *int, seen *bool, obj map[string]json.RawMessage) error {
	aggValue, aggPresent := 0, false
	if raw, ok := obj["cache_creation_input_tokens"]; ok {
		value, present, err := parseReportedCounter(raw)
		if err != nil {
			return err
		}
		if present {
			aggPresent = true
			aggValue = value
			*seen = true
		}
	}
	ttlSum, ttlSeen := 0, false
	if raw, ok := obj["cache_creation"]; ok {
		sum, seenTTL, err := reportedTTLSum(raw)
		if err != nil {
			return err
		}
		ttlSum, ttlSeen = sum, seenTTL
		if ttlSeen {
			*seen = true
		}
	}
	switch {
	case aggPresent && aggValue > 0:
		*dst = aggValue
	case ttlSum > 0:
		*dst = ttlSum
	}
	return nil
}

func reportedTTLSum(raw json.RawMessage) (int, bool, error) {
	if jsonNull(raw) {
		return 0, false, nil
	}
	obj, err := reportedUsageObject(raw)
	if err != nil {
		return 0, false, err
	}
	hour, hourSeen, err := optionalReportedCounter(obj, "ephemeral_1h_input_tokens")
	if err != nil {
		return 0, false, err
	}
	minute, minuteSeen, err := optionalReportedCounter(obj, "ephemeral_5m_input_tokens")
	if err != nil {
		return 0, false, err
	}
	if !hourSeen && !minuteSeen {
		return 0, false, nil
	}
	h, m := 0, 0
	if hourSeen {
		h = hour
	}
	if minuteSeen {
		m = minute
	}
	// Sum only this frame. Absent and null members contribute zero, not a
	// previous frame's TTL value.
	sum, err := addReportedUsage(h, m)
	if err != nil {
		return 0, false, err
	}
	return sum, true, nil
}

func optionalReportedCounter(obj map[string]json.RawMessage, key string) (int, bool, error) {
	raw, ok := obj[key]
	if !ok {
		return 0, false, nil
	}
	return parseReportedCounter(raw)
}

func reportedUsageRaw(data []byte, fromMessage bool) (json.RawMessage, bool, error) {
	obj, err := reportedJSONObject(data)
	if err != nil {
		return nil, false, err
	}
	if fromMessage {
		raw, ok := obj["message"]
		if !ok {
			return nil, false, nil
		}
		obj, err = reportedJSONObject(raw)
		if err != nil {
			return nil, false, err
		}
	}
	raw, ok := obj["usage"]
	if !ok {
		return nil, false, nil
	}
	return raw, true, nil
}

func reportedJSONObject(data []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ir.ProtocolError("malformed stream event")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(trimmed, &obj) != nil || obj == nil {
		return nil, ir.ProtocolError("malformed stream event")
	}
	return obj, nil
}

func reportedUsageObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ir.ProtocolError("invalid usage object")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(trimmed, &obj) != nil || obj == nil {
		return nil, ir.ProtocolError("invalid usage object")
	}
	return obj, nil
}

func parseReportedCounter(raw json.RawMessage) (int, bool, error) {
	if jsonNull(raw) {
		return 0, false, nil
	}
	n, err := parseReportedInt(raw)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

func parseReportedInt(raw json.RawMessage) (int, error) {
	token := bytes.TrimSpace(raw)
	if len(token) == 0 {
		return 0, ir.ProtocolError("invalid usage counter")
	}
	i := 0
	if token[0] == '-' {
		i = 1
	}
	if i == len(token) {
		return 0, ir.ProtocolError("invalid usage counter")
	}
	for _, c := range token[i:] {
		if c < '0' || c > '9' {
			return 0, ir.ProtocolError("invalid usage counter")
		}
	}
	n, err := strconv.ParseInt(string(token), 10, 64)
	if err != nil {
		return 0, ir.ProtocolError("usage total overflow")
	}
	if n < 0 {
		return 0, ir.ProtocolError("invalid usage counter")
	}
	if n > int64(math.MaxInt) {
		return 0, ir.ProtocolError("usage total overflow")
	}
	return int(n), nil
}

func jsonNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func addReportedUsage(a, b int) (int, error) {
	if a < 0 || b < 0 {
		return 0, ir.ProtocolError("invalid usage counter")
	}
	if b > 0 && a > math.MaxInt-b {
		return 0, ir.ProtocolError("usage total overflow")
	}
	return a + b, nil
}

func reportedInclusive(ordinary, read, write int) (int, error) {
	sum, err := addReportedUsage(ordinary, read)
	if err != nil {
		return 0, err
	}
	return addReportedUsage(sum, write)
}

func reportedTotal(input, output int) error {
	_, err := addReportedUsage(input, output)
	return err
}

func strictOpenBlocks(thinking, text, tool, server, redacted int) error {
	if thinking != 0 {
		return ir.ProtocolError("thinking block closed by stream end")
	}
	if server != 0 {
		return ir.ProtocolError("server block closed by stream end")
	}
	if text != 0 || tool != 0 || redacted != 0 {
		return ir.ProtocolError("stream ended with an open content block")
	}
	return nil
}

func strictEventObject(data []byte) error {
	_, err := strictJSONObject(data)
	return err
}

func strictJSONObject(data []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ir.ProtocolError("malformed stream event")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(trimmed, &obj) != nil || obj == nil {
		return nil, ir.ProtocolError("malformed stream event")
	}
	return obj, nil
}

func strictMessageStartObject(data []byte) error {
	obj, err := strictJSONObject(data)
	if err != nil {
		return err
	}
	raw, ok := obj["message"]
	if !ok {
		return ir.ProtocolError("malformed stream event")
	}
	_, err = strictJSONObject(raw)
	return err
}

func strictContentObject(data []byte) (map[string]json.RawMessage, error) {
	obj, err := strictJSONObject(data)
	if err != nil {
		return nil, err
	}
	// Missing and null indexes must not become Go's default block index zero.
	var index *int
	if json.Unmarshal(obj["index"], &index) != nil || index == nil || *index < 0 {
		return nil, ir.ProtocolError("malformed content block index")
	}
	return obj, nil
}

func strictContentStartObject(data []byte) error {
	obj, err := strictContentObject(data)
	if err != nil {
		return err
	}
	raw, ok := obj["content_block"]
	if !ok {
		return ir.ProtocolError("malformed stream event")
	}
	_, err = strictJSONObject(raw)
	return err
}

func strictContentDeltaObject(data []byte) error {
	obj, err := strictContentObject(data)
	if err != nil {
		return err
	}
	raw, ok := obj["delta"]
	if !ok {
		return ir.ProtocolError("malformed stream event")
	}
	_, err = strictJSONObject(raw)
	return err
}

// strictReceivedStopReason reports a JSON string stop_reason. An empty string
// is still a string, so it is terminal and later maps through stopReason2.
// Null and omitted reasons are not terminal. Other JSON types are malformed
// and must not be copied into the error.
func strictReceivedStopReason(data []byte) (string, bool, error) {
	obj, err := strictJSONObject(data)
	if err != nil {
		return "", false, err
	}
	raw, ok := obj["delta"]
	if !ok {
		return "", false, ir.ProtocolError("malformed stream event")
	}
	delta, err := strictJSONObject(raw)
	if err != nil {
		return "", false, err
	}
	reason, ok := delta["stop_reason"]
	if !ok {
		return "", false, nil
	}
	reason = bytes.TrimSpace(reason)
	if len(reason) == 0 || bytes.Equal(reason, []byte("null")) {
		return "", false, nil
	}
	if reason[0] != '"' {
		return "", false, ir.ProtocolError("malformed stream event")
	}
	var value string
	if json.Unmarshal(reason, &value) != nil {
		return "", false, ir.ProtocolError("malformed stream event")
	}
	return value, true, nil
}

// compactionDeltaFields extracts the verified compaction update fields. Content
// is required and must be a JSON string. encrypted_content is optional and must
// be a string or null. Unknown delta fields are not forwarded.
func compactionDeltaFields(data []byte) (json.RawMessage, error) {
	var wrap struct {
		Delta json.RawMessage `json:"delta"`
	}
	if json.Unmarshal(data, &wrap) != nil {
		return nil, errors.New("missing delta")
	}
	trimmed := bytes.TrimSpace(wrap.Delta)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("delta must be an object")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(trimmed, &obj) != nil {
		return nil, errors.New("delta must be an object")
	}
	content, ok := obj["content"]
	if !ok {
		return nil, errors.New("content is required")
	}
	content = bytes.TrimSpace(content)
	if len(content) == 0 || content[0] != '"' {
		return nil, errors.New("content must be a string")
	}
	out := map[string]json.RawMessage{"content": content}
	if enc, ok := obj["encrypted_content"]; ok {
		enc = bytes.TrimSpace(enc)
		if len(enc) > 0 && !bytes.Equal(enc, nullJSON) {
			if enc[0] != '"' {
				return nil, errors.New("encrypted_content must be a string")
			}
		}
		out["encrypted_content"] = enc
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
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
	// texts maps a source text-block index to its open output block. The key -1
	// is the legacy unindexed text stream. Entries are removed on stop, so the
	// map is bounded by simultaneously open text blocks.
	texts      map[int]int
	textSource int
	servers    map[int]*serverEncodeBlock
	// serverOpenBytes and serverStoppedThrough bound simultaneous open server
	// state and reject index reuse. Bytes are released on stop; this is not a
	// cumulative history counter.
	serverOpenBytes      int
	serverStoppedThrough int
	startContainer       json.RawMessage
	startContext         json.RawMessage
}

// maxOpenServerBlocks bounds simultaneously open provider-owned blocks.
const maxOpenServerBlocks = 32

// maxServerInputBytes bounds retained server input fragments for one stream.
const maxServerInputBytes = 1 << 20

// maxServerStartBytes bounds one server content_block_start object. An SSE
// frame may be larger, but an open server block must not retain that much per
// index across the open-block cap.
const maxServerStartBytes = 1 << 20

// maxOpenThinkingBlocks bounds simultaneously open source thinking blocks.
// Completed blocks are released, so this is not a history limit.
const maxOpenThinkingBlocks = 64

// maxOpenTextBlocks bounds simultaneously open source text blocks.
const maxOpenTextBlocks = 64

// maxOpenToolBlocks bounds simultaneously open source tool_use blocks.
// Completed tools are released, so this is not a history limit.
const maxOpenToolBlocks = 64

// thinkingBlock is one open source-indexed thinking block. block is its wire
// index after content_block_start. A signature before the start is a protocol
// error; signatures are not queued out of order.
type thinkingBlock struct {
	block  int
	signed bool
}

type serverEncodeBlock struct {
	block int
	typ   string
	bytes int
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
		texts: map[int]int{}, textSource: -1,
		stoppedThrough: -1, serverStoppedThrough: -1,
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
	message := map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": e.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": anthUsageMap(startUsage, 0),
	}
	if len(e.startContainer) > 0 {
		message["container"] = json.RawMessage(e.startContainer)
	}
	if len(e.startContext) > 0 {
		message["context_management"] = json.RawMessage(e.startContext)
	}
	return e.event(w, "message_start", map[string]any{"message": message})
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
	if e.textSource >= -1 && e.texts != nil {
		if block, ok := e.texts[e.textSource]; ok && block == idx {
			delete(e.texts, e.textSource)
		}
	}
	e.textSource = -1
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

func (e *StreamEncoder) encodeServerComplete(w *sse.Writer, ev ir.StreamEvent) error {
	if e.explicitThinkingOpen() {
		return ir.ProtocolError("server block while thinking block is open")
	}
	if ev.Index < 0 {
		return ir.ProtocolError("negative content block index")
	}
	if ev.Index <= e.serverStoppedThrough {
		return ir.ProtocolError("server block restarted after stop")
	}
	block, err := serverStartObject(ev)
	if err != nil {
		return err
	}
	if len(block) > maxServerStartBytes {
		return ir.ProtocolError("server start exceeded limit")
	}
	if err := e.ensureStart(w); err != nil {
		return err
	}
	if err := e.closeOpenText(w); err != nil {
		return err
	}
	idx := e.nextIndex
	e.nextIndex++
	if err := e.event(w, "content_block_start", map[string]any{"index": idx, "content_block": block}); err != nil {
		return err
	}
	if ev.Index > e.serverStoppedThrough {
		e.serverStoppedThrough = ev.Index
	}
	return e.event(w, "content_block_stop", map[string]any{"index": idx})
}

func (e *StreamEncoder) encodeServerStart(w *sse.Writer, ev ir.StreamEvent) error {
	if e.explicitThinkingOpen() {
		return ir.ProtocolError("server block while thinking block is open")
	}
	if ev.Index < 0 {
		return ir.ProtocolError("negative content block index")
	}
	if ev.Index <= e.serverStoppedThrough {
		return ir.ProtocolError("server block restarted after stop")
	}
	if e.servers != nil && e.servers[ev.Index] != nil {
		return ir.ProtocolError("server block started twice")
	}
	if e.servers != nil && len(e.servers) >= maxOpenServerBlocks {
		return ir.ProtocolError("too many open server blocks")
	}
	block, err := serverStartObject(ev)
	if err != nil {
		return err
	}
	if len(block) > maxServerStartBytes {
		return ir.ProtocolError("server start exceeded limit")
	}
	if e.serverOpenBytes+len(block) > maxServerInputBytes {
		return ir.ProtocolError("server input exceeded limit")
	}
	if err := e.ensureStart(w); err != nil {
		return err
	}
	if err := e.closeOpenText(w); err != nil {
		return err
	}
	idx := e.nextIndex
	e.nextIndex++
	if e.servers == nil {
		e.servers = map[int]*serverEncodeBlock{}
	}
	e.servers[ev.Index] = &serverEncodeBlock{block: idx, typ: ev.ServerType, bytes: len(block)}
	e.serverOpenBytes += len(block)
	return e.event(w, "content_block_start", map[string]any{"index": idx, "content_block": json.RawMessage(block)})
}

func (e *StreamEncoder) encodeServerDelta(w *sse.Writer, ev ir.StreamEvent) error {
	sb := e.servers[ev.Index]
	if sb == nil {
		return ir.ProtocolError("server delta without an open server block")
	}
	if sb.typ == "compaction" {
		return ir.ProtocolError("compaction block received an input delta")
	}
	if e.serverOpenBytes+len(ev.ArgsFrag) > maxServerInputBytes {
		return ir.ProtocolError("server input exceeded limit")
	}
	sb.bytes += len(ev.ArgsFrag)
	e.serverOpenBytes += len(ev.ArgsFrag)
	return e.event(w, "content_block_delta", map[string]any{
		"index": sb.block, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ArgsFrag},
	})
}

// encodeServerUpdate forwards one verified provider-owned field replacement,
// currently a compaction content/encrypted_content update. The pure encoder
// retains no start object or fragment history; it only remembers the open wire
// index and type.
func (e *StreamEncoder) encodeServerUpdate(w *sse.Writer, ev ir.StreamEvent) error {
	sb := e.servers[ev.Index]
	if sb == nil {
		return ir.ProtocolError("server update without an open server block")
	}
	if sb.typ != "compaction" || ev.ServerType != "compaction" {
		return ir.ProtocolError("server update is not supported for this block")
	}
	trimmed := bytes.TrimSpace([]byte(ev.Data))
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return ir.ProtocolError("server update is malformed")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(trimmed, &fields) != nil {
		return ir.ProtocolError("server update is malformed")
	}
	if e.serverOpenBytes+len(trimmed) > maxServerInputBytes {
		return ir.ProtocolError("server input exceeded limit")
	}
	sb.bytes += len(trimmed)
	e.serverOpenBytes += len(trimmed)
	delta := map[string]any{"type": "compaction_delta"}
	for key, value := range fields {
		delta[key] = value
	}
	return e.event(w, "content_block_delta", map[string]any{"index": sb.block, "delta": delta})
}

func (e *StreamEncoder) encodeServerEnd(w *sse.Writer, ev ir.StreamEvent) error {
	sb := e.servers[ev.Index]
	if sb == nil {
		return ir.ProtocolError("server stop without an open server block")
	}
	delete(e.servers, ev.Index)
	e.serverOpenBytes -= sb.bytes
	if ev.Index > e.serverStoppedThrough {
		e.serverStoppedThrough = ev.Index
	}
	if e.openIndex == sb.block {
		e.openKind = blockNone
	}
	return e.event(w, "content_block_stop", map[string]any{"index": sb.block})
}

// openTextBlock opens one output text block bound to a source index. The key
// -1 is reserved for legacy unindexed text. A previous text block is closed and
// released before its replacement starts.
func (e *StreamEncoder) openTextBlock(w *sse.Writer, source int, initial string) (int, error) {
	if err := e.ensureStart(w); err != nil {
		return 0, err
	}
	if e.openKind == blockText || e.openKind == blockReasoning {
		if err := e.closeBlock(w); err != nil {
			return 0, err
		}
	}
	idx := e.nextIndex
	e.nextIndex++
	e.openKind = blockText
	e.openIndex = idx
	e.textSource = source
	e.texts[source] = idx
	if err := e.event(w, "content_block_start", map[string]any{
		"index": idx, "content_block": map[string]any{"type": "text", "text": initial},
	}); err != nil {
		return 0, err
	}
	return idx, nil
}

func (e *StreamEncoder) textBlockFor(source int) (int, bool) {
	if e.openKind == blockText && e.textSource == source {
		return e.openIndex, true
	}
	return 0, false
}

func (e *StreamEncoder) closeOpenText(w *sse.Writer) error {
	if e.openKind == blockText || e.openKind == blockReasoning {
		return e.closeBlock(w)
	}
	return nil
}

func serverStartObject(ev ir.StreamEvent) (json.RawMessage, error) {
	if ev.ServerType == "" || !knownServerBlockType(ev.ServerType) {
		return nil, ir.ProtocolError("unsupported server content block")
	}
	block := json.RawMessage(ev.Data)
	if len(block) == 0 {
		return json.RawMessage(`{"type":` + quoteJSON(ev.ServerType) + `}`), nil
	}
	if !json.Valid(block) || block[0] != '{' {
		return nil, ir.ProtocolError("server block is malformed")
	}
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(block, &probe) != nil || probe.Type != ev.ServerType {
		return nil, ir.ProtocolError("server block type is invalid")
	}
	return append(json.RawMessage(nil), block...), nil
}

func (e *StreamEncoder) Encode(ev ir.StreamEvent, w *sse.Writer) error {
	switch ev.Kind {
	case ir.EventMessageStart:
		e.id = ev.ID
		if ev.Model != "" {
			e.model = ev.Model
		}
		if len(ev.Container) > 0 {
			v, err := validateOptionalObject(ev.Container, "container")
			if err != nil {
				return ir.ProtocolError("message_start container is malformed")
			}
			e.startContainer = v
		}
		if len(ev.ContextManagement) > 0 {
			v, err := validateOptionalObject(ev.ContextManagement, "context_management")
			if err != nil {
				return ir.ProtocolError("message_start context_management is malformed")
			}
			e.startContext = v
		}
		e.applyUsage(ev)
		return e.ensureStart(w)

	case ir.EventTextStart:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("text block started while thinking block is open")
		}
		if ev.Index < 0 {
			return ir.ProtocolError("negative text block index")
		}
		if _, ok := e.texts[ev.Index]; ok {
			return ir.ProtocolError("text block started twice")
		}
		if len(e.texts) >= maxOpenTextBlocks {
			return ir.ProtocolError("too many open text blocks")
		}
		_, err := e.openTextBlock(w, ev.Index, ev.Text)
		return err

	case ir.EventTextEnd:
		block, ok := e.texts[ev.Index]
		if !ok {
			return nil
		}
		if e.openKind == blockText && e.openIndex == block {
			return e.closeBlock(w)
		}
		delete(e.texts, ev.Index)
		if e.textSource == ev.Index {
			e.textSource = -1
		}
		return nil

	case ir.EventTextDelta:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("text delta while thinking block is open")
		}
		source := -1
		if ev.TextIndexed {
			source = ev.Index
		}
		block, ok := e.textBlockFor(source)
		if !ok {
			if ev.TextIndexed && ev.Text == "" {
				return nil
			}
			var err error
			block, err = e.openTextBlock(w, source, "")
			if err != nil {
				return err
			}
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": block, "delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})

	case ir.EventTextCitation:
		if e.explicitThinkingOpen() {
			return ir.ProtocolError("citation while thinking block is open")
		}
		citations, err := decodeCitationItems([]json.RawMessage{json.RawMessage(ev.Data)})
		if err != nil {
			return ir.ProtocolError("citation is malformed")
		}
		source := -1
		if ev.TextIndexed {
			source = ev.Index
		} else if block, open := e.textBlockFor(ev.Index); open && block >= 0 {
			// An older citation carried only Index. Bind it only to that open
			// source block, never to whichever unrelated text block is current.
			source = ev.Index
		}
		block, ok := e.textBlockFor(source)
		if !ok {
			block, err = e.openTextBlock(w, source, "")
			if err != nil {
				return err
			}
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": block,
			"delta": map[string]any{"type": "citations_delta", "citation": json.RawMessage(citations[0].Raw)},
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

	case ir.EventServerBlock:
		return e.encodeServerComplete(w, ev)
	case ir.EventServerStart:
		return e.encodeServerStart(w, ev)
	case ir.EventServerDelta:
		return e.encodeServerDelta(w, ev)
	case ir.EventServerUpdate:
		return e.encodeServerUpdate(w, ev)
	case ir.EventServerEnd:
		return e.encodeServerEnd(w, ev)

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
		delta := map[string]any{"stop_reason": stopReasonWire(ev.StopReason), "stop_sequence": nil}
		if ev.StopSequence != nil {
			delta["stop_sequence"] = *ev.StopSequence
		}
		if len(ev.StopDetails) > 0 {
			v, err := validateOptionalObject(ev.StopDetails, "stop_details")
			if err != nil {
				return ir.ProtocolError("stop_details is malformed")
			}
			if len(v) > 0 {
				delta["stop_details"] = json.RawMessage(v)
			}
		}
		// The pinned SDK reads message_delta.delta.container, not a top-level
		// container. context_management and input_transformations stay on the
		// event itself.
		if len(ev.Container) > 0 {
			v, err := validateOptionalObject(ev.Container, "container")
			if err != nil {
				return ir.ProtocolError("container is malformed")
			}
			if len(v) > 0 {
				delta["container"] = json.RawMessage(v)
			}
		}
		payload := map[string]any{"delta": delta, "usage": usage}
		if len(ev.ContextManagement) > 0 {
			v, err := validateOptionalObject(ev.ContextManagement, "context_management")
			if err != nil {
				return ir.ProtocolError("context_management is malformed")
			}
			if len(v) > 0 {
				payload["context_management"] = json.RawMessage(v)
			}
		}
		if len(ev.InputTransformations) > 0 {
			v, err := validateOptionalArray(ev.InputTransformations, "input_transformations")
			if err != nil {
				return ir.ProtocolError("input_transformations is malformed")
			}
			if len(v) > 0 {
				payload["input_transformations"] = json.RawMessage(v)
			}
		}
		if err := e.event(w, "message_delta", payload); err != nil {
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
	if len(e.servers) != 0 {
		return ir.ProtocolError("server block closed by stream end")
	}
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
