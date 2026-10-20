package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFinalReviewExactSystemText(t *testing.T) {
	const text = "  lead and trail\n "
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"system":[{"type":"text","text":"  lead and trail\n "}],"messages":[{"role":"user","content":"hi"}]}`)
	out, err := Prepare(body, PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile()})
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	json.Unmarshal(out, &obj)
	if obj.System[len(obj.System)-1].Text != text {
		t.Fatalf("caller text changed to %q", obj.System[len(obj.System)-1].Text)
	}
}
