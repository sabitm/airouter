package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// qfmodel thinking_config shape from the verified native catalog.
const canonicalThinkingConfig = `{
	"key":"qfmodel",
	"is_reasoning":true,
	"thinking_config":{
		"disabled":{},
		"enabled":{
			"efforts":{"xhigh":{},"low":{},"medium":{"is_default":true}},
			"is_default":true
		},
		"is_default":true
	}
}`

func mustCaps(t *testing.T, config string) reasoningCaps {
	t.Helper()
	return parseReasoningCaps(json.RawMessage(config))
}

func TestParseReasoningCapsCanonical(t *testing.T) {
	caps := mustCaps(t, canonicalThinkingConfig)
	if !caps.Supported {
		t.Fatal("canonical thinking_config not recognized as supported")
	}
	if !caps.CanDisable {
		t.Fatal("disabled block did not advertise disable support")
	}
	want := []string{"low", "medium", "xhigh"}
	if len(caps.Efforts) != len(want) {
		t.Fatalf("efforts = %v, want %v", caps.Efforts, want)
	}
	for i, e := range want {
		if caps.Efforts[i] != e {
			t.Fatalf("efforts = %v, want %v", caps.Efforts, want)
		}
	}
	if caps.DefaultEffort != "medium" {
		t.Fatalf("default = %q, want medium", caps.DefaultEffort)
	}
	if caps.DefaultDisabled {
		t.Fatal("canonical catalog must not advertise a disabled default")
	}
}

// Capability resolution must not depend on JSON key order.
func TestParseReasoningCapsOrderingIndependent(t *testing.T) {
	reversed := `{
		"key":"qfmodel",
		"is_reasoning":true,
		"thinking_config":{
			"is_default":true,
			"enabled":{
				"is_default":true,
				"efforts":{"medium":{"is_default":true},"low":{},"xhigh":{}}
			},
			"disabled":{}
		}
	}`
	base, rev := mustCaps(t, canonicalThinkingConfig), mustCaps(t, reversed)
	if base.Supported != rev.Supported || base.CanDisable != rev.CanDisable || base.DefaultDisabled != rev.DefaultDisabled {
		t.Fatalf("flag mismatch: %+v vs %+v", base, rev)
	}
	if base.DefaultEffort != rev.DefaultEffort {
		t.Fatalf("default mismatch: %q vs %q", base.DefaultEffort, rev.DefaultEffort)
	}
	if len(base.Efforts) != len(rev.Efforts) {
		t.Fatalf("efforts mismatch: %v vs %v", base.Efforts, rev.Efforts)
	}
	for i := range base.Efforts {
		if base.Efforts[i] != rev.Efforts[i] {
			t.Fatalf("efforts mismatch: %v vs %v", base.Efforts, rev.Efforts)
		}
	}
}

func TestParseReasoningCapsAliases(t *testing.T) {
	configs := []string{
		`{"thinking_config":{"enabled":{"reasoningEfforts":{"low":{},"high":{}}}}}`,
		`{"thinking_config":{"enabled":{"supported_effort_levels":["high","low"]}}}`,
		`{"thinking_config":{"effortLevels":{"low":{},"high":{}}}}`,
		`{"thinkingConfig":{"enabledConfig":{"levels":["low","high"]}}}`,
	}
	for _, config := range configs {
		caps := mustCaps(t, config)
		if !caps.Supported {
			t.Fatalf("not supported: %s", config)
		}
		if len(caps.Efforts) != 2 || caps.Efforts[0] != "low" || caps.Efforts[1] != "high" {
			t.Fatalf("efforts = %v for %s", caps.Efforts, config)
		}
	}
	field := mustCaps(t, `{"thinking_config":{"enabled":{"efforts":{"low":{},"high":{}},"defaultEffort":"high"}}}`)
	if field.DefaultEffort != "high" {
		t.Fatalf("defaultEffort = %q, want high", field.DefaultEffort)
	}
	flag := mustCaps(t, `{"thinking_config":{"supports_disabled":true,"enabled":{"efforts":{"low":{}}}}}`)
	if !flag.CanDisable {
		t.Fatal("supports_disabled did not advertise disable support")
	}
}

func TestParseReasoningCapsDisableEdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   bool
	}{
		{"absent", `{"thinking_config":{"enabled":{"efforts":{"low":{}}}}}`, false},
		{"null block", `{"thinking_config":{"disabled":null,"enabled":{"efforts":{"low":{}}}}}`, false},
		{"false block", `{"thinking_config":{"disabled":false,"enabled":{"efforts":{"low":{}}}}}`, false},
		{"empty block", `{"thinking_config":{"disabled":{},"enabled":{"efforts":{"low":{}}}}}`, true},
		{"true block", `{"thinking_config":{"disabled":true,"enabled":{"efforts":{"low":{}}}}}`, true},
		{"string block", `{"thinking_config":{"disabled":"no","enabled":{"efforts":{"low":{}}}}}`, true},
		{"explicit flag false only", `{"thinking_config":{"supports_disabled":false,"enabled":{"efforts":{"low":{}}}}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := mustCaps(t, tc.config)
			if caps.CanDisable != tc.want {
				t.Fatalf("CanDisable = %v, want %v", caps.CanDisable, tc.want)
			}
		})
	}
}

func TestParseReasoningCapsMalformedMetadata(t *testing.T) {
	cases := []struct {
		name      string
		config    string
		supported bool
	}{
		{"missing container", `{"key":"m","is_reasoning":true}`, false},
		{"null container", `{"key":"m","thinking_config":null}`, false},
		{"string container", `{"key":"m","thinking_config":"yes"}`, false},
		{"malformed root", `not json`, false},
		{"null root", `null`, false},
		{"empty container", `{"thinking_config":{}}`, true},
		{"malformed efforts", `{"thinking_config":{"enabled":{"efforts":"low"}}}`, true},
		{"unknown efforts only", `{"thinking_config":{"enabled":{"efforts":{"superduper":{}}}}}`, true},
		{"malformed is_reasoning flag", `{"thinking_config":{"disabled":{}},"is_reasoning":"yes"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := mustCaps(t, tc.config)
			if caps.Supported != tc.supported {
				t.Fatalf("Supported = %v, want %v (%+v)", caps.Supported, tc.supported, caps)
			}
			if len(caps.Efforts) != 0 {
				t.Fatalf("unexpected efforts %v", caps.Efforts)
			}
		})
	}
}

