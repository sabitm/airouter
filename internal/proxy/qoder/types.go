package qoder

import "encoding/json"

// qoderRequest is the plaintext chat envelope Qoder expects before WAF encoding.
type qoderRequest struct {
	RequestID    string           `json:"request_id"`
	RequestSetID string           `json:"request_set_id"`
	ChatRecordID string           `json:"chat_record_id"`
	SessionID    string           `json:"session_id"`
	Stream       bool             `json:"stream"`
	ChatTask     string           `json:"chat_task"`
	IsReply      bool             `json:"is_reply"`
	IsRetry      bool             `json:"is_retry"`
	Source       int              `json:"source"`
	Version      string           `json:"version"`
	SessionType  string           `json:"session_type"`
	AgentID      string           `json:"agent_id"`
	TaskID       string           `json:"task_id"`
	CodeLanguage string           `json:"code_language"`
	ChatPrompt   string           `json:"chat_prompt"`
	ImageURLs    any              `json:"image_urls"`
	AliyunUser   string           `json:"aliyun_user_type"`
	System       string           `json:"system"`
	Messages     []map[string]any `json:"messages"`
	Tools        []map[string]any `json:"tools"`
	Parameters   qoderParameters  `json:"parameters"`
	ChatContext  qoderChatContext `json:"chat_context"`
	// ModelConfig is the server-published block; empty until InjectModelConfig.
	ModelConfig json.RawMessage `json:"model_config"`
	Business    qoderBusiness   `json:"business"`
}

type qoderParameters struct {
	MaxTokens int `json:"max_tokens"`
}

type qoderChatContext struct {
	ChatPrompt string         `json:"chatPrompt"`
	ImageURLs  any            `json:"imageUrls"`
	Extra      map[string]any `json:"extra"`
	Features   []any          `json:"features"`
	Text       string         `json:"text"`
}

type qoderBusiness struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Type    string `json:"type"`
	Stage   string `json:"stage"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	BeginAt int64  `json:"begin_at"`
}
