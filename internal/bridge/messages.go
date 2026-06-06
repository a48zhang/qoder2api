package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"qoder2api/internal/protocol"
	"qoder2api/internal/qoder"
)

func (h *Handler) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req protocol.AnthropicMessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	chatReq, err := normalizeAnthropicMessagesRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	model := strings.TrimSpace(chatReq.Model)
	if model == "" {
		model = h.defaultModel
	}

	ctx := r.Context()
	if chatReq.Stream {
		h.handleAnthropicMessagesStream(ctx, w, req, chatReq, model)
		return
	}
	h.handleAnthropicMessagesSingle(ctx, w, req, chatReq, model)
}

func normalizeAnthropicMessagesRequest(req protocol.AnthropicMessagesRequest) (protocol.ChatCompletionRequest, error) {
	messages := anthropicMessagesToTranscript(req)
	if len(messages) == 0 {
		return protocol.ChatCompletionRequest{}, fmt.Errorf("messages are required")
	}
	imageURLs := collectImageURLs(messages)
	return protocol.ChatCompletionRequest{
		Model:           req.Model,
		Messages:        messages,
		Stream:          req.Stream,
		Tools:           normalizeToolDefinitions(req.Tools),
		ReasoningEffort: anthropicThinkingToReasoningEffort(req.Thinking),
		ThinkingType:    anthropicThinkingType(req.Thinking),
		ThinkingBudget:  anthropicThinkingBudget(req.Thinking),
		ImageURLs:       imageURLs,
	}, nil
}

func anthropicSystemToText(system any) string {
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(obj["type"]) == "text" {
				parts = append(parts, stringValue(obj["text"]))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func normalizeAnthropicContent(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []map[string]any:
		return v
	case []any:
		parts := make([]map[string]any, 0, len(v))
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringValue(obj["type"]) {
			case "text":
				parts = append(parts, map[string]any{
					"type": "text",
					"text": stringValue(obj["text"]),
				})
			case "image":
				if source, ok := obj["source"].(map[string]any); ok {
					if filePart, ok := anthropicImageSourceToFilePart(source); ok {
						parts = append(parts, filePart)
					} else if imageURL := anthropicImageSourceToURL(source); imageURL != "" {
						parts = append(parts, map[string]any{
							"type":      "image_url",
							"image_url": imageURL,
						})
					}
				}
			case "tool_use":
				parts = append(parts, map[string]any{
					"type":  "tool_use",
					"id":    stringValue(obj["id"]),
					"name":  stringValue(obj["name"]),
					"input": objectOrRawString(obj["input"]),
				})
			case "tool_result":
				parts = append(parts, map[string]any{
					"type":        "tool_result",
					"tool_use_id": stringValue(obj["tool_use_id"]),
					"is_error":    obj["is_error"],
					"content":     normalizeToolResultContent(obj["content"]),
				})
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return content
	}
}

func normalizeToolResultContent(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []map[string]any:
		return v
	case []any:
		parts := make([]map[string]any, 0, len(v))
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(obj["type"]) == "text" {
				parts = append(parts, map[string]any{
					"type": "text",
					"text": stringValue(obj["text"]),
				})
			}
			if stringValue(obj["type"]) == "image" {
				if source, ok := obj["source"].(map[string]any); ok {
					if filePart, ok := anthropicImageSourceToFilePart(source); ok {
						parts = append(parts, filePart)
					} else if imageURL := anthropicImageSourceToURL(source); imageURL != "" {
						parts = append(parts, map[string]any{
							"type":      "image_url",
							"image_url": imageURL,
						})
					}
				}
			}
		}
		if len(parts) == 0 {
			return mustJSON(content)
		}
		return parts
	default:
		if content == nil {
			return ""
		}
		return mustJSON(content)
	}
}

