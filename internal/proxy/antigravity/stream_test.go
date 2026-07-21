package antigravity

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeStreamTextAndFinish(t *testing.T) {
	sse := "" +
		"data: {\"response\":{\"responseId\":\"r1\",\"modelVersion\":\"gemini-x\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}}\n\n" +
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"!\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2}}}\n\n"
	var events []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var finish *ir.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case ir.EventTextDelta:
			text.WriteString(events[i].Text)
		case ir.EventFinish:
			finish = &events[i]
		}
	}
	if text.String() != "hi!" {
		t.Fatalf("text %q", text.String())
	}
	if finish == nil || finish.StopReason != ir.StopEndTurn || finish.OutputTokens != 2 {
		t.Fatalf("finish %+v", finish)
	}
}

func TestDecodeStreamToolDecloakAndThoughtDrop(t *testing.T) {
	sse := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[" +
		"{\"text\":\"think\",\"thought\":true}," +
		"{\"functionCall\":{\"name\":\"Shell_ide\",\"args\":{\"cmd\":\"ls\"}}}" +
		"]},\"finishReason\":\"STOP\"}]}}\n\n"
	var events []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawText bool
	var toolName string
	var stop ir.StopReason
	for _, ev := range events {
		if ev.Kind == ir.EventTextDelta {
			sawText = true
		}
		if ev.Kind == ir.EventToolCallStart {
			toolName = ev.ToolName
		}
		if ev.Kind == ir.EventFinish {
			stop = ev.StopReason
		}
	}
	if sawText {
		t.Fatal("thought text should be dropped")
	}
	if toolName != "Shell" {
		t.Fatalf("decloak got %q", toolName)
	}
	if stop != ir.StopToolUse {
		t.Fatalf("stop %q", stop)
	}
}
