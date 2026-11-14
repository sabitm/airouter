package ir

import "encoding/json"

type ImageGenerationRequest struct {
	Model          string
	Prompt         string
	N              int
	Size           string
	Quality        string
	Background     string
	OutputFormat   string
	ResponseFormat string
	User           *string
}

type GeneratedImage struct {
	Data         string
	GenerationID string
}

type ImageGenerationResponse struct {
	Created      *int64
	Images       []GeneratedImage
	Background   string
	Quality      string
	Size         string
	OutputFormat string
	Usage        *ImageGenerationUsage
}

// Raw preserves image/text token partitions without adding them to inclusive totals.
type ImageGenerationUsage struct {
	InputTokens  int
	OutputTokens int
	Raw          json.RawMessage
}
