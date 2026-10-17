package responses

import (
	"strings"

	"airouter/internal/proxy/ir"
)

// Responses reasoning retention mirrors the tool budgets. Summary parts are
// additionally capped because a summary_index flood must not allocate an
// unbounded slice. Overhead is charged before an item or part is allocated.
const (
	maxRetainedReasoningBytes = 64 << 20
	maxTrackedReasoningItems  = 1024
	maxTrackedReasoningParts  = 1024
	reasoningItemOverhead     = 128
	reasoningPartOverhead     = 128
	// reasoningKeyOverhead approximates one map entry keyed by an item ID or a
	// streamed output index. ID bytes are charged separately with the string.
	reasoningKeyOverhead = 32
)

type responsesReasoningPart struct {
	typ  string
	text strings.Builder
	// emitted is the number of summary bytes already sent as deltas. A quiet
	// final reconcile grows text without emitting; the ordered final pass sends
	// only the un-emitted suffix and stays idempotent across repeated snapshots.
	emitted int
}

// responsesReasoningItem tracks one Responses reasoning item across streamed
// summary deltas and terminal snapshots. itemID and streamed output index are
// distinct identities; neither is the terminal output-array ordinal.
type responsesReasoningItem struct {
	id           string
	hasIndex     bool
	outputIndex  int
	fromFinal    bool
	finalOrdinal int
	// finalMatched records that a terminal output-array item claimed this
	// streamed item. A second, different terminal ordinal must not reuse it.
	finalMatched bool

	status    string
	encrypted *string
	parts     []*responsesReasoningPart
	// orderMsgs is the message count when the item was created. An unresolved
	// message registered after the item does not defer it.
	orderMsgs int
	// wired records that at least one event reached the consumer, so a later
	// final-only slot at an earlier array ordinal cannot silently reorder it.
	wired bool

	// done marks that a terminal snapshot was accepted. Later summary growth is
	// a conflict; late absent-to-present opaque metadata is still allowed.
	done bool
	// emitted is the last complete snapshot handed to the emitter, used to
	// dedup repeated item.done and response.completed snapshots.
	emitted *ir.ResponsesReasoning
}

type reasoningIdentity struct {
	outputIndex  int
	hasIndex     bool
	itemID       string
	finalOrdinal int
}

// responsesReasoningDecoder reconciles summary deltas and complete reasoning
// snapshots for one Responses stream. It emits only readable summary bytes as
// EventReasoningDelta and carries opaque state in EventReasoningItemDone.
type responsesReasoningDecoder struct {
	emit          func(ir.StreamEvent) error
	ensureStarted func() error
	maxBytes      int
	maxItems      int
	maxParts      int

	items   []*responsesReasoningItem
	byItem  map[string]*responsesReasoningItem
	byIndex map[int]*responsesReasoningItem
	byFinal map[int]*responsesReasoningItem

	bytes int
	parts int
	done  bool

	// orderEmit defers one ordering-sensitive event behind an unresolved
	// earlier message. It is nil for a standalone decoder, which emits directly.
	orderEmit func(item *responsesReasoningItem, ev ir.StreamEvent) error
	// msgCount reports the number of message items registered so far, so a new
	// item can record its position relative to an open message.
	msgCount func() int
}

func newResponsesReasoningDecoder(emit func(ir.StreamEvent) error, ensureStarted func() error, maxBytes, maxItems, maxParts int) *responsesReasoningDecoder {
	return &responsesReasoningDecoder{
		emit: emit, ensureStarted: ensureStarted,
		maxBytes: maxBytes, maxItems: maxItems, maxParts: maxParts,
	}
}

func (r *responsesReasoningDecoder) ensureMessageStart() error {
	if r.ensureStarted == nil {
		return nil
	}
	return r.ensureStarted()
}

// emitOrdered routes one event through the ordering gate when one is set.
func (r *responsesReasoningDecoder) emitOrdered(item *responsesReasoningItem, ev ir.StreamEvent) error {
	if r.orderEmit != nil {
		return r.orderEmit(item, ev)
	}
	return r.emit(ev)
}

