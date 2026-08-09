package antigravity

// Cloud Code envelope and Gemini-shaped wire types for Antigravity chat.

type envelope struct {
	Project     string        `json:"project"`
	Model       string        `json:"model"`
	UserAgent   string        `json:"userAgent"`
	RequestType string        `json:"requestType"`
	RequestID   string        `json:"requestId,omitempty"`
	Request     geminiRequest `json:"request"`
}

type geminiRequest struct {
	SessionID         string            `json:"sessionId,omitempty"`
	Contents          []geminiContent   `json:"contents,omitempty"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
	Tools             []geminiToolGroup `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts,omitempty"`
}

type geminiPart struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type functionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name,omitempty"`
	Args map[string]any `json:"args,omitempty"`
}

type functionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

type geminiToolGroup struct {
	FunctionDeclarations []functionDecl `json:"functionDeclarations,omitempty"`
}

type functionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig *functionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type functionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type generationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// Stream response shapes (SSE data JSON).

type streamChunk struct {
	Response *geminiResponse `json:"response,omitempty"`
	// Bare fields when not wrapped.
	Candidates    []candidate    `json:"candidates,omitempty"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
	ResponseID    string         `json:"responseId,omitempty"`
	ModelVersion  string         `json:"modelVersion,omitempty"`
}

type geminiResponse struct {
	Candidates    []candidate    `json:"candidates,omitempty"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
	ResponseID    string         `json:"responseId,omitempty"`
	ModelVersion  string         `json:"modelVersion,omitempty"`
}

type candidate struct {
	Content      *geminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount int `json:"promptTokenCount"`
	// CandidatesTokenCount is pointer-aware so an explicit zero is preferred
	// over deriving candidates from totalTokenCount.
	CandidatesTokenCount    *int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int  `json:"thoughtsTokenCount"`
	CachedContentTokenCount int  `json:"cachedContentTokenCount"`
	// TotalTokenCount is pointer-aware so a present zero can still authorize
	// total-based candidate derivation when candidates is absent.
	TotalTokenCount *int `json:"totalTokenCount"`
}

// inputTokens is promptTokenCount only. cachedContentTokenCount is a subset of
// prompt and must not be added again.
func (u *usageMetadata) inputTokens() int {
	if u == nil {
		return 0
	}
	return u.PromptTokenCount
}

// hasAuthoritativeOutput reports whether this metadata carries known output:
// explicit candidates (including zero), a total for derivation, or positive
// thoughts when the other aggregate fields are omitted.
func (u *usageMetadata) hasAuthoritativeOutput() bool {
	if u == nil {
		return false
	}
	return u.CandidatesTokenCount != nil || u.TotalTokenCount != nil || u.ThoughtsTokenCount > 0
}

// outputTokens is candidates + thoughts. When candidates is absent and total is
// present, candidates are derived as max(total-prompt-thoughts, 0).
func (u *usageMetadata) outputTokens() int {
	if u == nil {
		return 0
	}
	candidates := 0
	if u.CandidatesTokenCount != nil {
		candidates = *u.CandidatesTokenCount
		if candidates < 0 {
			candidates = 0
		}
	} else if u.TotalTokenCount != nil {
		candidates = *u.TotalTokenCount - u.PromptTokenCount - u.ThoughtsTokenCount
		if candidates < 0 {
			candidates = 0
		}
	}
	return candidates + u.ThoughtsTokenCount
}
