package images

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/media"
)

const (
	maxEditImages = 5
	// Local safety caps. They are not provider entitlements and are not the
	// shared media attachment budget, which also accepts GIF and raw base64.
	maxEditImageBytes = 10 << 20
	maxEditTotalBytes = 32 << 20
)

// BudgetError is a local reference or body budget failure. It is an HTTP 413
// client error and must not contact or penalize an upstream.
type BudgetError struct {
	Message string
}

func (e *BudgetError) Error() string {
	if e == nil || e.Message == "" {
		return "images: request exceeds size limit"
	}
	return e.Message
}

func budgetExceeded(message string) error {
	return &BudgetError{Message: message}
}

func IsBudgetExceeded(err error) bool {
	var budget *BudgetError
	return errors.As(err, &budget)
}

func DecodeEditRequest(body []byte) (*ir.ImageEditRequest, error) {
	fields, err := objectFields(body)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(fields, editFields); err != nil {
		return nil, err
	}
	ctrl, err := parseControls(fields)
	if err != nil {
		return nil, err
	}
	raw, ok := fields["images"]
	if !ok {
		return nil, ir.InvalidRequest("images: images must contain 1 to 5 references")
	}
	refs, err := parseReferences(raw)
	if err != nil {
		return nil, err
	}
	return ctrl.edit(refs), nil
}

func parseReferences(raw json.RawMessage) ([]ir.ImageReference, error) {
	if isNull(raw) || len(raw) == 0 || raw[0] != '[' {
		return nil, ir.InvalidRequest("images: images must contain 1 to 5 references")
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) < 1 || len(items) > maxEditImages {
		return nil, ir.InvalidRequest("images: images must contain 1 to 5 references")
	}
	refs := make([]ir.ImageReference, 0, len(items))
	total := 0
	for _, item := range items {
		ref, n, err := parseReference(item)
		if err != nil {
			return nil, err
		}
		if total > maxEditTotalBytes-n {
			return nil, budgetExceeded("images: references exceed total size budget")
		}
		total += n
		refs = append(refs, ref)
	}
	return refs, nil
}

func parseReference(raw json.RawMessage) (ir.ImageReference, int, error) {
	trimmed := bytes.TrimSpace(raw)
	if isNull(raw) || len(trimmed) == 0 || trimmed[0] != '{' {
		return ir.ImageReference{}, 0, ir.InvalidRequest("images: invalid image reference")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 1 {
		return ir.ImageReference{}, 0, ir.InvalidRequest("images: invalid image reference")
	}
	urlRaw, ok := fields["image_url"]
	if !ok || isNull(urlRaw) {
		return ir.ImageReference{}, 0, ir.InvalidRequest("images: invalid image reference")
	}
	var url string
	if json.Unmarshal(urlRaw, &url) != nil || url == "" {
		return ir.ImageReference{}, 0, ir.InvalidRequest("images: invalid image reference")
	}
	return decodeReference(url)
}

func decodeReference(value string) (ir.ImageReference, int, error) {
	mime, payload, err := splitDataURL(value)
	if err != nil {
		return ir.ImageReference{}, 0, err
	}
	canon := canonicalEditMIME(mime)
	if canon == "" {
		return ir.ImageReference{}, 0, ir.InvalidRequest("images: unsupported image type")
	}
	if err := canonicalBase64Form(payload); err != nil {
		return ir.ImageReference{}, 0, err
	}
	n, err := decodedBase64Size(payload)
	if err != nil {
		return ir.ImageReference{}, 0, err
	}
	if n > maxEditImageBytes {
		return ir.ImageReference{}, 0, budgetExceeded("images: reference exceeds size limit")
	}
	ref, err := materializeReference(canon, payload, n)
	if err != nil {
		return ir.ImageReference{}, 0, err
	}
	return ref, n, nil
}

func splitDataURL(value string) (string, string, error) {
	const marker = "data:"
	if len(value) < len(marker) || !strings.EqualFold(value[:len(marker)], marker) || strings.ContainsAny(value, " \t\r\n") {
		return "", "", ir.InvalidRequest("images: invalid image reference")
	}
	rest := value[len(marker):]
	comma := strings.IndexByte(rest, ',')
	if comma <= 0 || comma == len(rest)-1 {
		return "", "", ir.InvalidRequest("images: invalid image reference")
	}
	meta, payload := rest[:comma], rest[comma+1:]
	semi := strings.IndexByte(meta, ';')
	if semi <= 0 || semi != strings.LastIndexByte(meta, ';') || !strings.EqualFold(meta[semi+1:], "base64") {
		return "", "", ir.InvalidRequest("images: invalid image reference")
	}
	return strings.ToLower(meta[:semi]), payload, nil
}

func canonicalEditMIME(mime string) string {
	switch mime {
	case "image/jpg":
		return "image/jpeg"
	case "image/png", "image/jpeg", "image/webp":
		return mime
	default:
		return ""
	}
}

func canonicalBase64Form(payload string) error {
	if payload == "" || len(payload)%4 != 0 {
		return ir.InvalidRequest("images: invalid image reference")
	}
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' {
			continue
		}
		if c == '=' && i >= len(payload)-2 {
			continue
		}
		return ir.InvalidRequest("images: invalid image reference")
	}
	if strings.HasSuffix(payload, "===") || strings.Contains(payload[:len(payload)-2], "=") {
		return ir.InvalidRequest("images: invalid image reference")
	}
	return nil
}