func TestParseReasoningCapsDefaultResolution(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		def     string
		disable bool
	}{
		{"entry is_default", `{"thinking_config":{"enabled":{"efforts":{"low":{},"high":{"is_default":true}}}}}`, "high", false},
		{"default_effort field", `{"thinking_config":{"enabled":{"efforts":{"low":{},"high":{}},"default_effort":"low"}}}`, "low", false},
		{"field and entry agree", `{"thinking_config":{"enabled":{"efforts":{"low":{"is_default":true},"high":{}},"default_effort":"low"}}}`, "low", false},
		{"field outside enum", `{"thinking_config":{"enabled":{"efforts":{"low":{}},"default_effort":"high"}}}`, "", false},
		{"conflicting entries", `{"thinking_config":{"enabled":{"efforts":{"low":{"is_default":true},"high":{"is_default":true}}}}}`, "", false},
		{"field conflicts with entry", `{"thinking_config":{"enabled":{"efforts":{"low":{"is_default":true},"high":{}},"default_effort":"high"}}}`, "", false},
		{"unknown is_default", `{"thinking_config":{"enabled":{"efforts":{"low":{},"superduper":{"is_default":true}}}}}`, "", false},
		{"unknown default_effort", `{"thinking_config":{"enabled":{"efforts":{"low":{}},"default_effort":"ultra"}}}`, "", false},
		{"no efforts enum", `{"thinking_config":{"enabled":{"default_effort":"high"}}}`, "", false},
		{"disabled default", `{"thinking_config":{"disabled":{"is_default":true},"enabled":{"efforts":{"low":{}}}}}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := mustCaps(t, tc.config)
			if caps.DefaultEffort != tc.def {
				t.Fatalf("DefaultEffort = %q, want %q", caps.DefaultEffort, tc.def)
			}
			if caps.DefaultDisabled != tc.disable {
				t.Fatalf("DefaultDisabled = %v, want %v", caps.DefaultDisabled, tc.disable)
			}
		})
	}
}

func TestParseReasoningCapsRootMetadata(t *testing.T) {
	rootDefault := mustCaps(t, `{"key":"m","is_reasoning":true,"default_effort":"high","thinking_config":{"enabled":{"efforts":{"low":{},"high":{}}}}}`)
	if rootDefault.DefaultEffort != "high" {
		t.Fatalf("root default_effort = %q, want high", rootDefault.DefaultEffort)
	}
	rootCamel := mustCaps(t, `{"key":"m","is_reasoning":true,"defaultEffort":"low","thinking_config":{"enabled":{"efforts":{"low":{},"high":{}}}}}`)
	if rootCamel.DefaultEffort != "low" {
		t.Fatalf("root defaultEffort = %q, want low", rootCamel.DefaultEffort)
	}
	rootDisable := mustCaps(t, `{"key":"m","is_reasoning":true,"supports_disabled":true,"thinking_config":{"enabled":{"efforts":{"low":{}}}}}`)
	if !rootDisable.CanDisable {
		t.Fatal("root supports_disabled did not advertise disable support")
	}
	rootEnum := mustCaps(t, `{"key":"m","is_reasoning":true,"efforts":{"low":{},"high":{"is_default":true}}}`)
	if !rootEnum.Supported || !rootEnum.CanEnable || len(rootEnum.Efforts) != 2 || rootEnum.DefaultEffort != "high" {
		t.Fatalf("root efforts enum parsed incoherently: %+v", rootEnum)
	}
	camelContainer := mustCaps(t, `{"thinkingConfig":{"enabled":{"efforts":{"low":{}}}},"default_effort":"low"}`)
	if camelContainer.DefaultEffort != "low" || !camelContainer.Supported {
		t.Fatalf("thinkingConfig container with root default parsed incoherently: %+v", camelContainer)
	}
	malformed := mustCaps(t, `{"key":"m","is_reasoning":true,"default_effort":7,"thinking_config":{"enabled":{"efforts":{"low":{}}}}}`)
	if malformed.DefaultEffort != "" {
		t.Fatalf("malformed root default coerced: %q", malformed.DefaultEffort)
	}
	flagOnly := mustCaps(t, `{"key":"m","is_reasoning":true}`)
	if flagOnly.Supported {
		t.Fatal("a bare nonreasoning flag must not advertise support")
	}
}

func TestParseReasoningCapsFlagClassification(t *testing.T) {
	cases := []struct {
		name         string
		config       string
		flag         reasoningFlag
		nonReasoning bool
	}{
		{"missing", `{"thinking_config":{"enabled":{}}}`, reasoningFlagMissing, false},
		{"false", `{"is_reasoning":false,"thinking_config":{"enabled":{}}}`, reasoningFlagFalse, true},
		{"true", `{"is_reasoning":true,"thinking_config":{"enabled":{}}}`, reasoningFlagTrue, false},
		{"malformed string", `{"is_reasoning":"yes","thinking_config":{"enabled":{}}}`, reasoningFlagMalformed, false},
		{"number", `{"is_reasoning":0,"thinking_config":{"enabled":{}}}`, reasoningFlagMalformed, false},
		// JSON null is a present member that is not a boolean, so it is
		// malformed like any other published non-boolean. The safe direction
		// (never enable, mirror stays false) is unchanged.
		{"null", `{"is_reasoning":null,"thinking_config":{"enabled":{}}}`, reasoningFlagMalformed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := mustCaps(t, tc.config)
			if caps.Flag != tc.flag || caps.NonReasoning != tc.nonReasoning {
				t.Fatalf("flag=%d nonReasoning=%v, want flag=%d nonReasoning=%v", caps.Flag, caps.NonReasoning, tc.flag, tc.nonReasoning)
			}
		})
	}
}

// An explicitly nonreasoning model keeps its parsed advertisement but loses
// every default: stale enabled metadata must never promote or describe it.
func TestParseReasoningCapsNonReasoningSuppressesDefaults(t *testing.T) {
	non := mustCaps(t, `{"key":"m","is_reasoning":false,"thinking_config":{"enabled":{"efforts":{"high":{"is_default":true}}}}}`)
	if !non.NonReasoning || non.DefaultEffort != "" || non.DefaultDisabled {
		t.Fatalf("stale defaults not suppressed: %+v", non)
	}
	if non.CanDisable {
		t.Fatal("disable support invented for a nonreasoning model")
	}
	if !non.CanEnable || len(non.Efforts) != 1 || non.Efforts[0] != "high" {
		t.Fatalf("advertisement lost: %+v", non)
	}
	reasoning := mustCaps(t, `{"key":"m","is_reasoning":true,"thinking_config":{"enabled":{"efforts":{"high":{"is_default":true}}}}}`)
	if reasoning.DefaultEffort != "high" {
		t.Fatalf("same shape with is_reasoning:true lost its default: %+v", reasoning)
	}
}

// An effort enum that explicitly lists none/off advertises disable support by
// itself, and those entries never become positive effort candidates.
func TestParseReasoningCapsEnumDisableSupport(t *testing.T) {
	withNone := mustCaps(t, `{"key":"m","is_reasoning":true,"efforts":{"none":{},"low":{}}}`)
	if !withNone.CanDisable {
		t.Fatalf("enum none entry did not advertise disable support: %+v", withNone)
	}
	if !withNone.CanEnable || len(withNone.Efforts) != 1 || withNone.Efforts[0] != "low" {
		t.Fatalf("positive efforts wrong: %+v", withNone)
	}
	withOff := mustCaps(t, `{"key":"m","is_reasoning":true,"efforts":{"off":{},"low":{},"medium":{}}}`)
	if !withOff.CanDisable || len(withOff.Efforts) != 2 {
		t.Fatalf("off alias not recognized: %+v", withOff)
	}
	nested := mustCaps(t, `{"key":"m","is_reasoning":true,"thinking_config":{"enabled":{"efforts":{"none":{}}}}}`)
	if !nested.CanDisable || !nested.CanEnable || len(nested.Efforts) != 0 {
		t.Fatalf("nested none entry parsed incoherently: %+v", nested)
	}
	without := mustCaps(t, `{"key":"m","is_reasoning":true,"efforts":{"low":{}}}`)
	if without.CanDisable {
		t.Fatalf("disable support invented without none/off or a disabled block: %+v", without)
	}
	// The disabled levels stay out of the positive enum: a max request remaps
	// to the highest positive effort, never to none/off.
	got, err := resolveReasoning(level("max"), withOff, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got.state != reasoningOn || got.effort != "medium" {
		t.Fatalf("max remapped to %+v, want on/medium", got)
	}
}

func TestParseReasoningCapsDefaultCandidates(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		def     string
		disable bool
	}{
		// A disabled default and a positive default conflict: ambiguous, and the
		// disabled state never wins by precedence.
		{"conflicting disabled and positive", `{"thinking_config":{"disabled":{"is_default":true},"enabled":{"efforts":{"low":{"is_default":true}}}}}`, "", false},
		{"none default with disable support", `{"thinking_config":{"disabled":{},"default_effort":"none","enabled":{"efforts":{"low":{}}}}}`, "", true},
		{"off alias default", `{"thinking_config":{"disabled":{},"default_effort":"off","enabled":{"efforts":{"low":{}}}}}`, "", true},
		{"root none default", `{"default_effort":"none","supports_disabled":true,"thinking_config":{"enabled":{"efforts":{"low":{}}}}}`, "", true},
		// Without advertised disable support a none/off default is dropped, not
		// honored; an otherwise valid positive default still stands.
		{"none default without disable support", `{"thinking_config":{"default_effort":"none","enabled":{"efforts":{"low":{"is_default":true}}}}}`, "low", false},
		{"same default via aliases agrees", `{"thinking_config":{"enabled":{"efforts":{"low":{"is_default":true},"high":{}},"default_effort":"low"},"defaultEffort":"low"}}`, "low", false},
		{"none and positive conflict", `{"thinking_config":{"disabled":{},"enabled":{"efforts":{"low":{"is_default":true}},"default_effort":"none"}}}`, "", false},
		{"enum none is_default", `{"key":"m","is_reasoning":true,"efforts":{"none":{"is_default":true},"low":{}}}`, "", true},
		{"enum off isDefault", `{"key":"m","is_reasoning":true,"efforts":{"off":{"isDefault":true},"low":{}}}`, "", true},
		{"enum none conflicts with positive", `{"key":"m","is_reasoning":true,"efforts":{"none":{"is_default":true},"low":{"is_default":true}}}`, "", false},
		{"enum none without is_default", `{"key":"m","is_reasoning":true,"efforts":{"none":{},"low":{}}}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := mustCaps(t, tc.config)
			if caps.DefaultEffort != tc.def || caps.DefaultDisabled != tc.disable {
				t.Fatalf("default = %q disabled = %v, want %q %v", caps.DefaultEffort, caps.DefaultDisabled, tc.def, tc.disable)
			}
		})
	}
}

