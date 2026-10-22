package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"airouter/internal/proxy/thinking"
)

// PrepareInput is the per-send Claude Code preparation. SessionID must match
// the session header. AccountUUID and DeviceID come from the resolved
// credential snapshot, never from the client body. Token is recorded so a
// caller can see which credential the body was prepared for. The body itself
// does not contain the token. OAuth-specific billing and beta fields are
// selected by the header seam from the same snapshot, not by rewriting this body
// into an OAuth envelope.
type PrepareInput struct {
	Token       string
	SessionID   string
	AccountUUID string
	DeviceID    string
	ParentID    string
	Profile     Profile
	BaseURL     string
	// VertexTransport is set only when the backend is an actual Vertex
	// transport. A hostname is not evidence.
	VertexTransport bool
	// CacheDefault, when true and the caller set no cache policy, anchors
	// identity and unmarked caller system text. Explicit caller markers stay.
	CacheDefault bool
	CacheTTL     string
	// Attribution carries optional validated billing fields. Empty fields are omitted.
	Attribution BillingInput
}

// Prepare applies official CLI preparation to an encoded Anthropic Messages
// body. It replaces only the system array and metadata.user_id. Other
// top-level fields, including context_management, output_config, thinking,
// tool_choice, and tool declarations, stay as the Anthropic encoder wrote them.
// Unknown top-level ingress fields are not forwarded: this function edits the
// already-validated Anthropic body, it does not accept a raw client envelope.
func Prepare(body []byte, in PrepareInput) ([]byte, error) {
	var doc map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("claude-code: decode for prepare: %w", err)
	}
	if err := validatePreparedBody(doc); err != nil {
		return nil, err
	}
	profile := in.Profile
	if profile.Version == "" {
		profile.Version = CLIVersion
	}
	if profile.Entrypoint == "" {
		profile.Entrypoint = DefaultEntrypoint
	}
	caller, prepared := extractCallerSystem(doc["system"])
	firstUser := firstUserText(doc["messages"])
	bill := in.Attribution
	bill.Entrypoint = profile.Entrypoint
	if bill.Workload == "" {
		bill.Workload = profile.Workload
	}
	baseURL := strings.TrimSpace(in.BaseURL)
	if baseURL == "" {
		// A blank source base resolves to the same default endpoint as the preset.
		baseURL = DefaultBaseURL
	}
	bill.OfficialHost = IsOfficialHost(baseURL)
	bill.VertexTransport = in.VertexTransport
	bill.FirstUserText = firstUser
	bill.Fingerprint = Fingerprint(firstUser)
	billing := BillingHeader(bill)

	// A quoted cache_control string inside schema or text is not a marker.
	// Only top-level cache_control and the API's marker positions (system
	// blocks, tools, top-level message content blocks) are policy. A default
	// anchor pair written by an earlier Prepare is recognized only on a body
	// that still carries the full billing and identity prefix; a fresh caller
	// marker at the same position stays caller policy.
	policy, stripDefault := callerCachePolicy(doc, caller, prepared)
	if stripDefault >= 0 && stripDefault < len(caller) {
		caller[stripDefault].CacheControl = nil
	}
	cache := in.CacheDefault && !policy
	ttl := ""
	if cache {
		ttl = in.CacheTTL
		if ttl != "5m" && ttl != "1h" {
			ttl = "5m"
		}
	}
	blocks := SystemBlocks(billing, profile.IdentityLine(), caller, cache, ttl)
	raw, err := json.Marshal(blocks)
	if err != nil {
		return nil, fmt.Errorf("claude-code: encode system: %w", err)
	}
	doc["system"] = raw

	uid, err := MarshalUserID(UserID{
		DeviceID:        in.DeviceID,
		AccountUUID:     in.AccountUUID,
		SessionID:       in.SessionID,
		ParentSessionID: in.ParentID,
	})
	if err != nil {
		return nil, err
	}
	meta, err := mergeUserID(doc["metadata"], uid)
	if err != nil {
		return nil, err
	}
	doc["metadata"] = meta
	if raw := doc["model"]; len(raw) > 0 {
		var model string
		if json.Unmarshal(raw, &model) == nil {
			if clean := thinking.StripContextSelectors(model); clean != model {
				doc["model"], _ = json.Marshal(clean)
			}
		}
	}
	return json.Marshal(doc)
}

