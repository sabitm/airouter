package grokbuild

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"airouter/internal/codexgrant"
	"airouter/internal/proxy/ir"
	"airouter/internal/xaigrant"
)

// ChatBridgeFormat is the Airouter-owned reasoning_details format. It is not
// OpenRouter's xai-responses-v1 carrier. Pi 1.6.0 keeps extra fields on a
// reasoning.encrypted entry and replays that entry unchanged. Adjacent
// reasoning.summary entries are concatenated by Pi, so they are readable
// fallback only and are not the signed state.
const ChatBridgeFormat = "airouter-grok-build-v1"

const (
	chatBridgeCanonical = "airouter-grok-build-chat/1\n"
	chatBridgeProtocol  = "grok-build"
	chatBridgeMaxItems  = 1024
	chatBridgeMaxParts  = 1024
	chatBridgeMaxMeta   = 1 << 20
	chatBridgeMaxCipher = 64 << 20
	// chatBridgeMACBytes is the decoded HMAC length. The shared budget charges
	// the encoded form of this size, not a MAC computed only to measure it.
	chatBridgeMACBytes = sha256.Size
	// ChatWireBudget is the maximum encoded reasoning_details array. It matches
	// the SSE event payload limit. A Chat chunk also carries id, model, and the
	// data-line prefix, so the emitter checks the serialized frame separately
	// and omits an optional marker that would not parse.
	ChatWireBudget = 8 << 20
)

// ErrChatBridgeRejected is a sanitized rejection. The error text is safe to
// log. Callers must not wrap it with marker bytes, ciphertext, or tokens.
var ErrChatBridgeRejected = errors.New("invalid grok build replay state")

// ErrCodexReplayRejected is the sanitized rejection for a Codex Chat marker.
var ErrCodexReplayRejected = errors.New("invalid codex replay state")

// ErrXAIReplayRejected is the sanitized rejection for a public xAI Chat marker.
var ErrXAIReplayRejected = errors.New("invalid xai replay state")

// CodexReplayFormat is the versioned reasoning_details format for Codex.
// It is not the Grok Build format and is not an OpenRouter carrier.
const CodexReplayFormat = "airouter-codex-v1"

// XAIReplayFormat is the versioned reasoning_details format for the public
// xAI Responses bridge. It is not the Grok Build or Codex format.
const XAIReplayFormat = "airouter-xai-v1"

const codexReplayCanonical = "airouter-codex-chat/1\n"
const codexReplayProtocol = "codex"
const xaiReplayCanonical = "airouter-xai-chat/1\n"
const xaiReplayProtocol = "xai-public"

// ReplayProfile selects one Chat replay proof domain. The Grok profile keeps
// the original format, canonical prefix, and protocol bytes.
type ReplayProfile struct {
	Format          string
	Canonical       string
	Protocol        string
	Rejected        error
	IncludeEndpoint bool
	// NormalizeEndpoint validates the signed endpoint for profiles that include
	// it. Nil profiles do not carry an endpoint.
	NormalizeEndpoint func(string) (string, bool)
	// RequirePrincipal is true when a signed marker must name a stable account.
	RequirePrincipal bool
	// AuthoritativeEmpty keeps an explicit empty encrypted_content as replay
	// state when no nonempty sibling exists. Grok leaves this false, so its
	// historical nonempty requirement and marker bytes stay unchanged.
	AuthoritativeEmpty bool
}

// GrokReplayProfile is the historical Grok Build proof domain.
func GrokReplayProfile() ReplayProfile {
	return grokReplayProfile()
}

func grokReplayProfile() ReplayProfile {
	return ReplayProfile{
		Format: ChatBridgeFormat, Canonical: chatBridgeCanonical,
		Protocol: chatBridgeProtocol, Rejected: ErrChatBridgeRejected,
	}
}

// CodexReplayProfile is the Codex proof domain. Its canonical prefix, protocol,
// format, and MAC key are distinct from Grok Build.
func CodexReplayProfile() ReplayProfile {
	return ReplayProfile{
		Format: CodexReplayFormat, Canonical: codexReplayCanonical,
		Protocol: codexReplayProtocol, Rejected: ErrCodexReplayRejected,
		IncludeEndpoint: true, NormalizeEndpoint: codexgrant.NormalizeEndpoint,
		RequirePrincipal: true, AuthoritativeEmpty: true,
	}
}

// XAIReplayProfile is the public xAI proof domain. The official endpoint is
// signed because a different origin cannot replay this provider state.
func XAIReplayProfile() ReplayProfile {
	return ReplayProfile{
		Format: XAIReplayFormat, Canonical: xaiReplayCanonical,
		Protocol: xaiReplayProtocol, Rejected: ErrXAIReplayRejected,
		IncludeEndpoint: true, NormalizeEndpoint: xaigrant.NormalizeEndpoint,
		AuthoritativeEmpty: true,
	}
}

// KnownReplayProfiles is the set of recognizable Chat replay formats.
func KnownReplayProfiles() []ReplayProfile {
	return []ReplayProfile{grokReplayProfile(), CodexReplayProfile(), XAIReplayProfile()}
}

func (p ReplayProfile) rejected() error {
	if p.Rejected != nil {
		return p.Rejected
	}
	return ErrChatBridgeRejected
}

func (p ReplayProfile) valid() bool {
	return p.Format != "" && p.Canonical != "" && p.Protocol != ""
}

// errChatBridgeBudget means the replay marker does not fit the retention
// budget. Callers omit the marker and keep the readable reply.
var errChatBridgeBudget = errors.New("grok build replay budget")

// chatBridgeEncodedBudget caps replicated layout and metadata before it is
// copied into every encrypted entry. Tests may lower it.
var chatBridgeEncodedBudget = chatBridgeMaxMeta

// ChatBind is the authenticated provenance of one Grok Build Chat replay.
// It does not include access tokens, refresh tokens, or request ids.
// Model is the effective upstream model after suffix handling. RouteModel is
// the combo target string, so a different effort suffix is a different route.
type ChatBind struct {
	Scope      string
	ComboID    int64
	ProviderID int64
	Generation string
	Principal  string
	Model      string
	RouteModel string
	// Endpoint is the normalized inference origin. Grok markers leave it empty
	// so their canonical payload bytes stay unchanged.
	Endpoint string
}

// ChatLayoutItem is one ordered output item. Text is not copied; TextLen and
// TextHash authenticate the segment the client already carries. Tool argument
// bytes are not authenticated because Pi reserializes them.
type ChatLayoutItem struct {
	Kind     string
	TextLen  int
	TextHash string
	ID       string
	Name     string
	ItemID   string
}

// ChatReasoningItem is one reasoning item. Data is the exact xAI ciphertext
// when HasData is true. EmptyData distinguishes an explicit empty string from
// an absent value. ID-only items have neither.
type ChatReasoningItem struct {
	ID        string
	Status    string
	Data      string
	HasData   bool
	EmptyData bool
	Parts     []ir.ReasoningSummaryPart
	Index     int
}

// ChatTurn is the latest assistant reply's verified replay state.
type ChatTurn struct {
	Bind      ChatBind
	Layout    []ChatLayoutItem
	Reasoning []ChatReasoningItem
}

// AssistantNote is one assistant message in chat order. Summary is the signed
// readable text. Turn is set for every verified marked assistant. Only the
// latest assistant's turn may pin routing. A verified marker authenticates
// Airouter provenance. It is not a claim that ciphertext is portable across
// accounts.
type AssistantNote struct {
	Summary  string
	Turn     *ChatTurn
	Marked   bool
	Protocol string
}