func decodedBase64Size(payload string) (int, error) {
	pad := 0
	if strings.HasSuffix(payload, "==") {
		pad = 2
	} else if strings.HasSuffix(payload, "=") {
		pad = 1
	}
	n := len(payload)/4*3 - pad
	if n <= 0 {
		return 0, ir.InvalidRequest("images: invalid image reference")
	}
	return n, nil
}

func materializeReference(mime, payload string, n int) (ir.ImageReference, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(payload)
	if len(raw) > 0 {
		defer clear(raw)
	}
	if err != nil || len(raw) != n || base64.StdEncoding.EncodeToString(raw) != payload {
		return ir.ImageReference{}, ir.InvalidRequest("images: invalid image reference")
	}
	if media.DetectImageMIME(raw) != mime {
		return ir.ImageReference{}, ir.InvalidRequest("images: image content does not match declared type")
	}
	return ir.ImageReference{MIME: mime, Base64: payload}, nil
}

func validateStoredReferences(refs []ir.ImageReference) error {
	if len(refs) < 1 || len(refs) > maxEditImages {
		return ir.InvalidRequest("images: images must contain 1 to 5 references")
	}
	total := 0
	for _, ref := range refs {
		if canonicalEditMIME(ref.MIME) != ref.MIME {
			return ir.InvalidRequest("images: unsupported image type")
		}
		if err := canonicalBase64Form(ref.Base64); err != nil {
			return err
		}
		n, err := decodedBase64Size(ref.Base64)
		if err != nil {
			return err
		}
		if n > maxEditImageBytes {
			return budgetExceeded("images: reference exceeds size limit")
		}
		if total > maxEditTotalBytes-n {
			return budgetExceeded("images: references exceed total size budget")
		}
		total += n
		if _, err := materializeReference(ref.MIME, ref.Base64, n); err != nil {
			return err
		}
	}
	return nil
}

// EncodeCodexEditRequest builds the native edit body. Codex CLI omits n; this
// encoder sends n=1, matching the typed request and 9router. That choice is
// not evidence that every account accepts the field. output_format,
// response_format, stream, and user are ingress-only.
func EncodeCodexEditRequest(req *ir.ImageEditRequest) ([]byte, error) {
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
	if err := validateStoredReferences(req.Images); err != nil {
		return nil, err
	}
	type editImage struct {
		ImageURL string `json:"image_url"`
	}
	encoded := make([]editImage, len(req.Images))
	for i, ref := range req.Images {
		encoded[i].ImageURL = "data:" + ref.MIME + ";base64," + ref.Base64
	}
	return json.Marshal(struct {
		Model      string      `json:"model"`
		Prompt     string      `json:"prompt"`
		Images     []editImage `json:"images"`
		N          int         `json:"n"`
		Size       string      `json:"size"`
		Quality    string      `json:"quality"`
		Background string      `json:"background"`
	}{
		Model: req.Model, Prompt: req.Prompt, Images: encoded,
		N: 1, Size: "auto", Quality: req.Quality, Background: req.Background,
	})
}
