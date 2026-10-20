package responses

import (
	"strconv"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// Encoder-side reasoning retention bounds. Identity, summary, and opaque bytes
// all count so a hostile stream cannot grow the final response snapshot without
// bound. The limits are per stream and never raise the decoder or collector
// budgets.
const (
	maxEncoderReasoningItems     = 1024
	maxEncoderReasoningBytes     = 64 << 20
	maxEncoderReasoningParts     = 1024
	encoderReasoningItemOverhead = 128
	encoderReasoningPartOverhead = 128
	// encoderReasoningKeyOverhead approximates one map entry. The key string
	// bytes are charged separately where a key is retained.
	encoderReasoningKeyOverhead = 32
	reasoningLegacyKey          = "legacy"
	// Encoder work held behind an unresolved reasoning identity mirrors the
	// translated pending bound: 8 MiB and 1024 events. The limits are local
	// because the responses package cannot import the proxy package; they do not
	// raise the proxy bound and the reasoning retention budget stays separate.
	maxEncoderPendingEvents = 1024
	maxEncoderPendingBytes  = 8 << 20
	encoderPendingOverhead  = 64
)

// blockingDeferredReasoning returns the first retained reasoning item that has
// no source ID and is neither open nor done. Its wire ID would be generated if
// released, so later output-ordering work must wait.
func (e *StreamEncoder) blockingDeferredReasoning() *respReasoningItem {
	for _, item := range e.reasoningDeferred {
		if item == nil || item.done || item.open || item.id != "" {
			continue
		}
		return item
	}
	return nil
}

// resolvesDeferredHead reports whether ev supplies the source ID for the
// unresolved item. Only a matching streamed output index proves the same item;
// an ID alone cannot be attached to an index-keyed item.
func (e *StreamEncoder) resolvesDeferredHead(ev ir.StreamEvent, head *respReasoningItem) bool {
	if head == nil || ev.ReasoningID == "" {
		return false
	}
	return head.srcIndexSet && ev.ReasoningIndexSet && ev.ReasoningIndex == head.srcIndex
}

// encoderPendingEventBytes is the retained payload plus fixed bookkeeping for
// one queued event. Strings that will also be charged on replay are counted
// again while queued, which is conservative.
func encoderPendingEventBytes(ev ir.StreamEvent) int {
	n := encoderPendingOverhead + len(ev.ID) + len(ev.Model) + len(ev.Text) +
		len(ev.ToolID) + len(ev.ToolName) + len(ev.ArgsFrag) + len(ev.Signature) +
		len(ev.Data) + len(ev.ReasoningID)
	if ev.Reasoning != nil {
		n += len(ev.Reasoning.ID) + len(ev.Reasoning.Status)
		if ev.Reasoning.EncryptedContent != nil {
			n += len(*ev.Reasoning.EncryptedContent)
		}
		for _, p := range ev.Reasoning.Summary {
			n += len(p.Type) + len(p.Text)
		}
	}
	return n
}

// mustQueueForDeferred reports whether ev would close, open, or append content
// ahead of an unresolved deferred item. Events that do not force output
// ordering pass through. An event that supplies the head identity passes too.
func (e *StreamEncoder) mustQueueForDeferred(ev ir.StreamEvent) bool {
	head := e.blockingDeferredReasoning()
	if head == nil {
		return false
	}
	switch ev.Kind {
	case ir.EventTextStart:
		return ev.Text != "" && e.open != openMessage
	case ir.EventTextDelta:
		return ev.Text != "" && e.open != openMessage
	case ir.EventToolCallStart:
		if ev.ToolName == "" {
			return false
		}
		t := e.tools[ev.Index]
		return t == nil || t.itemID == ""
	case ir.EventReasoningDelta:
		// Anthropic thinking and legacy readable-only deltas keep the immediate
		// path. Responses deltas queue only when their item identity is pending,
		// and a delta that carries the pending identity is appended like any
		// other event; binding happens at the queue head in arrival order.
		if ev.Indexed || (ev.ReasoningID == "" && !ev.ReasoningIndexSet) {
			return false
		}
		head := e.blockingDeferredReasoning()
		if head == nil {
			return false
		}
		return !e.resolvesDeferredHead(ev, head)
	case ir.EventReasoningItemDone:
		return !e.resolvesDeferredHead(ev, head)
	case ir.EventReasoningStart:
		// An Anthropic thinking start carries the block index, not Responses
		// identity, and never forces finalization.
		if ev.Indexed || (ev.ReasoningID == "" && !ev.ReasoningIndexSet) {
			return false
		}
		return !e.resolvesDeferredHead(ev, head)
	default:
		return false
	}
}

// queueEvent retains one event behind an unresolved identity. The count and
// payload budgets mirror the translated pending bound and never grow the
// reasoning retention budget.
func (e *StreamEncoder) queueEvent(ev ir.StreamEvent) error {
	n := encoderPendingEventBytes(ev)
	if len(e.pendingEvents) >= maxEncoderPendingEvents || n > maxEncoderPendingBytes-e.pendingBytes {
		return ir.ProtocolError("reasoning identity pending limit exceeded")
	}
	e.pendingEvents = append(e.pendingEvents, ev)
	e.pendingBytes += n
	return nil
}

// bindPendingIdentity applies a source ID from a later queued event to item
// before any frame is emitted. Only a matching streamed index proves the same
// item, so a queue cannot attach an unrelated ID.
func (e *StreamEncoder) bindPendingIdentity(item *respReasoningItem) error {
	if item == nil || item.id != "" || !item.srcIndexSet {
		return nil
	}
	for i := range e.pendingEvents {
		ev := &e.pendingEvents[i]
		if ev.ReasoningID == "" || !ev.ReasoningIndexSet || ev.ReasoningIndex != item.srcIndex {
			continue
		}
		return e.bindReasoningID(item, ev.ReasoningID)
	}
	return nil
}

type respReasoningPart struct {
	typ    string
	text   strings.Builder
	closed bool
}

// respReasoningItem is one open, deferred, or completed Responses reasoning item
// on the encoder side. id is the source-known item ID, which can arrive after
// streamed summary bytes. wireID is the single ID used by every emitted frame;
// it is fixed when output_item.added is written. deferred marks retained summary
// bytes that have not been emitted because the identity was not yet known.
type respReasoningItem struct {
	key       string
	id        string
	wireID    string
	outIdx    int
	status    string
	encrypted *string
	parts     []*respReasoningPart
	deferred  bool
	queued    bool
	open      bool
	done      bool
	item      map[string]any
	// srcIndex is the source streamed output_index when the item was keyed by
	// it. It lets a later queued event with the same index supply the missing
	// source ID before any generated wire ID is emitted.
	srcIndex    int
	srcIndexSet bool
}

func reasoningEventKey(ev ir.StreamEvent) string {
	if ev.ReasoningID != "" {
		return "id:" + ev.ReasoningID
	}
	if ev.ReasoningIndexSet {
		return "idx:" + strconv.Itoa(ev.ReasoningIndex)
	}
	if ev.Indexed {
		// Anthropic thinking blocks use their own content-block index. Distinct
		// blocks must stay distinct Reasoning items when projected to Responses.
		return "anx:" + strconv.Itoa(ev.Index)
	}
	return reasoningLegacyKey
}

func (e *StreamEncoder) chargeReasoning(n int) error {
	if n <= 0 {
		return nil
	}
	if n > e.maxReasoningBytes-e.reasoningBytes {
		return ir.ProtocolError("reasoning state too large")
	}
	e.reasoningBytes += n
	return nil
}

func (e *StreamEncoder) ensureReasoningItem(key string) (*respReasoningItem, error) {
	if item := e.reasoningItems[key]; item != nil {
		return item, nil
	}
	if len(e.reasoningItems) >= e.maxReasoningItems {
		return nil, ir.ProtocolError("too many reasoning items")
	}
	if err := e.chargeReasoning(encoderReasoningItemOverhead + len(key) + encoderReasoningKeyOverhead); err != nil {
		return nil, err
	}
	item := &respReasoningItem{key: key}
	if e.reasoningItems == nil {
		e.reasoningItems = map[string]*respReasoningItem{}
	}
	e.reasoningItems[key] = item
	return item, nil
}

// newDetachedReasoningItem creates an item that no streamed delta can bind to.
// It keeps two unidentified complete snapshots from collapsing into one item.
func (e *StreamEncoder) newDetachedReasoningItem(kind string) (*respReasoningItem, error) {
	e.reasoningSeq++
	return e.ensureReasoningItem(kind + ":" + strconv.Itoa(e.reasoningSeq))
}

// bindReasoningID records the source item ID once and keeps the streamed index
// alias, so later index-only frames still resolve the same item the way the
// decoder's byIndex/byItem maps do. Charging happens before the string is
// retained. A conflicting second ID is a payload-free error.
func (e *StreamEncoder) bindReasoningID(item *respReasoningItem, id string) error {
	if id == "" || item.id == id {
		return nil
	}
	if item.id != "" {
		return ir.ProtocolError("conflicting reasoning item identity")
	}
	idKey := "id:" + id
	if err := e.chargeReasoning(len(id) + len(idKey) + encoderReasoningKeyOverhead); err != nil {
		return err
	}
	item.id = id
	if e.reasoningItems == nil {
		e.reasoningItems = map[string]*respReasoningItem{}
	}
	if e.reasoningItems[idKey] == nil {
		e.reasoningItems[idKey] = item
	}
	return nil
}

// structuredReasoningForEvent resolves an item for an event that carries an ID,
// an output index, or both. An index-only item re-keys when the source ID
// arrives, so the source ID becomes the emitted wire ID and no generated ID is
// ever changed after output_item.added.
func (e *StreamEncoder) structuredReasoningForEvent(ev ir.StreamEvent) (*respReasoningItem, error) {
	idKey := ""
	if ev.ReasoningID != "" {
		idKey = "id:" + ev.ReasoningID
		if item := e.reasoningItems[idKey]; item != nil {
			return item, nil
		}
	}
	if ev.ReasoningIndexSet {
		idxKey := "idx:" + strconv.Itoa(ev.ReasoningIndex)
		if item := e.reasoningItems[idxKey]; item != nil {
			if !item.srcIndexSet {
				item.srcIndex = ev.ReasoningIndex
				item.srcIndexSet = true
			}
			return item, nil
		}
	}
	item, err := e.ensureReasoningItem(reasoningEventKey(ev))
	if err != nil {
		return nil, err
	}
	if ev.ReasoningIndexSet && !item.srcIndexSet {
		item.srcIndex = ev.ReasoningIndex
		item.srcIndexSet = true
	}
	return item, nil
}

// reasoningForDelta resolves the item for a summary delta. Legacy unindexed
// deltas start a new item after the previous segment closed so repeated readable
// segments stay distinct. They carry no late identity, so they stream
// immediately with a generated wire ID.
func (e *StreamEncoder) reasoningForDelta(ev ir.StreamEvent) (*respReasoningItem, error) {
	if ev.ReasoningID == "" && !ev.ReasoningIndexSet && !ev.Indexed {
		item := e.reasoningItems[reasoningLegacyKey]
		if item == nil {
			return e.ensureReasoningItem(reasoningLegacyKey)
		}
		if item.done {
			// Detach the completed segment so its retained state can be released
			// and the item cap does not accumulate one entry per segment.
			delete(e.reasoningItems, reasoningLegacyKey)
			return e.ensureReasoningItem(reasoningLegacyKey)
		}
		return item, nil
	}
	return e.structuredReasoningForEvent(ev)
}

// reasoningForDone resolves the item for a complete snapshot. An unidentified
// snapshot claims the still-open legacy segment; otherwise it becomes its own
// item so distinct unidentified items never collapse into one.
func (e *StreamEncoder) reasoningForDone(ev ir.StreamEvent) (*respReasoningItem, error) {
	if ev.ReasoningID == "" && !ev.ReasoningIndexSet && !ev.Indexed {
		if item := e.reasoningItems[reasoningLegacyKey]; item != nil && !item.done {
			return item, nil
		}
		return e.newDetachedReasoningItem("done")
	}
	return e.structuredReasoningForEvent(ev)
}

// reasoningPart returns the summary part at index, creating bounded empty
// placeholders for gaps. Creation is charged before the slice grows.
func (e *StreamEncoder) reasoningPart(item *respReasoningItem, index int) (*respReasoningPart, error) {
	if index < 0 || index >= e.maxReasoningParts {
		return nil, ir.ProtocolError("reasoning summary index out of range")
	}
	for len(item.parts) <= index {
		if e.reasoningParts >= e.maxReasoningParts {
			return nil, ir.ProtocolError("too many reasoning summary parts")
		}
		if err := e.chargeReasoning(encoderReasoningPartOverhead); err != nil {
			return nil, err
		}
		item.parts = append(item.parts, &respReasoningPart{})
		e.reasoningParts++
	}
	return item.parts[index], nil
}

// openReasoning emits output_item.added once and makes item the current open
// reasoning item. A deferred item replays its retained summary parts as frames.
// Any open message, reasoning, or function item closes first so output ordering
// follows arrival order.
func (e *StreamEncoder) openReasoning(w *sse.Writer, item *respReasoningItem) error {
	if item.done {
		return ir.ProtocolError("reasoning item reused after done")
	}
	if item.open {
		return nil
	}
	if err := e.ensureCreated(w); err != nil {
		return err
	}
	if err := e.flushDeferredBefore(w, item); err != nil {
		return err
	}
	if e.open == openMessage || e.open == openReasoning {
		if err := e.closeOpen(w); err != nil {
			return err
		}
	}
	if err := e.closeOpenFunctions(w); err != nil {
		return err
	}
	if item.wireID == "" {
		if item.id != "" {
			item.wireID = item.id
		} else {
			if err := e.chargeReasoning(len("rs_") + 16); err != nil {
				return err
			}
			item.wireID = ir.NewID("rs_")
		}
	}
	item.outIdx = e.outputIndex
	e.outputIndex++
	item.open = true
	e.reasoningOpen = item
	e.reasoningOpenPart = -1
	e.open = openReasoning
	e.openItemID = item.wireID
	e.openOutIdx = item.outIdx
	if err := e.emit(w, "response.output_item.added", map[string]any{
		"output_index": item.outIdx,
		"item":         map[string]any{"id": item.wireID, "type": "reasoning", "summary": []any{}},
	}); err != nil {
		return err
	}
	if item.deferred {
		item.deferred = false
		if err := e.replayDeferredReasoningParts(w, item); err != nil {
			return err
		}
	}
	return nil
}

// replayDeferredReasoningParts emits the retained summary bytes of an item that
// was buffered while its identity was unknown. Bytes were charged when
// retained, so no new charge happens here.
func (e *StreamEncoder) replayDeferredReasoningParts(w *sse.Writer, item *respReasoningItem) error {
	for i, part := range item.parts {
		if part == nil {
			continue
		}
		if _, err := e.openReasoningPart(w, item, i); err != nil {
			return err
		}
		full := part.text.String()
		if full == "" {
			continue
		}
		if err := e.emit(w, "response.reasoning_summary_text.delta", map[string]any{
			"item_id": item.wireID, "output_index": item.outIdx, "summary_index": i, "delta": full,
		}); err != nil {
			return err
		}
	}
	return nil
}

// openReasoningPart closes the current part and opens index. Empty parts keep
// their boundary: each part emits its own added/done pair.
func (e *StreamEncoder) openReasoningPart(w *sse.Writer, item *respReasoningItem, index int) (*respReasoningPart, error) {
	part, err := e.reasoningPart(item, index)
	if err != nil {
		return nil, err
	}
	if e.reasoningOpen == item && e.reasoningOpenPart == index {
		return part, nil
	}
	if err := e.closeReasoningPart(w); err != nil {
		return nil, err
	}
	if part.closed {
		return nil, ir.ProtocolError("reasoning summary part reused")
	}
	if part.typ == "" {
		part.typ = "summary_text"
	}
	e.reasoningOpen = item
	e.reasoningOpenPart = index
	return part, e.emit(w, "response.reasoning_summary_part.added", map[string]any{
		"item_id": item.wireID, "output_index": item.outIdx, "summary_index": index,
		"part": map[string]any{"type": part.typ, "text": ""},
	})
}

func (e *StreamEncoder) closeReasoningPart(w *sse.Writer) error {
	if e.reasoningOpen == nil || e.reasoningOpenPart < 0 {
		return nil
	}
	item := e.reasoningOpen
	index := e.reasoningOpenPart
	part := item.parts[index]
	full := part.text.String()
	typ := part.typ
	if typ == "" {
		typ = "summary_text"
	}
	if err := e.emit(w, "response.reasoning_summary_text.done", map[string]any{
		"item_id": item.wireID, "output_index": item.outIdx, "summary_index": index, "text": full,
	}); err != nil {
		return err
	}
	if err := e.emit(w, "response.reasoning_summary_part.done", map[string]any{
		"item_id": item.wireID, "output_index": item.outIdx, "summary_index": index,
		"part": map[string]any{"type": typ, "text": full},
	}); err != nil {
		return err
	}
	part.closed = true
	e.reasoningOpenPart = -1
	return nil
}

// appendDeferredReasoningText retains summary bytes for an item whose identity
// is not yet known. The item is queued for replay in arrival order.
func (e *StreamEncoder) appendDeferredReasoningText(item *respReasoningItem, index int, text string) error {
	if text == "" {
		return nil
	}
	part, err := e.reasoningPart(item, index)
	if err != nil {
		return err
	}
	if part.closed {
		return ir.ProtocolError("reasoning summary part reused")
	}
	if err := e.chargeReasoning(len(text)); err != nil {
		return err
	}
	part.text.WriteString(text)
	if !item.deferred {
		item.deferred = true
		e.reasoningDeferred = append(e.reasoningDeferred, item)
	}
	item.queued = true
	return nil
}

// flushDeferredBefore closes deferred items that arrived before item. It keeps
// output order when a later identified item opens ahead of an earlier
// index-only item. A deferred item that is no longer queued is already being
// closed by closeDeferredReasoning; every queued item then arrived after it, so
// flushing would reverse the order and close the item that is being opened.
func (e *StreamEncoder) flushDeferredBefore(w *sse.Writer, item *respReasoningItem) error {
	if item.deferred && !item.queued {
		return nil
	}
	for len(e.reasoningDeferred) > 0 && e.reasoningDeferred[0] != item {
		next := e.reasoningDeferred[0]
		e.reasoningDeferred = e.reasoningDeferred[1:]
		next.queued = false
		if next.done {
			continue
		}
		if err := e.closeReasoningItem(w, next); err != nil {
			return err
		}
	}
	return nil
}

// closeDeferredReasoning closes every remaining deferred item in arrival order.
// The head stays queued while it closes so flushDeferredBefore cannot close
// later items first and re-enter the item being opened.
func (e *StreamEncoder) closeDeferredReasoning(w *sse.Writer) error {
	for len(e.reasoningDeferred) > 0 {
		item := e.reasoningDeferred[0]
		if item.done {
			e.reasoningDeferred = e.reasoningDeferred[1:]
			item.queued = false
			continue
		}
		if err := e.closeReasoningItem(w, item); err != nil {
			return err
		}
		if len(e.reasoningDeferred) > 0 && e.reasoningDeferred[0] == item {
			e.reasoningDeferred = e.reasoningDeferred[1:]
		}
		item.queued = false
	}
	return nil
}

// encodeReasoningDelta appends readable summary bytes to the tracked item and
// emits one delta frame. An index-only item defers frames until its ID is known
// or the item must close.
func (e *StreamEncoder) encodeReasoningDelta(w *sse.Writer, ev ir.StreamEvent) error {
	if ev.Text == "" {
		return nil
	}
	item, err := e.reasoningForDelta(ev)
	if err != nil {
		return err
	}
	if ev.ReasoningID != "" {
		if err := e.bindReasoningID(item, ev.ReasoningID); err != nil {
			return err
		}
	}
	if err := e.bindPendingIdentity(item); err != nil {
		return err
	}
	if item.done {
		return ir.ProtocolError("reasoning summary after item done")
	}
	if item.id == "" && ev.ReasoningIndexSet && !item.open && !e.flushing {
		return e.appendDeferredReasoningText(item, ev.SummaryIndex, ev.Text)
	}
	if err := e.openReasoning(w, item); err != nil {
		return err
	}
	part, err := e.openReasoningPart(w, item, ev.SummaryIndex)
	if err != nil {
		return err
	}
	if err := e.chargeReasoning(len(ev.Text)); err != nil {
		return err
	}
	part.text.WriteString(ev.Text)
	return e.emit(w, "response.reasoning_summary_text.delta", map[string]any{
		"item_id": item.wireID, "output_index": item.outIdx, "summary_index": ev.SummaryIndex, "delta": ev.Text,
	})
}

// encodeReasoningDone applies one complete snapshot, emits any summary suffix
// the encoder has not written yet, and closes the item. A repeated identical
// snapshot is a no-op. Conflicting text, identity, or opaque state is a
// payload-free protocol error. A late source ID is used only while the item has
// not emitted output_item.added, so no wire ID changes mid-lifecycle.
func (e *StreamEncoder) encodeReasoningDone(w *sse.Writer, ev ir.StreamEvent) error {
	item, err := e.reasoningForDone(ev)
	if err != nil {
		return err
	}
	snap := ev.Reasoning
	if snap != nil && snap.ID != "" {
		if err := e.bindReasoningID(item, snap.ID); err != nil {
			return err
		}
	}
	if err := e.bindPendingIdentity(item); err != nil {
		return err
	}
	if item.done {
		return e.applyReasoningMetadata(item, snap)
	}
	if snap != nil {
		// A complete snapshot may not shrink below the summary parts already
		// retained from deltas or earlier snapshots.
		if snap.Summary != nil && len(snap.Summary) < len(item.parts) {
			return ir.ProtocolError("conflicting reasoning summary")
		}
	}
	if err := e.openReasoning(w, item); err != nil {
		return err
	}
	if snap != nil {
		for i, part := range snap.Summary {
			if err := e.reconcileReasoningSummary(w, item, i, part); err != nil {
				return err
			}
		}
		if err := e.applyReasoningMetadata(item, snap); err != nil {
			return err
		}
	}
	return e.closeReasoningItem(w, item)
}

// reconcileDeferredSnapshot grows an unopened deferred item's retained parts
// from a complete snapshot without emitting frames. openReasoning replays the
// full parts afterwards in index order. Type and text conflicts fail closed.
func (e *StreamEncoder) reconcileDeferredSnapshot(item *respReasoningItem, snap *ir.ResponsesReasoning) error {
	if snap.Summary != nil {
		for i, part := range snap.Summary {
			target, err := e.reasoningPart(item, i)
			if err != nil {
				return err
			}
			if part.Type != "" {
				if target.typ != "" && target.typ != part.Type {
					return ir.ProtocolError("conflicting reasoning summary")
				}
				if target.typ == "" {
					if err := e.chargeReasoning(len(part.Type)); err != nil {
						return err
					}
					target.typ = part.Type
				}
			}
			retained := target.text.String()
			switch {
			case retained == part.Text:
			case strings.HasPrefix(part.Text, retained):
				suffix := part.Text[len(retained):]
				if err := e.chargeReasoning(len(suffix)); err != nil {
					return err
				}
				target.text.WriteString(suffix)
			default:
				return ir.ProtocolError("conflicting reasoning summary")
			}
		}
	}
	return nil
}

// reconcileReasoningSummary grows one part to the snapshot text. Bytes already
// streamed are not emitted again; a snapshot that does not extend them errors.
// A part that already closed is only accepted when it matches exactly. Each
// accepted part text is written once and emits at most one delta frame.
func (e *StreamEncoder) reconcileReasoningSummary(w *sse.Writer, item *respReasoningItem, index int, part ir.ReasoningSummaryPart) error {
	if err := e.openReasoning(w, item); err != nil {
		return err
	}
	target, err := e.reasoningPart(item, index)
	if err != nil {
		return err
	}
	if part.Type != "" && target.typ == "" {
		if err := e.chargeReasoning(len(part.Type)); err != nil {
			return err
		}
		target.typ = part.Type
	}
	retained := target.text.String()
	var suffix string
	switch {
	case retained == part.Text:
		// Nothing to grow, but an unopened part still needs its own added frame
		// so the SSE part lifecycle matches the final output array.
		if target.closed || (item.deferred && !item.open) {
			return nil
		}
		_, err = e.openReasoningPart(w, item, index)
		return err
	case strings.HasPrefix(part.Text, retained):
		suffix = part.Text[len(retained):]
	default:
		return ir.ProtocolError("conflicting reasoning summary")
	}
	if target.closed {
		// A closed part only accepts its exact final text; a suffix would
		// contradict the done frame the client already saw.
		return ir.ProtocolError("conflicting reasoning summary")
	}
	if err := e.chargeReasoning(len(suffix)); err != nil {
		return err
	}
	target.text.WriteString(suffix)
	_, err = e.openReasoningPart(w, item, index)
	if err != nil {
		return err
	}
	return e.emit(w, "response.reasoning_summary_text.delta", map[string]any{
		"item_id": item.wireID, "output_index": item.outIdx, "summary_index": index, "delta": suffix,
	})
}

// applyReasoningMetadata merges status and opaque content into the item. Status
// may advance before the item closes; after that a different value conflicts.
// Growth is charged before the retained copy grows. Once the item is done, a
// snapshot may add absent metadata and consistent extra empty parts only: any
// shorter summary array, changed part type, or changed text contradicts the
// closed item and fails instead of silently replacing state.
func (e *StreamEncoder) applyReasoningMetadata(item *respReasoningItem, snap *ir.ResponsesReasoning) error {
	if snap == nil {
		return nil
	}
	if item.done && snap.Summary != nil {
		if err := e.validateClosedSummary(item, snap); err != nil {
			return err
		}
	}
	if snap.Status != "" && item.status != snap.Status {
		if item.status != "" && item.done {
			return ir.ProtocolError("conflicting reasoning item status")
		}
		if growth := len(snap.Status) - len(item.status); growth > 0 {
			if err := e.chargeReasoning(growth); err != nil {
				return err
			}
		}
		item.status = snap.Status
	}
	if snap.EncryptedContent != nil {
		if item.encrypted != nil {
			if *item.encrypted != *snap.EncryptedContent {
				return ir.ProtocolError("conflicting reasoning encrypted content")
			}
		} else {
			if err := e.chargeReasoning(len(*snap.EncryptedContent)); err != nil {
				return err
			}
			v := *snap.EncryptedContent
			item.encrypted = &v
		}
	}
	if item.done && item.item != nil {
		item.item["status"] = item.status
		if item.status == "" {
			delete(item.item, "status")
		}
		if item.encrypted != nil {
			item.item["encrypted_content"] = *item.encrypted
		}
		// The decoder rejects a non-empty suffix after item done, but it still
		// accepts extra empty summary parts on a late snapshot, so the terminal
		// item must widen to match.
		summary, _ := item.item["summary"].([]any)
		for len(summary) < len(snap.Summary) {
			next := snap.Summary[len(summary)]
			if next.Text != "" {
				return ir.ProtocolError("conflicting reasoning summary")
			}
			if e.reasoningParts >= e.maxReasoningParts {
				return ir.ProtocolError("too many reasoning summary parts")
			}
			typ := next.Type
			if typ == "" {
				typ = "summary_text"
			}
			if err := e.chargeReasoning(encoderReasoningPartOverhead + len(typ)); err != nil {
				return err
			}
			e.reasoningParts++
			summary = append(summary, map[string]any{"type": typ, "text": ""})
		}
		item.item["summary"] = summary
	}
	return nil
}

// validateClosedSummary checks a snapshot against the closed item's summary.
// A complete array may not shrink and may not change an already emitted part's
// type or text. Extra parts are accepted only when they are empty placeholders.
func (e *StreamEncoder) validateClosedSummary(item *respReasoningItem, snap *ir.ResponsesReasoning) error {
	summary, _ := item.item["summary"].([]any)
	if len(snap.Summary) < len(summary) {
		return ir.ProtocolError("conflicting reasoning summary")
	}
	for i := range summary {
		entry, _ := summary[i].(map[string]any)
		wantType, _ := entry["type"].(string)
		wantText, _ := entry["text"].(string)
		part := snap.Summary[i]
		if part.Type != "" && part.Type != wantType {
			return ir.ProtocolError("conflicting reasoning summary")
		}
		if part.Text != wantText {
			return ir.ProtocolError("conflicting reasoning summary")
		}
	}
	return nil
}

// closeReasoningItem emits the closing frames and records the item for the
// final response snapshot. A deferred item opens first. Summary parts come from
// the accumulated stream; a complete snapshot is applied by the caller before
// this point. The retained terminal map is charged before it is built.
func (e *StreamEncoder) closeReasoningItem(w *sse.Writer, item *respReasoningItem) error {
	if item.done {
		return nil
	}
	if err := e.openReasoning(w, item); err != nil {
		return err
	}
	if err := e.closeReasoningPart(w); err != nil {
		return err
	}
	retained := encoderReasoningItemOverhead + len(item.wireID) + len(item.status)
	if item.encrypted != nil {
		retained += len(*item.encrypted)
	}
	for _, part := range item.parts {
		retained += encoderReasoningPartOverhead
		if part.typ != "" {
			retained += len(part.typ)
		}
		retained += part.text.Len()
	}
	if err := e.chargeReasoning(retained); err != nil {
		return err
	}
	summary := make([]any, 0, len(item.parts))
	for _, part := range item.parts {
		typ := part.typ
		if typ == "" {
			typ = "summary_text"
		}
		summary = append(summary, map[string]any{"type": typ, "text": part.text.String()})
	}
	out := map[string]any{"id": item.wireID, "type": "reasoning", "summary": summary}
	if item.status != "" {
		out["status"] = item.status
	}
	if item.encrypted != nil {
		out["encrypted_content"] = *item.encrypted
	}
	item.item = out
	item.done = true
	item.open = false
	e.items = append(e.items, out)
	if e.reasoningOpen == item {
		e.reasoningOpen = nil
		e.reasoningOpenPart = -1
		e.open = openNone
	}
	return e.emit(w, "response.output_item.done", map[string]any{"output_index": item.outIdx, "item": out})
}