func TestSelectEffort(t *testing.T) {
	cases := []struct {
		name       string
		requested  string
		advertised []string
		want       string
	}{
		{"supported passes through", "medium", []string{"low", "medium", "xhigh"}, "medium"},
		{"max remaps to highest", "max", []string{"low", "medium", "xhigh"}, "xhigh"},
		{"max to medium when medium highest", "max", []string{"low", "medium"}, "medium"},
		{"supported max unchanged", "max", []string{"low", "medium", "xhigh", "max"}, "max"},
		{"max kept over existing ultra", "max", []string{"low", "max", "ultra"}, "max"},
		{"high to next at or above", "high", []string{"low", "medium", "xhigh"}, "xhigh"},
		{"high falls to highest", "high", []string{"low", "medium"}, "medium"},
		{"minimal to next at or above", "minimal", []string{"low", "medium"}, "low"},
		{"ultra to highest advertised", "ultra", []string{"high", "xhigh", "max"}, "max"},
		{"ultra supported", "ultra", []string{"high", "ultra"}, "ultra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectEffort(tc.requested, tc.advertised)
			if !ok || got != tc.want {
				t.Fatalf("selectEffort(%q, %v) = %q, %v; want %q", tc.requested, tc.advertised, got, ok, tc.want)
			}
		})
	}
	if _, ok := selectEffort("high", nil); ok {
		t.Fatal("empty enum must not select")
	}
}

