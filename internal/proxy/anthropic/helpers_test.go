package anthropic

import (
	"encoding/json"
	"testing"
)

func textBlocks(texts ...string) json.RawMessage {
	blocks := make([]map[string]string, len(texts))
	for i, t := range texts {
		blocks[i] = map[string]string{"type": "text", "text": t}
	}
	raw, _ := json.Marshal(blocks)
	return raw
}

func jsonString(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

func TestSystemToTextStripsBillingHeader(t *testing.T) {
	const opener = "You are Claude Code, Anthropic's official CLI for Claude."
	const prompt = "You are an interactive agent that helps users with software engineering tasks."
	const ccBlock = opener + "\n" + prompt
	const billing = "x-anthropic-billing-header: cc_version=2.1.177.01c; cc_entrypoint=cli; cch=256ac;"
	cases := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{
			name: "array block with billing prefix and identity opener",
			raw:  textBlocks(billing + ccBlock),
			want: prompt,
		},
		{
			name: "billing header as its own block",
			raw:  textBlocks(billing, ccBlock),
			want: prompt,
		},
		{
			name: "string system with billing prefix and opener",
			raw:  jsonString("x-anthropic-billing-header: cc_version=2.1.177.01c; cch=256ac;" + ccBlock),
			want: prompt,
		},
		{
			name: "identity opener without billing header",
			raw:  textBlocks(ccBlock),
			want: prompt,
		},
		{
			name: "no markers is untouched",
			raw:  textBlocks(prompt),
			want: prompt,
		},
		{
			name: "incidental mid-prompt Claude Code mention preserved",
			raw:  textBlocks(prompt + "\nClaude Code is available as a CLI."),
			want: prompt + "\nClaude Code is available as a CLI.",
		},
		{
			name: "unrelated leading text untouched",
			raw:  jsonString("x-something-else: keep me;rest"),
			want: "x-something-else: keep me;rest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := systemToText(tc.raw)
			if got != tc.want {
				t.Fatalf("systemToText:\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}