// ChatDecision is the pre-token classification of one Chat request.
// Pin means the latest assistant reply has verified replay state. Source
// availability is decided by the caller. Strip means some assistant message
// carries this bridge and the portable body must drop it.
type ChatDecision struct {
	Pin        bool
	Strip      bool
	Protocol   string
	Turn       ChatTurn
	Assistants []AssistantNote
}

// Grok reports whether the pinned marker is a Grok Build marker.
func (d ChatDecision) Grok() bool { return d.Pin && d.Protocol == chatBridgeProtocol }

// Codex reports whether the pinned marker is a Codex marker.
func (d ChatDecision) Codex() bool { return d.Pin && d.Protocol == codexReplayProtocol }

// XAI reports whether the pinned marker is a public xAI marker.
func (d ChatDecision) XAI() bool { return d.Pin && d.Protocol == xaiReplayProtocol }

type macFunc func([]byte) ([]byte, bool)
type verifyFunc func([]byte, []byte) bool
type protocolVerifyFunc func(protocol string, payload, mac []byte) bool

type parseState struct {
	profiles []ReplayProfile
	verify   protocolVerifyFunc
}

func (s parseState) rejected(raw []byte) error {
	text := string(raw)
	for _, p := range s.profiles {
		if p.Format != "" && strings.Contains(text, p.Format) {
			return p.rejected()
		}
		if p.Protocol != "" && strings.Contains(text, p.Protocol) {
			return p.rejected()
		}
	}
	if len(s.profiles) > 0 {
		return s.profiles[0].rejected()
	}
	return ErrChatBridgeRejected
}

func (s parseState) profile(format, protocol string) (ReplayProfile, bool) {
	for _, p := range s.profiles {
		if format != "" && format == p.Format {
			return p, true
		}
	}
	for _, p := range s.profiles {
		if protocol != "" && protocol == p.Protocol {
			return p, true
		}
	}
	return ReplayProfile{}, false
}