func level(l string) *ir.Thinking {
	return &ir.Thinking{Mode: ir.ThinkingLevel, Level: l}
}

func TestResolveReasoning(t *testing.T) {
	full := mustCaps(t, canonicalThinkingConfig)
	toggleOnly := mustCaps(t, `{"thinking_config":{"disabled":{},"enabled":{}}}`)
	none := reasoningCaps{}
	disabledDefault := mustCaps(t, `{"thinking_config":{"disabled":{"is_default":true},"enabled":{"efforts":{"low":{}}}}}`)

	cases := []struct {
		name      string
		intent    *ir.Thinking
		caps      reasoningCaps
		state     reasoningState
		effort    string
		wantError string
	}{
		{"nil uses default", nil, full, reasoningOn, "medium", ""},
		{"auto uses default", &ir.Thinking{Mode: ir.ThinkingAuto}, full, reasoningOn, "medium", ""},
		{"nil without default is unset", nil, mustCaps(t, `{"thinking_config":{"enabled":{"efforts":{"low":{}}}}}`), reasoningUnset, "", ""},
		{"auto without default is unset", &ir.Thinking{Mode: ir.ThinkingAuto}, toggleOnly, reasoningUnset, "", ""},
		{"disabled default applies to nil", nil, disabledDefault, reasoningOff, "", ""},
		{"disabled default applies to auto", &ir.Thinking{Mode: ir.ThinkingAuto}, disabledDefault, reasoningOff, "", ""},
		{"none disables", &ir.Thinking{Mode: ir.ThinkingNone}, full, reasoningOff, "", ""},
		{"off disables", &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableDisabled}, full, reasoningOff, "", ""},
		{"none on toggle-only disables", &ir.Thinking{Mode: ir.ThinkingNone}, toggleOnly, reasoningOff, "", ""},
		{"none without disable support errors", &ir.Thinking{Mode: ir.ThinkingNone}, mustCaps(t, `{"thinking_config":{"enabled":{"efforts":{"low":{}}}}}`), 0, "", "does not support disabling"},
		{"none without catalog errors", &ir.Thinking{Mode: ir.ThinkingNone}, none, 0, "", "does not support disabling"},
		{"auto with disable flag disables", &ir.Thinking{Mode: ir.ThinkingAuto, Enable: ir.ThinkingEnableDisabled}, full, reasoningOff, "", ""},
		{"supported level passes", level("xhigh"), full, reasoningOn, "xhigh", ""},
		{"max remaps", level("max"), full, reasoningOn, "xhigh", ""},
		{"minimal remaps", level("minimal"), full, reasoningOn, "low", ""},
		{"high remaps to next at or above", level("high"), full, reasoningOn, "xhigh", ""},
		{"level case insensitive", level("XHigh"), full, reasoningOn, "xhigh", ""},
		{"unknown level errors", level("super"), full, 0, "", "unknown reasoning effort"},
		{"level on toggle-only errors", level("high"), toggleOnly, 0, "", "does not advertise"},
		{"level without catalog errors", level("high"), none, 0, "", "does not support reasoning"},
		{"budget rejected", &ir.Thinking{Mode: ir.ThinkingBudget, Budget: 1024}, full, 0, "", "budget"},
		{"budget rejected without catalog", &ir.Thinking{Mode: ir.ThinkingBudget, Budget: 1}, none, 0, "", "budget"},
		// Intensity wins over an independent disable flag, matching the shared
		// writers' Mode precedence.
		{"level with disable flag keeps level", &ir.Thinking{Mode: ir.ThinkingLevel, Level: "xhigh", Enable: ir.ThinkingEnableDisabled}, full, reasoningOn, "xhigh", ""},
		{"empty level falls to default", &ir.Thinking{Mode: ir.ThinkingLevel}, full, reasoningOn, "medium", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveReasoning(tc.intent, tc.caps, "qfmodel")
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got %+v", tc.wantError, got)
				}
				if !contains(err.Error(), tc.wantError) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.state != tc.state || got.effort != tc.effort {
				t.Fatalf("got %+v, want state %d effort %q", got, tc.state, tc.effort)
			}
		})
	}
}

