package proxy

import (
	"encoding/json"
	"log/slog"
	"strings"

	"airouter/internal/proxy/grokbuild"
	"airouter/internal/proxy/ir"
)

// testGrokChatWireBudget, when positive, replaces the shared wire budget for
// one test. Production leaves it zero. It does not raise the parser or SSE cap.
var testGrokChatWireBudget int

func grokChatWireBudget() int {
	if testGrokChatWireBudget > 0 {
		return testGrokChatWireBudget
	}
	return grokbuild.ChatWireBudget
}

type grokVisibleTool struct {
	id     string
	name   string
	itemID string
}

// grokChatSlot is one source item held for an optional Chat marker.
// Reasoning ciphertext is the decoder snapshot pointer. It is not copied
// until a marker is actually stamped, and it is dropped when signing is off
// or the budget is exceeded.
type grokChatSlot struct {
	kind        string
	source      int
	hasSource   bool
	terminal    int
	hasTerminal bool
	seq         int
	text        strings.Builder
	toolID      string
	toolName    string
	itemID      string
	id          string
	reasoning   *ir.ResponsesReasoning
	snapBytes   int
}

// grokChatOrder derives one layout for Chat SSE and unary. It is attempt-local.
// observe is not a client emit path and never fails the decode.
type grokChatOrder struct {
	bind      grokbuild.ChatBind
	mac       func([]byte) ([]byte, bool)
	budget    int
	res       *reqResult
	logger    *slog.Logger
	ingressID string

	slots          []*grokChatSlot
	reasonByID     map[string]*grokChatSlot
	reasonBySource map[int]*grokChatSlot
	toolByID       map[string]*grokChatSlot
	textBySource   map[int]*grokChatSlot
	visibleText    strings.Builder
	visibleTools   []grokVisibleTool
	held           int
	nextSeq        int
	omit           bool
	sawOpaque      bool
}

func newGrokChatOrder(bind grokbuild.ChatBind, mac func([]byte) ([]byte, bool), res *reqResult, logger *slog.Logger, ingressID string) *grokChatOrder {
	return &grokChatOrder{
		bind: bind, mac: mac, budget: grokChatWireBudget(),
		res: res, logger: logger, ingressID: ingressID,
	}
}

func (o *grokChatOrder) observe(ev ir.StreamEvent) {
	if o == nil || o.omit {
		return
	}
	switch ev.Kind {
	case ir.EventReasoningReserve:
		o.noteReasoning(ev, false)
	case ir.EventReasoningItemDone:
		o.noteReasoning(ev, true)
	case ir.EventTextDelta:
		o.noteText(ev)
	case ir.EventToolCallStart:
		o.noteTool(ev)
	}
	if ev.TerminalSet {
		o.noteTerminal(ev)
	}
}

func (o *grokChatOrder) noteVisible(ev ir.StreamEvent) {
	if o == nil || o.omit {
		return
	}
	switch ev.Kind {
	case ir.EventTextStart, ir.EventTextDelta:
		if ev.Refusal || ev.Text == "" {
			return
		}
		if !o.charge(len(ev.Text)) {
			return
		}
		o.visibleText.WriteString(ev.Text)
	case ir.EventToolCallStart:
		if ev.ToolID == "" && ev.ToolName == "" && ev.ResponsesItemID == "" {
			return
		}
		if !o.charge(len(ev.ToolID) + len(ev.ToolName) + len(ev.ResponsesItemID)) {
			return
		}
		for i := range o.visibleTools {
			cur := &o.visibleTools[i]
			if ev.ToolID != "" && cur.id == ev.ToolID {
				if cur.name == "" {
					cur.name = ev.ToolName
				}
				if cur.itemID == "" {
					cur.itemID = ev.ResponsesItemID
				}
				return
			}
		}
		o.visibleTools = append(o.visibleTools, grokVisibleTool{
			id: ev.ToolID, name: ev.ToolName, itemID: ev.ResponsesItemID,
		})
	}
}

