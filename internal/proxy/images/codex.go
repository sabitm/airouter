package images

import (
	"encoding/json"

	"airouter/internal/proxy/ir"
)

func EncodeCodexRequest(req *ir.ImageGenerationRequest) ([]byte, error) {
	if req == nil {
		return nil, ir.InvalidRequest("images: model and prompt must be nonempty strings")
	}
	if err := validateControls(imageControls{
		Model: req.Model, Prompt: req.Prompt, N: req.N, Size: req.Size,
		Quality: req.Quality, Background: req.Background,
		OutputFormat: req.OutputFormat, ResponseFormat: req.ResponseFormat,
	}); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Model        string `json:"model"`
		Prompt       string `json:"prompt"`
		N            int    `json:"n"`
		Size         string `json:"size"`
		Quality      string `json:"quality"`
		Background   string `json:"background"`
		OutputFormat string `json:"output_format"`
	}{req.Model, req.Prompt, req.N, req.Size, req.Quality, req.Background, req.OutputFormat})
}