// Enable and toggle semantics: an explicit enable with no stated intensity
// selects an unambiguous positive default, enables the toggle alone when the
// default is disabled, absent, or ambiguous, and is rejected when the catalog
// does not advertise enabling.
func TestResolveReasoningEnable(t *testing.T) {
	full := mustCaps(t, canonicalThinkingConfig)
	toggleOnly := mustCaps(t, `{"thinking_config":{"disabled":{},"enabled":{}}}`)
	disabledDefault := mustCaps(t, `{"thinking_config":{"disabled":{"is_default":true},"enabled":{"efforts":{"low":{}}}}}`)
	ambiguous := mustCaps(t, `{"thinking_config":{"disabled":{"is_default":true},"enabled":{"efforts":{"low":{"is_default":true}}}}}`)
	disabledOnly := mustCaps(t, `{"thinking_config":{"disabled":{}}}`)
	nonReasoning := mustCaps(t, `{"is_reasoning":false,"thinking_config":{"enabled":{"efforts":{"high":{"is_default":true}}}}}`)
	enabled := &ir.Thinking{Mode: ir.ThinkingAuto, Enable: ir.ThinkingEnableEnabled}

	cases := []struct {
		name      string
		intent    *ir.Thinking
		caps      reasoningCaps
		state     reasoningState
		effort    string
		wantError string
	}{
		{"enable on toggle-only", enabled, toggleOnly, reasoningOn, "", ""},
		{"enable with disabled default", enabled, disabledDefault, reasoningOn, "", ""},
		{"enable with positive default", enabled, full, reasoningOn, "medium", ""},
		{"enable with ambiguous default", enabled, ambiguous, reasoningOn, "", ""},
		{"enable on nonreasoning errors", enabled, nonReasoning, 0, "", "not advertised as a reasoning model"},
		{"enable without capability errors", enabled, reasoningCaps{}, 0, "", "does not support enabling"},
		{"enable on disabled-only errors", enabled, disabledOnly, 0, "", "does not support enabling"},
		{"unset mode with enable flag", &ir.Thinking{Enable: ir.ThinkingEnableEnabled}, toggleOnly, reasoningOn, "", ""},
		// Mode none states the disable directly; the contradictory enable flag
		// does not revive it.
		{"mode none beats enable flag", &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableEnabled}, full, reasoningOff, "", ""},
		// Intensity keeps precedence over both toggle directions.
		{"level with enable flag keeps level", &ir.Thinking{Mode: ir.ThinkingLevel, Level: "xhigh", Enable: ir.ThinkingEnableEnabled}, full, reasoningOn, "xhigh", ""},
		{"empty level with enable flag", &ir.Thinking{Mode: ir.ThinkingLevel, Enable: ir.ThinkingEnableEnabled}, full, reasoningOn, "medium", ""},
		// BudgetSet alone after a suffix replacement is not a budget rejection.
		{"budget set alone with level", &ir.Thinking{Mode: ir.ThinkingLevel, Level: "low", BudgetSet: true}, full, reasoningOn, "low", ""},
		{"budget set alone with auto", &ir.Thinking{Mode: ir.ThinkingAuto, BudgetSet: true}, full, reasoningOn, "medium", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveReasoning(tc.intent, tc.caps, "qfmodel")
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got %+v", tc.wantError, got)
				}
				if !contains(err.Error(), tc.wantError) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.state != tc.state || got.effort != tc.effort {
				t.Fatalf("got %+v, want state %d effort %q", got, tc.state, tc.effort)
			}
		})
	}
}