func (r *responsesReasoningDecoder) countMsgs() int {
	if r.msgCount == nil {
		return 0
	}
	return r.msgCount()
}

// observeAdded binds an output_item.added reasoning item and applies any
// snapshot fields the frame supplied.
func (r *responsesReasoningDecoder) observeAdded(env *streamEnvelope) error {
	if env.Item == nil {
		return nil
	}
	item, err := r.bind(reasoningIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet,
		itemID: env.Item.ID,
	}, true, false)
	if err != nil {
		return err
	}
	return r.applyItemSnapshot(item, reasoningSnapshot{Summary: env.Item.Summary, SummarySet: env.Item.SummarySet, EncryptedContent: env.Item.EncryptedContent, Status: env.Item.Status}, false, true)
}

// observeDelta appends one summary delta. An empty delta binds identity but
// emits nothing, matching the legacy empty-frame behavior.
func (r *responsesReasoningDecoder) observeDelta(env *streamEnvelope) error {
	itemID := env.ItemID
	if itemID == "" && env.Item != nil {
		itemID = env.Item.ID
	}
	if env.Delta == "" && itemID == "" && !env.OutputIndexSet {
		return nil
	}
	item, err := r.bind(reasoningIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet, itemID: itemID,
	}, true, false)
	if err != nil {
		return err
	}
	if env.Delta == "" {
		return nil
	}
	return r.appendDelta(item, env.SummaryIndex, env.Delta)
}

// observePart applies a summary_part.added or summary_part.done frame. Part
// text is reconciled once; already streamed bytes are never re-emitted.
func (r *responsesReasoningDecoder) observePart(env *streamEnvelope, done bool) error {
	itemID := env.ItemID
	if itemID == "" && env.Item != nil {
		itemID = env.Item.ID
	}
	if env.Part == nil && env.Text == "" && itemID == "" && !env.OutputIndexSet {
		return nil
	}
	item, err := r.bind(reasoningIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet, itemID: itemID,
	}, true, false)
	if err != nil {
		return err
	}
	if env.Part != nil {
		if env.Part.Type != "" {
			part, err := r.part(item, env.SummaryIndex)
			if err != nil {
				return err
			}
			if err := r.applyPartType(part, env.Part.Type); err != nil {
				return err
			}
		}
		if env.Part.TextSet {
			return r.reconcileSummary(item, env.SummaryIndex, env.Part.Text, false, true)
		}
	}
	if env.TextSet {
		return r.reconcileSummary(item, env.SummaryIndex, env.Text, false, true)
	}
	_ = done
	return nil
}

// observeTextDone applies a reasoning_summary_text.done frame whose text is the
// complete part text. It recovers summary bytes when deltas were lost.
func (r *responsesReasoningDecoder) observeTextDone(env *streamEnvelope) error {
	return r.observePart(env, true)
}

// observeItemDone applies a complete item snapshot and closes the item. Some
// producers omit output_item.done entirely; the final response pass recovers
// those items.
func (r *responsesReasoningDecoder) observeItemDone(env *streamEnvelope) error {
	if env.Item == nil {
		return nil
	}
	item, err := r.bind(reasoningIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet,
		itemID: env.Item.ID,
	}, true, false)
	if err != nil {
		return err
	}
	if err := r.applyItemSnapshot(item, reasoningSnapshot{Summary: env.Item.Summary, SummarySet: env.Item.SummarySet, EncryptedContent: env.Item.EncryptedContent, Status: env.Item.Status}, true, true); err != nil {
		return err
	}
	return r.emitDone(item)
}