func anthropicMessagesToTranscript(req protocol.AnthropicMessagesRequest) []protocol.Message {
	var out []protocol.Message
	if systemText := anthropicSystemToText(req.System); strings.TrimSpace(systemText) != "" {
		out = append(out, protocol.Message{Role: "system", Content: systemText})
	}
	for _, msg := range req.Messages {
		role := firstNonBlank(msg.Role, "user")
		switch role {
		case "assistant":
			content := normalizeAnthropicContent(msg.Content)
			text, toolCalls := extractAnthropicAssistantParts(content)
			out = append(out, protocol.Message{
				Role:      "assistant",
				Content:   text,
				ToolCalls: toolCalls,
			})
		case "user":
			content := normalizeAnthropicContent(msg.Content)
			text, toolMessages := extractAnthropicUserParts(content)
			if userContent, ok := anthropicUserMessageContent(content, text); ok {
				out = append(out, protocol.Message{Role: "user", Content: userContent})
			}
			out = append(out, toolMessages...)
		default:
			out = append(out, protocol.Message{
				Role:    role,
				Content: normalizeAnthropicContent(msg.Content),
			})
		}
	}
	return out
}

func extractAnthropicAssistantParts(content any) (string, json.RawMessage) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []map[string]any:
		return extractAnthropicAssistantPartsFromMaps(v)
	case []any:
		maps := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if obj, ok := item.(map[string]any); ok {
				maps = append(maps, obj)
			}
		}
		return extractAnthropicAssistantPartsFromMaps(maps)
	default:
		return mustJSON(v), nil
	}
}

func extractAnthropicAssistantPartsFromMaps(parts []map[string]any) (string, json.RawMessage) {
	var texts []string
	toolCalls := make([]map[string]any, 0, len(parts))
	for idx, part := range parts {
		switch stringValue(part["type"]) {
		case "text":
			texts = append(texts, stringValue(part["text"]))
		case "tool_use":
			args, _ := json.Marshal(objectOrRawString(part["input"]))
			toolCall := map[string]any{
				"id":   stringValue(part["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      stringValue(part["name"]),
					"arguments": string(args),
				},
			}
			if idx > 0 {
				toolCall["index"] = idx
			}
			toolCalls = append(toolCalls, toolCall)
		}
	}
	var raw json.RawMessage
	if len(toolCalls) > 0 {
		raw, _ = json.Marshal(toolCalls)
	}
	return strings.Join(texts, "\n"), raw
}

func extractAnthropicUserParts(content any) (string, []protocol.Message) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []map[string]any:
		return extractAnthropicUserPartsFromMaps(v)
	case []any:
		maps := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if obj, ok := item.(map[string]any); ok {
				maps = append(maps, obj)
			}
		}
		return extractAnthropicUserPartsFromMaps(maps)
	default:
		return mustJSON(v), nil
	}
}

func extractAnthropicUserPartsFromMaps(parts []map[string]any) (string, []protocol.Message) {
	var texts []string
	var tools []protocol.Message
	for _, part := range parts {
		switch stringValue(part["type"]) {
		case "text":
			texts = append(texts, stringValue(part["text"]))
		case "tool_result":
			tools = append(tools, protocol.Message{
				Role:       "tool",
				ToolCallID: stringValue(part["tool_use_id"]),
				Content:    normalizeToolResultContent(part["content"]),
			})
		}
	}
	return strings.Join(texts, "\n"), tools
}

func anthropicUserMessageContent(content any, text string) (any, bool) {
	switch v := content.(type) {
	case []map[string]any:
		return anthropicRenderableUserContentFromMaps(v, text)
	case []any:
		maps := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if obj, ok := item.(map[string]any); ok {
				maps = append(maps, obj)
			}
		}
		return anthropicRenderableUserContentFromMaps(maps, text)
	default:
		if strings.TrimSpace(text) == "" {
			return nil, false
		}
		return text, true
	}
}

