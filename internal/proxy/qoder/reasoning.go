package qoder

import (
	"encoding/json"
	"fmt"
	"strings"

	"airouter/internal/proxy/ir"
)

// Catalog-aware reasoning control resolution for the Qoder backend.
//
// The live model_config catalog is the only source of reasoning capability
// (qfmodel advertises thinking_config with a disabled block, an enabled block,
// and an efforts enum). Requested intent arrives as IR thinking, already
// suffix-merged by the shared apply path. Every explicit control must be
// provably supported by the advertisement; anything else is a client error
// before inference instead of a silent guess or an invented wire setting. An
// explicitly nonreasoning catalog entry is never promoted: stale enabled
// metadata is suppressed, and explicit enabling or positive effort is
// rejected.

// effortRank orders the recognized positive effort levels. Unknown catalog
// levels are never sent and never become remap targets.
var effortRank = map[string]int{
	"minimal": 0,
	"low":     1,
	"medium":  2,
	"high":    3,
	"xhigh":   4,
	"max":     5,
	"ultra":   6,
}

// effortsKeyAliases mirrors the field names the native CLI's embedded catalog
// parser recognizes for the effort enum.
var effortsKeyAliases = []string{
	"efforts", "reasoning_efforts", "reasoningEfforts",
	"reasoning_effort_levels", "reasoningEffortLevels",
	"effort_level", "effortLevel", "effort_levels", "effortLevels",
	"supported_efforts", "supportedEfforts",
	"supported_effort_levels", "supportedEffortLevels",
	"levels",
}

type reasoningState int

const (
	// reasoningUnset writes no reasoning fields: intent was omitted or auto
	// and the catalog advertised no usable default.
	reasoningUnset reasoningState = iota
	// reasoningOn sends enable_thinking=true plus the selected effort when one
	// was resolved. An empty effort enables the toggle alone and removes any
	// stale reasoning_effort; an empty string is never sent as
	// reasoning_effort.
	reasoningOn
	// reasoningOff sends effort "none", enable_thinking=false, and clears the
	// mirrored is_reasoning flags.
	reasoningOff
)

// reasoningFlag classifies the catalog's is_reasoning member. The flag never
// manufactures capability: an explicit false vetoes enabling, and a malformed
// value is never overwritten in either direction.
type reasoningFlag int

const (
	reasoningFlagMissing reasoningFlag = iota
	reasoningFlagFalse
	reasoningFlagTrue
	// reasoningFlagMalformed marks a present member that is not a JSON boolean,
	// including null. Published garbage is never coerced to true or false.
	reasoningFlagMalformed
)

// effectiveReasoning is the final wire selection for one request.
type effectiveReasoning struct {
	state  reasoningState
	effort string // set when state == reasoningOn; empty enables the toggle alone
}

// reasoningCaps is the capability advertisement parsed from one catalog entry.
// Absent metadata stays absent: it never implies support.
type reasoningCaps struct {
	Supported bool // a thinking_config container or root reasoning metadata is advertised
	// CanDisable is true when the disabled block exists (non-null, not false),
	// supports_disabled is true (container or root), or the effort enum
	// explicitly lists none/off.
	CanDisable bool
	// CanEnable reports an advertised way to turn reasoning on: a valid enabled
	// block or a usable effort enum. A disabled-only container is not proof of
	// enable support.
	CanEnable bool
	// Efforts lists the advertised recognized positive efforts in ascending
	// rank order. Unknown advertised levels are dropped entirely.
	Efforts []string
	// DefaultEffort is the advertised default when it is recognized, present in
	// Efforts, and unambiguous. DefaultDisabled marks a disabled default, from
	// a disabled block marked is_default, an advertised none/off enum entry
	// marked is_default, or an explicit default_effort of none/off. The two
	// are mutually exclusive: conflicting valid candidates resolve to no
	// default instead of letting the disabled state win by precedence.
	DefaultEffort   string
	DefaultDisabled bool
	// NonReasoning is the catalog's explicit is_reasoning:false. It vetoes
	// enabling and suppresses stale enabled defaults; it is never flipped.
	NonReasoning bool
	Flag         reasoningFlag
}

