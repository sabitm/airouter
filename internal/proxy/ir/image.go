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

// ImageReference is one ordered edit input. MIME is the canonical image type
// and Base64 is padded standard encoding. Remote URLs and provider file IDs
// are not stored.
type ImageReference struct {
	MIME   string
	Base64 string
}

// ImageEditRequest is the native edit operation. It is separate from
// generation so reference fields cannot change generation behavior.
type ImageEditRequest struct {
	Model          string
	Prompt         string
	Images         []ImageReference
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
