package images

import (
	"bytes"
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
)

func DecodeRequest(body []byte) (*ir.ImageGenerationRequest, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, ir.InvalidRequest("images: expected a JSON object")
	}
	for key := range fields {
		switch key {
		case "model", "prompt", "n", "size", "quality", "background", "output_format", "response_format", "stream", "user":
		default:
			return nil, ir.InvalidRequest("images: unsupported request field")
		}
	}
	req := &ir.ImageGenerationRequest{
		N: 1, Size: "auto", Quality: "auto", Background: "auto",
		OutputFormat: "png", ResponseFormat: "b64_json",
	}
	stringsFields := []struct {
		name string
		dst  *string
	}{
		{"model", &req.Model}, {"prompt", &req.Prompt}, {"size", &req.Size},
		{"quality", &req.Quality}, {"background", &req.Background},
		{"output_format", &req.OutputFormat}, {"response_format", &req.ResponseFormat},
	}
	for _, field := range stringsFields {
		if raw, ok := fields[field.name]; ok {
			if isNull(raw) || json.Unmarshal(raw, field.dst) != nil {
				return nil, ir.InvalidRequest("images: " + field.name + " must be a string")
			}
		}
	}
	if strings.TrimSpace(req.Model) == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, ir.InvalidRequest("images: model and prompt must be nonempty strings")
	}
	if raw, ok := fields["n"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &req.N) != nil || req.N != 1 {
			return nil, ir.InvalidRequest("images: n must be 1")
		}
	}
	if raw, ok := fields["stream"]; ok {
		var stream bool
		if isNull(raw) || json.Unmarshal(raw, &stream) != nil || stream {
			return nil, ir.InvalidRequest("images: streaming is not supported")
		}
	}
	if raw, ok := fields["user"]; ok {
		var user string
		if isNull(raw) || json.Unmarshal(raw, &user) != nil {
			return nil, ir.InvalidRequest("images: user must be a string")
		}
		req.User = &user
	}
	req.OutputFormat = strings.ToLower(req.OutputFormat)
	if err := validateControls(req); err != nil {
		return nil, err
	}
	return req, nil
}

func validateControls(req *ir.ImageGenerationRequest) error {
	if req == nil || strings.TrimSpace(req.Model) == "" || strings.TrimSpace(req.Prompt) == "" {
		return ir.InvalidRequest("images: model and prompt must be nonempty strings")
	}
	if req.N != 1 || req.Size != "auto" || req.OutputFormat != "png" || req.ResponseFormat != "b64_json" {
		return ir.InvalidRequest("images: only one auto-sized PNG with b64_json output is supported")
	}
	switch req.Quality {
	case "auto", "low", "medium", "high":
	default:
		return ir.InvalidRequest("images: unsupported quality")
	}
	if req.Background != "auto" && req.Background != "opaque" {
		return ir.InvalidRequest("images: unsupported background")
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