// parseReasoningCaps extracts reasoning capability from a raw catalog entry.
// Malformed or missing metadata degrades to "no advertisement" so a broken
// catalog never introduces unsupported wire settings. Capability fields are
// read from the enabled block, the thinking_config container, and the catalog
// root; only parseable values count toward support.
func parseReasoningCaps(config json.RawMessage) reasoningCaps {
	var cfg map[string]json.RawMessage
	if json.Unmarshal(config, &cfg) != nil || cfg == nil {
		return reasoningCaps{}
	}
	caps := reasoningCaps{Flag: parseReasoningFlag(cfg)}
	caps.NonReasoning = caps.Flag == reasoningFlagFalse
	tc, tcOK := objectField(cfg, "thinking_config")
	if !tcOK {
		tc, tcOK = objectField(cfg, "thinkingConfig")
	}
	scopes := reasoningScopes(tc, tcOK, cfg)
	if tcOK {
		if raw, present := tc["disabled"]; present && !rawIsNullOrFalse(raw) {
			caps.CanDisable = true
			// A disabled entry carrying is_default advertises the disabled default.
			if entry, ok := objectField(tc, "disabled"); ok && flagIsTrue(entry, "is_default", "isDefault") {
				caps.DefaultDisabled = true
			}
		}
		if flagIsTrue(tc, "supports_disabled", "supportsDisabled") {
			caps.CanDisable = true
		}
	}
	if flagIsTrue(cfg, "supports_disabled", "supportsDisabled") {
		caps.CanDisable = true
	}
	efforts := parseAdvertisedEfforts(scopes)
	// An enum that lists none/off advertises the disabled state by itself, even
	// without a disabled block or supports_disabled flag.
	if enumAdvertisesDisable(efforts) {
		caps.CanDisable = true
	}
	if len(efforts) > 0 {
		caps.Efforts = recognizedEfforts(efforts)
	}
	enabledBlock := false
	if tcOK {
		for _, key := range []string{"enabled", "enabledConfig"} {
			if _, ok := objectField(tc, key); ok {
				enabledBlock = true
			}
		}
	}
	caps.CanEnable = enabledBlock || len(caps.Efforts) > 0
	caps.Supported = tcOK || rootReasoningAdvertised(cfg)
	caps.DefaultEffort, caps.DefaultDisabled = resolveAdvertisedDefault(scopes, efforts, caps.DefaultDisabled, caps.CanDisable)
	if caps.NonReasoning || caps.Flag == reasoningFlagMalformed {
		// Stale enabled or disabled defaults must never promote or describe an
		// explicitly nonreasoning model, and a malformed flag leaves the model's
		// reasoning-ness unknown, so no automatic default can be trusted either.
		// Omitted and auto intent stay silent.
		caps.DefaultEffort = ""
		caps.DefaultDisabled = false
	}
	return caps
}

// rootReasoningAdvertised reports root-level reasoning metadata a catalog may
// publish outside the thinking_config container. Only parseable values count,
// so a malformed root field never manufactures support.
func rootReasoningAdvertised(cfg map[string]json.RawMessage) bool {
	for _, key := range effortsKeyAliases {
		raw, ok := cfg[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil && len(obj) > 0 {
			return true
		}
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
			return true
		}
	}
	for _, key := range []string{"default_effort", "defaultEffort"} {
		raw, ok := cfg[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return true
		}
	}
	for _, key := range []string{"supports_disabled", "supportsDisabled"} {
		raw, ok := cfg[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return true
		}
	}
	return false
}

