package opencode

import (
	"io"

	"airouter/internal/proxy/ir"
)

// FilterResponse restores caller tool spellings in a decoded backend response
// and rejects an injected decoy call before the ingress encoder sees it.
func FilterResponse(resp *ir.Response, mapping ToolMap) (*ir.Response, error) {
	if resp == nil || !mapping.Active() {
		return resp, nil
	}
	for i := range resp.Content {
		b := &resp.Content[i]
		if b.Type != ir.BlockToolUse {
			continue
		}
		name, err := RestoreCallerName(b.ToolName, mapping)
		if err != nil {
			return nil, err
		}
		b.ToolName = name
	}
	return resp, nil
}

// DecodeResponse wraps a backend unary decoder with the request-local tool map.
func DecodeResponse(decode func([]byte) (*ir.Response, error), mapping ToolMap) func([]byte) (*ir.Response, error) {
	if decode == nil || !mapping.Active() {
		return decode
	}
	return func(body []byte) (*ir.Response, error) {
		resp, err := decode(body)
		if err != nil {
			return nil, err
		}
		return FilterResponse(resp, mapping)
	}
}

// Bounds on arguments and identity retained before a tool name is accepted.
// The unary collector caps bytes it receives; these caps stop a nameless
// upstream stream from using the hold as unbounded memory. All three are
// per decoded stream, never global.
const (
	maxHeldToolBytes     = 8 << 20
	maxHeldToolFragments = 4096
	maxHeldToolIndices   = 1024
)

// errHeldBudget reports a pending call that outgrew the shared hold budget.
// The message must stay payload-free: it never quotes held bytes.
var errHeldBudget = ir.ProtocolError("tool arguments arrived before any tool name")

// toolHold is the per-index pending state for one decoded stream. Identity
// (id plus restored name) stays on the hold after acceptance so repeated
// starts re-forward it, so its charge persists for the hold lifetime.
// argCharged tracks only argument fragments, which release on acceptance.
type toolHold struct {
	id           string
	name         string
	known        bool
	identCharged int
	argCharged   int
	frags        []string
}

// toolFilter holds pre-identity tool events until the name passes decoy
// validation. It replaces the emit callback's target with a validated stream
// so a decoy ID, name, or argument fragment never reaches the caller.
type toolFilter struct {
	mapping   ToolMap
	holds     map[int]*toolHold
	heldBytes int
	heldFrags int
}

func newToolFilter(mapping ToolMap) *toolFilter {
	return &toolFilter{mapping: mapping, holds: map[int]*toolHold{}}
}

func (f *toolFilter) hold(index int) (*toolHold, error) {
	h := f.holds[index]
	if h != nil {
		return h, nil
	}
	if len(f.holds) >= maxHeldToolIndices {
		return nil, ir.ProtocolError("too many tool calls without a name")
	}
	h = &toolHold{}
	f.holds[index] = h
	return h, nil
}

// charge bills delta bytes of retained call data against the shared
// per-stream budget, split into identity and argument subtotals so release
// refunds exactly the bytes that actually clear. Overwrites bill the net size
// change against a fully charged base, so no subtotal can go negative.
func (f *toolFilter) charge(h *toolHold, delta int, identity bool) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 && f.heldBytes > maxHeldToolBytes-delta {
		return errHeldBudget
	}
	f.heldBytes += delta
	if identity {
		h.identCharged += delta
	} else {
		h.argCharged += delta
	}
	return nil
}

// holdID retains the newest non-empty call ID. Repeated starts may overwrite
// it; only the net size change is billed, and the charge lives as long as the
// retained ID does.
func (f *toolFilter) holdID(h *toolHold, id string) error {
	if id == "" || id == h.id {
		return nil
	}
	if err := f.charge(h, len(id)-len(h.id), true); err != nil {
		return err
	}
	h.id = id
	return nil
}

func (f *toolFilter) holdArgs(h *toolHold, frag string) error {
	if frag == "" {
		return nil
	}
	if f.heldFrags >= maxHeldToolFragments {
		return errHeldBudget
	}
	if err := f.charge(h, len(frag), false); err != nil {
		return err
	}
	h.frags = append(h.frags, frag)
	f.heldFrags++
	return nil
}

// releaseArgs stops billing a hold's argument fragments once the call is
// accepted and forwarded. The retained ID and restored name stay billed for
// the hold lifetime: repeated starts re-forward them, so unbilling here would
// let 1024 holds retain SSE-sized identity outside the shared budget.
func (f *toolFilter) releaseArgs(h *toolHold) {
	f.heldBytes -= h.argCharged
	f.heldFrags -= len(h.frags)
	h.argCharged = 0
	h.frags = nil
}

// pending reports whether any hold was never accepted. Every created hold is
// either accepted or retains identity, arguments, or a bare start, so a clean
// EOF with an unknown hold must fail rather than drop the call.
func (f *toolFilter) pending() bool {
	for _, h := range f.holds {
		if !h.known {
			return true
		}
	}
	return false
}