func anthropicRenderableUserContentFromMaps(parts []map[string]any, text string) (any, bool) {
	renderable := make([]map[string]any, 0, len(parts))
	hasMedia := false
	for _, part := range parts {
		switch stringValue(part["type"]) {
		case "text":
			if strings.TrimSpace(stringValue(part["text"])) != "" {
				renderable = append(renderable, part)
			}
		case "image_url", "file":
			renderable = append(renderable, part)
			hasMedia = true
		}
	}
	if hasMedia {
		return renderable, true
	}
	if strings.TrimSpace(text) != "" {
		return text, true
	}
	return nil, false
}

func (h *Handler) handleAnthropicMessagesSingle(ctx context.Context, w http.ResponseWriter, original protocol.AnthropicMessagesRequest, normalized protocol.ChatCompletionRequest, model string) {
	resp, err := h.backend.Complete(ctx, qoder.CompleteRequest{
		Model:            model,
		Messages:         normalized.Messages,
		Tools:            normalized.Tools,
		ReasoningEffort:  normalized.ReasoningEffort,
		ThinkingType:     normalized.ThinkingType,
		ThinkingBudget:   normalized.ThinkingBudget,
		ImageURLs:        normalized.ImageURLs,
		OriginalImageURL: firstImageURL(normalized.Messages),
		ImageParts:       collectImageParts(normalized.Messages),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	content := []map[string]any{}
	exposeThinking := anthropicShouldExposeThinking(normalized)
	if exposeThinking && strings.TrimSpace(resp.Reasoning) != "" {
		signature := anthropicThinkingSignature(resp.Reasoning)
		content = append(content, map[string]any{
			"type":      "thinking",
			"thinking":  resp.Reasoning,
			"signature": signature,
		})
	}
	if resp.Content != "" {
		content = append(content, map[string]any{
			"type": "text",
			"text": resp.Content,
		})
	}
	if !rawJSONIsEmpty(resp.ToolCalls) {
		if decoded := rawJSONOrNil(resp.ToolCalls); decoded != nil {
			if arr, ok := decoded.([]any); ok {
				for _, item := range arr {
					obj, ok := item.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := obj["function"].(map[string]any)
					name := firstNonBlank(stringValue(fn["name"]), "tool_call")
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    firstNonBlank(stringValue(obj["id"]), "toolu_"+randomHex(16)),
						"name":  name,
						"input": mustJSONObject(stringValue(fn["arguments"])),
					})
				}
			}
		}
	}

	stopReason := "end_turn"
	if !rawJSONIsEmpty(resp.ToolCalls) {
		stopReason = "tool_use"
	}

	out := map[string]any{
		"id":            "msg_" + randomHex(24),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                usagePromptTokens(resp.Usage),
			"output_tokens":               usageCompletionTokens(resp.Usage),
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     usageCachedTokens(resp.Usage),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) handleAnthropicMessagesStream(ctx context.Context, w http.ResponseWriter, original protocol.AnthropicMessagesRequest, normalized protocol.ChatCompletionRequest, model string) {
	if h.shouldFallbackAnthropicImageStream(normalized) {
		h.handleAnthropicMessagesStreamFallback(ctx, w, normalized, model)
		return
	}

	stream, err := h.backend.Stream(ctx, qoder.CompleteRequest{
		Model:            model,
		Messages:         normalized.Messages,
		Tools:            normalized.Tools,
		ReasoningEffort:  normalized.ReasoningEffort,
		ThinkingType:     normalized.ThinkingType,
		ThinkingBudget:   normalized.ThinkingBudget,
		ImageURLs:        normalized.ImageURLs,
		OriginalImageURL: firstImageURL(normalized.Messages),
		ImageParts:       collectImageParts(normalized.Messages),
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

	messageID := "msg_" + randomHex(24)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	writeAnthropicEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	})
	flusher.Flush()

	toolUsed := false
	currentIndex := 0
	currentBlockType := ""
	var usage *protocol.Usage
	var currentThinking strings.Builder
	exposeThinking := anthropicShouldExposeThinking(normalized)
	toolStates := map[int]anthropicToolStreamState{}
	for {
		event, err := stream.Next()
		if err != nil {
			return
		}
		if event.Done {
			break
		}
		if event.Usage != nil {
			usage = event.Usage
		}
		if exposeThinking && event.Reasoning != "" {
			if currentBlockType != "thinking" {
				if currentBlockType != "" {
					writeAnthropicEvent(w, "content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": currentIndex,
					})
					currentIndex++
				}
				writeAnthropicEvent(w, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": currentIndex,
					"content_block": map[string]any{
						"type":      "thinking",
						"thinking":  "",
						"signature": "",
					},
				})
				currentBlockType = "thinking"
				currentThinking.Reset()
			}
			currentThinking.WriteString(event.Reasoning)
			writeAnthropicEvent(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": currentIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": event.Reasoning,
				},
			})
			flusher.Flush()
		}
		if event.Content != "" {
			if currentBlockType != "text" {
				if currentBlockType != "" {
					if currentBlockType == "thinking" && currentThinking.Len() > 0 {
						writeAnthropicEvent(w, "content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": currentIndex,
							"delta": map[string]any{
								"type":      "signature_delta",
								"signature": anthropicThinkingSignature(currentThinking.String()),
							},
						})
					}
					writeAnthropicEvent(w, "content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": currentIndex,
					})
					currentIndex++
				}
				writeAnthropicEvent(w, "content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": currentIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				currentBlockType = "text"
			}
			writeAnthropicEvent(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": currentIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": event.Content,
				},
			})
			flusher.Flush()
		}
		if !rawJSONIsEmpty(event.ToolCalls) {
			toolUsed = true
			if currentBlockType != "" {
				if currentBlockType == "thinking" && currentThinking.Len() > 0 {
					writeAnthropicEvent(w, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": currentIndex,
						"delta": map[string]any{
							"type":      "signature_delta",
							"signature": anthropicThinkingSignature(currentThinking.String()),
						},
					})
				}
				writeAnthropicEvent(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": currentIndex,
				})
				currentIndex++
				currentBlockType = ""
			}
			blocks, nextStates, nextIndex := anthropicToolUseBlocks(event.ToolCalls, toolStates, currentIndex)
			toolStates = nextStates
			currentIndex = nextIndex
			for _, block := range blocks {
				if block.start == nil {
					continue
				}
				writeAnthropicEvent(w, "content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         block.index,
					"content_block": block.start,
				})
			}
			for _, block := range blocks {
				if block.partialJSON != "" {
					writeAnthropicEvent(w, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": block.index,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": block.partialJSON,
						},
					})
				}
				if block.stop {
					writeAnthropicEvent(w, "content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": block.index,
					})
				}
			}
			flusher.Flush()
		}
	}

	for _, state := range toolStates {
		if state.open {
			writeAnthropicEvent(w, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": state.blockIndex,
			})
		}
	}

	if currentBlockType != "" {
		if currentBlockType == "thinking" && currentThinking.Len() > 0 {
			writeAnthropicEvent(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": currentIndex,
				"delta": map[string]any{
					"type":      "signature_delta",
					"signature": anthropicThinkingSignature(currentThinking.String()),
				},
			})
		}
		writeAnthropicEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": currentIndex,
		})
	}
	writeAnthropicEvent(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": func() string {
				if toolUsed {
					return "tool_use"
				}
				return "end_turn"
			}(),
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":                usagePromptTokens(usage),
			"output_tokens":               usageCompletionTokens(usage),
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     usageCachedTokens(usage),
		},
	})
	writeAnthropicEvent(w, "message_stop", map[string]any{
		"type": "message_stop",
	})
	flusher.Flush()
}