// matchFinal binds one terminal output-array reasoning item. The array ordinal
// is not treated as a streamed output_index; it only disambiguates an item that
// carries no usable identity.
func (r *responsesReasoningDecoder) matchFinal(ordinal int, item *respOutputItem) (*responsesReasoningItem, error) {
	if known := r.byFinal[ordinal]; known != nil {
		// Same terminal position, but a changed identity is a provider
		// contradiction, not a metadata update.
		if err := r.assign(known, reasoningIdentity{itemID: item.ID}); err != nil {
			return nil, err
		}
		return known, nil
	}
	if item.ID != "" {
		if found, err := r.bind(reasoningIdentity{itemID: item.ID}, false, false); err != nil {
			return nil, err
		} else if found != nil {
			if err := r.claimFinal(found, ordinal); err != nil {
				return nil, err
			}
			return found, nil
		}
		if found := r.uniqueIndexMatch(ordinal); found != nil {
			if err := r.assign(found, reasoningIdentity{itemID: item.ID}); err != nil {
				return nil, err
			}
			if err := r.claimFinal(found, ordinal); err != nil {
				return nil, err
			}
			return found, nil
		}
		if r.blocksNewFinal(item) {
			return nil, ir.ProtocolError("conflicting reasoning item identity")
		}
		return r.bind(reasoningIdentity{itemID: item.ID, finalOrdinal: ordinal}, true, true)
	}
	if found := r.uniqueIndexMatch(ordinal); found != nil {
		if err := r.claimFinal(found, ordinal); err != nil {
			return nil, err
		}
		return found, nil
	}
	if r.hasStreamed() {
		return nil, ir.ProtocolError("conflicting reasoning item identity")
	}
	return r.bind(reasoningIdentity{finalOrdinal: ordinal}, true, true)
}

// claimFinal records that a streamed item is the terminal output-array item at
// ordinal. A different ordinal claiming the same item is contradictory.
func (r *responsesReasoningDecoder) claimFinal(item *responsesReasoningItem, ordinal int) error {
	if item.finalMatched && item.finalOrdinal != ordinal {
		return ir.ProtocolError("conflicting reasoning item identity")
	}
	item.finalMatched = true
	item.finalOrdinal = ordinal
	return nil
}

// applyFinal reconciles one terminal snapshot quietly. Text recovery emits in
// the caller's ordered final pass so recovered deltas do not jump ahead of
// interleaved final-only tools or text. A final-only item still emits its
// readable summary exactly once through emitRecoveredDeltas.
func (r *responsesReasoningDecoder) applyFinal(item *responsesReasoningItem, wire *respOutputItem) error {
	return r.applyItemSnapshot(item, reasoningSnapshot{Summary: wire.Summary, SummarySet: wire.SummarySet, EncryptedContent: wire.EncryptedContent, Status: wire.Status}, true, false)
}

// emitRecoveredDeltas sends the summary bytes retained by a quiet snapshot but
// not yet emitted. Repeated terminal snapshots emit nothing new.
func (r *responsesReasoningDecoder) emitRecoveredDeltas(item *responsesReasoningItem) error {
	for i, part := range item.parts {
		if part == nil {
			continue
		}
		if part.emitted >= part.text.Len() {
			continue
		}
		frag := part.text.String()[part.emitted:]
		if err := r.ensureMessageStart(); err != nil {
			return err
		}
		if err := r.emitOrdered(item, ir.StreamEvent{
			Kind:              ir.EventReasoningDelta,
			ReasoningID:       item.id,
			ReasoningIndexSet: item.hasIndex,
			ReasoningIndex:    item.outputIndex,
			SummaryIndex:      i,
			Text:              frag,
		}); err != nil {
			return err
		}
		part.emitted = part.text.Len()
	}
	return nil
}

// reasoningSnapshot is the shared shape of an item snapshot from a stream item
// frame and from a terminal output-array item.
type reasoningSnapshot struct {
	Summary []reasoningSummary
	// SummarySet distinguishes an absent or null summary field from an explicit
	// array. A complete snapshot with an explicit shorter array contradicts the
	// parts already retained.
	SummarySet       bool
	EncryptedContent *string
	Status           string
}

func (r *responsesReasoningDecoder) emitFinal(item *responsesReasoningItem) error {
	// Late metadata on an already-closed item still updates the snapshot;
	// identical repeats are deduplicated inside emitDone.
	item.done = true
	return r.emitDone(item)
}

func (r *responsesReasoningDecoder) markDone() {
	r.done = true
}