func (o *grokChatOrder) noteText(ev ir.StreamEvent) {
	if ev.Text == "" || !ev.SourceOutputSet {
		return
	}
	if !o.charge(len(ev.Text)) {
		return
	}
	slot := o.textBySource[ev.SourceOutputIndex]
	if slot == nil {
		if len(o.slots) >= 1024 {
			o.failBudget()
			return
		}
		slot = o.newSlot("text")
		slot.hasSource = true
		slot.source = ev.SourceOutputIndex
		if o.textBySource == nil {
			o.textBySource = map[int]*grokChatSlot{}
		}
		o.textBySource[ev.SourceOutputIndex] = slot
	}
	slot.text.WriteString(ev.Text)
}

func (o *grokChatOrder) noteTool(ev ir.StreamEvent) {
	if !ev.SourceOutputSet && ev.ToolID == "" {
		return
	}
	id := ev.ToolID
	if id == "" {
		id = ev.ResponsesItemID
	}
	if id == "" {
		return
	}
	slot := o.toolByID[id]
	if slot == nil {
		if len(o.slots) >= 1024 {
			o.failBudget()
			return
		}
		n := len(ev.ToolID) + len(ev.ToolName) + len(ev.ResponsesItemID)
		if !o.charge(n) {
			return
		}
		slot = o.newSlot("tool")
		slot.toolID = ev.ToolID
		slot.toolName = ev.ToolName
		slot.itemID = ev.ResponsesItemID
		if o.toolByID == nil {
			o.toolByID = map[string]*grokChatSlot{}
		}
		o.toolByID[id] = slot
	}
	if slot.toolID == "" {
		slot.toolID = ev.ToolID
	}
	if slot.toolName == "" {
		slot.toolName = ev.ToolName
	}
	if slot.itemID == "" {
		slot.itemID = ev.ResponsesItemID
	}
	if ev.SourceOutputSet && !slot.hasSource {
		slot.hasSource = true
		slot.source = ev.SourceOutputIndex
	}
}

func (o *grokChatOrder) noteReasoning(ev ir.StreamEvent, done bool) {
	id := ev.ReasoningID
	if ev.Reasoning != nil && ev.Reasoning.ID != "" {
		id = ev.Reasoning.ID
	}
	slot := o.findReasoning(id, ev.ReasoningIndex, ev.ReasoningIndexSet)
	if slot == nil {
		if id == "" && !ev.ReasoningIndexSet {
			return
		}
		if len(o.slots) >= 1024 {
			o.failBudget()
			return
		}
		if !o.charge(len(id)) {
			return
		}
		slot = o.newSlot("reasoning")
		slot.id = id
	}
	if id != "" && slot.id == "" {
		slot.id = id
	}
	if ev.ReasoningIndexSet && !slot.hasSource {
		slot.hasSource = true
		slot.source = ev.ReasoningIndex
	}
	o.bindReasoning(slot)
	if !done || ev.Reasoning == nil {
		return
	}
	if ev.Reasoning.ProviderState() {
		o.sawOpaque = true
	}
	next := snapshotBytes(ev.Reasoning)
	growth := next - slot.snapBytes
	if growth > 0 && !o.charge(growth) {
		return
	}
	// Keep the decoder pointer. It is not mutated after emission.
	slot.reasoning = ev.Reasoning
	slot.snapBytes = next
	if slot.reasoning.ID != "" {
		slot.id = slot.reasoning.ID
		o.bindReasoning(slot)
	}
}

func (o *grokChatOrder) noteTerminal(ev ir.StreamEvent) {
	switch ev.Kind {
	case ir.EventReasoningReserve, ir.EventReasoningItemDone:
		slot := o.findReasoning(ev.ReasoningID, ev.ReasoningIndex, ev.ReasoningIndexSet)
		if slot == nil {
			return
		}
		slot.hasTerminal = true
		slot.terminal = ev.TerminalIndex
	case ir.EventToolCallStart:
		id := ev.ToolID
		if id == "" {
			id = ev.ResponsesItemID
		}
		slot := o.toolByID[id]
		if slot == nil {
			return
		}
		slot.hasTerminal = true
		slot.terminal = ev.TerminalIndex
	case ir.EventTextDelta:
		if !ev.SourceOutputSet {
			return
		}
		slot := o.textBySource[ev.SourceOutputIndex]
		if slot == nil {
			return
		}
		slot.hasTerminal = true
		slot.terminal = ev.TerminalIndex
	}
}

