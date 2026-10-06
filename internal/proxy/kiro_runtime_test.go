package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/sse"
	"airouter/internal/store"
)

func runtimeMetadataStream() []byte {
	var frames bytes.Buffer
	frames.Write(buildKiroFrame("reasoningContentEvent", `{"text":"think"}`))
	frames.Write(buildKiroFrame("assistantResponseEvent", `{"content":"answer"}`))
	frames.Write(buildKiroFrame("metadataEvent", `{"stopReason":"END_TURN","stopDetails":{"unknown":{"value":true}},"tokenUsage":{"uncachedInputTokens":5,"cacheReadInputTokens":2,"cacheWriteInputTokens":3,"outputTokens":4}}`))
	return frames.Bytes()
}

func setupRuntimeKiro(t *testing.T, responses ...[]byte) (string, string, *store.Store, []*atomic.Int32) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	var targets []domain.ComboTarget
	var hits []*atomic.Int32
	for i, response := range responses {
		hit := &atomic.Int32{}
		hits = append(hits, hit)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if kiroTestIsCatalog(r) {
				kiroTestCatalog(w, "runtime-model")
				return
			}
			hit.Add(1)
			if r.Method != http.MethodPost || r.URL.Path != "/custom/" || r.URL.RawQuery != "" {
				t.Errorf("Runtime method=%s path=%s query=%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
			for name, want := range map[string]string{
				"Content-Type": "application/x-amz-json-1.0", "X-Amz-Target": "KiroRuntimeService.GenerateAssistantResponse",
				"TokenType": "SSO_OIDC", "Authorization": "Bearer oauth-token", "X-Kiro-Idp": "AWSIdC",
				"X-Kiro-Profile-Arn": "arn:aws:codewhisperer:eu-central-1:1:profile/A", "Accept": "application/vnd.amazon.eventstream",
			} {
				if values := r.Header.Values(name); len(values) != 1 || values[0] != want {
					t.Errorf("received %s=%v", name, values)
				}
			}
			if r.Header.Get("x-amzn-kiro-profile-arn") != "" {
				t.Error("unused Runtime profile header sent")
			}
			var body struct {
				ProfileArn        string `json:"profileArn"`
				ConversationState struct {
					CurrentMessage struct {
						UserInputMessage struct {
							ModelID string `json:"modelId"`
						} `json:"userInputMessage"`
					} `json:"currentMessage"`
				} `json:"conversationState"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProfileArn != "arn:aws:codewhisperer:eu-central-1:1:profile/A" || body.ConversationState.CurrentMessage.UserInputMessage.ModelID != "runtime-model" {
				t.Errorf("Runtime body profile=%q model=%q err=%v", body.ProfileArn, body.ConversationState.CurrentMessage.UserInputMessage.ModelID, err)
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(response)
			w.(http.Flusher).Flush()
		}))
		t.Cleanup(upstream.Close)
		p := runtimeProvider("runtime-"+strconv.Itoa(i), upstream.URL+"/custom")
		if err := st.CreateProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "runtime-model", Enabled: true})
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: targets}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, key.Token, st, hits
}

func TestKiroRuntimeMetadataPublicUsage(t *testing.T) {
	for _, ingress := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(ingress+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
				base, key, st, hits := setupRuntimeKiro(t, runtimeMetadataStream())
				request := `{"model":"default","max_tokens":20,"stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"hi"}]}`
				response, body := postStream(t, base+ingress, key, request)
				if response.StatusCode != http.StatusOK || hits[0].Load() != 1 {
					t.Fatalf("status=%d hits=%d body=%s", response.StatusCode, hits[0].Load(), body)
				}
				var in, out, total int
				if stream {
					text, done := collectStreamText(t, ingress, body)
					if text != "answer" || !done {
						t.Fatalf("text=%q done=%v body=%s", text, done, body)
					}
					if ingress == "/v1/messages" {
						reader := sse.NewReader(strings.NewReader(body))
						for {
							ev, err := reader.Next()
							if err == io.EOF {
								break
							}
							if err != nil {
								t.Fatal(err)
							}
							if ev.Name == "message_delta" {
								var chunk struct {
									Usage struct {
										Input  int `json:"input_tokens"`
										Output int `json:"output_tokens"`
										Read   int `json:"cache_read_input_tokens"`
										Write  int `json:"cache_creation_input_tokens"`
									} `json:"usage"`
								}
								if err := json.Unmarshal(ev.Data, &chunk); err != nil {
									t.Fatal(err)
								}
								in, out = chunk.Usage.Input+chunk.Usage.Read+chunk.Usage.Write, chunk.Usage.Output
								total = in + out
								if chunk.Usage.Read != 2 || chunk.Usage.Write != 3 {
									t.Fatal("cache subsets were lost")
								}
							}
						}
					} else {
						in, out, total = collectOpenAIUsage(t, body)
					}
				} else {
					var parsed struct {
						Choices []struct {
							Message struct {
								Content string `json:"content"`
							} `json:"message"`
							FinishReason string `json:"finish_reason"`
						} `json:"choices"`
						Content []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
						StopReason string `json:"stop_reason"`
						Usage      struct {
							Prompt     int `json:"prompt_tokens"`
							Output     int `json:"completion_tokens"`
							Total      int `json:"total_tokens"`
							Input      int `json:"input_tokens"`
							AnthOutput int `json:"output_tokens"`
							Read       int `json:"cache_read_input_tokens"`
							Write      int `json:"cache_creation_input_tokens"`
						} `json:"usage"`
					}
					if err := json.Unmarshal([]byte(body), &parsed); err != nil {
						t.Fatal(err)
					}
					if ingress == "/v1/messages" {
						var text string
						for _, block := range parsed.Content {
							if block.Type == "text" {
								text += block.Text
							}
						}
						if text != "answer" || parsed.StopReason != "end_turn" {
							t.Fatalf("Anthropic response=%s", body)
						}
						in = parsed.Usage.Input + parsed.Usage.Read + parsed.Usage.Write
						out = parsed.Usage.AnthOutput
						total = in + out
					} else {
						if len(parsed.Choices) != 1 || parsed.Choices[0].Message.Content != "answer" || parsed.Choices[0].FinishReason != "stop" {
							t.Fatalf("OpenAI response=%s", body)
						}
						in, out, total = parsed.Usage.Prompt, parsed.Usage.Output, parsed.Usage.Total
					}
				}
				if in != 10 || out != 4 || total != 14 {
					t.Fatalf("usage=%d/%d/%d body=%s", in, out, total, body)
				}
				log := waitForLogs(t, st, 1)[0]
				if log.InputTokens != 10 || log.OutputTokens != 4 {
					t.Fatalf("logged usage=%d/%d", log.InputTokens, log.OutputTokens)
				}
			})
		}
	}
}