// A malformed catalog is_reasoning flag must never produce a contradictory
// native request: plain omitted or auto intent stays silent, and every
// explicit control is rejected before inference. JSON null counts as
// malformed. Numeric budgets stay rejected ahead of the flag policy.
func TestResolveReasoningMalformedFlag(t *testing.T) {
	malformed := mustCaps(t, `{"key":"m","is_reasoning":"yes","thinking_config":{"enabled":{"efforts":{"low":{"is_default":true}}}}}`)
	if malformed.DefaultEffort != "" || malformed.DefaultDisabled {
		t.Fatalf("malformed flag kept an automatic default: %+v", malformed)
	}
	nullFlag := mustCaps(t, `{"key":"m","is_reasoning":null,"thinking_config":{"enabled":{"efforts":{"low":{"is_default":true}}}}}`)
	if nullFlag.Flag != reasoningFlagMalformed || nullFlag.DefaultEffort != "" {
		t.Fatalf("null flag not treated as malformed: %+v", nullFlag)
	}
	enabled := &ir.Thinking{Mode: ir.ThinkingAuto, Enable: ir.ThinkingEnableEnabled}
	disabled := &ir.Thinking{Mode: ir.ThinkingAuto, Enable: ir.ThinkingEnableDisabled}
	cases := []struct {
		name      string
		intent    *ir.Thinking
		caps      reasoningCaps
		state     reasoningState
		wantError string
	}{
		{"nil stays silent", nil, malformed, reasoningUnset, ""},
		{"auto stays silent", &ir.Thinking{Mode: ir.ThinkingAuto}, malformed, reasoningUnset, ""},
		{"empty level stays silent", &ir.Thinking{Mode: ir.ThinkingLevel}, malformed, reasoningUnset, ""},
		{"null flag nil stays silent", nil, nullFlag, reasoningUnset, ""},
		{"null flag auto stays silent", &ir.Thinking{Mode: ir.ThinkingAuto}, nullFlag, reasoningUnset, ""},
		{"explicit enable rejected", enabled, malformed, 0, "malformed"},
		{"explicit disable rejected", disabled, malformed, 0, "malformed"},
		{"mode none rejected", &ir.Thinking{Mode: ir.ThinkingNone}, malformed, 0, "malformed"},
		{"suffix none rejected", &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableDisabled}, malformed, 0, "malformed"},
		{"effort rejected", level("low"), malformed, 0, "malformed"},
		{"null flag effort rejected", level("low"), nullFlag, 0, "malformed"},
		{"budget still rejected first", &ir.Thinking{Mode: ir.ThinkingBudget, Budget: 8}, malformed, 0, "budget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveReasoning(tc.intent, tc.caps, "qfmodel")
			if tc.wantError != "" {
				if err == nil || !contains(err.Error(), tc.wantError) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.state != tc.state || got.effort != "" {
				t.Fatalf("got %+v, want state %d with no effort", got, tc.state)
			}
		})
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// qoderReasoningBody is a plaintext envelope with the mirrored structures the
// encoder writes, plus an extra catalog value with a large exact number.
const qoderReasoningBody = `{
	"request_id":"r1",
	"parameters":{"max_tokens":4096},
	"chat_context":{"chatPrompt":"","extra":{"modelConfig":{"key":"qfmodel","is_reasoning":true},"context":[]},"text":"hi"},
	"model_config":{"key":"qfmodel","is_reasoning":true,"max_output_tokens":32768,"context_window":9007199254740993}
}`

func reasoningParams(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m struct {
		Parameters map[string]json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode parameters: %v", err)
	}
	return m.Parameters
}

func reasoningFlags(t *testing.T, body []byte) (top, mirror bool) {
	t.Helper()
	var m struct {
		ModelConfig struct {
			IsReasoning bool `json:"is_reasoning"`
		} `json:"model_config"`
		ChatContext struct {
			Extra struct {
				ModelConfig struct {
					IsReasoning bool `json:"is_reasoning"`
				} `json:"modelConfig"`
			} `json:"extra"`
		} `json:"chat_context"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	return m.ModelConfig.IsReasoning, m.ChatContext.Extra.ModelConfig.IsReasoning
}

func TestApplyReasoningWireOn(t *testing.T) {
	out, err := applyReasoningWire([]byte(qoderReasoningBody), effectiveReasoning{state: reasoningOn, effort: "xhigh"}, reasoningFlagTrue)
	if err != nil {
		t.Fatal(err)
	}
	params := reasoningParams(t, out)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("reasoning_effort = %s", params["reasoning_effort"])
	}
	if string(params["enable_thinking"]) != "true" {
		t.Fatalf("enable_thinking = %s", params["enable_thinking"])
	}
	if string(params["max_tokens"]) != "4096" {
		t.Fatalf("max_tokens changed: %s", params["max_tokens"])
	}
	top, mirror := reasoningFlags(t, out)
	if !top || !mirror {
		t.Fatalf("flags not both true: top=%v mirror=%v", top, mirror)
	}
	// Exact numeric catalog values survive without a float round trip.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if !contains(string(raw["model_config"]), "9007199254740993") {
		t.Fatalf("large catalog number altered: %s", raw["model_config"])
	}
	var mc map[string]json.RawMessage
	if err := json.Unmarshal(raw["model_config"], &mc); err != nil {
		t.Fatal(err)
	}
	if string(mc["max_output_tokens"]) != "32768" || string(mc["key"]) != `"qfmodel"` {
		t.Fatalf("other catalog fields not preserved: %s", raw["model_config"])
	}
}

func TestApplyReasoningWireOff(t *testing.T) {
	out, err := applyReasoningWire([]byte(qoderReasoningBody), effectiveReasoning{state: reasoningOff}, reasoningFlagTrue)
	if err != nil {
		t.Fatal(err)
	}
	params := reasoningParams(t, out)
	if string(params["reasoning_effort"]) != `"none"` {
		t.Fatalf("reasoning_effort = %s", params["reasoning_effort"])
	}
	if string(params["enable_thinking"]) != "false" {
		t.Fatalf("enable_thinking = %s", params["enable_thinking"])
	}
	top, mirror := reasoningFlags(t, out)
	if top || mirror {
		t.Fatalf("flags not both false: top=%v mirror=%v", top, mirror)
	}
}

func TestApplyReasoningWireUnset(t *testing.T) {
	withStale := `{"parameters":{"max_tokens":4096,"reasoning_effort":"high","enable_thinking":true},"model_config":{"is_reasoning":true},"chat_context":{"extra":{"modelConfig":{"is_reasoning":true}}}}`
	out, err := applyReasoningWire([]byte(withStale), effectiveReasoning{state: reasoningUnset}, reasoningFlagTrue)
	if err != nil {
		t.Fatal(err)
	}
	params := reasoningParams(t, out)
	if _, ok := params["reasoning_effort"]; ok {
		t.Fatal("unset left reasoning_effort behind")
	}
	if _, ok := params["enable_thinking"]; ok {
		t.Fatal("unset left enable_thinking behind")
	}
	if string(params["max_tokens"]) != "4096" {
		t.Fatalf("max_tokens changed: %s", params["max_tokens"])
	}
	// Unset leaves the catalog-driven flags alone.
	top, mirror := reasoningFlags(t, out)
	if !top || !mirror {
		t.Fatalf("unset changed catalog flags: top=%v mirror=%v", top, mirror)
	}
	noParams := `{"model_config":{"is_reasoning":true}}`
	out2, err := applyReasoningWire([]byte(noParams), effectiveReasoning{state: reasoningUnset}, reasoningFlagTrue)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(out2), "parameters") {
		t.Fatalf("unset created a parameters object: %s", out2)
	}
}

// An enabled selection without a resolved effort writes the toggle alone; an
// empty string is never sent as reasoning_effort.
func TestApplyReasoningWireOnWithoutEffort(t *testing.T) {
	out, err := applyReasoningWire([]byte(qoderReasoningBody), effectiveReasoning{state: reasoningOn}, reasoningFlagTrue)
	if err != nil {
		t.Fatal(err)
	}
	params := reasoningParams(t, out)
	if _, ok := params["reasoning_effort"]; ok {
		t.Fatalf("toggle-only enable invented reasoning_effort: %v", params)
	}
	if string(params["enable_thinking"]) != "true" {
		t.Fatalf("enable_thinking = %s", params["enable_thinking"])
	}
	top, mirror := reasoningFlags(t, out)
	if !top || !mirror {
		t.Fatalf("flags not both true: top=%v mirror=%v", top, mirror)
	}
}

// A malformed published is_reasoning flag is never overwritten in either
// direction; a missing flag follows the effective state.
func TestApplyReasoningWireFlagPolicy(t *testing.T) {
	malformed := `{"parameters":{},"model_config":{"is_reasoning":"yes"},"chat_context":{"extra":{"modelConfig":{"is_reasoning":false}}}}`
	out, err := applyReasoningWire([]byte(malformed), effectiveReasoning{state: reasoningOn, effort: "low"}, reasoningFlagMalformed)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		ModelConfig map[string]json.RawMessage `json:"model_config"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw.ModelConfig["is_reasoning"]) != `"yes"` {
		t.Fatalf("malformed flag was coerced: %s", raw.ModelConfig["is_reasoning"])
	}
	var mirrorRaw struct {
		ChatContext struct {
			Extra struct {
				ModelConfig map[string]json.RawMessage `json:"modelConfig"`
			} `json:"extra"`
		} `json:"chat_context"`
	}
	if err := json.Unmarshal(out, &mirrorRaw); err != nil {
		t.Fatal(err)
	}
	if string(mirrorRaw.ChatContext.Extra.ModelConfig["is_reasoning"]) != "false" {
		t.Fatalf("malformed flag leaked into the mirror: %s", mirrorRaw.ChatContext.Extra.ModelConfig["is_reasoning"])
	}
	if string(reasoningParams(t, out)["reasoning_effort"]) != `"low"` {
		t.Fatal("controls were not written")
	}

	missing := `{"parameters":{},"model_config":{"key":"m"},"chat_context":{"extra":{"modelConfig":{"key":"m"}}}}`
	out, err = applyReasoningWire([]byte(missing), effectiveReasoning{state: reasoningOn, effort: "low"}, reasoningFlagMissing)
	if err != nil {
		t.Fatal(err)
	}
	top, mirror := reasoningFlags(t, out)
	if !top || !mirror {
		t.Fatalf("missing flag did not follow the enabled state: top=%v mirror=%v", top, mirror)
	}
	out, err = applyReasoningWire([]byte(missing), effectiveReasoning{state: reasoningOff}, reasoningFlagMissing)
	if err != nil {
		t.Fatal(err)
	}
	top, mirror = reasoningFlags(t, out)
	if top || mirror {
		t.Fatalf("missing flag did not follow the disabled state: top=%v mirror=%v", top, mirror)
	}
}

func TestApplyThinkingControlsErrors(t *testing.T) {
	cfg := []byte(canonicalThinkingConfig)
	if _, err := applyThinkingControls([]byte(qoderReasoningBody), cfg, "qfmodel", &ir.Thinking{Mode: ir.ThinkingBudget, Budget: 1024}); err == nil {
		t.Fatal("budget not rejected")
	}
	if _, err := applyThinkingControls([]byte(qoderReasoningBody), cfg, "qfmodel", level("super")); err == nil {
		t.Fatal("unknown effort not rejected")
	}
}

// Toggle-only enabling must remove a stale reasoning_effort from the prepared
// body instead of leaving a contradictory "none" beside enable_thinking:true.
// Unrelated parameters keep their exact values, and the fix is idempotent.
func TestApplyReasoningWireToggleOnlyRemovesStaleEffort(t *testing.T) {
	body := []byte(`{"parameters":{"max_tokens":128,"reasoning_effort":"none","unknown_number":9050000000000000001},"model_config":{"is_reasoning":true},"chat_context":{"extra":{"modelConfig":{"is_reasoning":true}}}}`)
	intent := &ir.Thinking{Mode: ir.ThinkingAuto, Enable: ir.ThinkingEnableEnabled}
	catalog := []byte(`{"key":"m","is_reasoning":true,"thinking_config":{"disabled":{"is_default":true},"enabled":{}}}`)
	out, err := applyThinkingControls(body, catalog, "m", intent)
	if err != nil {
		t.Fatal(err)
	}
	params := reasoningParams(t, out)
	if _, ok := params["reasoning_effort"]; ok {
		t.Fatalf("stale reasoning_effort survived toggle-only enable: %v", params)
	}
	if string(params["enable_thinking"]) != "true" {
		t.Fatalf("enable_thinking = %s", params["enable_thinking"])
	}
	if string(params["max_tokens"]) != "128" || string(params["unknown_number"]) != "9050000000000000001" {
		t.Fatalf("unrelated parameters altered: %v", params)
	}
	top, mirror := reasoningFlags(t, out)
	if !top || !mirror {
		t.Fatalf("flags: top=%v mirror=%v", top, mirror)
	}
	again, err := applyThinkingControls(out, catalog, "m", intent)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Fatalf("toggle-only enable is not idempotent:\n%s\n%s", out, again)
	}
}