func (h *Handler) shouldFallbackAnthropicImageStream(normalized protocol.ChatCompletionRequest) bool {
	if len(normalized.ImageURLs) == 0 {
		return false
	}
	if !rawJSONIsEmpty(normalized.Tools) {
		return false
	}
	reporter, ok := h.backend.(qoder.CapabilityReporter)
	if !ok {
		return false
	}
	return reporter.Capabilities().Legacy
}

func (h *Handler) handleAnthropicMessagesStreamFallback(ctx context.Context, w http.ResponseWriter, normalized protocol.ChatCompletionRequest, model string) {
	resp, err := h.backend.Complete(ctx, qoder.CompleteRequest{
		Model:            model,
		Messages:         normalized.Messages,
		Tools:            normalized.Tools,
		ReasoningEffort:  normalized.ReasoningEffort,
		ThinkingType:     normalized.ThinkingType,
		ThinkingBudget:   normalized.ThinkingBudget,
		ImageURLs:        normalized.ImageURLs,
		OriginalImageURL: firstImageURL(normalized.Messages),
		ImageParts:       collectImageParts(normalized.Messages),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}

	messageID := "msg_" + randomHex(24)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	writeAnthropicEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	})
	flusher.Flush()

	contentIndex := 0
	exposeThinking := anthropicShouldExposeThinking(normalized)
	if exposeThinking && strings.TrimSpace(resp.Reasoning) != "" {
		writeAnthropicEvent(w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": contentIndex,
			"content_block": map[string]any{
				"type":      "thinking",
				"thinking":  "",
				"signature": "",
			},
		})
		writeAnthropicEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": contentIndex,
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": resp.Reasoning,
			},
		})
		writeAnthropicEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": contentIndex,
			"delta": map[string]any{
				"type":      "signature_delta",
				"signature": anthropicThinkingSignature(resp.Reasoning),
			},
		})
		writeAnthropicEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": contentIndex,
		})
		contentIndex++
	}

	toolUsed := !rawJSONIsEmpty(resp.ToolCalls)
	if resp.Content != "" {
		writeAnthropicEvent(w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": contentIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		})
		writeAnthropicEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": contentIndex,
			"delta": map[string]any{
				"type": "text_delta",
				"text": resp.Content,
			},
		})
		writeAnthropicEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": contentIndex,
		})
		contentIndex++
	}

	if toolUsed {
		blocks, _, _ := anthropicToolUseBlocks(resp.ToolCalls, nil, contentIndex)
		for _, block := range blocks {
			if block.start == nil {
				continue
			}
			writeAnthropicEvent(w, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         block.index,
				"content_block": block.start,
			})
			if block.partialJSON != "" {
				writeAnthropicEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": block.index,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": block.partialJSON,
					},
				})
			}
			writeAnthropicEvent(w, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": block.index,
			})
		}
	}

	writeAnthropicEvent(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": func() string {
				if toolUsed {
					return "tool_use"
				}
				return "end_turn"
			}(),
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":                usagePromptTokens(resp.Usage),
			"output_tokens":               usageCompletionTokens(resp.Usage),
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     usageCachedTokens(resp.Usage),
		},
	})
	writeAnthropicEvent(w, "message_stop", map[string]any{
		"type": "message_stop",
	})
	flusher.Flush()
}

