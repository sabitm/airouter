package images

import (
	"bytes"
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
)

func objectFields(body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, ir.InvalidRequest("images: expected a JSON object")
	}
	return fields, nil
}

func rejectUnknown(fields map[string]json.RawMessage, allowed map[string]struct{}) error {
	for key := range fields {
		if _, ok := allowed[key]; !ok {
			return ir.InvalidRequest("images: unsupported request field")
		}
	}
	return nil
}

var generationFields = map[string]struct{}{
	"model": {}, "prompt": {}, "n": {}, "size": {}, "quality": {}, "background": {},
	"output_format": {}, "response_format": {}, "stream": {}, "user": {},
}

var editFields = map[string]struct{}{
	"model": {}, "prompt": {}, "images": {}, "n": {}, "size": {}, "quality": {}, "background": {},
	"output_format": {}, "response_format": {}, "stream": {}, "user": {},
}

type imageControls struct {
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

func parseControls(fields map[string]json.RawMessage) (imageControls, error) {
	ctrl := imageControls{
		N: 1, Size: "auto", Quality: "auto", Background: "auto",
		OutputFormat: "png", ResponseFormat: "b64_json",
	}
	stringFields := []struct {
		name string
		dst  *string
	}{
		{"model", &ctrl.Model}, {"prompt", &ctrl.Prompt}, {"size", &ctrl.Size},
		{"quality", &ctrl.Quality}, {"background", &ctrl.Background},
		{"output_format", &ctrl.OutputFormat}, {"response_format", &ctrl.ResponseFormat},
	}
	for _, field := range stringFields {
		if raw, ok := fields[field.name]; ok {
			if isNull(raw) || json.Unmarshal(raw, field.dst) != nil {
				return imageControls{}, ir.InvalidRequest("images: " + field.name + " must be a string")
			}
		}
	}
	if strings.TrimSpace(ctrl.Model) == "" || strings.TrimSpace(ctrl.Prompt) == "" {
		return imageControls{}, ir.InvalidRequest("images: model and prompt must be nonempty strings")
	}
	if raw, ok := fields["n"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &ctrl.N) != nil || ctrl.N != 1 {
			return imageControls{}, ir.InvalidRequest("images: n must be 1")
		}
	}
	if raw, ok := fields["stream"]; ok {
		var stream bool
		if isNull(raw) || json.Unmarshal(raw, &stream) != nil || stream {
			return imageControls{}, ir.InvalidRequest("images: streaming is not supported")
		}
	}
	if raw, ok := fields["user"]; ok {
		var user string
		if isNull(raw) || json.Unmarshal(raw, &user) != nil {
			return imageControls{}, ir.InvalidRequest("images: user must be a string")
		}
		ctrl.User = &user
	}
	ctrl.OutputFormat = strings.ToLower(ctrl.OutputFormat)
	if err := validateControls(ctrl); err != nil {
		return imageControls{}, err
	}
	return ctrl, nil
}

func validateControls(ctrl imageControls) error {
	if strings.TrimSpace(ctrl.Model) == "" || strings.TrimSpace(ctrl.Prompt) == "" {
		return ir.InvalidRequest("images: model and prompt must be nonempty strings")
	}
	if ctrl.N != 1 || ctrl.Size != "auto" || ctrl.OutputFormat != "png" || ctrl.ResponseFormat != "b64_json" {
		return ir.InvalidRequest("images: only one auto-sized PNG with b64_json output is supported")
	}
	switch ctrl.Quality {
	case "auto", "low", "medium", "high":
	default:
		return ir.InvalidRequest("images: unsupported quality")
	}
	if ctrl.Background != "auto" && ctrl.Background != "opaque" {
		return ir.InvalidRequest("images: unsupported background")
	}
	return nil
}

func (ctrl imageControls) generation() *ir.ImageGenerationRequest {
	return &ir.ImageGenerationRequest{
		Model: ctrl.Model, Prompt: ctrl.Prompt, N: ctrl.N, Size: ctrl.Size,
		Quality: ctrl.Quality, Background: ctrl.Background,
		OutputFormat: ctrl.OutputFormat, ResponseFormat: ctrl.ResponseFormat, User: ctrl.User,
	}
}

func (ctrl imageControls) edit(refs []ir.ImageReference) *ir.ImageEditRequest {
	return &ir.ImageEditRequest{
		Model: ctrl.Model, Prompt: ctrl.Prompt, Images: refs, N: ctrl.N, Size: ctrl.Size,
		Quality: ctrl.Quality, Background: ctrl.Background,
		OutputFormat: ctrl.OutputFormat, ResponseFormat: ctrl.ResponseFormat, User: ctrl.User,
	}
}

func DecodeRequest(body []byte) (*ir.ImageGenerationRequest, error) {
	fields, err := objectFields(body)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(fields, generationFields); err != nil {
		return nil, err
	}
	ctrl, err := parseControls(fields)
	if err != nil {
		return nil, err
	}
	return ctrl.generation(), nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
