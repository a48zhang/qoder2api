package qoder

import (
	"context"
	"encoding/json"
	"net/http"

	"qoder2api/internal/protocol"
)

type CompleteRequest struct {
	Model                   string
	Messages                []protocol.Message
	Tools                   json.RawMessage
	ReasoningEffort         string
	ThinkingType            string
	ThinkingBudget          int
	ImageURLs               []string
	OriginalImageURL        string
	ImageParts              []protocol.ContentPart
	LegacyPromptOverride    string
	LegacyAllowDefaultTools bool
	LegacySessionID         string
	LegacyRequestSetID      string
	LegacyChatRecordID      string
	LegacyRequestID         string
	LegacyBusinessID        string
	LegacyBusinessBeginAt   int64
}

type CompleteResponse struct {
	Content   string
	Reasoning string
	ToolCalls json.RawMessage
	Usage     *protocol.Usage
}

type Stream interface {
	Next() (protocol.DeltaEvent, error)
	Close() error
}

// ModelInfo 描述一个可用模型，用于 /v1/models 列表返回。
type ModelInfo struct {
	ID          string
	DisplayName string
}

type Backend interface {
	Complete(context.Context, CompleteRequest) (CompleteResponse, error)
	Stream(context.Context, CompleteRequest) (Stream, error)
	Models(context.Context) ([]ModelInfo, error)
}

type Capabilities struct {
	Legacy bool
}

type CapabilityReporter interface {
	Capabilities() Capabilities
}

type Options struct {
	PAT           string
	AuthJSON      string
	BaseURL       string
	WorkspaceRoot string
	DefaultModel  string
}

func NewBackend(opts Options, client *http.Client) (Backend, error) {
	if opts.PAT != "" {
		return NewCloudBackend(opts, client), nil
	}
	return NewLegacyBackend(opts, client)
}
