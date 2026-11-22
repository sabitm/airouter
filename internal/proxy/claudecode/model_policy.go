package claudecode

import "airouter/internal/proxy/thinking"

// Native context windows are separate from supports_1m_beta in the raw
// catalog. A selector does not need a beta on these pinned 2.1.318 models.
func modelHasNativeOneMillion(model string) bool {
	switch canonicalModel(model) {
	case "claude-haiku-5-5", "claude-sonnet-5", "claude-sonnet-5-5",
		"claude-opus-4-7", "claude-opus-4-8", "claude-opus-5", "claude-opus-5-5",
		"claude-fable-5", "claude-fable-5-1", "claude-mythos-5", "claude-mythos-5-1":
		return true
	default:
		return false
	}
}

func modelHasBaselineBetas(model string) bool {
	_, known := LookupModel(model)
	return known
}

func requestNeedsLongContextBeta(req BetaRequest, model string) bool {
	selector := req.ContextOneMillion || thinking.HasOneMillionContextSelector(req.Model)
	if selector && !modelHasNativeOneMillion(model) {
		return req.OneMillion
	}
	// Explicit requests retain the catalog gate even on native-1M models.
	// OneMillion without selector intent is the legacy structured beta request.
	return (req.ExplicitOneMillion || req.OneMillion && !selector) && modelSupportsOneMillion(model)
}