// Probe D through the full prepare path: a body that already carries a stale
// reasoning_effort must come out clean when a toggle-only catalog enables.
func TestPrepareToggleEnableRemovesStaleEffort(t *testing.T) {
	p := qoderTestProvider("reasoning-stale-user", "tok")
	t.Cleanup(func() { ClearCatalogForTest() })
	SeedCatalogForTest(p, map[string]json.RawMessage{
		"m1": json.RawMessage(`{"key":"m1","is_reasoning":true,"thinking_config":{"disabled":{"is_default":true},"enabled":{}}}`),
	}, []string{"m1"})
	var env map[string]json.RawMessage
	if err := json.Unmarshal(mustEncodeQoderRequest(t, "qoder/m1"), &env); err != nil {
		t.Fatal(err)
	}
	env["parameters"] = json.RawMessage(`{"max_tokens":128,"reasoning_effort":"none"}`)
	patched, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := PrepareWireBodyWithThinking(t.Context(), p, 128, &ir.Thinking{Mode: ir.ThinkingAuto, Enable: ir.ThinkingEnableEnabled}, patched)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := DecodeWireBody(prep.Wire)
	if err != nil {
		t.Fatal(err)
	}
	params := reasoningParams(t, plain)
	if _, ok := params["reasoning_effort"]; ok {
		t.Fatalf("stale reasoning_effort survived prepare: %v", params)
	}
	if string(params["enable_thinking"]) != "true" || string(params["max_tokens"]) != "128" {
		t.Fatalf("controls: %v", params)
	}
	top, mirror := reasoningFlags(t, plain)
	if !top || !mirror {
		t.Fatalf("flags: top=%v mirror=%v", top, mirror)
	}
}

