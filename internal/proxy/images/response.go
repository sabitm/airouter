package images

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"airouter/internal/proxy/ir"
)

const maxResponseBytes = 64 << 20
const maxDecodedImageBytes = 48 << 20

var errInvalidResponse = errors.New("images: upstream returned an invalid image response")

func DecodeCodexResponse(body []byte) (*ir.ImageGenerationResponse, error) {
	if len(body) > maxResponseBytes {
		return nil, errInvalidResponse
	}
	var wire struct {
		Created *int64 `json:"created"`
		Data    []struct {
			Base64       string `json:"b64_json"`
			GenerationID string `json:"generation_id"`
		} `json:"data"`
		Background   string          `json:"background"`
		Quality      string          `json:"quality"`
		Size         string          `json:"size"`
		OutputFormat string          `json:"output_format"`
		Usage        json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return nil, errInvalidResponse
	}
	resp := &ir.ImageGenerationResponse{
		Created:    wire.Created,
		Background: wire.Background, Quality: wire.Quality, Size: wire.Size, OutputFormat: wire.OutputFormat,
		Usage: decodeUsage(wire.Usage),
	}
	if len(wire.Data) != 1 || (wire.Created != nil && *wire.Created < 0) ||
		(wire.OutputFormat != "" && wire.OutputFormat != "png") || !validPNG(wire.Data[0].Base64) {
		return resp, errInvalidResponse
	}
	resp.Images = []ir.GeneratedImage{{Data: wire.Data[0].Base64, GenerationID: wire.Data[0].GenerationID}}
	return resp, nil
}

func validPNG(data string) bool {
	if data == "" || len(data) > maxResponseBytes {
		return false
	}
	// Inspect the signature and exhaust the decoder without retaining a second image copy.
	dec := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(data))
	var signature [8]byte
	if _, err := io.ReadFull(dec, signature[:]); err != nil ||
		!bytes.Equal(signature[:], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return false
	}
	n, err := io.Copy(io.Discard, io.LimitReader(dec, maxDecodedImageBytes-8+1))
	return err == nil && n > 0 && n <= maxDecodedImageBytes-8
}

func decodeUsage(raw json.RawMessage) *ir.ImageGenerationUsage {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	input, ok := tokenCount(fields["input_tokens"])
	if !ok {
		return nil
	}
	output, ok := tokenCount(fields["output_tokens"])
	if !ok {
		return nil
	}
	if total, exists := fields["total_tokens"]; exists {
		if _, ok := tokenCount(total); !ok {
			return nil
		}
	}
	for _, name := range []string{"input_tokens_details", "output_tokens_details"} {
		if detail, exists := fields[name]; exists && !isNull(detail) {
			var counts map[string]json.RawMessage
			if json.Unmarshal(detail, &counts) != nil || counts == nil {
				return nil
			}
			for _, count := range counts {
				if _, ok := tokenCount(count); !ok {
					return nil
				}
			}
		}
	}
	return &ir.ImageGenerationUsage{InputTokens: input, OutputTokens: output, Raw: append(json.RawMessage(nil), raw...)}
}

func tokenCount(raw json.RawMessage) (int, bool) {
	var n int
	if len(raw) == 0 || isNull(raw) || json.Unmarshal(raw, &n) != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func EncodeResponse(resp *ir.ImageGenerationResponse) ([]byte, error) {
	if resp == nil || len(resp.Images) != 1 {
		return nil, errInvalidResponse
	}
	created := time.Now().Unix()
	if resp.Created != nil {
		created = *resp.Created
	}
	data := map[string]any{"b64_json": resp.Images[0].Data}
	if resp.Images[0].GenerationID != "" {
		data["generation_id"] = resp.Images[0].GenerationID
	}
	out := map[string]any{"created": created, "data": []any{data}}
	for name, value := range map[string]string{
		"background": resp.Background, "quality": resp.Quality, "size": resp.Size, "output_format": resp.OutputFormat,
	} {
		if value != "" {
			out[name] = value
		}
	}
	if resp.Usage != nil {
		out["usage"] = resp.Usage.Raw
	}
	body, err := json.Marshal(out)
	if err != nil || len(body) > maxResponseBytes {
		return nil, errInvalidResponse
	}
	return body, nil
}