// bind looks up a reasoning item by item ID and streamed output index. create
// allocates only when no key matches and the response is not terminal.
func (r *responsesReasoningDecoder) bind(id reasoningIdentity, create, fromFinal bool) (*responsesReasoningItem, error) {
	var found *responsesReasoningItem
	match := func(item *responsesReasoningItem) error {
		if item == nil {
			return nil
		}
		if found == nil {
			found = item
			return nil
		}
		if found != item {
			return ir.ProtocolError("conflicting reasoning item identity")
		}
		return nil
	}
	if id.itemID != "" {
		if err := match(r.byItem[id.itemID]); err != nil {
			return nil, err
		}
	}
	if id.hasIndex {
		if err := match(r.byIndex[id.outputIndex]); err != nil {
			return nil, err
		}
	}
	if found == nil {
		if !create {
			return nil, nil
		}
		if r.done {
			return nil, ir.ProtocolError("conflicting reasoning item identity")
		}
		if len(r.items) >= r.maxItems {
			return nil, ir.ProtocolError("too many reasoning items")
		}
		if err := r.charge(reasoningItemOverhead); err != nil {
			return nil, err
		}
		found = &responsesReasoningItem{fromFinal: fromFinal, orderMsgs: r.countMsgs()}
		r.items = append(r.items, found)
		if fromFinal {
			if err := r.charge(reasoningKeyOverhead); err != nil {
				return nil, err
			}
			if r.byFinal == nil {
				r.byFinal = map[int]*responsesReasoningItem{}
			}
			r.byFinal[id.finalOrdinal] = found
			found.finalMatched = true
			found.finalOrdinal = id.finalOrdinal
		}
	}
	if err := r.assign(found, id); err != nil {
		return nil, err
	}
	return found, nil
}

func (r *responsesReasoningDecoder) assign(item *responsesReasoningItem, id reasoningIdentity) error {
	if id.itemID != "" {
		if item.id != "" && item.id != id.itemID {
			return ir.ProtocolError("conflicting reasoning item identity")
		}
		if other := r.byItem[id.itemID]; other != nil && other != item {
			return ir.ProtocolError("conflicting reasoning item identity")
		}
	}
	if id.hasIndex {
		if item.hasIndex && item.outputIndex != id.outputIndex {
			return ir.ProtocolError("conflicting reasoning item index")
		}
		if other := r.byIndex[id.outputIndex]; other != nil && other != item {
			return ir.ProtocolError("conflicting reasoning item index")
		}
	}
	if r.byItem == nil {
		r.byItem = map[string]*responsesReasoningItem{}
		r.byIndex = map[int]*responsesReasoningItem{}
	}
	if id.itemID != "" && item.id == "" {
		// The retained ID and its map key are both new bytes.
		if err := r.charge(len(id.itemID) + reasoningKeyOverhead); err != nil {
			return err
		}
		item.id = id.itemID
		r.byItem[id.itemID] = item
	}
	if id.hasIndex && !item.hasIndex {
		if err := r.charge(reasoningKeyOverhead); err != nil {
			return err
		}
		item.hasIndex = true
		item.outputIndex = id.outputIndex
		r.byIndex[id.outputIndex] = item
	}
	return nil
}

func (r *responsesReasoningDecoder) uniqueIndexMatch(ordinal int) *responsesReasoningItem {
	var found *responsesReasoningItem
	for _, item := range r.items {
		if item.fromFinal || !item.hasIndex || item.outputIndex != ordinal {
			continue
		}
		if found != nil {
			return nil
		}
		found = item
	}
	return found
}

// blocksNewFinal reports that a streamed item cannot be proved distinct from a
// terminal item with the same ID. A streamed item with a non-empty, different
// ID is distinct; anything else could be this item.
func (r *responsesReasoningDecoder) blocksNewFinal(item *respOutputItem) bool {
	for _, known := range r.items {
		if known.fromFinal {
			continue
		}
		if known.id != "" && known.id != item.ID {
			continue
		}
		return true
	}
	return false
}

func (r *responsesReasoningDecoder) hasStreamed() bool {
	for _, item := range r.items {
		if !item.fromFinal {
			return true
		}
	}
	return false
}

func (r *responsesReasoningDecoder) charge(n int) error {
	if n <= 0 {
		return nil
	}
	if n > r.maxBytes-r.bytes {
		return ir.ProtocolError("reasoning state too large")
	}
	r.bytes += n
	return nil
}

