package protocol

import "encoding/json"

type ChatCompletionRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Stream          bool            `json:"stream"`
	Tools           json.RawMessage `json:"tools,omitempty"`
	ReasoningEffort string          `json:"-"`
	ThinkingType    string          `json:"-"`
	ThinkingBudget  int             `json:"-"`
	ImageURLs       []string        `json:"-"`
}

type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              any             `json:"input"`
	Instructions       *string         `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Stream             bool            `json:"stream"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	Reasoning          *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
}

type AnthropicMessagesRequest struct {
	Model     string          `json:"model"`
	System    any             `json:"system,omitempty"`
	Messages  []Message       `json:"messages"`
	Stream    bool            `json:"stream"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Tools     json.RawMessage `json:"tools,omitempty"`
	Thinking  *struct {
		Type         string `json:"type,omitempty"`
		BudgetTokens int    `json:"budget_tokens,omitempty"`
	} `json:"thinking,omitempty"`
}

type Message struct {
	Role       string          `json:"role"`
	Content    any             `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

type ContentPart struct {
	Type      string
	Text      string
	ImageURL  string
	MIMEType  string
	FileName  string
	FileID    string
	FileURL   string
	Data      string
	MediaType string
}

type DeltaEvent struct {
	Content   string
	Reasoning string
	ToolCalls json.RawMessage
	Usage     *Usage
	Done      bool
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int
	CachedTokens     int
	InputTokens      int
	OutputTokens     int
}
