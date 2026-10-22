package claudecode

import "airouter/internal/proxy/thinking"

// modelCaps is one row of the 2.1.303 first-party catalog. Empty capability
// lists are empty. They are not inferred from the family name.
type modelCaps struct {
	Adaptive        bool
	Effort          bool
	MaxEffort       bool
	XHigh           bool
	RejectsDisabled bool
	ContextMgmt     bool
	FastMode        bool
	OneMillion      bool
	MaxOutput       int
}

// catalog is the pinned 2.1.303 capability table. Dated provider ids are
// normalized to the catalog id before lookup. An unknown id has no row.
// OneMillion follows context.supports_1m_beta in the pinned 2.1.303 catalog.
// A suffix-only row is not enough. claude-haiku-4-5 has supports_1m_suffix and
// does not have supports_1m_beta, so it is not a long-context beta model.
var catalog = map[string]modelCaps{
	"claude-3-5-haiku":  {MaxOutput: 8192},
	"claude-3-5-sonnet": {MaxOutput: 8192},
	"claude-3-7-sonnet": {MaxOutput: 64000},
	"claude-haiku-4-5":  {ContextMgmt: true, MaxOutput: 64000},
	"claude-haiku-5-5":  {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, RejectsDisabled: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
	"claude-sonnet-4-0": {ContextMgmt: true, OneMillion: true, MaxOutput: 64000},
	"claude-sonnet-4-5": {ContextMgmt: true, OneMillion: true, MaxOutput: 64000},
	"claude-sonnet-4-6": {Adaptive: true, Effort: true, MaxEffort: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
	"claude-sonnet-5":   {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
	"claude-sonnet-5-5": {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, RejectsDisabled: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
	"claude-opus-4-0":   {ContextMgmt: true, MaxOutput: 32000},
	"claude-opus-4-1":   {ContextMgmt: true, MaxOutput: 32000},
	"claude-opus-4-5":   {ContextMgmt: true, MaxOutput: 64000},
	"claude-opus-4-6":   {Adaptive: true, Effort: true, MaxEffort: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
	"claude-opus-4-7":   {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
	"claude-opus-4-8":   {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, ContextMgmt: true, FastMode: true, OneMillion: true, MaxOutput: 128000},
	"claude-opus-5":     {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, ContextMgmt: true, FastMode: true, OneMillion: true, MaxOutput: 128000},
	"claude-opus-5-5":   {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, RejectsDisabled: true, ContextMgmt: true, FastMode: true, OneMillion: true, MaxOutput: 128000},
	"claude-fable-5":    {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, RejectsDisabled: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
	"claude-fable-5-1":  {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, RejectsDisabled: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
	"claude-mythos-5":   {OneMillion: true, MaxOutput: 128000},
	"claude-mythos-5-1": {Adaptive: true, Effort: true, MaxEffort: true, XHigh: true, RejectsDisabled: true, ContextMgmt: true, OneMillion: true, MaxOutput: 128000},
}

// LookupModel returns the catalog row for a model id. ok is false when the
// id is not in the pinned table. A false row is not a guess.
func LookupModel(model string) (modelCaps, bool) {
	row, ok := catalog[thinking.ClaudeCodeCapabilityID(model)]
	return row, ok
}
