package antigravity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"airouter/internal/proxy/ir"
)

var nonFuncNameChars = regexp.MustCompile(`[^a-zA-Z0-9_.:\-]`)

// EncodeRequest renders the IR as a Cloud Code Antigravity envelope with an
// empty project field. The proxy injects OAuthCreds.ProjectID via InjectProjectID.
func EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("antigravity: nil request")
	}
	inner := geminiRequest{
		SessionID: newSessionID(),
		Contents:  buildContents(req.Messages),
	}
	if sys := strings.TrimSpace(req.System); sys != "" {
		inner.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: sys}}}
	}
	inner.GenerationConfig = &generationConfig{
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxOutputTokens: clampMaxTokens(req.MaxTokens),
	}
	if len(req.StopSequences) > 0 {
		inner.GenerationConfig.StopSequences = append([]string(nil), req.StopSequences...)
	}
	if len(req.Tools) > 0 {
		decls := make([]functionDecl, 0, len(req.Tools))
		seen := map[string]bool{}
		for _, t := range req.Tools {
			name := sanitizeFunctionName(t.Name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			decls = append(decls, functionDecl{
				Name:        name,
				Description: t.Description,
				Parameters:  CleanJSONSchemaForAntigravity(t.Parameters),
			})
		}
		if len(decls) > 0 {
			inner.Tools = []geminiToolGroup{{FunctionDeclarations: decls}}
			inner.ToolConfig = &toolConfig{
				FunctionCallingConfig: &functionCallingConfig{Mode: "VALIDATED"},
			}
		}
	}

	// Cloak after schema clean / name sanitize so _ide applies to wire names.
	_ = CloakTools(&inner)

	env := envelope{
		Project:     "",
		Model:       req.Model,
		UserAgent:   UserAgentEnvelope,
		RequestType: RequestTypeAgent,
		RequestID:   buildRequestID(inner.SessionID, req.Model, len(inner.Contents)),
		Request:     inner,
	}
	return json.Marshal(env)
}

func clampMaxTokens(n int) int {
	if n <= 0 {
		return DefaultMaxTokens
	}
	if n > MaxOutputTokens {
		return MaxOutputTokens
	}
	return n
}

func sanitizeFunctionName(name string) string {
	if name == "" {
		return "_unknown"
	}
	s := nonFuncNameChars.ReplaceAllString(name, "_")
	if s == "" {
		return "_unknown"
	}
	if c := s[0]; !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
		s = "_" + s
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func buildContents(msgs []ir.Message) []geminiContent {
	// tool id -> name for functionResponse.name
	toolNames := map[string]string{}
	var out []geminiContent

	for _, m := range msgs {
		role := "user"
		if m.Role == ir.RoleAssistant {
			role = "model"
		}
		var parts []geminiPart
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockText:
				if b.Text != "" {
					parts = append(parts, geminiPart{Text: b.Text})
				}
			case ir.BlockImage:
				if b.Image != nil && b.Image.Data != "" {
					mt := b.Image.MediaType
					if mt == "" {
						mt = "image/png"
					}
					parts = append(parts, geminiPart{InlineData: &inlineData{MimeType: mt, Data: b.Image.Data}})
				}
			case ir.BlockFile:
				if b.File != nil && b.File.Data != "" {
					mt := b.File.MediaType
					if mt == "" {
						mt = "application/octet-stream"
					}
					parts = append(parts, geminiPart{InlineData: &inlineData{MimeType: mt, Data: b.File.Data}})
				}
			case ir.BlockToolUse:
				args := map[string]any{}
				if len(b.ToolInput) > 0 {
					_ = json.Unmarshal(b.ToolInput, &args)
				}
				if b.ToolID != "" {
					toolNames[b.ToolID] = b.ToolName
				}
				// functionCall must be on model role
				role = "model"
				parts = append(parts, geminiPart{
					FunctionCall: &functionCall{
						ID:   b.ToolID,
						Name: b.ToolName,
						Args: args,
					},
					ThoughtSignature: DefaultThoughtSignature,
				})
			case ir.BlockToolResult:
				name := toolNames[b.ToolUseID]
				if name == "" {
					name = "tool"
				}
				resultText := toolResultText(b)
				// functionResponse must be on user role
				role = "user"
				parts = append(parts, geminiPart{
					FunctionResponse: &functionResponse{
						ID:   b.ToolUseID,
						Name: name,
						Response: map[string]any{
							"result": resultText,
						},
					},
				})
			}
		}
		if len(parts) == 0 {
			continue
		}
		// Merge consecutive same-role contents (Gemini prefers alternating).
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Parts = append(out[n-1].Parts, parts...)
			continue
		}
		out = append(out, geminiContent{Role: role, Parts: parts})
	}
	return out
}

func toolResultText(b ir.ContentBlock) string {
	var sb strings.Builder
	for _, c := range b.ToolResult {
		if c.Type == ir.BlockText {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 && b.IsError {
		return "error"
	}
	return sb.String()
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s%d", hex.EncodeToString(b[:]), time.Now().UnixMilli())
}

func buildRequestID(sessionID, model string, contentCount int) string {
	conv := uuidLike(sessionID + ":conversation")
	traj := uuidLike(sessionID + ":" + model + ":agent")
	step := contentCount*2 - 1
	if step < 1 {
		step = 1
	}
	return fmt.Sprintf("agent/%s/%d/%s/%d", conv, time.Now().UnixMilli(), traj, step)
}

func uuidLike(seed string) string {
	// Deterministic-ish UUID v4 shape from seed bytes (not crypto UUID).
	sum := make([]byte, 16)
	h := []byte(seed)
	for i := 0; i < 16; i++ {
		if i < len(h) {
			sum[i] = h[i]
		} else {
			sum[i] = byte(i * 17)
		}
		if i+16 < len(h) {
			sum[i] ^= h[i+16]
		}
	}
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	hexs := hex.EncodeToString(sum)
	return hexs[0:8] + "-" + hexs[8:12] + "-" + hexs[12:16] + "-" + hexs[16:20] + "-" + hexs[20:32]
}