func (f *toolFilter) emitStart(emit func(ir.StreamEvent) error, index int, h *toolHold, id string) error {
	if id == "" {
		id = h.id
	}
	return emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: index, ToolID: id, ToolName: h.name})
}

// accept validates one wire name, then flushes the held ID and argument
// fragments in original order. An error means the call was a decoy or its
// retained identity outgrew the budget, and nothing of it was forwarded.
func (f *toolFilter) accept(emit func(ir.StreamEvent) error, index int, h *toolHold, name, id string) error {
	restored, err := RestoreCallerName(name, f.mapping)
	if err != nil {
		return err
	}
	// Both retained-identity charges are validated before the offending start
	// is forwarded. The restored name is retained for repeated starts and can
	// be larger than the wire name, so its retained bytes count too.
	if err := f.holdID(h, id); err != nil {
		return err
	}
	if err := f.charge(h, len(restored)-len(h.name), true); err != nil {
		return err
	}
	h.known = true
	h.name = restored
	if err := f.emitStart(emit, index, h, id); err != nil {
		return err
	}
	for _, frag := range h.frags {
		if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: index, ArgsFrag: frag}); err != nil {
			return err
		}
	}
	f.releaseArgs(h)
	return nil
}

// DecodeStream wraps a backend stream decoder. Tool identity is held until the
// name is accepted so a decoy ID, name, or argument fragment never reaches the
// caller. Unknown events and non-tool events pass through unchanged. A stream
// that ends with an unresolved tool call fails instead of emitting a normal
// finish, both on EventFinish and on a clean EOF. A bare start with no ID and
// no name is tracked as unresolved the same way, so malformed upstream calls
// are rejected instead of silently dropped.
func DecodeStream(decode func(io.Reader, func(ir.StreamEvent) error) error, mapping ToolMap) func(io.Reader, func(ir.StreamEvent) error) error {
	if decode == nil || !mapping.Active() {
		return decode
	}
	return func(r io.Reader, emit func(ir.StreamEvent) error) error {
		f := newToolFilter(mapping)
		err := decode(r, func(ev ir.StreamEvent) error {
			return f.handle(ev, emit)
		})
		if err != nil {
			return err
		}
		if f.pending() {
			return ir.ProtocolError("tool call ended without a name")
		}
		return nil
	}
}

func (f *toolFilter) handle(ev ir.StreamEvent, emit func(ir.StreamEvent) error) error {
	switch ev.Kind {
	case ir.EventToolCallStart:
		return f.handleStart(ev, emit)
	case ir.EventToolCallDelta:
		return f.handleDelta(ev, emit)
	case ir.EventFinish:
		if f.pending() {
			return ir.ProtocolError("tool call ended without a name")
		}
		return emit(ev)
	default:
		return emit(ev)
	}
}

func (f *toolFilter) handleStart(ev ir.StreamEvent, emit func(ir.StreamEvent) error) error {
	if ev.ToolName == "" && ev.ToolID == "" {
		if h := f.holds[ev.Index]; h != nil {
			if h.known {
				return emit(ev)
			}
			return nil
		}
		// A bare start carries no caller-visible content, but it must not
		// vanish: the index stays unresolved so EventFinish and clean EOF
		// reject the malformed call. Upstream decoders emit it for malformed
		// Anthropic tool_use blocks and synthesized Responses items.
		if _, err := f.hold(ev.Index); err != nil {
			return err
		}
		return nil
	}
	h, err := f.hold(ev.Index)
	if err != nil {
		return err
	}
	if ev.ToolName == "" {
		// A late ID on an accepted call replaces the retained fallback ID; bill
		// the net change before forwarding so repeated starts re-forward the
		// newest ID and the hold never outgrows the budget.
		if err := f.holdID(h, ev.ToolID); err != nil {
			return err
		}
		if h.known {
			return emit(ev)
		}
		return nil
	}
	return f.accept(emit, ev.Index, h, ev.ToolName, ev.ToolID)
}

func (f *toolFilter) handleDelta(ev ir.StreamEvent, emit func(ir.StreamEvent) error) error {
	h := f.holds[ev.Index]
	if h != nil && h.known {
		return emit(ev)
	}
	if ev.ToolName != "" {
		if h == nil {
			var err error
			h, err = f.hold(ev.Index)
			if err != nil {
				return err
			}
		}
		if err := f.accept(emit, ev.Index, h, ev.ToolName, ev.ToolID); err != nil {
			return err
		}
		if ev.ArgsFrag == "" {
			return nil
		}
		return emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: ev.Index, ArgsFrag: ev.ArgsFrag})
	}
	if ev.ArgsFrag == "" && ev.ToolID == "" {
		return nil
	}
	if h == nil {
		var err error
		h, err = f.hold(ev.Index)
		if err != nil {
			return err
		}
	}
	if err := f.holdID(h, ev.ToolID); err != nil {
		return err
	}
	return f.holdArgs(h, ev.ArgsFrag)
}