func (o *grokChatOrder) findReasoning(id string, index int, hasIndex bool) *grokChatSlot {
	if id != "" && o.reasonByID != nil {
		if slot := o.reasonByID[id]; slot != nil {
			return slot
		}
	}
	if hasIndex && o.reasonBySource != nil {
		return o.reasonBySource[index]
	}
	return nil
}

func (o *grokChatOrder) bindReasoning(slot *grokChatSlot) {
	if slot == nil {
		return
	}
	if slot.id != "" {
		if o.reasonByID == nil {
			o.reasonByID = map[string]*grokChatSlot{}
		}
		o.reasonByID[slot.id] = slot
	}
	if slot.hasSource {
		if o.reasonBySource == nil {
			o.reasonBySource = map[int]*grokChatSlot{}
		}
		o.reasonBySource[slot.source] = slot
	}
}

func (o *grokChatOrder) newSlot(kind string) *grokChatSlot {
	slot := &grokChatSlot{kind: kind, seq: o.nextSeq}
	o.nextSeq++
	o.slots = append(o.slots, slot)
	return slot
}

func (o *grokChatOrder) charge(n int) bool {
	if o == nil || o.omit {
		return false
	}
	if n < 0 {
		n = 0
	}
	if n > o.budget-o.held {
		o.failBudget()
		return false
	}
	o.held += n
	return true
}

func (o *grokChatOrder) failBudget() {
	if o == nil {
		return
	}
	o.omit = true
	for _, slot := range o.slots {
		if slot == nil {
			continue
		}
		slot.reasoning = nil
		slot.snapBytes = 0
		slot.text.Reset()
	}
	o.slots = nil
	o.visibleText.Reset()
	o.visibleTools = nil
	o.held = 0
}

func (o *grokChatOrder) details() ([]json.RawMessage, bool) {
	blocks, ok := o.blocks()
	if !ok {
		return nil, false
	}
	details, err := grokbuild.StampChatDetails(blocks, o.bind, o.mac)
	if err != nil || len(details) == 0 {
		return nil, false
	}
	raw, err := json.Marshal(details)
	if err != nil || len(raw) > o.budget {
		return nil, false
	}
	return details, true
}

func (o *grokChatOrder) blocks() ([]ir.ContentBlock, bool) {
	if o == nil || o.omit || !o.sawOpaque {
		return nil, false
	}
	// Pi 1.6.0 joins projected Chat text into one ordinary block and drops that
	// block when trim() is empty. Signing the dropped text cannot be replayed.
	// Empty text never becomes a block, so a tool-only turn still gets a marker.
	// The check is the full projection, not one Responses segment: Pi collects
	// every content delta into that single block.
	if piDropsProjectedText(o.visibleText.String()) {
		return nil, false
	}
	ordered, ok := o.ordered()
	if !ok {
		return nil, false
	}
	var text strings.Builder
	tools := map[string]grokVisibleTool{}
	blocks := make([]ir.ContentBlock, 0, len(ordered))
	for _, slot := range ordered {
		switch slot.kind {
		case "text":
			if slot.text.Len() == 0 {
				continue
			}
			s := slot.text.String()
			text.WriteString(s)
			blocks = append(blocks, ir.ContentBlock{Type: ir.BlockText, Text: s})
		case "tool":
			if slot.toolID == "" {
				return nil, false
			}
			if _, dup := tools[slot.toolID]; dup {
				return nil, false
			}
			tools[slot.toolID] = grokVisibleTool{id: slot.toolID, name: slot.toolName, itemID: slot.itemID}
			blocks = append(blocks, ir.ContentBlock{
				Type: ir.BlockToolUse, ToolID: slot.toolID, ToolName: slot.toolName, ResponsesItemID: slot.itemID,
			})
		case "reasoning":
			if slot.reasoning == nil || slot.id == "" || !slot.reasoning.ProviderState() {
				return nil, false
			}
			rr := slot.reasoning.Clone()
			blocks = append(blocks, ir.ContentBlock{
				Type: ir.BlockReasoning, Text: rr.ReadableText(), ResponsesReasoning: rr,
			})
		default:
			return nil, false
		}
	}
	if text.String() != o.visibleText.String() || !sameVisibleTools(tools, o.visibleTools) {
		return nil, false
	}
	return blocks, true
}

