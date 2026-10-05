// Package kiro implements the AWS CodeWhisperer-backed Kiro provider as a
// backend-only codec. Kiro is not OpenAI/Anthropic compatible: requests are
// encoded as CodeWhisperer conversationState JSON and responses arrive as a
// binary AWS EventStream (not SSE). It is therefore never an ingress format;
// every request to a Kiro provider translates through the IR, and the upstream
// is stream-only (even a non-streaming client request is sent as a stream and
// collected back into a unary response).
package kiro

import "encoding/json"

// DefaultBaseURL is the CodeWhisperer host used when a Kiro provider's base
// URL is left blank. It accepts API-key, IDC, and external_idp credentials;
// UpstreamPath is appended to it.
const DefaultBaseURL = "https://codewhisperer.us-east-1.amazonaws.com"

// UpstreamPath is appended to the provider base URL for the legacy chat endpoint.
const UpstreamPath = "/generateAssistantResponse"

// RuntimeUpstreamPath marks the runtime codec. ChatURL replaces it with the
// regional runtime host root, or with an explicit non-public BaseURL root.
// The active serializer posts to "/", not the unused HTTP operation path.
const RuntimeUpstreamPath = "kiro-runtime:/"

// XAmzTarget identifies the CodeWhisperer streaming operation.
const XAmzTarget = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"

// RuntimeTarget identifies the Kiro Runtime RPC operation. The active AWS JSON
// 1.0 serializer sends this target; it does not apply the HTTP path trait.
const RuntimeTarget = RuntimeServiceTarget

// EventStreamAccept is the upstream Accept value: Kiro returns a binary AWS
// EventStream rather than text/event-stream.
const EventStreamAccept = "application/vnd.amazon.eventstream"

// DefaultMaxTokens is the inferenceConfig.maxTokens value. The Kiro translator
// hardcodes this; the client max_tokens is not honored upstream.
const DefaultMaxTokens = 32000

// Identity headers the upstream expects; a malformed User-Agent is rejected.
const (
	UserAgent     = "AWS-SDK-JS/3.0.0 kiro-ide/1.0.0"
	XAmzUserAgent = "aws-sdk-js/3.0.0 kiro-ide/1.0.0"
	AmzSdkRequest = "attempt=1; max=3"
)

// cwRequest is the top-level CodeWhisperer request body.
type cwRequest struct {
	ConversationState cwConversationState `json:"conversationState"`
	ProfileArn        string              `json:"profileArn,omitempty"`
	InferenceConfig   cwInferenceConfig   `json:"inferenceConfig"`
}

type cwConversationState struct {
	ChatTriggerType string      `json:"chatTriggerType"`
	ConversationID  string      `json:"conversationId"`
	CurrentMessage  cwMessage   `json:"currentMessage"`
	History         []cwHistory `json:"history,omitempty"`
}

// cwMessage wraps the current turn; only user input is ever current.
type cwMessage struct {
	UserInputMessage *cwUserInputMessage `json:"userInputMessage,omitempty"`
}

// cwHistory is one prior turn: exactly one of the two fields is set.
type cwHistory struct {
	UserInputMessage         *cwUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *cwAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type cwUserInputMessage struct {
	Content                 string                     `json:"content"`
	ModelID                 string                     `json:"modelId,omitempty"`
	Origin                  string                     `json:"origin,omitempty"`
	Images                  []cwImage                  `json:"images,omitempty"`
	UserInputMessageContext *cwUserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type cwUserInputMessageContext struct {
	Tools       []cwTool       `json:"tools,omitempty"`
	ToolResults []cwToolResult `json:"toolResults,omitempty"`
}

type cwAssistantResponseMessage struct {
	Content  string      `json:"content"`
	ToolUses []cwToolUse `json:"toolUses,omitempty"`
}

type cwTool struct {
	ToolSpecification cwToolSpecification `json:"toolSpecification"`
}

type cwToolSpecification struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	InputSchema cwInputSchema `json:"inputSchema"`
}

type cwInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type cwToolResult struct {
	ToolUseID string             `json:"toolUseId"`
	Status    string             `json:"status"`
	Content   []cwToolResultText `json:"content"`
}

type cwToolResultText struct {
	Text string `json:"text"`
}

type cwToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type cwImage struct {
	Format string        `json:"format"`
	Source cwImageSource `json:"source"`
}

type cwImageSource struct {
	Bytes string `json:"bytes"`
}

type cwInferenceConfig struct {
	MaxTokens   int      `json:"maxTokens"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

const runtimeOrigin = "AI_EDITOR"

// rtRequest is the official Runtime GenerateAssistantResponse input. The active
// schema writer omits unknown fields, and inferenceConfig is not a member.
// systemPrompt and additionalModelRequestFields stay omitted in this pass.
type rtRequest struct {
	ConversationState rtConversationState `json:"conversationState"`
	ProfileArn        string              `json:"profileArn,omitempty"`
	AgentMode         string              `json:"agentMode,omitempty"`
}

// rtConversationState follows the official conversationState members. Current
// message and chat trigger are required. Continuation and task identity are
// optional and are not fabricated from IR.
type rtConversationState struct {
	CurrentMessage     cwMessage   `json:"currentMessage"`
	ChatTriggerType    string      `json:"chatTriggerType"`
	ConversationID     string      `json:"conversationId,omitempty"`
	History            []cwHistory `json:"history,omitempty"`
	RootConversationID string      `json:"rootConversationId,omitempty"`
}