func writeAnthropicEvent(w http.ResponseWriter, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}

func mustJSONObject(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func mustJSON(v any) string {
	buf, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(buf)
}

func objectOrRawString(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case []any:
		return x
	case string:
		var out any
		if err := json.Unmarshal([]byte(x), &out); err == nil {
			return out
		}
		return map[string]any{"value": x}
	default:
		return map[string]any{}
	}
}

func anthropicThinkingSignature(thinking string) string {
	sum := sha256.Sum256([]byte(thinking))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func anthropicShouldExposeThinking(req protocol.ChatCompletionRequest) bool {
	if req.ThinkingBudget > 0 {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(req.ThinkingType)) {
	case "enabled", "adaptive":
		return true
	default:
		return false
	}
}

func anthropicThinkingToReasoningEffort(thinking *struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}) string {
	if thinking == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "", "disabled":
		return ""
	case "enabled":
		return budgetToReasoningEffort(thinking.BudgetTokens)
	case "adaptive":
		if thinking.BudgetTokens > 0 {
			return budgetToReasoningEffort(thinking.BudgetTokens)
		}
		return "medium"
	default:
		if thinking.BudgetTokens > 0 {
			return budgetToReasoningEffort(thinking.BudgetTokens)
		}
		return ""
	}
}