func TestRequestedReasoningIdentity(t *testing.T) {
	cases := []struct {
		name string
		in   *ir.Thinking
		want recordReasoning
	}{
		{"nil", nil, recordReasoning{}},
		{"auto", &ir.Thinking{Mode: ir.ThinkingAuto}, recordReasoning{}},
		{"none", &ir.Thinking{Mode: ir.ThinkingNone}, recordReasoning{State: "off"}},
		{"level", level("high"), recordReasoning{State: "on", Effort: "high"}},
		{"budget", &ir.Thinking{Mode: ir.ThinkingBudget, Budget: 8192}, recordReasoning{}},
		{"empty level", &ir.Thinking{Mode: ir.ThinkingLevel}, recordReasoning{}},
	}
	for _, tc := range cases {
		if got := requestedReasoningIdentity(tc.in); got != tc.want {
			t.Fatalf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestReasoningIdentityFromParams(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   recordReasoning
	}{
		{"no controls", `{"max_tokens":1}`, recordReasoning{}},
		{"enabled effort", `{"reasoning_effort":"high","enable_thinking":true}`, recordReasoning{State: "on", Effort: "high"}},
		{"disabled", `{"reasoning_effort":"none","enable_thinking":false}`, recordReasoning{State: "off"}},
		{"enable false alone", `{"enable_thinking":false}`, recordReasoning{State: "off"}},
		{"none effort alone", `{"reasoning_effort":"none"}`, recordReasoning{State: "off"}},
		{"enable true without effort", `{"enable_thinking":true}`, recordReasoning{State: "on"}},
		{"malformed flags ignored", `{"reasoning_effort":7,"enable_thinking":"yes"}`, recordReasoning{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var params map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.params), &params); err != nil {
				t.Fatal(err)
			}
			if got := reasoningIdentityFromParams(params); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Equal effective settings must produce equal prepared identities even when
// the requested intent differed; different effective settings must not.
func TestPrepareIdentityEqualEffectiveReasoning(t *testing.T) {
	p := qoderTestProvider("reasoning-identity-user", "tok")
	t.Cleanup(func() { ClearCatalogForTest() })
	SeedCatalogForTest(p, map[string]json.RawMessage{
		"m1": json.RawMessage(canonicalThinkingConfig),
	}, []string{"m1"})

	xhigh := &ir.Thinking{Mode: ir.ThinkingLevel, Level: "xhigh"}
	max := &ir.Thinking{Mode: ir.ThinkingLevel, Level: "max"}
	none := &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableDisabled}

	a, err := PrepareWireBodyWithThinking(t.Context(), p, 0, xhigh, mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := PrepareWireBodyWithThinking(t.Context(), p, 0, max, mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestSetID != b.RequestSetID || a.RequestID == b.RequestID {
		t.Fatalf("max and explicit xhigh must share the set id %q/%q and differ in request id", a.RequestSetID, b.RequestSetID)
	}
	c, err := PrepareWireBodyWithThinking(t.Context(), p, 0, level("low"), mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestSetID == a.RequestSetID {
		t.Fatal("different effective effort must change the set id")
	}
	// Wire bytes reflect the remapped selection.
	plain, err := DecodeWireBody(b.Wire)
	if err != nil {
		t.Fatal(err)
	}
	params := reasoningParams(t, plain)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("max remap not applied: %s", params["reasoning_effort"])
	}
	// Default (nil) equals explicit medium; both differ from unset.
	def, err := PrepareWireBodyWithThinking(t.Context(), p, 0, nil, mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	med, err := PrepareWireBodyWithThinking(t.Context(), p, 0, level("medium"), mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	if def.RequestSetID != med.RequestSetID {
		t.Fatal("nil and explicit default effort must share the set id")
	}
	if def.RequestSetID == c.RequestSetID {
		t.Fatal("default effort must differ from other efforts")
	}
	// Disabled intent differs from every enabled identity.
	off, err := PrepareWireBodyWithThinking(t.Context(), p, 0, none, mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	if off.RequestSetID == def.RequestSetID {
		t.Fatal("disable must differ from enabled identities")
	}
	plainOff, err := DecodeWireBody(off.Wire)
	if err != nil {
		t.Fatal(err)
	}
	offParams := reasoningParams(t, plainOff)
	if string(offParams["reasoning_effort"]) != `"none"` || string(offParams["enable_thinking"]) != "false" {
		t.Fatalf("disable wire controls wrong: %v", offParams)
	}
	top, mirror := reasoningFlags(t, plainOff)
	if top || mirror {
		t.Fatalf("disable did not clear both flags: top=%v mirror=%v", top, mirror)
	}
}

// A catalog without a usable default must not introduce reasoning fields for
// omitted intent, and the identity stays equal to the encoder placeholder.
func TestPrepareNilIntentWithoutDefault(t *testing.T) {
	p := qoderTestProvider("reasoning-nodefault-user", "tok")
	t.Cleanup(func() { ClearCatalogForTest() })
	SeedCatalogForTest(p, map[string]json.RawMessage{
		"m1": json.RawMessage(`{"key":"m1","is_reasoning":true,"thinking_config":{"enabled":{"efforts":{"low":{},"high":{}}}}}`),
	}, []string{"m1"})
	prep, err := PrepareWireBodyWithThinking(t.Context(), p, 0, nil, mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := DecodeWireBody(prep.Wire)
	if err != nil {
		t.Fatal(err)
	}
	params := reasoningParams(t, plain)
	if _, ok := params["reasoning_effort"]; ok {
		t.Fatalf("no-default catalog invented reasoning_effort: %v", params)
	}
	if _, ok := params["enable_thinking"]; ok {
		t.Fatalf("no-default catalog invented enable_thinking: %v", params)
	}
	// The mirror follows the catalog is_reasoning flag.
	_, mirror := reasoningFlags(t, plain)
	if !mirror {
		t.Fatal("mirror lost the catalog is_reasoning flag")
	}
}

// Unknown catalog levels must never be sent: an unsupported explicit level
// remaps to the highest recognized advertised effort, and unknown levels are
// invisible to both selection and default resolution.
func TestPrepareUnknownCatalogLevelNeverSent(t *testing.T) {
	p := qoderTestProvider("reasoning-unknown-user", "tok")
	t.Cleanup(func() { ClearCatalogForTest() })
	SeedCatalogForTest(p, map[string]json.RawMessage{
		"m1": json.RawMessage(`{"key":"m1","is_reasoning":true,"thinking_config":{"enabled":{"efforts":{"superduper":{"is_default":true},"medium":{}}}}}`),
	}, []string{"m1"})
	prep, err := PrepareWireBodyWithThinking(t.Context(), p, 0, level("max"), mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := DecodeWireBody(prep.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(reasoningParams(t, plain)["reasoning_effort"]); got != `"medium"` {
		t.Fatalf("reasoning_effort = %s, want medium", got)
	}
	// Omitted intent has no usable default (superduper is unranked), so no
	// fields are written.
	quiet, err := PrepareWireBodyWithThinking(t.Context(), p, 0, nil, mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	plainQuiet, err := DecodeWireBody(quiet.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reasoningParams(t, plainQuiet)["reasoning_effort"]; ok {
		t.Fatal("unknown default level was sent")
	}
}