type wirePart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type wireLayout struct {
	K    string `json:"k"`
	N    int    `json:"n,omitempty"`
	H    string `json:"h,omitempty"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Item string `json:"item,omitempty"`
}

type wirePlain struct {
	ID     string     `json:"id,omitempty"`
	Status string     `json:"status,omitempty"`
	Index  int        `json:"index"`
	Empty  bool       `json:"empty,omitempty"`
	Parts  []wirePart `json:"parts,omitempty"`
}

type wireBind struct {
	Scope      string `json:"scope"`
	Combo      string `json:"combo"`
	Provider   string `json:"provider"`
	Protocol   string `json:"protocol"`
	Generation string `json:"generation"`
	Principal  string `json:"principal"`
	Model      string `json:"model"`
	Route      string `json:"route"`
	Endpoint   string `json:"endpoint,omitempty"`
}

type wireEncrypted struct {
	Type   string       `json:"type"`
	ID     string       `json:"id"`
	Format string       `json:"format"`
	Index  int          `json:"index"`
	Data   string       `json:"data"`
	Status string       `json:"status,omitempty"`
	Parts  []wirePart   `json:"parts,omitempty"`
	Layout []wireLayout `json:"layout"`
	Plain  []wirePlain  `json:"plain,omitempty"`
	Bind   wireBind     `json:"bind"`
	MAC    string       `json:"mac"`
}

type wireSummary struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Format  string `json:"format,omitempty"`
	Index   int    `json:"index"`
	Summary string `json:"summary"`
}

// StampChatDetails builds OpenRouter-shaped reasoning_details for one assistant
// turn. A blank generation, or a turn with no nonempty ciphertext, returns
// nil, nil. Pi drops a reasoning-only assistant message, so a caller must not
// claim those turns replay. The MAC covers ciphertext, ids, summary parts,
// status, layout, and bind. Ciphertext is stored once, in data.
func StampChatDetails(content []ir.ContentBlock, bind ChatBind, mac macFunc) ([]json.RawMessage, error) {
	return StampChatDetailsProfile(grokReplayProfile(), content, bind, mac)
}

// StampChatDetailsProfile builds reasoning_details for one replay profile.
// A Grok profile produces the historical marker bytes. A blank generation, or a
// Grok turn with no nonempty ciphertext, returns nil, nil. A Codex profile also
// stamps an explicit empty encrypted_content. That empty value is authoritative,
// not absent and not provisional.
func StampChatDetailsProfile(profile ReplayProfile, content []ir.ContentBlock, bind ChatBind, mac macFunc) ([]json.RawMessage, error) {
	if !profile.valid() || strings.TrimSpace(bind.Generation) == "" || bind.ProviderID == 0 || bind.ComboID == 0 {
		return nil, nil
	}
	if profile.IncludeEndpoint && strings.TrimSpace(bind.Endpoint) == "" {
		return nil, nil
	}
	turn, err := turnFromContentProfile(profile, content, bind)
	if err != nil || turn == nil {
		if errors.Is(err, errChatBridgeBudget) {
			return nil, nil
		}
		return nil, err
	}
	details, err := marshalTurn(profile, *turn, mac)
	if errors.Is(err, errChatBridgeBudget) {
		return nil, nil
	}
	return details, err
}

func turnFromContent(content []ir.ContentBlock, bind ChatBind) (*ChatTurn, error) {
	return turnFromContentProfile(grokReplayProfile(), content, bind)
}

func turnFromContentProfile(profile ReplayProfile, content []ir.ContentBlock, bind ChatBind) (*ChatTurn, error) {
	turn := &ChatTurn{Bind: bind}
	var sawData bool
	for _, b := range content {
		switch b.Type {
		case ir.BlockText:
			if b.Refusal || b.Text == "" {
				continue
			}
			item := ChatLayoutItem{Kind: "text", TextLen: len(b.Text), TextHash: hashText(b.Text)}
			turn.Layout = append(turn.Layout, item)
		case ir.BlockReasoning:
			rr := b.ResponsesReasoning
			if rr == nil || !rr.ProviderState() {
				continue
			}
			// An empty id cannot be correlated after Pi reserializes the
			// entry. Skip the whole marker rather than emit an ambiguous one.
			if rr.ID == "" {
				return nil, nil
			}
			item := ChatReasoningItem{
				ID:     rr.ID,
				Status: rr.Status,
				Parts:  append([]ir.ReasoningSummaryPart(nil), rr.Summary...),
				Index:  len(turn.Layout),
			}
			if rr.EncryptedContent != nil {
				if *rr.EncryptedContent != "" {
					item.Data = *rr.EncryptedContent
					item.HasData = true
					sawData = true
				} else if profile.AuthoritativeEmpty {
					// Explicit empty is present. Do not require a nonempty sibling.
					item.HasData = true
					item.EmptyData = true
					sawData = true
				} else {
					item.EmptyData = true
				}
			}
			turn.Reasoning = append(turn.Reasoning, item)
			turn.Layout = append(turn.Layout, ChatLayoutItem{Kind: "reasoning", ID: item.ID})
		case ir.BlockToolUse:
			item := ChatLayoutItem{Kind: "tool", ID: b.ToolID, Name: b.ToolName, ItemID: b.ResponsesItemID}
			turn.Layout = append(turn.Layout, item)
		}
		if len(turn.Layout) > chatBridgeMaxItems || len(turn.Reasoning) > chatBridgeMaxItems {
			return nil, errChatBridgeBudget
		}
	}
	if !sawData {
		return nil, nil
	}
	// One charge, before signing or encoding. The parser uses this same function.
	if err := newBridgeAccount().chargeTurnProfile(profile, turn); err != nil {
		return nil, err
	}
	return turn, nil
}

func marshalTurn(profile ReplayProfile, turn ChatTurn, mac macFunc) ([]json.RawMessage, error) {
	if err := newBridgeAccount().chargeTurnProfile(profile, &turn); err != nil {
		return nil, err
	}
	layout := wireLayoutOf(turn.Layout)
	plains := wirePlainsOf(turn.Reasoning)
	bind := wireBindOf(profile, turn.Bind)
	var out []json.RawMessage
	for _, item := range turn.Reasoning {
		if !item.HasData {
			out = append(out, summaryDetails(profile, item)...)
			continue
		}
		enc := wireEncrypted{
			Type:   "reasoning.encrypted",
			ID:     item.ID,
			Format: profile.Format,
			Index:  item.Index,
			Data:   item.Data,
			Status: item.Status,
			Parts:  wirePartsOf(item.Parts),
			Layout: layout,
			Plain:  plains,
			Bind:   bind,
		}
		// Sign the JSON-normalized entry. Pi reserializes the object, so the
		// MAC cannot depend on Go's in-memory field presence.
		normRaw, err := json.Marshal(enc)
		if err != nil {
			return nil, profile.rejected()
		}
		var norm wireEncrypted
		if json.Unmarshal(normRaw, &norm) != nil {
			return nil, profile.rejected()
		}
		payload, err := payloadFromWire(profile, norm)
		if err != nil {
			return nil, err
		}
		sum, ok := mac(payload)
		// No private replay key: omit the marker. Do not treat the turn as signed.
		if !ok {
			return nil, nil
		}
		if len(sum) != sha256.Size {
			return nil, profile.rejected()
		}
		enc.MAC = base64.RawURLEncoding.EncodeToString(sum)
		raw, err := json.Marshal(enc)
		if err != nil {
			return nil, profile.rejected()
		}
		out = append(out, raw)
		out = append(out, summaryDetails(profile, item)...)
	}
	return out, nil
}

func summaryDetails(profile ReplayProfile, item ChatReasoningItem) []json.RawMessage {
	var out []json.RawMessage
	for _, p := range item.Parts {
		if p.Text == "" {
			continue
		}
		raw, err := json.Marshal(wireSummary{
			Type:    "reasoning.summary",
			ID:      item.ID,
			Format:  profile.Format,
			Index:   item.Index,
			Summary: p.Text,
		})
		if err != nil {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// ParseChatBridge classifies a Chat Completions body before any token or
// inference call. Tampered, malformed, wrong-caller, wrong-combo, mixed, and
// unmatched markers return ErrChatBridgeRejected. A legitimate marker whose
// source is gone is still returned so the caller can fail over without it.
func ParseChatBridge(body []byte, scope string, comboID int64, verify verifyFunc) (ChatDecision, error) {
	return ParseChatReplay(body, scope, comboID, []ReplayProfile{grokReplayProfile()}, func(protocol string, payload, mac []byte) bool {
		return protocol == chatBridgeProtocol && verify != nil && verify(payload, mac)
	})
}

// ParseChatReplay classifies every recognizable profile before any token or
// inference call. A verifier that cannot check a profile fails that marker
// closed. Unrelated third-party details stay on the ordinary path.
func ParseChatReplay(body []byte, scope string, comboID int64, profiles []ReplayProfile, verify protocolVerifyFunc) (ChatDecision, error) {
	return parseChatReplay(body, scope, comboID, parseState{profiles: profiles, verify: verify})
}

func parseChatReplay(body []byte, scope string, comboID int64, state parseState) (ChatDecision, error) {
	var decision ChatDecision
	// Collapsing maps drop duplicate and losing case-alias keys. Reject a
	// recognizable marker in a dropped value before any later path can forward
	// the original body.
	marked, err := rejectHiddenReplayMarkers(body, state.profiles)
	if err != nil {
		return ChatDecision{}, err
	}
	msgs, ok := chatMessages(body)
	if !ok {
		if marked {
			return decision, state.rejected(body)
		}
		return decision, nil
	}
	var assistants []int
	for i, msg := range msgs {
		role := jsonString(msg["role"])
		if role != "assistant" {
			if messageHasBridge(state, msg) {
				return ChatDecision{}, state.rejected(body)
			}
			continue
		}
		if fieldsOutsideDetailsHaveBridge(state, msg) {
			return ChatDecision{}, state.rejected(body)
		}
		assistants = append(assistants, i)
	}
	if len(assistants) == 0 {
		if marked {
			return decision, state.rejected(body)
		}
		return decision, nil
	}
	decision.Assistants = make([]AssistantNote, len(assistants))
	for n, idx := range assistants {
		note, err := parseAssistant(state, msgs[idx], scope, comboID)
		if err != nil {
			return ChatDecision{}, err
		}
		// Every verified turn is retained. Only the latest assistant reply pins.
		decision.Assistants[n] = note
		if note.Marked {
			decision.Strip = true
		}
	}
	if marked && !decision.Strip {
		return ChatDecision{}, state.rejected(body)
	}
	last := decision.Assistants[len(decision.Assistants)-1]
	if last.Turn != nil {
		decision.Pin = true
		decision.Turn = *last.Turn
		decision.Protocol = last.Protocol
	}
	return decision, nil
}

func parseAssistant(state parseState, msg map[string]json.RawMessage, scope string, comboID int64) (AssistantNote, error) {
	var note AssistantNote
	rawDetails, ok := msg["reasoning_details"]
	trimmed := bytes.TrimSpace(rawDetails)
	if !ok || len(trimmed) == 0 {
		return note, nil
	}
	if trimmed[0] != '[' {
		// A wrong container is not a detail array. Decode it before the raw
		// substring check so an escaped format or bind cannot bypass rejection.
		if structuralBridge(state, trimmed) {
			return note, state.rejected(trimmed)
		}
		return note, nil
	}
	var details []json.RawMessage
	if json.Unmarshal(rawDetails, &details) != nil {
		// Unrelated malformed details stay on the generic Chat path.
		// A broken copy of this bridge is rejected instead of forwarded.
		if structuralBridge(state, trimmed) {
			return note, state.rejected(trimmed)
		}
		return note, nil
	}
	if len(details) > chatBridgeMaxItems*2 {
		// Do not decode an oversized array unless it already has a bridge marker.
		if structuralBridge(state, trimmed) {
			return note, state.rejected(trimmed)
		}
		return note, nil
	}
	var ours []wireEncrypted
	var profile ReplayProfile
	haveProfile := false
	foreignEncrypted := false
	sawSummary := false
	for _, raw := range details {
		kind, matched, ok := classifyDetail(state, raw)
		if !ok {
			return note, state.rejected(raw)
		}
		switch kind {
		case detailMalformed:
			return note, state.rejected(raw)
		case detailOursSummary:
			// An unsigned companion may be absent. A companion without a signed
			// entry is still recognizable and is not authenticated.
			if haveProfile && matched.Protocol != profile.Protocol {
				return note, state.rejected(raw)
			}
			sawSummary = true
			if !haveProfile {
				profile = matched
				haveProfile = true
			}
		case detailOursEncrypted:
			var item wireEncrypted
			if json.Unmarshal(raw, &item) != nil || item.Type != "reasoning.encrypted" || item.Format != matched.Format {
				return note, state.rejected(raw)
			}
			if haveProfile && matched.Protocol != profile.Protocol {
				return note, state.rejected(raw)
			}
			profile = matched
			haveProfile = true
			ours = append(ours, item)
		case detailForeignEncrypted:
			foreignEncrypted = true
		}
	}
	if len(ours) == 0 {
		if sawSummary {
			return note, state.rejected(trimmed)
		}
		return note, nil
	}
	if foreignEncrypted || !haveProfile {
		return note, state.rejected(trimmed)
	}
	note.Marked = true
	note.Protocol = profile.Protocol
	turn, err := verifyEncrypted(state, profile, ours)
	if err != nil {
		return note, err
	}
	if turn.Bind.Scope != scope || turn.Bind.ComboID != comboID {
		return note, profile.rejected()
	}
	if err := matchAssistant(profile, msg, *turn); err != nil {
		return note, err
	}
	note.Summary = readableSummary(turn.Reasoning)
	note.Turn = turn
	return note, nil
}

func verifyEncrypted(state parseState, profile ReplayProfile, items []wireEncrypted) (*ChatTurn, error) {
	if len(items) == 0 || state.verify == nil || !profile.valid() {
		return nil, profile.rejected()
	}
	first := items[0]
	bind, err := bindFromWire(profile, first.Bind)
	if err != nil {
		return nil, err
	}
	layout, err := layoutFromWire(first.Layout)
	if err != nil {
		return nil, profile.rejected()
	}
	plains := plainsFromWire(first.Plain)
	turn := &ChatTurn{Bind: bind, Layout: layout}
	seen := map[string]bool{}
	for _, item := range items {
		gotBind, err := bindFromWire(profile, item.Bind)
		if err != nil || gotBind != bind {
			return nil, profile.rejected()
		}
		gotLayout, err := layoutFromWire(item.Layout)
		if err != nil || !sameLayout(gotLayout, layout) {
			return nil, profile.rejected()
		}
		if !samePlains(plainsFromWire(item.Plain), plains) {
			return nil, profile.rejected()
		}
		if item.Index < 0 || item.Index >= len(layout) {
			return nil, profile.rejected()
		}
		if item.Data == "" && !profile.AuthoritativeEmpty {
			return nil, profile.rejected()
		}
		if layout[item.Index].Kind != "reasoning" || layout[item.Index].ID != item.ID {
			return nil, profile.rejected()
		}
		if seen[item.ID] {
			return nil, profile.rejected()
		}
		seen[item.ID] = true
		// A wrong encoded length is structural. Do not decode it or build the
		// canonical payload before the shared budget accepts the turn.
		if len(item.MAC) != macEncodedLen() {
			return nil, profile.rejected()
		}
		turn.Reasoning = append(turn.Reasoning, ChatReasoningItem{
			ID: item.ID, Status: item.Status, Data: item.Data, HasData: true,
			EmptyData: item.Data == "" && profile.AuthoritativeEmpty,
			Parts:     partsFromWire(item.Parts), Index: item.Index,
		})
	}
	for _, plain := range plains {
		// A plaintext sibling of an encrypted item is the same reasoning item.
		// Rejecting the repeated id would make every stamped summary fail.
		if seen[plain.ID] {
			continue
		}
		seen[plain.ID] = true
		if plain.Index < 0 || plain.Index >= len(layout) || layout[plain.Index].Kind != "reasoning" || layout[plain.Index].ID != plain.ID {
			return nil, profile.rejected()
		}
		turn.Reasoning = append(turn.Reasoning, plain)
	}
	for _, item := range layout {
		if item.Kind == "reasoning" && !seen[item.ID] {
			return nil, profile.rejected()
		}
	}
	if err := newBridgeAccount().chargeTurnProfile(profile, turn); err != nil {
		return nil, profile.rejected()
	}
	for _, item := range items {
		mac, err := base64.RawURLEncoding.DecodeString(item.MAC)
		if err != nil || len(mac) != chatBridgeMACBytes {
			return nil, profile.rejected()
		}
		payload, err := payloadFromWire(profile, item)
		if err != nil || !state.verify(profile.Protocol, payload, mac) {
			return nil, profile.rejected()
		}
	}
	sortReasoning(turn.Reasoning)
	return turn, nil
}

func matchAssistant(profile ReplayProfile, msg map[string]json.RawMessage, turn ChatTurn) error {
	content := chatContentText(msg["content"])
	tools, err := chatToolCalls(msg["tool_calls"])
	if err != nil {
		return profile.rejected()
	}
	off := 0
	used := map[string]bool{}
	for _, item := range turn.Layout {
		switch item.Kind {
		case "text":
			if item.TextLen < 0 || off+item.TextLen > len(content) || hashText(content[off:off+item.TextLen]) != item.TextHash {
				return profile.rejected()
			}
			off += item.TextLen
		case "tool":
			got, ok := tools[item.ID]
			if !ok || used[item.ID] || got != item.Name {
				return profile.rejected()
			}
			used[item.ID] = true
		case "reasoning":
			if item.ID == "" {
				return profile.rejected()
			}
		default:
			return profile.rejected()
		}
	}
	if off != len(content) || len(used) != len(tools) {
		return profile.rejected()
	}
	return nil
}

// PortableChatBody returns a Chat body with this bridge's ciphertext,
// provenance, and provider-owned ids removed. Visible content, tool calls,
// tool results, and unknown fields such as stream_options stay. When the
// scalar reasoning field is missing, signed summary text is restored.
// A body with nothing to remove is returned unchanged.
func PortableChatBody(body []byte, decision ChatDecision) ([]byte, error) {
	return PortableChatReplay(body, decision, []ReplayProfile{grokReplayProfile()})
}

// PortableChatReplay strips every recognized profile's signed details,
// companions, and provider-owned item ids. Readable text, summaries, tool
// call ids, names, arguments, and results stay.
func PortableChatReplay(body []byte, decision ChatDecision, profiles []ReplayProfile) ([]byte, error) {
	if !decision.Strip {
		return body, nil
	}
	state := parseState{profiles: profiles}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil, ErrChatBridgeRejected
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(root["messages"], &msgs) != nil {
		return nil, ErrChatBridgeRejected
	}
	n := 0
	changed := false
	for i := range msgs {
		if jsonString(msgs[i]["role"]) != "assistant" {
			continue
		}
		if n >= len(decision.Assistants) {
			return nil, ErrChatBridgeRejected
		}
		note := decision.Assistants[n]
		n++
		if !note.Marked {
			continue
		}
		next, did, err := stripAssistant(state, msgs[i], note.Summary)
		if err != nil {
			return nil, err
		}
		if did {
			msgs[i] = next
			changed = true
		}
	}
	if n != len(decision.Assistants) || !changed {
		return nil, ErrChatBridgeRejected
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		return nil, ErrChatBridgeRejected
	}
	root["messages"] = raw
	out, err := json.Marshal(root)
	if err != nil {
		return nil, ErrChatBridgeRejected
	}
	return out, nil
}

func stripAssistant(state parseState, msg map[string]json.RawMessage, summary string) (map[string]json.RawMessage, bool, error) {
	rawDetails := msg["reasoning_details"]
	var details []json.RawMessage
	if json.Unmarshal(rawDetails, &details) != nil {
		return nil, false, state.rejected(rawDetails)
	}
	kept := make([]json.RawMessage, 0, len(details))
	removed := false
	for _, raw := range details {
		kind, _, ok := classifyDetail(state, raw)
		if !ok {
			return nil, false, state.rejected(raw)
		}
		switch kind {
		case detailOursEncrypted, detailOursSummary, detailMalformed:
			removed = true
			continue
		}
		kept = append(kept, append(json.RawMessage(nil), raw...))
	}
	if !removed {
		return nil, false, state.rejected(rawDetails)
	}
	if len(kept) == 0 {
		delete(msg, "reasoning_details")
	} else {
		raw, err := json.Marshal(kept)
		if err != nil {
			return nil, false, state.rejected(nil)
		}
		msg["reasoning_details"] = raw
	}
	if summary != "" && jsonString(msg["reasoning_content"]) == "" && jsonString(msg["reasoning"]) == "" {
		raw, err := json.Marshal(summary)
		if err != nil {
			return nil, false, state.rejected(nil)
		}
		msg["reasoning_content"] = raw
	}
	return msg, true, nil
}

// ApplyChatTurn rebuilds one assistant message so Responses encoding keeps
// the original item order and provider-owned ids. Tool arguments come from
// the decoded call with the same call_id and name. Refusal blocks stay after
// the layout; Chat cannot interleave them with content.
func ApplyChatTurn(blocks []ir.ContentBlock, turn ChatTurn) ([]ir.ContentBlock, error) {
	tools := map[string]ir.ContentBlock{}
	var text strings.Builder
	var refusals []ir.ContentBlock
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockToolUse:
			if _, ok := tools[b.ToolID]; ok {
				return nil, ErrChatBridgeRejected
			}
			tools[b.ToolID] = b
		case ir.BlockText:
			if b.Refusal {
				refusals = append(refusals, b)
				continue
			}
			text.WriteString(b.Text)
		}
	}
	byID := map[string]ChatReasoningItem{}
	for _, item := range turn.Reasoning {
		byID[item.ID] = item
	}
	content := text.String()
	off := 0
	usedTools := map[string]bool{}
	usedReasoning := map[string]bool{}
	var out []ir.ContentBlock
	for _, item := range turn.Layout {
		switch item.Kind {
		case "text":
			if item.TextLen < 0 || off+item.TextLen > len(content) || hashText(content[off:off+item.TextLen]) != item.TextHash {
				return nil, ErrChatBridgeRejected
			}
			out = append(out, ir.ContentBlock{Type: ir.BlockText, Text: content[off : off+item.TextLen]})
			off += item.TextLen
		case "tool":
			b, ok := tools[item.ID]
			if !ok || usedTools[item.ID] || b.ToolName != item.Name {
				return nil, ErrChatBridgeRejected
			}
			usedTools[item.ID] = true
			b.ResponsesItemID = item.ItemID
			out = append(out, b)
		case "reasoning":
			rr, ok := byID[item.ID]
			if !ok || usedReasoning[item.ID] {
				return nil, ErrChatBridgeRejected
			}
			usedReasoning[item.ID] = true
			out = append(out, reasoningBlock(rr))
		default:
			return nil, ErrChatBridgeRejected
		}
	}
	if off != len(content) || len(usedTools) != len(tools) || len(usedReasoning) != len(byID) {
		return nil, ErrChatBridgeRejected
	}
	return append(out, refusals...), nil
}
func InjectChatDetails(body []byte, details []json.RawMessage) ([]byte, error) {
	if len(details) == 0 {
		return body, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil, ErrChatBridgeRejected
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(root["choices"], &choices) != nil || len(choices) == 0 {
		return nil, ErrChatBridgeRejected
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(choices[0]["message"], &msg) != nil {
		return nil, ErrChatBridgeRejected
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return nil, ErrChatBridgeRejected
	}
	msg["reasoning_details"] = raw
	msgRaw, err := json.Marshal(msg)
	if err != nil {
		return nil, ErrChatBridgeRejected
	}
	choices[0]["message"] = msgRaw
	choicesRaw, err := json.Marshal(choices)
	if err != nil {
		return nil, ErrChatBridgeRejected
	}
	root["choices"] = choicesRaw
	return json.Marshal(root)
}

func reasoningBlock(item ChatReasoningItem) ir.ContentBlock {
	rr := &ir.ResponsesReasoning{ID: item.ID, Status: item.Status, Summary: append([]ir.ReasoningSummaryPart(nil), item.Parts...)}
	if item.HasData {
		v := item.Data
		rr.EncryptedContent = &v
	} else if item.EmptyData {
		v := ""
		rr.EncryptedContent = &v
	}
	return ir.ContentBlock{Type: ir.BlockReasoning, Text: rr.ReadableText(), ResponsesReasoning: rr}
}

func readableSummary(items []ChatReasoningItem) string {
	var b strings.Builder
	ordered := append([]ChatReasoningItem(nil), items...)
	sortReasoning(ordered)
	for _, item := range ordered {
		for _, p := range item.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func payloadFromWire(profile ReplayProfile, item wireEncrypted) ([]byte, error) {
	bind, err := bindFromWire(profile, item.Bind)
	if err != nil {
		return nil, err
	}
	layout, err := layoutFromWire(item.Layout)
	if err != nil {
		return nil, err
	}
	plains := plainsFromWire(item.Plain)
	rr := ChatReasoningItem{
		ID: item.ID, Status: item.Status, Data: item.Data, HasData: true,
		EmptyData: item.Data == "" && profile.AuthoritativeEmpty,
		Parts:     partsFromWire(item.Parts), Index: item.Index,
	}
	joined := make([]ChatReasoningItem, len(plains)+1)
	copy(joined, plains)
	joined[len(plains)] = rr
	return canonicalPayload(profile, bind, layout, joined, rr), nil
}

func canonicalPayload(profile ReplayProfile, bind ChatBind, layout []ChatLayoutItem, reasoning []ChatReasoningItem, item ChatReasoningItem) []byte {
	var b bytes.Buffer
	b.WriteString(profile.Canonical)
	putStr(&b, bind.Scope)
	putStr(&b, strconv.FormatInt(bind.ComboID, 10))
	putStr(&b, strconv.FormatInt(bind.ProviderID, 10))
	putStr(&b, profile.Protocol)
	putStr(&b, bind.Generation)
	putStr(&b, bind.Principal)
	putStr(&b, bind.Model)
	putStr(&b, bind.RouteModel)
	if profile.IncludeEndpoint {
		putStr(&b, bind.Endpoint)
	}
	putU32(&b, uint32(len(layout)))
	for _, it := range layout {
		putStr(&b, it.Kind)
		putU32(&b, uint32(it.TextLen))
		putStr(&b, it.TextHash)
		putStr(&b, it.ID)
		putStr(&b, it.Name)
		putStr(&b, it.ItemID)
	}
	plains := make([]ChatReasoningItem, 0, len(reasoning))
	for _, rr := range reasoning {
		if !rr.HasData {
			plains = append(plains, rr)
		}
	}
	sortReasoning(plains)
	putU32(&b, uint32(len(plains)))
	for _, rr := range plains {
		putReasoning(&b, rr, false)
	}
	putReasoning(&b, item, true)
	return b.Bytes()
}

func putReasoning(b *bytes.Buffer, item ChatReasoningItem, withData bool) {
	putStr(b, item.ID)
	putStr(b, item.Status)
	putU32(b, uint32(item.Index))
	if item.EmptyData {
		putU32(b, 1)
	} else {
		putU32(b, 0)
	}
	if withData {
		putStr(b, item.Data)
	}
	putU32(b, uint32(len(item.Parts)))
	for _, p := range item.Parts {
		putStr(b, p.Type)
		putStr(b, p.Text)
	}
}

func wireBindOf(profile ReplayProfile, bind ChatBind) wireBind {
	w := wireBind{
		Scope: bind.Scope, Combo: strconv.FormatInt(bind.ComboID, 10),
		Provider: strconv.FormatInt(bind.ProviderID, 10), Protocol: profile.Protocol,
		Generation: bind.Generation, Principal: bind.Principal,
		Model: bind.Model, Route: bind.RouteModel,
	}
	if profile.IncludeEndpoint {
		w.Endpoint = bind.Endpoint
	}
	return w
}

func bindFromWire(profile ReplayProfile, w wireBind) (ChatBind, error) {
	if w.Protocol != profile.Protocol || w.Generation == "" || w.Model == "" || w.Route == "" || w.Scope == "" {
		return ChatBind{}, profile.rejected()
	}
	endpoint := ""
	if profile.IncludeEndpoint {
		if profile.NormalizeEndpoint == nil {
			return ChatBind{}, profile.rejected()
		}
		norm, ok := profile.NormalizeEndpoint(w.Endpoint)
		if !ok || profile.RequirePrincipal && strings.TrimSpace(w.Principal) == "" {
			return ChatBind{}, profile.rejected()
		}
		endpoint = norm
	} else if w.Endpoint != "" {
		return ChatBind{}, profile.rejected()
	}
	combo, err := strconv.ParseInt(w.Combo, 10, 64)
	if err != nil || combo == 0 {
		return ChatBind{}, profile.rejected()
	}
	provider, err := strconv.ParseInt(w.Provider, 10, 64)
	if err != nil || provider == 0 {
		return ChatBind{}, profile.rejected()
	}
	return ChatBind{
		Scope: w.Scope, ComboID: combo, ProviderID: provider, Generation: w.Generation,
		Principal: w.Principal, Model: w.Model, RouteModel: w.Route, Endpoint: endpoint,
	}, nil
}

func wireLayoutOf(items []ChatLayoutItem) []wireLayout {
	out := make([]wireLayout, len(items))
	for i, item := range items {
		out[i] = wireLayout{K: item.Kind, N: item.TextLen, H: item.TextHash, ID: item.ID, Name: item.Name, Item: item.ItemID}
	}
	return out
}

func layoutFromWire(items []wireLayout) ([]ChatLayoutItem, error) {
	if len(items) == 0 || len(items) > chatBridgeMaxItems {
		return nil, ErrChatBridgeRejected
	}
	out := make([]ChatLayoutItem, len(items))
	for i, item := range items {
		switch item.K {
		case "text", "reasoning", "tool":
		default:
			return nil, ErrChatBridgeRejected
		}
		if item.N < 0 {
			return nil, ErrChatBridgeRejected
		}
		out[i] = ChatLayoutItem{Kind: item.K, TextLen: item.N, TextHash: item.H, ID: item.ID, Name: item.Name, ItemID: item.Item}
	}
	return out, nil
}

func wirePlainsOf(items []ChatReasoningItem) []wirePlain {
	var out []wirePlain
	for _, item := range items {
		if item.HasData {
			continue
		}
		out = append(out, wirePlain{ID: item.ID, Status: item.Status, Index: item.Index, Empty: item.EmptyData, Parts: wirePartsOf(item.Parts)})
	}
	return out
}

func plainsFromWire(items []wirePlain) []ChatReasoningItem {
	out := make([]ChatReasoningItem, len(items))
	for i, item := range items {
		out[i] = ChatReasoningItem{ID: item.ID, Status: item.Status, Index: item.Index, EmptyData: item.Empty, Parts: partsFromWire(item.Parts)}
	}
	return out
}

func wirePartsOf(parts []ir.ReasoningSummaryPart) []wirePart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]wirePart, len(parts))
	for i, p := range parts {
		out[i] = wirePart{Type: p.Type, Text: p.Text}
	}
	return out
}

func partsFromWire(parts []wirePart) []ir.ReasoningSummaryPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]ir.ReasoningSummaryPart, len(parts))
	for i, p := range parts {
		out[i] = ir.ReasoningSummaryPart{Type: p.Type, Text: p.Text}
	}
	return out
}

func sameLayout(a, b []ChatLayoutItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func samePlains(a, b []ChatReasoningItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Status != b[i].Status || a[i].Index != b[i].Index || a[i].EmptyData != b[i].EmptyData || len(a[i].Parts) != len(b[i].Parts) {
			return false
		}
		for j := range a[i].Parts {
			if a[i].Parts[j] != b[i].Parts[j] {
				return false
			}
		}
	}
	return true
}

func sortReasoning(items []ChatReasoningItem) {
	for i := 1; i < len(items); i++ {
		j := i
		for j > 0 && items[j].Index < items[j-1].Index {
			items[j], items[j-1] = items[j-1], items[j]
			j--
		}
	}
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func chatMessages(body []byte) ([]map[string]json.RawMessage, bool) {
	var root struct {
		Messages json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Messages) == 0 || root.Messages[0] != '[' {
		return nil, false
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(root.Messages, &msgs) != nil {
		return nil, false
	}
	return msgs, true
}

func chatContentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return s
	}
	if raw[0] != '[' {
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func chatToolCalls(raw json.RawMessage) (map[string]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]string{}, nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return nil, ErrChatBridgeRejected
	}
	out := make(map[string]string, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			return nil, ErrChatBridgeRejected
		}
		if _, ok := out[call.ID]; ok {
			return nil, ErrChatBridgeRejected
		}
		out[call.ID] = call.Function.Name
	}
	return out, nil
}

const (
	detailNone = iota
	detailOursEncrypted
	detailOursSummary
	detailForeignEncrypted
	detailMalformed
)

// bridgeAccount is the one stamp/parse budget. Lengths are logical field
// sizes. The MAC is the encoded size of chatBridgeMACBytes, not a computed tag.
type bridgeAccount struct {
	meta      int
	cipher    int
	encoded   int
	aggregate int
	parts     int
	details   int
}

func newBridgeAccount() *bridgeAccount { return &bridgeAccount{} }

func macEncodedLen() int {
	return base64.RawURLEncoding.EncodedLen(chatBridgeMACBytes)
}

func addBounded(dst *int, n, limit int) bool {
	if dst == nil || n < 0 || n > limit || *dst > limit-n {
		return false
	}
	*dst += n
	return true
}

func bindMetaLen(profile ReplayProfile, bind ChatBind) int {
	n := len(bind.Scope) + len(bind.Generation) + len(bind.Principal) + len(bind.Model) + len(bind.RouteModel) + len(profile.Protocol) + len(strconv.FormatInt(bind.ComboID, 10)) + len(strconv.FormatInt(bind.ProviderID, 10))
	if profile.IncludeEndpoint {
		n += len(bind.Endpoint)
	}
	return n
}

func (a *bridgeAccount) chargeTurn(turn *ChatTurn) error {
	return a.chargeTurnProfile(grokReplayProfile(), turn)
}

func (a *bridgeAccount) chargeTurnProfile(profile ReplayProfile, turn *ChatTurn) error {
	if a == nil || turn == nil {
		return errChatBridgeBudget
	}
	if len(turn.Layout) > chatBridgeMaxItems || len(turn.Reasoning) > chatBridgeMaxItems {
		return errChatBridgeBudget
	}
	a.meta = 0
	a.cipher = 0
	a.encoded = 0
	a.aggregate = len(turn.Layout) + len(turn.Reasoning)
	a.parts = 0
	a.details = 0
	layoutLen := 0
	for _, item := range turn.Layout {
		n := len(item.Kind) + len(item.TextHash) + len(item.ID) + len(item.Name) + len(item.ItemID)
		if !addBounded(&layoutLen, n, chatBridgeMaxMeta) || !addBounded(&a.meta, n, chatBridgeMaxMeta) {
			return errChatBridgeBudget
		}
	}
	bindLen := bindMetaLen(profile, turn.Bind)
	if !addBounded(&a.meta, bindLen, chatBridgeMaxMeta) {
		return errChatBridgeBudget
	}
	plainLen := 0
	encrypted := 0
	for _, item := range turn.Reasoning {
		if len(item.Parts) > chatBridgeMaxParts {
			return errChatBridgeBudget
		}
		a.parts += len(item.Parts)
		itemLen := len(item.ID) + len(item.Status)
		for _, p := range item.Parts {
			if !addBounded(&itemLen, len(p.Type)+len(p.Text), chatBridgeMaxMeta) {
				return errChatBridgeBudget
			}
		}
		if !addBounded(&a.meta, itemLen, chatBridgeMaxMeta) {
			return errChatBridgeBudget
		}
		if item.HasData {
			encrypted++
			if !addBounded(&a.cipher, len(item.Data), chatBridgeMaxCipher) {
				return errChatBridgeBudget
			}
			if !addBounded(&a.meta, macEncodedLen(), chatBridgeMaxMeta) {
				return errChatBridgeBudget
			}
			a.encoded += len(item.Data) + macEncodedLen()
			a.details++
			for _, p := range item.Parts {
				if p.Text != "" {
					a.details++
				}
			}
			continue
		}
		plainLen += itemLen
		for _, p := range item.Parts {
			if p.Text != "" {
				a.details++
			}
		}
	}
	if a.parts > chatBridgeMaxParts*chatBridgeMaxItems || a.details > chatBridgeMaxItems*2 {
		return errChatBridgeBudget
	}
	// These aggregates are copied into every encrypted entry. Count that copy
	// without marshaling the plaintext, which can itself be a megabyte.
	replicated := layoutLen + plainLen + bindLen
	if encrypted > 0 && replicated > 0 && encrypted > chatBridgeEncodedBudget/replicated {
		return errChatBridgeBudget
	}
	a.encoded += replicated * encrypted
	if a.meta > chatBridgeMaxMeta || a.cipher > chatBridgeMaxCipher {
		return errChatBridgeBudget
	}
	return nil
}

func fieldsOutsideDetailsHaveBridge(state parseState, msg map[string]json.RawMessage) bool {
	for key, raw := range msg {
		if key == "reasoning_details" {
			continue
		}
		if recognizableBridgeRaw(state, raw) {
			return true
		}
	}
	return false
}

func messageHasBridge(state parseState, msg map[string]json.RawMessage) bool {
	for _, raw := range msg {
		if recognizableBridgeRaw(state, raw) {
			return true
		}
	}
	return false
}

func recognizableBridgeRaw(state parseState, raw json.RawMessage) bool {
	return structuralBridge(state, raw)
}

func profileNeedle(state parseState, raw []byte) bool {
	for _, p := range state.profiles {
		if p.Format != "" && bytes.Contains(raw, []byte(p.Format)) {
			return true
		}
		if p.Protocol != "" && bytes.Contains(raw, []byte(p.Protocol)) {
			return true
		}
	}
	return false
}

// structuralBridge decodes before it decides. A JSON escape can hide the
// format or protocol name from a raw substring scan. A string that only quotes
// those names is not a bridge artifact.
func structuralBridge(state parseState, raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') {
		return false
	}
	// An unescaped ordinary value cannot hide a format or bind. Escapes use a
	// backslash, so this does not skip a unicode-escaped name.
	if !bytes.Contains(raw, []byte(`\`)) && !profileNeedle(state, raw) && !bytes.Contains(raw, []byte(`"format"`)) && !bytes.Contains(raw, []byte(`"bind"`)) && !bytes.Contains(raw, []byte(`"mac"`)) && !bytes.Contains(raw, []byte(`"layout"`)) {
		return false
	}
	var v any
	if !decodeReplayJSON(raw, &v) {
		return false
	}
	return valueHasBridge(state, v)
}

func decodeReplayJSON(raw []byte, dst any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// An unrelated large number must not hide a marker from an untyped decode.
	dec.UseNumber()
	if dec.Decode(dst) != nil {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

func valueHasBridge(state parseState, v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := objectIsBridge(state, t); ok {
			return true
		}
		for _, child := range t {
			if valueHasBridge(state, child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if valueHasBridge(state, child) {
				return true
			}
		}
	}
	return false
}

func objectIsBridge(state parseState, obj map[string]any) (ReplayProfile, bool) {
	format, _ := obj["format"].(string)
	if p, ok := state.profile(format, ""); ok {
		return p, true
	}
	bind, _ := obj["bind"].(map[string]any)
	if bind == nil {
		return ReplayProfile{}, false
	}
	proto, _ := bind["protocol"].(string)
	p, ok := state.profile("", proto)
	if !ok {
		return ReplayProfile{}, false
	}
	_, hasMAC := obj["mac"].(string)
	_, hasLayout := obj["layout"].([]any)
	if hasMAC || hasLayout {
		return p, true
	}
	return ReplayProfile{}, false
}

func nestedBridgeObject(state parseState, obj map[string]any) bool {
	for _, child := range obj {
		if valueHasBridge(state, child) {
			return true
		}
	}
	return false
}

// classifyDetail returns the detail class and, for a recognized profile, that
// profile. ok is false when the value is a recognizable malformed marker.
func classifyDetail(state parseState, raw json.RawMessage) (int, ReplayProfile, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return detailNone, ReplayProfile{}, true
	}
	if raw[0] != '{' {
		// An array can hide a stamped entry. A quoted format name cannot.
		if structuralBridge(state, raw) {
			return detailMalformed, ReplayProfile{}, false
		}
		return detailNone, ReplayProfile{}, true
	}
	var obj map[string]any
	if !decodeReplayJSON(raw, &obj) {
		return detailNone, ReplayProfile{}, true
	}
	kind, _ := obj["type"].(string)
	format, _ := obj["format"].(string)
	// Nested structure wins over a third-party type. Returning foreign here
	// would accept a signed entry hidden under reasoning.encrypted.
	if nestedBridgeObject(state, obj) {
		return detailMalformed, ReplayProfile{}, false
	}
	matched, ours := objectIsBridge(state, obj)
	if !ours {
		// Summary companions intentionally omit the signed bind. Their format
		// and id are still bridge fields and must be removed or rejected.
		if kind == "reasoning.summary" {
			if p, ok := state.profile(format, ""); ok {
				if _, has := obj["summary"].(string); !has {
					return detailMalformed, p, false
				}
				return detailOursSummary, p, true
			}
		}
		if kind == "reasoning.encrypted" {
			return detailForeignEncrypted, ReplayProfile{}, true
		}
		return detailNone, ReplayProfile{}, true
	}
	switch kind {
	case "reasoning.encrypted":
		if format != matched.Format {
			return detailMalformed, matched, false
		}
		if _, ok := obj["data"].(string); !ok {
			return detailMalformed, matched, false
		}
		return detailOursEncrypted, matched, true
	case "reasoning.summary":
		if format != matched.Format {
			return detailMalformed, matched, false
		}
		if _, ok := obj["summary"].(string); !ok {
			return detailMalformed, matched, false
		}
		return detailOursSummary, matched, true
	default:
		return detailMalformed, matched, false
	}
}

func jsonString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func putU32(b *bytes.Buffer, n uint32) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], n)
	b.Write(buf[:])
}

func putStr(b *bytes.Buffer, s string) {
	putU32(b, uint32(len(s)))
	_, _ = b.WriteString(s)
}

// Match encoding/json's nesting limit even when reading individual tokens.
const replayScanMaxDepth = 10000

// errReplayHidden is internal. Callers map it to a profile rejection and must
// not wrap it with request bytes.
var errReplayHidden = errors.New("hidden replay marker")

// rejectHiddenReplayMarkers rejects a recognizable marker that encoding/json
// would drop before the typed parser sees it. Dropped values include an
// earlier duplicate key, a losing case-alias of the root messages field, and
// marker fields whose last typed value is no longer recognizable. Ordinary
// strings, unrelated details, and duplicate keys that hide no marker stay on
// the normal path. The walk is one pass and stops at the body length.
func rejectHiddenReplayMarkers(body []byte, profiles []ReplayProfile) (bool, error) {
	if len(bytes.TrimSpace(body)) == 0 || !replayMarkerMayHide(body, profiles) {
		return false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	w := &replayWalker{dec: dec, profiles: profiles, limit: len(body) + 8}
	val, err := w.consume(0)
	if err != nil {
		if errors.Is(err, errReplayHidden) {
			return false, w.rejected()
		}
		return false, nil
	}
	if val.hidden {
		return false, w.rejected()
	}
	if _, err := dec.Token(); err != io.EOF {
		return false, nil
	}
	return val.marked, nil
}

func replayMarkerMayHide(body []byte, profiles []ReplayProfile) bool {
	if bytes.Contains(body, []byte(`\`)) || bytes.Contains(body, []byte(`"bind"`)) || bytes.Contains(body, []byte(`"format"`)) || bytes.Contains(body, []byte(`"mac"`)) || bytes.Contains(body, []byte(`"layout"`)) {
		return true
	}
	for _, p := range profiles {
		if p.Format != "" && bytes.Contains(body, []byte(p.Format)) ||
			p.Protocol != "" && bytes.Contains(body, []byte(p.Protocol)) {
			return true
		}
	}
	return false
}

type replayScan struct {
	hidden      bool
	marked      bool
	recAny      bool
	recWin      bool
	role        string
	details     *replayScan
	array       bool
	messages    bool
	detailOK    bool
	anyProtocol string
	protocol    string
}

type replayWalker struct {
	dec      *json.Decoder
	profiles []ReplayProfile
	limit    int
	tokens   int
	format   string
	protocol string
}

func (w *replayWalker) tick() error {
	if w.tokens >= w.limit {
		return errReplayHidden
	}
	w.tokens++
	return nil
}

func (w *replayWalker) note(format, protocol string) {
	for _, p := range w.profiles {
		if w.format == "" && format == p.Format {
			w.format = format
		}
		if w.protocol == "" && protocol == p.Protocol {
			w.protocol = protocol
		}
	}
}

func (w *replayWalker) rejected() error {
	for _, p := range w.profiles {
		if w.format != "" && w.format == p.Format {
			return p.rejected()
		}
	}
	for _, p := range w.profiles {
		if w.protocol != "" && w.protocol == p.Protocol {
			return p.rejected()
		}
	}
	if len(w.profiles) > 0 {
		return w.profiles[0].rejected()
	}
	return ErrChatBridgeRejected
}

func (w *replayWalker) consume(depth int) (replayScan, error) {
	if depth > replayScanMaxDepth {
		return replayScan{}, errReplayHidden
	}
	if err := w.tick(); err != nil {
		return replayScan{}, err
	}
	tok, err := w.dec.Token()
	if err != nil {
		return replayScan{}, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return w.object(depth, depth == 0)
		case '[':
			return w.array(depth)
		default:
			return replayScan{}, io.ErrUnexpectedEOF
		}
	case string:
		return replayScan{role: t}, nil
	default:
		return replayScan{}, nil
	}
}

func (w *replayWalker) object(depth int, root bool) (replayScan, error) {
	var out replayScan
	var messages replayScan
	haveMessages := false
	var anyFormat, winFormat, anyProto, winProto string
	var anyMAC, winMAC, anyLayout, winLayout bool
	for w.dec.More() {
		if err := w.tick(); err != nil {
			return replayScan{}, err
		}
		keyTok, err := w.dec.Token()
		if err != nil {
			return replayScan{}, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return replayScan{}, io.ErrUnexpectedEOF
		}
		val, err := w.consume(depth + 1)
		if err != nil {
			return replayScan{}, err
		}
		if strings.EqualFold(key, "protocol") {
			if key == "protocol" {
				out.protocol = val.role
			}
			if w.knownProtocol(val.role) {
				out.anyProtocol = val.role
			}
		}
		w.absorbIdentity(key, val, &anyFormat, &winFormat, &anyProto, &winProto, &anyMAC, &winMAC, &anyLayout, &winLayout)
		switch {
		case root && strings.EqualFold(key, "messages"):
			if haveMessages && (messages.marked || messages.hidden) {
				out.hidden = true
			}
			messages, haveMessages = val, true
		case !root && key == "reasoning_details":
			if out.details != nil && (out.details.marked || out.details.hidden) {
				out.hidden = true
			}
			cp := val
			out.details = &cp
		case val.marked || val.hidden:
			// A nested marker is never valid in an arbitrary field. Record it
			// immediately so no field map or arbitrary key limit is needed.
			out.hidden = true
		case !root && key == "role":
			out.role = val.role
		}
	}
	if err := w.tick(); err != nil {
		return replayScan{}, err
	}
	if _, err := w.dec.Token(); err != nil {
		return replayScan{}, err
	}
	out.recAny = w.recognizable(anyFormat, anyProto, anyMAC, anyLayout)
	out.recWin = w.recognizable(winFormat, winProto, winMAC, winLayout)
	if out.recAny {
		out.marked = true
		w.note(anyFormat, anyProto)
		// Only entries in an assistant's detail array may carry identity.
		if root || !out.recWin {
			out.hidden = true
		}
	}
	if root && (messages.marked || messages.hidden) {
		out.marked = true
		out.hidden = out.hidden || messages.hidden || !messages.messages
	}
	if out.details != nil && (out.details.marked || out.details.hidden) {
		out.marked = true
		out.hidden = out.hidden || out.details.hidden
	}
	return out, nil
}

func (w *replayWalker) absorbIdentity(key string, val replayScan, anyFormat, winFormat, anyProto, winProto *string, anyMAC, winMAC, anyLayout, winLayout *bool) {
	switch {
	case strings.EqualFold(key, "format"):
		if w.knownFormat(val.role) {
			*anyFormat = val.role
		}
		if key == "format" {
			*winFormat = val.role
		}
	case strings.EqualFold(key, "mac"):
		*anyMAC = *anyMAC || val.role != ""
		if key == "mac" {
			*winMAC = val.role != ""
		}
	case strings.EqualFold(key, "layout"):
		*anyLayout = *anyLayout || val.array
		if key == "layout" {
			*winLayout = val.array
		}
	case strings.EqualFold(key, "bind"):
		if val.anyProtocol != "" {
			*anyProto = val.anyProtocol
		}
		if key == "bind" {
			*winProto = val.protocol
		}
	}
}

func (w *replayWalker) knownFormat(s string) bool {
	for _, p := range w.profiles {
		if s != "" && s == p.Format {
			return true
		}
	}
	return false
}

func (w *replayWalker) knownProtocol(s string) bool {
	for _, p := range w.profiles {
		if s != "" && s == p.Protocol {
			return true
		}
	}
	return false
}

func (w *replayWalker) recognizable(format, protocol string, mac, layout bool) bool {
	for _, p := range w.profiles {
		if format != "" && format == p.Format {
			return true
		}
	}
	for _, p := range w.profiles {
		if protocol != "" && protocol == p.Protocol && (mac || layout) {
			return true
		}
	}
	return false
}

func (w *replayWalker) array(depth int) (replayScan, error) {
	out := replayScan{array: true, messages: true, detailOK: true}
	for w.dec.More() {
		el, err := w.consume(depth + 1)
		if err != nil {
			return replayScan{}, err
		}
		if el.hidden {
			out.hidden = true
		}
		if el.marked || el.hidden {
			out.marked = true
		}
		if !messageAllowsMarker(el) {
			out.messages = false
		}
		if !detailAllowsMarker(el) {
			out.detailOK = false
		}
	}
	if err := w.tick(); err != nil {
		return replayScan{}, err
	}
	if _, err := w.dec.Token(); err != nil {
		return replayScan{}, err
	}
	return out, nil
}

func messageAllowsMarker(el replayScan) bool {
	if el.hidden {
		return false
	}
	if !el.marked {
		return true
	}
	if el.role != "assistant" || el.recAny {
		return false
	}
	if el.details == nil {
		return false
	}
	return el.details.array && el.details.detailOK && !el.details.hidden
}

func detailAllowsMarker(el replayScan) bool {
	if el.hidden {
		return false
	}
	if !el.marked {
		return true
	}
	return el.recWin && (el.details == nil || !el.details.marked)
}