func TestKiroRuntimePublicFailureBoundaries(t *testing.T) {
	failures := []struct {
		name   string
		frames []byte
	}{
		{"internal", buildKiroFrame("error", `{"message":"failed","extra":"hidden"}`)},
		{"throttling", buildKiroFrame("throttlingError", `{"message":"failed"}`)},
		{"validation", buildKiroFrame("validationError", `{"message":"failed"}`)},
		{"unavailable", buildKiroFrame("serviceUnavailableError", `{"message":"failed"}`)},
		{"unknown stop", buildKiroFrame("metadataEvent", `{"stopReason":"unrecognized_failure"}`)},
		{"malformed metadata", buildKiroFrame("metadataEvent", `{"stopReason":`)},
		{"null stop", buildKiroFrame("messageStopEvent", `null`)},
		{"missing terminal", nil},
	}
	for _, failure := range failures {
		for _, ingress := range []string{"/v1/chat/completions", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				for _, partial := range []bool{false, true} {
					t.Run(failure.name+ingress+"/stream="+strconv.FormatBool(stream)+"/partial="+strconv.FormatBool(partial), func(t *testing.T) {
						var first bytes.Buffer
						if partial {
							first.Write(buildKiroFrame("assistantResponseEvent", `{"content":"partial"}`))
						}
						first.Write(failure.frames)
						if failure.name != "missing terminal" {
							first.Write(buildKiroFrame("metadataEvent", `{"stopReason":"END_TURN"}`))
						}
						base, key, st, hits := setupRuntimeKiro(t, first.Bytes(), runtimeMetadataStream())
						response, body := postStream(t, base+ingress, key, `{"model":"default","max_tokens":20,"stream":`+strconv.FormatBool(stream)+`,"messages":[{"role":"user","content":"hi"}]}`)
						if hits[0].Load() != 1 || strings.Contains(body, "hidden") || strings.Contains(body, "unrecognized_failure") {
							t.Fatalf("hits=%d body=%s", hits[0].Load(), body)
						}
						if stream && partial {
							if hits[1].Load() != 0 || !strings.Contains(body, "partial") || !strings.Contains(body, "error") || strings.Contains(body, "answer") {
								t.Fatalf("post-commit retry or missing error: hits=%d body=%s", hits[1].Load(), body)
							}
							_, done := collectStreamText(t, ingress, body)
							if done {
								t.Fatal("failure produced success trailer")
							}
						} else if response.StatusCode != http.StatusOK || hits[1].Load() != 1 || strings.Contains(body, "partial") || !strings.Contains(body, "answer") {
							t.Fatalf("pre-commit/unary failover status=%d hits=%d body=%s", response.StatusCode, hits[1].Load(), body)
						}
						waitForLogs(t, st, 1)
					})
				}
			}
		}
	}
}

