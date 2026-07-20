package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"qoder2api/internal/protocol"
	"qoder2api/internal/qoder"
)

type Handler struct {
	backend      qoder.Backend
	defaultModel string
	store        *conversationStore
}

func NewHandler(backend qoder.Backend, defaultModel string) http.Handler {
	h := &Handler{
		backend:      backend,
		defaultModel: defaultModel,
		store:        newConversationStore(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("/v1/responses", h.handleResponses)
	mux.HandleFunc("/v1/messages", h.handleAnthropicMessages)
	mux.HandleFunc("/v1/models", h.handleModels)
	return mux
}

func logRequest(r *http.Request, model string) {
	if model == "" {
		log.Printf("[q2a] %s %s", r.Method, r.URL.Path)
		return
	}
	log.Printf("[q2a] %s %s model=%s", r.Method, r.URL.Path, model)
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	logRequest(r, "")
	models, err := h.backend.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	created := time.Now().Unix()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		id := m.DisplayName
		if id == "" {
			id = m.ID
		}
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": "qoder",
		})
	}
	sort.Slice(data, func(i, j int) bool {
		return data[i]["id"].(string) < data[j]["id"].(string)
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req protocol.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = h.defaultModel
	}
	logRequest(r, model)
	if len(req.ImageURLs) == 0 {
		req.ImageURLs = collectImageURLs(req.Messages)
	}

	ctx := r.Context()
	if req.Stream {
		h.handleStream(ctx, w, req, model)
		return
	}
	h.handleSingle(ctx, w, req, model)
}

func (h *Handler) handleSingle(ctx context.Context, w http.ResponseWriter, req protocol.ChatCompletionRequest, model string) {
	resp, err := h.backend.Complete(ctx, qoder.CompleteRequest{
		Model:            model,
		Messages:         req.Messages,
		Tools:            req.Tools,
		ReasoningEffort:  req.ReasoningEffort,
		ThinkingType:     req.ThinkingType,
		ThinkingBudget:   req.ThinkingBudget,
		ImageURLs:        req.ImageURLs,
		OriginalImageURL: firstImageURL(req.Messages),
		ImageParts:       collectImageParts(req.Messages),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	out := map[string]any{
		"id":      "chatcmpl-" + randomHex(24),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":       "assistant",
				"content":    resp.Content,
				"tool_calls": rawJSONOrNil(resp.ToolCalls),
			},
			"finish_reason": finishReason(resp.ToolCalls),
		}},
		"usage": map[string]any{
			"prompt_tokens":     usagePromptTokens(resp.Usage),
			"completion_tokens": usageCompletionTokens(resp.Usage),
			"total_tokens":      usageTotalTokens(resp.Usage),
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": usageReasoningTokens(resp.Usage),
			},
			"prompt_tokens_details": map[string]any{
				"cached_tokens": usageCachedTokens(resp.Usage),
			},
		},
	}

	if rawJSONIsEmpty(resp.ToolCalls) {
		msg := out["choices"].([]map[string]any)[0]["message"].(map[string]any)
		delete(msg, "tool_calls")
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) handleStream(ctx context.Context, w http.ResponseWriter, req protocol.ChatCompletionRequest, model string) {
	stream, err := h.backend.Stream(ctx, qoder.CompleteRequest{
		Model:            model,
		Messages:         req.Messages,
		Tools:            req.Tools,
		ReasoningEffort:  req.ReasoningEffort,
		ThinkingType:     req.ThinkingType,
		ThinkingBudget:   req.ThinkingBudget,
		ImageURLs:        req.ImageURLs,
		OriginalImageURL: firstImageURL(req.Messages),
		ImageParts:       collectImageParts(req.Messages),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer stream.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}

	reqID := "chatcmpl-" + randomHex(24)
	created := time.Now().Unix()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	toolUsed := false
	for {
		event, err := stream.Next()
		if err != nil {
			return
		}
		if event.Done {
			break
		}
		if !rawJSONIsEmpty(event.ToolCalls) {
			toolUsed = true
		}
		chunk := map[string]any{
			"id":      reqID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": deltaPayload(event),
			}},
		}
		if err := writeSSEJSON(w, chunk); err != nil {
			return
		}
		flusher.Flush()
	}

	reason := "stop"
	if toolUsed {
		reason = "tool_calls"
	}
	done := map[string]any{
		"id":      reqID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	if err := writeSSEJSON(w, done); err != nil {
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func finishReason(toolCalls json.RawMessage) string {
	if rawJSONIsEmpty(toolCalls) {
		return "stop"
	}
	return "tool_calls"
}

func deltaPayload(event protocol.DeltaEvent) map[string]any {
	out := map[string]any{}
	if event.Content != "" {
		out["content"] = event.Content
	}
	if !rawJSONIsEmpty(event.ToolCalls) {
		out["tool_calls"] = rawJSONOrNil(event.ToolCalls)
	}
	return out
}

func rawJSONOrNil(raw json.RawMessage) any {
	if rawJSONIsEmpty(raw) {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func rawJSONIsEmpty(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null" || trimmed == "[]"
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    "qoder_error",
		},
	})
}

func writeSSEJSON(w http.ResponseWriter, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func usagePromptTokens(usage *protocol.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.PromptTokens != 0 {
		return usage.PromptTokens
	}
	return usage.InputTokens
}

func usageCompletionTokens(usage *protocol.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.CompletionTokens != 0 {
		return usage.CompletionTokens
	}
	return usage.OutputTokens
}

func usageTotalTokens(usage *protocol.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.TotalTokens != 0 {
		return usage.TotalTokens
	}
	return usagePromptTokens(usage) + usageCompletionTokens(usage)
}

func usageReasoningTokens(usage *protocol.Usage) int {
	if usage == nil {
		return 0
	}
	return usage.ReasoningTokens
}

func usageCachedTokens(usage *protocol.Usage) int {
	if usage == nil {
		return 0
	}
	return usage.CachedTokens
}

type conversationStore struct {
	mu        sync.RWMutex
	responses map[string]storedResponse
}

type storedResponse struct {
	Model    string
	Messages []protocol.Message
}

func newConversationStore() *conversationStore {
	return &conversationStore{
		responses: make(map[string]storedResponse),
	}
}

func (s *conversationStore) PutResponse(id, model string, messages []protocol.Message) {
	if strings.TrimSpace(id) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[id] = storedResponse{
		Model:    model,
		Messages: cloneMessages(messages),
	}
}

func (s *conversationStore) GetResponse(id string) (storedResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	resp, ok := s.responses[id]
	if !ok {
		return storedResponse{}, false
	}
	resp.Messages = cloneMessages(resp.Messages)
	return resp, true
}

func cloneMessages(messages []protocol.Message) []protocol.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]protocol.Message, 0, len(messages))
	for _, msg := range messages {
		cloned := msg
		if raw := msg.ToolCalls; len(raw) > 0 {
			cloned.ToolCalls = append(json.RawMessage(nil), raw...)
		}
		out = append(out, cloned)
	}
	return out
}