// parseReasoningFlag classifies the catalog's is_reasoning member. Absent and
// malformed stay distinct from false: only an explicit false vetoes enabling,
// and a malformed value (any present non-boolean, including null) is never
// overwritten by flag synchronization.
func parseReasoningFlag(cfg map[string]json.RawMessage) reasoningFlag {
	raw, ok := cfg["is_reasoning"]
	if !ok || len(raw) == 0 {
		return reasoningFlagMissing
	}
	// json.Unmarshal leaves the target bool untouched on null, which would
	// misread an explicit null as the false classification.
	if strings.TrimSpace(string(raw)) == "null" {
		return reasoningFlagMalformed
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return reasoningFlagMalformed
	}
	if b {
		return reasoningFlagTrue
	}
	return reasoningFlagFalse
}

// parseAdvertisedEfforts returns the effort enum from the first recognized
// alias found across the enabled block, the thinking_config container, and the
// catalog root. The value may be an object of entries or an array of names; a
// malformed value under the first present alias means no enum rather than a
// guess.
func parseAdvertisedEfforts(scopes []map[string]json.RawMessage) map[string]json.RawMessage {
	for _, scope := range scopes {
		for _, alias := range effortsKeyAliases {
			raw, ok := scope[alias]
			if !ok || len(raw) == 0 {
				continue
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) == nil && obj != nil {
				return obj
			}
			var list []json.RawMessage
			if json.Unmarshal(raw, &list) == nil && list != nil {
				obj := make(map[string]json.RawMessage, len(list))
				for _, item := range list {
					var name string
					if json.Unmarshal(item, &name) == nil && name != "" {
						obj[name] = nil
					}
				}
				return obj
			}
			return nil
		}
	}
	return nil
}

// reasoningScopes lists the containers that may carry the efforts enum and
// default_effort fields: the enabled block aliases, the thinking_config
// container, then the catalog root as the fallback scope.
func reasoningScopes(tc map[string]json.RawMessage, tcOK bool, root map[string]json.RawMessage) []map[string]json.RawMessage {
	var scopes []map[string]json.RawMessage
	if tcOK {
		for _, key := range []string{"enabled", "enabledConfig"} {
			if obj, ok := objectField(tc, key); ok {
				scopes = append(scopes, obj)
			}
		}
		scopes = append(scopes, tc)
	}
	return append(scopes, root)
}