// part returns the summary part at index, creating empty placeholders for gaps
// so order and index stay stable. The number of allocated parts is bounded.
func (r *responsesReasoningDecoder) part(item *responsesReasoningItem, index int) (*responsesReasoningPart, error) {
	if index < 0 || index >= r.maxParts {
		return nil, ir.ProtocolError("reasoning summary index out of range")
	}
	for len(item.parts) <= index {
		if r.parts >= r.maxParts {
			return nil, ir.ProtocolError("too many reasoning summary parts")
		}
		if err := r.charge(reasoningPartOverhead); err != nil {
			return nil, err
		}
		item.parts = append(item.parts, &responsesReasoningPart{})
		r.parts++
	}
	return item.parts[index], nil
}

func (r *responsesReasoningDecoder) appendDelta(item *responsesReasoningItem, index int, frag string) error {
	if frag == "" {
		return nil
	}
	if item.done || r.done {
		return ir.ProtocolError("conflicting reasoning summary")
	}
	part, err := r.part(item, index)
	if err != nil {
		return err
	}
	if err := r.charge(len(frag)); err != nil {
		return err
	}
	part.text.WriteString(frag)
	if err := r.ensureMessageStart(); err != nil {
		return err
	}
	if err := r.emitOrdered(item, ir.StreamEvent{
		Kind:              ir.EventReasoningDelta,
		ReasoningID:       item.id,
		ReasoningIndexSet: item.hasIndex,
		ReasoningIndex:    item.outputIndex,
		SummaryIndex:      index,
		Text:              frag,
	}); err != nil {
		return err
	}
	part.emitted = part.text.Len()
	return nil
}

// reconcileSummary grows one part to a complete snapshot. Identical snapshots
// add nothing; a snapshot that does not extend what was already streamed is a
// conflict, and no suffix is emitted after the item closed. emitDeltas is false
// during the quiet first final pass, where the ordered second pass emits the
// recovered suffix itself.
func (r *responsesReasoningDecoder) reconcileSummary(item *responsesReasoningItem, index int, snapshot string, terminal, emitDeltas bool) error {
	part, err := r.part(item, index)
	if err != nil {
		return err
	}
	retained := part.text.String()
	var suffix string
	switch {
	case retained == snapshot:
	case strings.HasPrefix(snapshot, retained):
		suffix = snapshot[len(retained):]
	default:
		return ir.ProtocolError("conflicting reasoning summary")
	}
	if item.done && suffix != "" {
		return ir.ProtocolError("conflicting reasoning summary")
	}
	if r.done && suffix != "" {
		return ir.ProtocolError("conflicting reasoning summary")
	}
	if suffix != "" {
		if err := r.charge(len(suffix)); err != nil {
			return err
		}
		part.text.WriteString(suffix)
		if emitDeltas {
			if err := r.ensureMessageStart(); err != nil {
				return err
			}
			if err := r.emitOrdered(item, ir.StreamEvent{
				Kind:              ir.EventReasoningDelta,
				ReasoningID:       item.id,
				ReasoningIndexSet: item.hasIndex,
				ReasoningIndex:    item.outputIndex,
				SummaryIndex:      index,
				Text:              suffix,
			}); err != nil {
				return err
			}
			part.emitted = part.text.Len()
		}
	}
	if terminal {
		item.done = true
	}
	return nil
}

func (r *responsesReasoningDecoder) applyItemSnapshot(item *responsesReasoningItem, wire reasoningSnapshot, terminal, emitDeltas bool) error {
	// A complete snapshot may not shrink below the parts already retained from
	// streamed deltas. An absent summary field is a placeholder, not a claim.
	if terminal && wire.SummarySet && len(wire.Summary) < len(item.parts) {
		return ir.ProtocolError("conflicting reasoning summary")
	}
	for i, part := range wire.Summary {
		existing := i < len(item.parts) && item.parts[i] != nil
		if part.Type != "" {
			p, err := r.part(item, i)
			if err != nil {
				return err
			}
			if terminal && existing && p.typ != "" && p.typ != part.Type {
				return ir.ProtocolError("conflicting reasoning summary")
			}
			if err := r.applyPartType(p, part.Type); err != nil {
				return err
			}
		}
		// Absent text is a placeholder; explicit text, including an empty
		// string, is validated against the retained part.
		if !part.TextSet {
			continue
		}
		if err := r.reconcileSummary(item, i, part.Text, false, emitDeltas); err != nil {
			return err
		}
	}
	if err := r.applyEncrypted(item, wire.EncryptedContent); err != nil {
		return err
	}
	if err := r.applyStatus(item, wire.Status); err != nil {
		return err
	}
	if terminal {
		item.done = true
	}
	return nil
}