func validatePreparedBody(doc map[string]json.RawMessage) error {
	if len(doc["model"]) == 0 || len(doc["messages"]) == 0 || len(doc["max_tokens"]) == 0 {
		return fmt.Errorf("claude-code: prepare requires model, messages, and max_tokens")
	}
	for _, key := range []string{"context_management", "output_config", "thinking", "tool_choice", "tools", "metadata"} {
		if len(doc[key]) == 0 {
			continue
		}
		if !json.Valid(doc[key]) {
			return fmt.Errorf("claude-code: %s is malformed", key)
		}
	}
	return nil
}

func mergeUserID(raw json.RawMessage, userID string) (json.RawMessage, error) {
	meta := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if raw[0] != '{' {
			return nil, fmt.Errorf("claude-code: metadata must be an object")
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("claude-code: metadata: %w", err)
		}
	}
	meta["user_id"] = userID
	out, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("claude-code: encode metadata: %w", err)
	}
	return out, nil
}

// extractCallerSystem returns caller system blocks with a prior billing marker
// and a prior official identity line removed. Block boundaries and cache marks
// stay. prepared reports that the array began with the billing and identity
// prefix this package writes, so default anchors from an earlier Prepare can be
// recognized without treating a fresh caller marker as synthesized. A billing
// string that also contains caller text after the header is split only on a
// complete billing field list, not on the first semicolon.
func extractCallerSystem(raw json.RawMessage) ([]SystemBlock, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		text, dropped := stripPreparedText(s)
		if strings.TrimSpace(text) == "" {
			return nil, false
		}
		return []SystemBlock{{Text: text}}, dropped
	}
	if raw[0] != '[' {
		return nil, false
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	var out []SystemBlock
	prepared := preparedSystemPrefix(blocks)
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		if typ != "" && typ != "text" {
			continue
		}
		text, _ := b["text"].(string)
		if strings.HasPrefix(strings.TrimSpace(text), BillingHeaderPrefix) {
			text, _ = stripPreparedText(text)
			if strings.TrimSpace(text) == "" {
				continue
			}
		}
		rawText := text
		text, dropped := stripPreparedText(text)
		if dropped && strings.TrimSpace(text) == "" {
			continue
		}
		if isIdentityLine(text) || isIdentityLine(rawText) {
			continue
		}
		mark := cacheMark(b["cache_control"])
		if strings.TrimSpace(text) == "" && mark == nil {
			continue
		}
		out = append(out, SystemBlock{Text: text, CacheControl: mark})
	}
	return out, prepared
}

// preparedSystemPrefix reports whether the array starts with the exact billing
// and identity blocks this package inserts. The marker on a later caller block
// is synthesized only when this full prefix is present; a fresh caller marker
// must never be mistaken for one.
func preparedSystemPrefix(blocks []map[string]any) bool {
	if len(blocks) < 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		typ, _ := blocks[i]["type"].(string)
		if typ != "" && typ != "text" {
			return false
		}
	}
	billing, _ := blocks[0]["text"].(string)
	billing = strings.TrimSpace(billing)
	if !strings.HasPrefix(billing, BillingHeaderPrefix) || !strings.Contains(billing, "cc_version=") || !strings.Contains(billing, "cc_entrypoint=") {
		return false
	}
	identity, _ := blocks[1]["text"].(string)
	return isIdentityLine(identity)
}

