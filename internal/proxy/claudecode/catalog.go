package claudecode

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

// datedCatalog maps a first-party dated id onto the catalog id.
var datedCatalog = map[string]string{
	"claude-haiku-4-5-20251001": "claude-haiku-4-5",
}

// LookupModel returns the catalog row for a model id. ok is false when the
// id is not in the pinned table. A false row is not a guess.
func LookupModel(model string) (modelCaps, bool) {
	id := canonicalModel(model)
	if mapped, ok := datedCatalog[id]; ok {
		id = mapped
	}
	// A trailing date suffix on a known family still maps when the undated id
	// is in the catalog. This covers first-party ids such as
	// claude-haiku-4-5-20251001 without treating an unknown name as that family.
	if _, ok := catalog[id]; !ok {
		if i := stringsLastDate(id); i > 0 {
			if _, ok := catalog[id[:i]]; ok {
				id = id[:i]
			}
		}
	}
	row, ok := catalog[id]
	return row, ok
}

func stringsLastDate(id string) int {
	// claude-haiku-4-5-20251001 -> cut before -20251001
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '-' && i+9 == len(id) && allDigits(id[i+1:]) {
			return i
		}
	}
	return -1
}

func allDigits(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