func anthropicThinkingType(thinking *struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}) string {
	if thinking == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(thinking.Type))
}

func anthropicThinkingBudget(thinking *struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}) int {
	if thinking == nil {
		return 0
	}
	return thinking.BudgetTokens
}

func anthropicImageSourceToURL(source map[string]any) string {
	sourceType := stringValue(source["type"])
	switch sourceType {
	case "url":
		return stringValue(source["url"])
	case "base64":
		data := stringValue(source["data"])
		mediaType := firstNonBlank(stringValue(source["media_type"]), "application/octet-stream")
		if strings.TrimSpace(data) == "" {
			return ""
		}
		return "data:" + mediaType + ";base64," + data
	default:
		return ""
	}
}

func anthropicImageSourceToFilePart(source map[string]any) (map[string]any, bool) {
	if stringValue(source["type"]) != "base64" {
		return nil, false
	}
	dataURL := anthropicImageSourceToURL(source)
	if strings.TrimSpace(dataURL) == "" {
		return nil, false
	}
	mimeType := firstNonBlank(stringValue(source["media_type"]), "application/octet-stream")
	part := map[string]any{
		"type":      "file",
		"data":      dataURL,
		"mime_type": mimeType,
	}
	if filename := anthropicImageFilename(mimeType); filename != "" {
		part["filename"] = filename
	}
	return part, true
}

func anthropicImageFilename(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return "image.jpg"
	case "image/gif":
		return "image.gif"
	case "image/webp":
		return "image.webp"
	case "image/bmp":
		return "image.bmp"
	case "image/svg+xml":
		return "image.svg"
	case "image/png":
		return "image.png"
	default:
		return "image.bin"
	}
}

func hasRenderableUserContent(content any) bool {
	switch v := content.(type) {
	case []map[string]any:
		for _, item := range v {
			switch stringValue(item["type"]) {
			case "image_url", "file":
				return true
			}
		}
	case []any:
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringValue(obj["type"]) {
			case "image_url", "file":
				return true
			}
		}
	}
	return false
}

type anthropicToolBlock struct {
	index       int
	start       map[string]any
	partialJSON string
	stop        bool
}

type anthropicToolStreamState struct {
	blockIndex int
	id         string
	name       string
	open       bool
}

func anthropicToolUseBlocks(raw json.RawMessage, states map[int]anthropicToolStreamState, currentIndex int) ([]anthropicToolBlock, map[int]anthropicToolStreamState, int) {
	deltas := qoder.ParseToolCallDeltas(raw)
	if len(deltas) == 0 {
		return nil, states, currentIndex
	}
	blocks := make([]anthropicToolBlock, 0, len(deltas))
	nextStates := make(map[int]anthropicToolStreamState, len(states))
	for k, v := range states {
		nextStates[k] = v
	}
	for _, delta := range deltas {
		state, ok := nextStates[delta.Index]
		if !ok {
			state = anthropicToolStreamState{
				blockIndex: currentIndex,
			}
			currentIndex++
		}
		if delta.ID != "" {
			state.id = delta.ID
		}
		if delta.FunctionName != "" {
			state.name = delta.FunctionName
		}
		block := anthropicToolBlock{
			index:       state.blockIndex,
			partialJSON: delta.ArgumentsFragment,
		}
		if !state.open && (state.name != "" || delta.FunctionName != "") {
			state.open = true
			block.start = map[string]any{
				"type":  "tool_use",
				"id":    firstNonBlank(state.id, "toolu_"+randomHex(16)),
				"name":  firstNonBlank(state.name, "tool_call"),
				"input": map[string]any{},
			}
		}
		nextStates[delta.Index] = state
		blocks = append(blocks, block)
	}
	return blocks, nextStates, currentIndex
}