func collectImageURLs(messages []protocol.Message) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, msg := range messages {
		for _, part := range messageContentParts(msg.Content) {
			imageRef := firstNonBlank(part.ImageURL, part.Data, part.FileURL)
			if strings.TrimSpace(imageRef) == "" {
				continue
			}
			if part.Type == "file" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(part.MIMEType)), "image/") && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(imageRef)), "data:image/") {
				continue
			}
			if _, ok := seen[imageRef]; ok {
				continue
			}
			seen[imageRef] = struct{}{}
			out = append(out, imageRef)
		}
	}
	return out
}

func firstImageURL(messages []protocol.Message) string {
	for _, msg := range messages {
		for _, part := range messageContentParts(msg.Content) {
			imageRef := firstNonBlank(part.ImageURL, part.Data, part.FileURL)
			if strings.TrimSpace(imageRef) == "" {
				continue
			}
			if part.Type == "file" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(part.MIMEType)), "image/") && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(imageRef)), "data:image/") {
				continue
			}
			return imageRef
		}
	}
	return ""
}

func collectImageParts(messages []protocol.Message) []protocol.ContentPart {
	var out []protocol.ContentPart
	for _, msg := range messages {
		for _, part := range messageContentParts(msg.Content) {
			imageRef := firstNonBlank(part.ImageURL, part.Data, part.FileURL)
			if strings.TrimSpace(imageRef) == "" {
				continue
			}
			if part.Type == "file" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(part.MIMEType)), "image/") && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(imageRef)), "data:image/") {
				continue
			}
			out = append(out, part)
		}
	}
	return out
}

func messageContentParts(content any) []protocol.ContentPart {
	switch v := content.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []protocol.ContentPart{{Type: "text", Text: v}}
	case []map[string]any:
		return mapsToContentParts(v)
	case []any:
		maps := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if obj, ok := item.(map[string]any); ok {
				maps = append(maps, obj)
			}
		}
		return mapsToContentParts(maps)
	default:
		return nil
	}
}

func mapsToContentParts(parts []map[string]any) []protocol.ContentPart {
	out := make([]protocol.ContentPart, 0, len(parts))
	for _, part := range parts {
		switch stringValue(part["type"]) {
		case "text":
			text := stringValue(part["text"])
			if strings.TrimSpace(text) != "" {
				out = append(out, protocol.ContentPart{Type: "text", Text: text})
			}
		case "image_url":
			imageURL := firstNonBlank(stringValue(part["image_url"]), nestedStringValue(part["image_url"], "url"))
			if strings.TrimSpace(imageURL) != "" {
				out = append(out, protocol.ContentPart{Type: "image_url", ImageURL: imageURL})
			}
		case "file":
			fileURL := firstNonBlank(stringValue(part["file_url"]), stringValue(part["data"]))
			out = append(out, protocol.ContentPart{
				Type:     "file",
				FileID:   stringValue(part["file_id"]),
				FileURL:  fileURL,
				FileName: firstNonBlank(stringValue(part["filename"]), stringValue(part["file_name"])),
				MIMEType: stringValue(part["mime_type"]),
				Data:     stringValue(part["data"]),
			})
		}
	}
	return out
}
