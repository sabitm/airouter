package openai

import "encoding/json"

// EncodeError renders an error in the OpenAI error envelope. Used when OpenAI is
// the ingress format.
func EncodeError(message, errType string) []byte {
	if errType == "" {
		errType = "invalid_request_error"
	}
	out := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    nil,
		},
	}
	raw, _ := json.Marshal(out)
	return raw
}
