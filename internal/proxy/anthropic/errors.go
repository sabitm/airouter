package anthropic

import "encoding/json"

// EncodeError renders an error in the Anthropic error envelope. Used when
// Anthropic is the ingress format.
func EncodeError(message, errType string) []byte {
	if errType == "" {
		errType = "invalid_request_error"
	}
	out := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	}
	raw, _ := json.Marshal(out)
	return raw
}