func TestKiroRuntimeRequestBodyAtProxyBoundary(t *testing.T) {
	toolRequest := `{` +
		`"model":"default","max_tokens":20,"temperature":0.2,"stream":%s,` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],` +
		`"messages":[` +
		`{"role":"system","content":"be brief"},` +
		`{"role":"user","content":"find"},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"found"},` +
		`{"role":"user","content":"thanks"}` +
		`]}`
	anthropicRequest := `{` +
		`"model":"default","max_tokens":20,"temperature":0.2,"stream":%s,"system":"be brief",` +
		`"tools":[{"name":"lookup","input_schema":{"type":"object"}}],` +
		`"messages":[` +
		`{"role":"user","content":"find"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"},{"type":"text","text":"thanks"}]}` +
		`]}`
	ingresses := []struct {
		path string
		body string
	}{
		{"/v1/chat/completions", toolRequest},
		{"/v1/messages", anthropicRequest},
	}
	for _, ingress := range ingresses {
		for _, stream := range []bool{false, true} {
			t.Run(ingress.path+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
				var saw []byte
				base, key, _, hits := setupRuntimeKiroBody(t, &saw, runtimeMetadataStream())
				response, body := postStream(t, base+ingress.path, key, fmtRuntimeBody(ingress.body, stream))
				if response.StatusCode != http.StatusOK || hits[0].Load() != 1 {
					t.Fatalf("status=%d hits=%d body=%s", response.StatusCode, hits[0].Load(), body)
				}
				assertRuntimeProxyBody(t, saw)
			})
		}
	}
}

func fmtRuntimeBody(format string, stream bool) string {
	return strings.Replace(format, "%s", strconv.FormatBool(stream), 1)
}

func setupRuntimeKiroBody(t *testing.T, saw *[]byte, response []byte) (string, string, *store.Store, []*atomic.Int32) {
	t.Helper()
	st := newTestStore(t)
	hit := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"modelId":"runtime-model"}]}`)
			return
		}
		hit.Add(1)
		raw, _ := io.ReadAll(r.Body)
		*saw = append([]byte(nil), raw...)
		if r.Header.Get("x-amzn-kiro-agent-mode") != "spec" {
			t.Errorf("agent header = %q", r.Header.Get("x-amzn-kiro-agent-mode"))
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(response)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	p := runtimeProvider("runtime-body", upstream.URL+"/custom")
	p.OAuthCreds.KiroAgentMode = "spec"
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "runtime-model", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "runtime-body")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, key.Token, st, []*atomic.Int32{hit}
}

func assertRuntimeProxyBody(t *testing.T, body []byte) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for key := range raw {
		switch key {
		case "conversationState", "profileArn", "agentMode":
		default:
			t.Errorf("unexpected key %q in %s", key, body)
		}
	}
	if _, ok := raw["inferenceConfig"]; ok || raw["agentMode"] != "spec" || raw["profileArn"] != "arn:aws:codewhisperer:eu-central-1:1:profile/A" {
		t.Fatalf("envelope = %s", body)
	}
	if _, ok := raw["systemPrompt"]; ok || strings.Count(string(body), "be brief") != 1 {
		t.Fatalf("system handling = %s", body)
	}
	state := raw["conversationState"].(map[string]any)
	if state["chatTriggerType"] != "MANUAL" || state["conversationId"] == "" || state["rootConversationId"] != state["conversationId"] {
		t.Fatalf("state = %#v", state)
	}
	if _, ok := state["agentContinuationId"]; ok {
		t.Fatal("continuation fabricated")
	}
	history := state["history"].([]any)
	for _, item := range history {
		msg := item.(map[string]any)
		if user, ok := msg["userInputMessage"].(map[string]any); ok {
			if user["modelId"] != "runtime-model" || user["origin"] != "AI_EDITOR" {
				t.Fatalf("history user = %#v", user)
			}
		}
		if assistant, ok := msg["assistantResponseMessage"].(map[string]any); ok {
			if _, hasModel := assistant["modelId"]; hasModel || assistant["reasoningContent"] != nil {
				t.Fatalf("assistant = %#v", assistant)
			}
			uses := assistant["toolUses"].([]any)
			use := uses[0].(map[string]any)
			if use["name"] != "lookup" || use["toolUseId"] == "" {
				t.Fatalf("tool use = %#v", use)
			}
		}
	}
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if current["modelId"] != "runtime-model" || current["origin"] != "AI_EDITOR" || !strings.Contains(current["content"].(string), "thanks") {
		t.Fatalf("current = %#v", current)
	}
	ctx := current["userInputMessageContext"].(map[string]any)
	results := ctx["toolResults"].([]any)
	result := results[0].(map[string]any)
	assistant := history[1].(map[string]any)["assistantResponseMessage"].(map[string]any)
	useID := assistant["toolUses"].([]any)[0].(map[string]any)["toolUseId"]
	if result["toolUseId"] != useID || result["status"] != "success" {
		t.Fatalf("result = %#v use=%v", result, useID)
	}
	tools := ctx["tools"].([]any)
	spec := tools[0].(map[string]any)["toolSpecification"].(map[string]any)
	if spec["name"] != "lookup" {
		t.Fatalf("catalog = %#v", spec)
	}
}

func TestKiroRuntimeUnaryFailureDoesNotReturnPartial(t *testing.T) {
	frames := append(buildKiroFrame("assistantResponseEvent", `{"content":"partial"}`), buildKiroFrame("validationError", `{"message":"failed"}`)...)
	base, key, st, _ := setupRuntimeKiro(t, frames)
	response, body := postStream(t, base+"/v1/chat/completions", key, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if response.StatusCode == http.StatusOK || strings.Contains(body, "partial") {
		t.Fatalf("partial unary success: status=%d body=%s", response.StatusCode, body)
	}
	waitForLogs(t, st, 1)
}