func (r *responsesReasoningDecoder) applyEncrypted(item *responsesReasoningItem, value *string) error {
	if value == nil {
		return nil
	}
	if item.encrypted != nil {
		if *item.encrypted != *value {
			return ir.ProtocolError("conflicting reasoning encrypted content")
		}
		return nil
	}
	if err := r.charge(len(*value)); err != nil {
		return err
	}
	v := *value
	item.encrypted = &v
	return nil
}

func (r *responsesReasoningDecoder) applyStatus(item *responsesReasoningItem, status string) error {
	if status == "" || item.status == status {
		return nil
	}
	// A non-terminal frame may advance in_progress -> completed. Once the item
	// closed, a different status is a conflict.
	if item.status != "" && item.done {
		return ir.ProtocolError("conflicting reasoning item status")
	}
	// Only retained growth is charged; a shorter replacement frees the old bytes.
	if growth := len(status) - len(item.status); growth > 0 {
		if err := r.charge(growth); err != nil {
			return err
		}
	}
	item.status = status
	return nil
}

// applyPartType records a summary part type once. The type string is retained
// state and is charged before the assignment; a repeated identical type is free.
func (r *responsesReasoningDecoder) applyPartType(part *responsesReasoningPart, typ string) error {
	if typ == "" || part.typ == typ {
		return nil
	}
	if part.typ != "" {
		// A later different type is ignored, matching the existing tolerant read.
		return nil
	}
	if err := r.charge(len(typ)); err != nil {
		return err
	}
	part.typ = typ
	return nil
}

// reasoningReservedSnapshot returns the decoder's retained byte cost of a
// snapshot built from the item's current state: ID, status, encrypted bytes,
// and one copy of every summary part. This is exactly the snapshot footprint;
// reading the state does not allocate.
func (item *responsesReasoningItem) reasoningReservedSnapshot() int {
	n := len(item.id) + len(item.status)
	if item.encrypted != nil {
		n += len(*item.encrypted)
	}
	for _, part := range item.parts {
		if part == nil {
			continue
		}
		n += reasoningPartOverhead
		if part.typ != "" {
			n += len(part.typ)
		} else {
			n += len("summary_text")
		}
		n += part.text.Len()
	}
	return n
}

// reservedResponsesReasoning is the footprint of would-be state plus a flag
// reporting whether the state equals what was already emitted. item must have
// an emitted snapshot when equal matters; without one the equality is false.
func (item *responsesReasoningItem) reservedResponsesReasoning() (int, bool) {
	reserved := item.reasoningReservedSnapshot()
	if item.emitted == nil {
		return reserved, false
	}
	emittedID, emittedStatus := item.emitted.ID, item.emitted.Status
	emittedEnc := item.emitted.EncryptedContent
	emittedParts := item.emitted.Summary
	return reserved, item.id == emittedID && item.status == emittedStatus &&
		sameStrPtr(item.encrypted, emittedEnc) && item.sameParts(emittedParts)
}

func (item *responsesReasoningItem) sameParts(parts []ir.ReasoningSummaryPart) bool {
	live := 0
	for _, p := range item.parts {
		if p == nil {
			continue
		}
		live++
	}
	if live != len(parts) {
		return false
	}
	for i, p := range item.parts {
		if p == nil {
			continue
		}
		// Part i in state maps to summary slot i; a nil slot inside the slice
		// cannot happen through part(), but an index mismatch would silently
		// compare unrelated parts.
		if i >= len(parts) {
			return false
		}
		want := parts[i]
		typ := p.typ
		if typ == "" {
			typ = "summary_text"
		}
		if typ != want.Type || p.text.String() != want.Text {
			return false
		}
	}
	return true
}