// callerCachePolicy reports whether the caller set an explicit cache policy.
// stripIndex is the caller block whose marker is a synthesized default anchor
// to ignore, or -1. Only the exact default pair on a still-prepared body is
// ignored: the identity block marker and the last caller block marker, both
// unscoped with the configured TTL. A fresh marker on the last block of an
// unprepared body stays caller policy.
func callerCachePolicy(doc map[string]json.RawMessage, caller []SystemBlock, prepared bool) (bool, int) {
	if topLevelCachePolicy(doc["cache_control"]) {
		return true, -1
	}
	if toolsCachePolicy(doc["tools"]) || messagesCachePolicy(doc["messages"]) {
		return true, -1
	}
	var identityMark *CacheMark
	if prepared {
		identityMark = preparedIdentityCacheMark(doc["system"])
	}
	identityDefault := defaultAnchor(identityMark)
	if len(caller) == 0 {
		return identityMark != nil && !identityDefault, -1
	}
	marked := -1
	count := 0
	for i, b := range caller {
		if b.CacheControl != nil {
			count++
			marked = i
		}
	}
	if count == 0 {
		return identityMark != nil && !identityDefault, -1
	}
	// A prior default pair has one marker on the identity block and one on the
	// last caller block, with identical scope and TTL. A different caller TTL or
	// scope is a fresh explicit policy and must not be rewritten.
	if prepared && count == 1 && marked == len(caller)-1 && identityDefault &&
		defaultAnchor(caller[marked].CacheControl) && sameCacheMark(caller[marked].CacheControl, identityMark) {
		return false, marked
	}
	return true, -1
}

// preparedIdentityMark reports the marker on the identity block of a prepared
// system array. isDefault is true only for the unscoped default anchor shape.
func preparedIdentityMark(raw json.RawMessage) (isDefault, marked bool) {
	mark := preparedIdentityCacheMark(raw)
	if mark == nil {
		return false, false
	}
	return defaultAnchor(mark), true
}

// preparedIdentityCacheMark returns the marker on the identity block, or nil
// when the block is unmarked or the array is not a prepared system array.
func preparedIdentityCacheMark(raw json.RawMessage) *CacheMark {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	if len(blocks) < 2 {
		return nil
	}
	return cacheMark(blocks[1]["cache_control"])
}

func sameCacheMark(a, b *CacheMark) bool {
	if a == nil || b == nil {
		return false
	}
	if a.TTL != b.TTL || a.Scope != b.Scope {
		return false
	}
	// Presence and value both matter: an explicit evict_on_complete on one side
	// is never the same marker as an omitted one.
	if (a.EvictOnComplete == nil) != (b.EvictOnComplete == nil) {
		return false
	}
	if a.EvictOnComplete != nil && *a.EvictOnComplete != *b.EvictOnComplete {
		return false
	}
	return true
}

func topLevelCachePolicy(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && len(obj) > 0
}

// toolsCachePolicy checks only the tools array elements, the API marker
// position. A property named cache_control inside a tool schema is not policy.
func toolsCachePolicy(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false
	}
	for _, t := range tools {
		if topLevelCachePolicy(t["cache_control"]) {
			return true
		}
	}
	return false
}

// messagesCachePolicy checks cache_control on top-level message content blocks.
// Nested tool-result content and schema properties are not marker positions.
func messagesCachePolicy(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msgs) != nil {
		return false
	}
	for _, m := range msgs {
		if len(m.Content) == 0 || m.Content[0] != '[' {
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if topLevelCachePolicy(b["cache_control"]) {
				return true
			}
		}
	}
	return false
}

func defaultAnchor(mark *CacheMark) bool {
	if mark == nil || mark.Scope != "" || mark.EvictOnComplete != nil {
		return false
	}
	return mark.TTL == "5m" || mark.TTL == "1h"
}

func cacheMark(v any) *CacheMark {
	m, ok := v.(map[string]any)
	if !ok || m["type"] != "ephemeral" {
		return nil
	}
	mark := &CacheMark{}
	if ttl, ok := m["ttl"].(string); ok && (ttl == "5m" || ttl == "1h") {
		mark.TTL = ttl
	}
	if scope, ok := m["scope"].(string); ok && (scope == "global" || scope == "org") {
		mark.Scope = scope
	}
	// A non-bool value is not caller policy this struct models; it stays unset
	// so the marker cannot be mistaken for an explicit eviction choice.
	if evict, ok := m["evict_on_complete"].(bool); ok {
		mark.EvictOnComplete = &evict
	}
	return mark
}

