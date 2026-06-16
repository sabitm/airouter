package responses

import "encoding/json"

// EncodeError renders an error in the Responses error envelope.
func EncodeError(message, errType string) []byte {
	if errType == "" {
		errType = "invalid_request_error"
	}
	out := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    nil,
			"param":   nil,
		},
	}
	raw, _ := json.Marshal(out)
	return raw
}
