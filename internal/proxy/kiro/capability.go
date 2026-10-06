package kiro

// Capability is the trusted reasoning advertisement for one exact model ID.
// Paths and levels come from additionalModelRequestFieldsSchema. A missing
// field means that control is unsupported. This type carries no credentials.
type Capability struct {
	HasEffort       bool
	EffortPath      string
	EffortLevels    []string
	DefaultEffort   string
	HasThinking     bool
	ThinkingToggle  bool
	ThinkingEnabled bool
}

// Clone returns an independent copy. Nil stays nil.
func (c *Capability) Clone() *Capability {
	if c == nil {
		return nil
	}
	out := *c
	if c.EffortLevels != nil {
		out.EffortLevels = append([]string(nil), c.EffortLevels...)
	}
	return &out
}