func (o *grokChatOrder) ordered() ([]*grokChatSlot, bool) {
	items := make([]*grokChatSlot, 0, len(o.slots))
	for _, slot := range o.slots {
		if slot == nil {
			continue
		}
		if slot.kind == "text" && slot.text.Len() == 0 {
			continue
		}
		if slot.kind == "reasoning" && (slot.reasoning == nil || !slot.reasoning.ProviderState()) {
			continue
		}
		items = append(items, slot)
	}
	if len(items) == 0 {
		return nil, false
	}
	indexed, terminal := 0, 0
	for _, slot := range items {
		if slot.hasSource {
			indexed++
		}
		if slot.hasTerminal {
			terminal++
		}
	}
	switch {
	case indexed == len(items) && uniqueSource(items):
		if terminal > 0 && !terminalsAgree(items) {
			return nil, false
		}
		sortBySource(items)
		return items, true
	case indexed == 0 && terminal == len(items) && uniqueTerminal(items):
		sortByTerminal(items)
		return items, true
	case indexed == 0 && terminal == 0:
		sortBySeq(items)
		return items, true
	default:
		return nil, false
	}
}

func uniqueSource(items []*grokChatSlot) bool {
	seen := map[int]bool{}
	for _, slot := range items {
		if seen[slot.source] {
			return false
		}
		seen[slot.source] = true
	}
	return true
}

func uniqueTerminal(items []*grokChatSlot) bool {
	seen := map[int]bool{}
	for _, slot := range items {
		if seen[slot.terminal] {
			return false
		}
		seen[slot.terminal] = true
	}
	return true
}

func terminalsAgree(items []*grokChatSlot) bool {
	for i, a := range items {
		if !a.hasTerminal {
			continue
		}
		for _, b := range items[i+1:] {
			if !b.hasTerminal {
				continue
			}
			if (a.source < b.source) != (a.terminal < b.terminal) {
				return false
			}
		}
	}
	return true
}

func sortBySource(items []*grokChatSlot) {
	for i := 1; i < len(items); i++ {
		cur := items[i]
		j := i
		for j > 0 && items[j-1].source > cur.source {
			items[j] = items[j-1]
			j--
		}
		items[j] = cur
	}
}

func sortByTerminal(items []*grokChatSlot) {
	for i := 1; i < len(items); i++ {
		cur := items[i]
		j := i
		for j > 0 && items[j-1].terminal > cur.terminal {
			items[j] = items[j-1]
			j--
		}
		items[j] = cur
	}
}

func sortBySeq(items []*grokChatSlot) {
	for i := 1; i < len(items); i++ {
		cur := items[i]
		j := i
		for j > 0 && items[j-1].seq > cur.seq {
			items[j] = items[j-1]
			j--
		}
		items[j] = cur
	}
}

func sameVisibleTools(got map[string]grokVisibleTool, want []grokVisibleTool) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, tool := range want {
		slot, ok := got[tool.id]
		if !ok || seen[tool.id] || slot.name != tool.name || slot.itemID != tool.itemID {
			return false
		}
		seen[tool.id] = true
	}
	return len(seen) == len(got)
}

// piDropsProjectedText reports whether Pi 1.6.0 drops this Chat text.
// An empty projection was never a text block. A nonempty projection is
// dropped only when ECMAScript trim() is empty, including U+FEFF.
// strings.TrimSpace is not the same: it removes U+0085 and keeps U+FEFF.
func piDropsProjectedText(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !ecmaTrimRune(r) {
			return false
		}
	}
	return true
}

// ecmaTrimRune matches String.prototype.trim in the Pi 1.6.0 runtime:
// ECMAScript WhiteSpace and LineTerminator, not Unicode White_Space.
func ecmaTrimRune(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00A0', '\uFEFF', '\u1680',
		'\u2028', '\u2029', '\u202F', '\u205F', '\u3000':
		return true
	}
	return r >= '\u2000' && r <= '\u200A'
}

func snapshotBytes(rr *ir.ResponsesReasoning) int {
	if rr == nil {
		return 0
	}
	n := len(rr.ID) + len(rr.Status)
	if rr.EncryptedContent != nil {
		n += len(*rr.EncryptedContent)
	}
	for _, p := range rr.Summary {
		n += len(p.Type) + len(p.Text)
	}
	return n
}