func sameStrPtr(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// retainedResponsesReasoning is the byte cost of one retained canonical
// snapshot: the state-derived reservation uses this accounting for the copy
// the decoder keeps after emission.
func retainedResponsesReasoning(rr *ir.ResponsesReasoning) int {
	if rr == nil {
		return 0
	}
	n := len(rr.ID) + len(rr.Status)
	if rr.EncryptedContent != nil {
		n += len(*rr.EncryptedContent)
	}
	for _, p := range rr.Summary {
		n += reasoningPartOverhead + len(p.Type) + len(p.Text)
	}
	return n
}

// reservedSnapshotBytes reports the retained byte cost the emitted snapshot
// currently holds. It is the same accounting the emitted copy was reserved
// against and stays for the small-limit tests.
func (item *responsesReasoningItem) reservedSnapshotBytes() int {
	if item.emitted == nil {
		return 0
	}
	return retainedResponsesReasoning(item.emitted)
}

// reservedSnapshotGrowth compares the state-derived cost with what the emitted
// snapshot already holds. Positive growth must be reserved before a snapshot
// is built or emitted.
func (item *responsesReasoningItem) reservedSnapshotGrowth() int {
	return item.reasoningReservedSnapshot() - item.reservedSnapshotBytes()
}

// snapshot copies the current state into the canonical wire shape. The default
// part type is filled in so the copy never reads back as an absent type.
func (r *responsesReasoningDecoder) snapshot(item *responsesReasoningItem) *ir.ResponsesReasoning {
	out := &ir.ResponsesReasoning{ID: item.id, Status: item.status}
	if item.encrypted != nil {
		v := *item.encrypted
		out.EncryptedContent = &v
	}
	out.Summary = make([]ir.ReasoningSummaryPart, 0, len(item.parts))
	for _, part := range item.parts {
		typ := part.typ
		if typ == "" {
			typ = "summary_text"
		}
		out.Summary = append(out.Summary, ir.ReasoningSummaryPart{Type: typ, Text: part.text.String()})
	}
	return out
}

// emitDone sends the complete structured snapshot unless it is byte-identical
// to the previously emitted one. Opaque bytes never touch Text. Growth is
// charged before the snapshot is emitted or retained, so a limit error cannot
// happen after a frame the client already saw. The emitted snapshot is retained
// as the dedup copy; neither the event nor the retained value is mutated later.
func (r *responsesReasoningDecoder) emitDone(item *responsesReasoningItem) error {
	// Compare and size from existing state. Equality first so an identical
	// repeated snapshot never re-reserves its footprint.
	reserved, equal := item.reservedResponsesReasoning()
	if item.emitted != nil && equal {
		return nil
	}
	if growth := reserved - item.reservedSnapshotBytes(); growth > 0 {
		if err := r.charge(growth); err != nil {
			return err
		}
	}
	snap := r.snapshot(item)
	if err := r.ensureMessageStart(); err != nil {
		return err
	}
	if err := r.emitOrdered(item, ir.StreamEvent{
		Kind:              ir.EventReasoningItemDone,
		ReasoningID:       item.id,
		ReasoningIndexSet: item.hasIndex,
		ReasoningIndex:    item.outputIndex,
		Reasoning:         snap,
	}); err != nil {
		return err
	}
	item.emitted = snap
	return nil
}

// retainedSnapshotBytes is the decoder's copy cost for one snapshot shape:
// ID, status, encrypted bytes, and every summary part copy. The emitted copy
// is charged through the state-derived reservation; this helper remains as the
// reference byte count used by the small-limit tests.
func retainedSnapshotBytes(rr *ir.ResponsesReasoning) int {
	if rr == nil {
		return 0
	}
	n := len(rr.ID) + len(rr.Status)
	if rr.EncryptedContent != nil {
		n += len(*rr.EncryptedContent)
	}
	for _, p := range rr.Summary {
		n += len(p.Type) + len(p.Text) + reasoningPartOverhead
	}
	return n
}
