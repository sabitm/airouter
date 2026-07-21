package antigravity

import (
	"encoding/json"
	"fmt"
)

// InjectProjectID sets the Cloud Code envelope project field. projectID must be
// non-empty; a blank value is a hard error so we never send a wrong-account id.
func InjectProjectID(body []byte, projectID string) ([]byte, error) {
	if projectID == "" {
		return nil, fmt.Errorf("antigravity: missing project id; reconnect OAuth")
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("antigravity: inject project: %w", err)
	}
	m["project"] = projectID
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}