// recognizedEfforts filters the enum to ranked levels in ascending order, so
// selection never depends on JSON key order.
func recognizedEfforts(efforts map[string]json.RawMessage) []string {
	out := make([]string, 0, len(efforts))
	for name := range efforts {
		if _, ranked := effortRank[name]; ranked {
			out = append(out, name)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && effortRank[out[j]] < effortRank[out[j-1]]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// enumAdvertisesDisable reports an effort enum that explicitly lists the
// canonical disabled levels. Publishing none/off as selectable efforts is an
// advertisement of disable support even when no disabled block exists.
func enumAdvertisesDisable(efforts map[string]json.RawMessage) bool {
	_, none := efforts["none"]
	_, off := efforts["off"]
	return none || off
}

// resolveAdvertisedDefault resolves the catalog default effort. Positive
// candidates (default_effort fields and entries marked is_default) must be
// recognized and advertised; a none/off default is usable only when disable
// support is advertised. Conflicting valid candidates are ambiguous and yield
// no default instead of a guess; the disabled state never wins by precedence.
// The same default repeated through aliases is one candidate, not a conflict.
func resolveAdvertisedDefault(scopes []map[string]json.RawMessage, efforts map[string]json.RawMessage, disabledDefault, canDisable bool) (string, bool) {
	positive := map[string]bool{}
	for _, scope := range scopes {
		for _, key := range []string{"default_effort", "defaultEffort"} {
			raw, ok := scope[key]
			if !ok || len(raw) == 0 {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) != nil || s == "" {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(s))
			if name == "none" || name == "off" {
				if canDisable {
					disabledDefault = true
				}
				continue
			}
			if _, ranked := effortRank[name]; !ranked {
				continue
			}
			if _, advertised := efforts[name]; !advertised {
				continue
			}
			positive[name] = true
		}
	}
	for name, entry := range efforts {
		if name == "none" || name == "off" {
			// An advertised none/off entry marked default is a disabled default
			// candidate; listing it as an effort already advertises disable
			// support. It is never a positive candidate.
			if entryDefaultTrue(entry) {
				disabledDefault = true
			}
			continue
		}
		if _, ranked := effortRank[name]; !ranked {
			continue
		}
		if entryDefaultTrue(entry) {
			positive[name] = true
		}
	}
	switch {
	case len(positive) > 1, len(positive) == 1 && disabledDefault:
		return "", false
	case len(positive) == 1:
		for name := range positive {
			return name, false
		}
	case disabledDefault:
		return "", true
	}
	return "", false
}

// selectEffort maps a recognized requested effort onto the advertised enum
// (ascending rank order). Supported efforts pass through; unsupported max and
// ultra fall to the highest advertised level; other unsupported levels take
// the next advertised level at or above the request, else the highest.
func selectEffort(requested string, advertised []string) (string, bool) {
	for _, a := range advertised {
		if a == requested {
			return requested, true
		}
	}
	if len(advertised) == 0 {
		return "", false
	}
	if requested == "max" || requested == "ultra" {
		return advertised[len(advertised)-1], true
	}
	rank := effortRank[requested]
	for _, a := range advertised {
		if effortRank[a] >= rank {
			return a, true
		}
	}
	return advertised[len(advertised)-1], true
}

// resolveReasoning interprets the effective IR intent against the catalog
// advertisement. Numeric budgets are out of scope for this backend and always
// rejected. An intensity level with an independent disable or enable flag
// keeps the level, matching the shared writers' Mode precedence; the toggle is
// honored only when no intensity was stated.
func resolveReasoning(t *ir.Thinking, caps reasoningCaps, modelKey string) (effectiveReasoning, error) {
	if t != nil && t.Mode == ir.ThinkingBudget {
		return effectiveReasoning{}, fmt.Errorf("qoder: numeric thinking budget is not supported by this backend")
	}
	if caps.Flag == reasoningFlagMalformed {
		// A malformed flag leaves the model's reasoning-ness unknown, so no
		// control can be proven supported and no default can be trusted.
		// Omitted and plain auto intent stay silent instead of guessing; every
		// explicit control is a client error before inference.
		if plainAutoIntent(t) {
			return effectiveReasoning{state: reasoningUnset}, nil
		}
		return effectiveReasoning{}, fmt.Errorf("qoder: model %q has a malformed is_reasoning flag in the model catalog; explicit reasoning controls are rejected", modelKey)
	}
	if t == nil {
		return defaultReasoning(caps), nil
	}
	if t.Mode == ir.ThinkingNone {
		// Mode none states the disable directly; a contradictory independent
		// enable flag does not revive it.
		if !caps.CanDisable {
			return effectiveReasoning{}, fmt.Errorf("qoder: model %q does not support disabling reasoning", modelKey)
		}
		return effectiveReasoning{state: reasoningOff}, nil
	}
	lvl := ""
	if t.Mode == ir.ThinkingLevel {
		lvl = strings.ToLower(strings.TrimSpace(t.Level))
	}
	if lvl == "" {
		switch t.Enable {
		case ir.ThinkingEnableDisabled:
			if !caps.CanDisable {
				return effectiveReasoning{}, fmt.Errorf("qoder: model %q does not support disabling reasoning", modelKey)
			}
			return effectiveReasoning{state: reasoningOff}, nil
		case ir.ThinkingEnableEnabled:
			return enableReasoning(caps, modelKey)
		}
		return defaultReasoning(caps), nil
	}
	if _, ranked := effortRank[lvl]; !ranked {
		return effectiveReasoning{}, fmt.Errorf("qoder: unknown reasoning effort %q", lvl)
	}
	if caps.NonReasoning {
		// The veto precedes the generic support check so an explicitly
		// nonreasoning model always reports its own cause.
		return effectiveReasoning{}, fmt.Errorf("qoder: model %q is not advertised as a reasoning model", modelKey)
	}
	if !caps.Supported {
		return effectiveReasoning{}, fmt.Errorf("qoder: model %q does not support reasoning controls", modelKey)
	}
	if len(caps.Efforts) == 0 {
		return effectiveReasoning{}, fmt.Errorf("qoder: model %q does not advertise reasoning effort levels", modelKey)
	}
	effort, ok := selectEffort(lvl, caps.Efforts)
	if !ok {
		return effectiveReasoning{}, fmt.Errorf("qoder: model %q does not advertise reasoning effort levels", modelKey)
	}
	return effectiveReasoning{state: reasoningOn, effort: effort}, nil
}

// plainAutoIntent reports intent that states neither an intensity nor an
// explicit toggle: omitted intent, Mode auto without an independent enable
// flag, and a degenerate level mode with no level. Everything else is an
// explicit control under the malformed-flag policy.
func plainAutoIntent(t *ir.Thinking) bool {
	if t == nil {
		return true
	}
	switch t.Mode {
	case ir.ThinkingAuto, "":
		return t.Enable == ir.ThinkingEnableUnset
	case ir.ThinkingLevel:
		return strings.TrimSpace(t.Level) == "" && t.Enable == ir.ThinkingEnableUnset
	default:
		return false
	}
}

// enableReasoning honors an explicit enable request with no stated intensity.
// An unambiguous advertised positive default is selected; a disabled, absent,
// or ambiguous default still enables the toggle without inventing an effort.
// An explicitly nonreasoning model is never enabled.
func enableReasoning(caps reasoningCaps, modelKey string) (effectiveReasoning, error) {
	if caps.NonReasoning {
		return effectiveReasoning{}, fmt.Errorf("qoder: model %q is not advertised as a reasoning model", modelKey)
	}
	if !caps.CanEnable {
		return effectiveReasoning{}, fmt.Errorf("qoder: model %q does not support enabling reasoning", modelKey)
	}
	if caps.DefaultEffort != "" {
		return effectiveReasoning{state: reasoningOn, effort: caps.DefaultEffort}, nil
	}
	return effectiveReasoning{state: reasoningOn}, nil
}

// defaultReasoning applies the advertised default for omitted or auto intent.
// Without a usable default nothing is written; no default is ever invented.
func defaultReasoning(caps reasoningCaps) effectiveReasoning {
	if caps.DefaultDisabled {
		return effectiveReasoning{state: reasoningOff}
	}
	if caps.DefaultEffort != "" {
		return effectiveReasoning{state: reasoningOn, effort: caps.DefaultEffort}
	}
	return effectiveReasoning{state: reasoningUnset}
}

// applyThinkingControls resolves the intent against the catalog entry and
// writes the resulting native controls into the prepared plaintext.
func applyThinkingControls(body, config []byte, modelKey string, t *ir.Thinking) ([]byte, error) {
	caps := parseReasoningCaps(config)
	eff, err := resolveReasoning(t, caps, modelKey)
	if err != nil {
		return nil, err
	}
	return applyReasoningWire(body, eff, caps.Flag)
}

// applyReasoningWire writes the effective selection in the native shape:
// parameters.reasoning_effort plus parameters.enable_thinking, with the
// is_reasoning flags kept identical in model_config and the mirrored
// chat_context.extra.modelConfig. Unset removes stale reasoning fields
// instead of guessing a default.
func applyReasoningWire(body []byte, eff effectiveReasoning, flag reasoningFlag) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("qoder: apply reasoning: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("qoder: apply reasoning: not a JSON object")
	}
	switch eff.state {
	case reasoningOn, reasoningOff:
		params, _ := objectField(m, "parameters")
		if params == nil {
			params = map[string]json.RawMessage{}
		}
		if eff.state == reasoningOn {
			if eff.effort != "" {
				effort, err := json.Marshal(eff.effort)
				if err != nil {
					return nil, err
				}
				params["reasoning_effort"] = effort
			} else {
				// Toggle-only enabling must drop a stale effort left in the
				// prepared body: enable_thinking:true beside
				// reasoning_effort:"none" is a contradictory native request,
				// and no effort may be invented here.
				delete(params, "reasoning_effort")
			}
			params["enable_thinking"] = json.RawMessage("true")
		} else {
			params["reasoning_effort"] = json.RawMessage(`"none"`)
			params["enable_thinking"] = json.RawMessage("false")
		}
		patched, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		m["parameters"] = patched
		setReasoningFlags(m, eff.state == reasoningOn, flag)
	case reasoningUnset:
		if params, ok := objectField(m, "parameters"); ok {
			delete(params, "reasoning_effort")
			delete(params, "enable_thinking")
			patched, err := json.Marshal(params)
			if err != nil {
				return nil, err
			}
			m["parameters"] = patched
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("qoder: apply reasoning: %w", err)
	}
	return out, nil
}

// setReasoningFlags synchronizes both is_reasoning flags. model_config is the
// raw server-published copy; only its is_reasoning member is rewritten, and
// every other catalog value keeps its exact bytes. A malformed published flag
// is never overwritten in either direction: the reasoning controls themselves
// carry the request intent, and this code refuses to guess the catalog's
// unparsable value. A missing flag follows the effective state.
func setReasoningFlags(m map[string]json.RawMessage, enabled bool, flag reasoningFlag) {
	if flag == reasoningFlagMalformed {
		return
	}
	flagRaw, err := json.Marshal(enabled)
	if err != nil {
		return
	}
	if mc, ok := objectField(m, "model_config"); ok {
		mc["is_reasoning"] = flagRaw
		if patched, err := json.Marshal(mc); err == nil {
			m["model_config"] = patched
		}
	}
	setModelConfigMirrorReasoning(m, enabled)
}

// setModelConfigMirrorReasoning rewrites the is_reasoning member of the
// chat_context.extra.modelConfig mirror when that structure exists. The
// encoder always builds the chain, so production bodies carry it; malformed
// bodies keep their shape.
func setModelConfigMirrorReasoning(m map[string]json.RawMessage, enabled bool) {
	flag, err := json.Marshal(enabled)
	if err != nil {
		return
	}
	cc, ok := objectField(m, "chat_context")
	if !ok {
		return
	}
	extra, ok := objectField(cc, "extra")
	if !ok {
		return
	}
	mc, ok := objectField(extra, "modelConfig")
	if !ok {
		return
	}
	mc["is_reasoning"] = flag
	if b, err := json.Marshal(mc); err == nil {
		extra["modelConfig"] = b
	}
	if b, err := json.Marshal(extra); err == nil {
		cc["extra"] = b
	}
	if b, err := json.Marshal(cc); err == nil {
		m["chat_context"] = b
	}
}

// objectField decodes one object-valued member. Absent, malformed, and null
// values all report false.
func objectField(m map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, ok := m[key]
	if !ok || len(raw) == 0 {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// rawIsNullOrFalse reports a JSON null or false literal. The native parser
// treats every other value of the disabled property as disable support.
func rawIsNullOrFalse(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "null" || s == "false"
}

// flagIsTrue reports whether any named key holds JSON true.
func flagIsTrue(m map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if entryIsTrue(m[key]) {
			return true
		}
	}
	return false
}

func entryIsTrue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

// entryDefaultTrue reports an effort entry whose is_default/isDefault member
// is true. The entry itself is an object; a bare bool or malformed entry
// never marks a default.
func entryDefaultTrue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil || entry == nil {
		return false
	}
	return flagIsTrue(entry, "is_default", "isDefault")
}