// CacheFeatures reports structured cache-control flags at API marker positions.
// A property named cache_control inside schema or text is not a marker. Eviction
// is the verified ephemeral shape with evict_on_complete exactly true; an
// explicit false or an invented type never activates it.
func CacheFeatures(body []byte) (globalScope, extendedTTL, evict bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return false, false, false
	}
	visit := func(raw json.RawMessage) {
		var m map[string]any
		if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &m) != nil {
			return
		}
		if m["scope"] == "global" {
			globalScope = true
		}
		if m["ttl"] == "1h" {
			extendedTTL = true
		}
		if m["type"] == "ephemeral" {
			if ev, ok := m["evict_on_complete"].(bool); ok && ev {
				evict = true
			}
		}
	}
	if topLevelCachePolicy(doc["cache_control"]) {
		visit(doc["cache_control"])
	}
	if len(doc["system"]) > 0 && doc["system"][0] == '[' {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(doc["system"], &blocks) == nil {
			for _, b := range blocks {
				visit(b["cache_control"])
			}
		}
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(doc["tools"], &tools) == nil {
		for _, t := range tools {
			visit(t["cache_control"])
		}
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(doc["messages"], &msgs) == nil {
		for _, msg := range msgs {
			if len(msg.Content) == 0 || msg.Content[0] != '[' {
				continue
			}
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(msg.Content, &blocks) == nil {
				for _, b := range blocks {
					visit(b["cache_control"])
				}
			}
		}
	}
	return globalScope, extendedTTL, evict
}

// stripPreparedText removes only an exact prep prefix at byte zero. Ordinary
// caller text, including leading and trailing whitespace, is returned unchanged.
// The billing prefix is recognized only when it starts the block, and only the
// known line break between prep blocks is removed.
func stripPreparedText(s string) (string, bool) {
	if strings.HasPrefix(s, BillingHeaderPrefix) {
		rest := stripBillingHeader(s)
		rest = trimPreparedLineBreak(rest)
		if rest == "" || isIdentityLine(rest) {
			return "", true
		}
		if next, ok := trimIdentityPrefix(rest); ok {
			return next, true
		}
		return rest, true
	}
	if isIdentityLine(s) {
		return "", true
	}
	return s, false
}

func trimPreparedLineBreak(s string) string {
	if strings.HasPrefix(s, "\r\n") {
		return s[2:]
	}
	if strings.HasPrefix(s, "\n") {
		return s[1:]
	}
	return s
}

func trimIdentityPrefix(s string) (string, bool) {
	for _, identity := range []string{IdentityCLI, IdentitySDK, IdentityAgent} {
		if s != identity && !strings.HasPrefix(s, identity+"\n") && !strings.HasPrefix(s, identity+"\r\n") {
			continue
		}
		return trimPreparedLineBreak(s[len(identity):]), true
	}
	return "", false
}

// stripBillingHeader removes one complete billing header. Fields are "; "-separated
// in the CLI builder, and the header ends at the first "; " that is not inside
// the generated field list. A caller suffix after that boundary is kept exactly.
func stripBillingHeader(s string) string {
	rest := strings.TrimPrefix(s, BillingHeaderPrefix)
	for {
		i := strings.Index(rest, ";")
		if i < 0 {
			return ""
		}
		after := rest[i+1:]
		if strings.HasPrefix(after, " ") && looksLikeBillingField(strings.TrimSpace(after)) {
			rest = after
			continue
		}
		return after
	}
}

func looksLikeBillingField(s string) bool {
	fields := []string{
		"cc_version=", "cc_entrypoint=", "cch=", "cc_workload=", "cc_is_subagent=",
		"cc_prev_req=", "cc_prompt_id=", "cc_turn_origin=", "cc_prompt_index=",
		"cc_turn_index=", "cc_gateway=", "cc_project=", "cc_project_src=",
	}
	for _, f := range fields {
		if strings.HasPrefix(s, f) {
			return true
		}
	}
	return false
}

func isIdentityLine(s string) bool {
	return s == IdentityCLI || s == IdentitySDK || s == IdentityAgent
}

func firstUserText(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '[' {
		return ""
	}
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msgs) != nil {
		return ""
	}
	for _, m := range msgs {
		if m.Role != "user" || len(m.Content) == 0 {
			continue
		}
		if m.Content[0] == '"' {
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				return s
			}
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if (b.Type == "" || b.Type == "text") && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}
