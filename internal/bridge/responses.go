package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"qoder2api/internal/protocol"
	"qoder2api/internal/qoder"
)

func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req protocol.ResponsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	chatReq, err := normalizeResponsesRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.PreviousResponseID != "" {
		previous, ok := h.store.GetResponse(req.PreviousResponseID)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("unknown previous_response_id: %s", req.PreviousResponseID))
			return
		}
		chatReq.Messages = append(previous.Messages, chatReq.Messages...)
		if strings.TrimSpace(chatReq.Model) == "" {
			chatReq.Model = previous.Model
		}
	}

	model := strings.TrimSpace(chatReq.Model)
	if model == "" {
		model = h.defaultModel
	}

	ctx := r.Context()
	if chatReq.Stream {
		h.handleResponsesStream(ctx, w, req, chatReq, model)
		return
	}
	h.handleResponsesSingle(ctx, w, req, chatReq, model)
}

func normalizeResponsesRequest(req protocol.ResponsesRequest) (protocol.ChatCompletionRequest, error) {
	messages, err := responsesInputToMessages(req.Input)
	if err != nil {
		return protocol.ChatCompletionRequest{}, err
	}
	if req.Instructions != nil && strings.TrimSpace(*req.Instructions) != "" {
		messages = append([]protocol.Message{{
			Role:    "system",
			Content: *req.Instructions,
		}}, messages...)
	}
	reasoningEffort := ""
	if req.Reasoning != nil {
		reasoningEffort = normalizeReasoningEffort(req.Reasoning.Effort)
	}
	imageURLs := collectImageURLs(messages)
	return protocol.ChatCompletionRequest{
		Model:           req.Model,
		Messages:        messages,
		Stream:          req.Stream,
		Tools:           normalizeToolDefinitions(req.Tools),
		ReasoningEffort: reasoningEffort,
		ImageURLs:       imageURLs,
	}, nil
}

func responsesInputToMessages(input any) ([]protocol.Message, error) {
	switch v := input.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("input is required")
		}
		return []protocol.Message{{Role: "user", Content: v}}, nil
	case []any:
		var out []protocol.Message
		for _, item := range v {
			msg, ok, err := responsesItemToMessage(item)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, msg)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("input produced no messages")
		}
		return out, nil
	case map[string]any:
		msg, ok, err := responsesItemToMessage(v)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unsupported input object")
		}
		return []protocol.Message{msg}, nil
	default:
		return nil, fmt.Errorf("unsupported input type")
	}
}

func responsesItemToMessage(item any) (protocol.Message, bool, error) {
	obj, ok := item.(map[string]any)
	if !ok {
		return protocol.Message{}, false, nil
	}
	itemType := stringValue(obj["type"])
	switch itemType {
	case "", "message", "input_text":
		role := firstNonBlank(stringValue(obj["role"]), "user")
		if itemType == "input_text" {
			role = "user"
		}
		content := obj["content"]
		if itemType == "input_text" {
			content = stringValue(obj["text"])
		}
		return protocol.Message{
			Role:    role,
			Content: normalizeResponsesContent(content),
		}, true, nil
	case "function_call":
		toolCalls, err := responsesFunctionCallToToolCalls(obj)
		if err != nil {
			return protocol.Message{}, false, err
		}
		return protocol.Message{
			Role:      "assistant",
			Content:   "",
			ToolCalls: toolCalls,
		}, true, nil
	case "function_call_output":
		return protocol.Message{
			Role:       "tool",
			Content:    normalizeFunctionCallOutputContent(obj["output"]),
			ToolCallID: stringValue(obj["call_id"]),
		}, true, nil
	default:
		return protocol.Message{}, false, nil
	}
}

func normalizeResponsesContent(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := normalizeResponsesParts(v)
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return content
	}
}

func normalizeResponsesParts(items []any) []map[string]any {
	parts := make([]map[string]any, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType := stringValue(obj["type"])
		switch itemType {
		case "input_text", "output_text", "text":
			text := firstNonBlank(stringValue(obj["text"]), stringValue(obj["content"]))
			if strings.TrimSpace(text) == "" {
				continue
			}
			parts = append(parts, map[string]any{
				"type": "text",
				"text": text,
			})
		case "input_image", "image_url":
			url := firstNonBlank(stringValue(obj["image_url"]), nestedStringValue(obj["image_url"], "url"))
			if strings.TrimSpace(url) == "" {
				continue
			}
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": url,
			})
		case "input_file":
			part := map[string]any{"type": "file"}
			if id := stringValue(obj["file_id"]); id != "" {
				part["file_id"] = id
			}
			if url := firstNonBlank(stringValue(obj["file_url"]), nestedStringValue(obj["file_data"], "url")); url != "" {
				part["file_url"] = url
			}
			if data := firstNonBlank(stringValue(obj["file_data"]), stringValue(obj["data"])); data != "" {
				part["data"] = data
			}
			if name := firstNonBlank(stringValue(obj["filename"]), stringValue(obj["file_name"])); name != "" {
				part["filename"] = name
			}
			if mime := firstNonBlank(stringValue(obj["mime_type"]), stringValue(obj["media_type"])); mime != "" {
				part["mime_type"] = mime
			}
			if len(part) > 1 {
				parts = append(parts, part)
			}
		}
	}
	return parts
}

func responsesFunctionCallToToolCalls(obj map[string]any) (json.RawMessage, error) {
	callID := firstNonBlank(stringValue(obj["call_id"]), stringValue(obj["id"]), "call_"+randomHex(16))
	arguments := firstNonBlank(stringValue(obj["arguments"]), "{}")
	toolCalls := []map[string]any{{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      stringValue(obj["name"]),
			"arguments": arguments,
		},
	}}
	buf, err := json.Marshal(toolCalls)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func normalizeFunctionCallOutputContent(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []map[string]any:
		return normalizeResponsesParts(mapsToAnySlice(v))
	case []any:
		parts := normalizeResponsesParts(v)
		if len(parts) == 0 {
			return mustJSON(content)
		}
		return parts
	case nil:
		return ""
	default:
		buf, _ := json.Marshal(v)
		return string(buf)
	}
}

func mapsToAnySlice(items []map[string]any) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func (h *Handler) handleResponsesSingle(ctx context.Context, w http.ResponseWriter, original protocol.ResponsesRequest, normalized protocol.ChatCompletionRequest, model string) {
	resp, err := h.backend.Complete(ctx, qoder.CompleteRequest{
		Model:            model,
		Messages:         normalized.Messages,
		Tools:            normalized.Tools,
		ReasoningEffort:  normalized.ReasoningEffort,
		ImageURLs:        normalized.ImageURLs,
		OriginalImageURL: firstImageURL(normalized.Messages),
		ImageParts:       collectImageParts(normalized.Messages),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	output := []map[string]any{}
	if strings.TrimSpace(resp.Reasoning) != "" {
		output = append(output, buildResponsesReasoningItem("rs_"+randomHex(24), "completed", resp.Reasoning))
	}
	if resp.Content != "" {
		output = append(output, map[string]any{
			"type":    "message",
			"id":      "msg_" + randomHex(24),
			"role":    "assistant",
			"status":  "completed",
			"content": []map[string]any{{"type": "output_text", "text": resp.Content}},
		})
	}
	if !rawJSONIsEmpty(resp.ToolCalls) {
		var toolCalls []map[string]any
		if decoded := rawJSONOrNil(resp.ToolCalls); decoded != nil {
			if arr, ok := decoded.([]any); ok {
				for _, item := range arr {
					obj, ok := item.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := obj["function"].(map[string]any)
					toolCalls = append(toolCalls, map[string]any{
						"type":    "function_call",
						"id":      "fc_" + firstNonBlank(stringValue(obj["id"]), randomHex(16)),
						"call_id": firstNonBlank(stringValue(obj["id"]), "call_"+randomHex(16)),
						"status":  "completed",
						"name":    stringValue(fn["name"]),
						"arguments": firstNonBlank(
							stringValue(fn["arguments"]),
							"{}",
						),
					})
				}
			}
		}
		output = append(output, toolCalls...)
	}

	out := map[string]any{
		"id":         "resp_" + randomHex(24),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"model":      model,
		"status":     "completed",
		"output":     output,
	}
	if original.PreviousResponseID != "" {
		out["previous_response_id"] = original.PreviousResponseID
	}
	if len(output) > 0 {
		out["output_text"] = collectResponsesOutputText(output)
	}
	out["usage"] = map[string]any{
		"input_tokens":  usagePromptTokens(resp.Usage),
		"output_tokens": usageCompletionTokens(resp.Usage),
		"total_tokens":  usageTotalTokens(resp.Usage),
		"input_tokens_details": map[string]any{
			"cached_tokens": usageCachedTokens(resp.Usage),
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": usageReasoningTokens(resp.Usage),
		},
	}
	h.store.PutResponse(out["id"].(string), model, appendAssistantResponseMessages(normalized.Messages, resp))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) handleResponsesStream(ctx context.Context, w http.ResponseWriter, original protocol.ResponsesRequest, normalized protocol.ChatCompletionRequest, model string) {
	stream, err := h.backend.Stream(ctx, qoder.CompleteRequest{
		Model:            model,
		Messages:         normalized.Messages,
		Tools:            normalized.Tools,
		ReasoningEffort:  normalized.ReasoningEffort,
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

	responseID := "resp_" + randomHex(24)
	createdAt := time.Now().Unix()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	writeResponsesEvent(w, "response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "model": model, "status": "in_progress", "previous_response_id": emptyToNil(original.PreviousResponseID)},
	})
	writeResponsesEvent(w, "response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "model": model, "status": "in_progress", "previous_response_id": emptyToNil(original.PreviousResponseID)},
	})
	flusher.Flush()

	toolStates := map[int]responsesToolStreamState{}
	nextOutputIndex := 0
	var content strings.Builder
	var reasoning strings.Builder
	var usage *protocol.Usage
	messageState := responsesMessageState{}
	reasoningState := responsesReasoningState{}
	for {
		event, err := stream.Next()
		if err != nil {
			return
		}
		if event.Done {
			break
		}
		if event.Reasoning != "" {
			if !reasoningState.itemAdded || reasoningState.itemDone {
				reasoningState = responsesReasoningState{
					itemID:      "rs_" + randomHex(24),
					outputIndex: nextOutputIndex,
					itemAdded:   true,
				}
				nextOutputIndex++
				writeResponsesEvent(w, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"response_id":  responseID,
					"output_index": reasoningState.outputIndex,
					"item":         buildResponsesReasoningItem(reasoningState.itemID, "in_progress", ""),
				})
			}
			reasoning.WriteString(event.Reasoning)
			reasoningState.summary.WriteString(event.Reasoning)
			writeResponsesEvent(w, "response.reasoning_summary_text.delta", map[string]any{
				"type":          "response.reasoning_summary_text.delta",
				"response_id":   responseID,
				"item_id":       reasoningState.itemID,
				"output_index":  reasoningState.outputIndex,
				"summary_index": 0,
				"delta":         event.Reasoning,
			})
			flusher.Flush()
		}
		if event.Content != "" {
			if reasoningState.itemAdded && !reasoningState.itemDone {
				writeResponsesReasoningDone(w, responseID, reasoningState)
				reasoningState.itemDone = true
			}
			if !messageState.itemAdded {
				messageState = responsesMessageState{
					itemID:      "msg_" + randomHex(24),
					outputIndex: nextOutputIndex,
					itemAdded:   true,
				}
				nextOutputIndex++
				writeResponsesEvent(w, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"response_id":  responseID,
					"output_index": messageState.outputIndex,
					"item": map[string]any{
						"id":      messageState.itemID,
						"type":    "message",
						"status":  "in_progress",
						"content": []any{},
						"role":    "assistant",
					},
				})
				messageState.itemAdded = true
			}
			if !messageState.contentAdded {
				writeResponsesEvent(w, "response.content_part.added", map[string]any{
					"type":          "response.content_part.added",
					"response_id":   responseID,
					"item_id":       messageState.itemID,
					"output_index":  messageState.outputIndex,
					"content_index": 0,
					"part": map[string]any{
						"type":        "output_text",
						"annotations": []any{},
						"logprobs":    []any{},
						"text":        "",
					},
				})
				messageState.contentAdded = true
			}
			content.WriteString(event.Content)
			writeResponsesEvent(w, "response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"response_id":   responseID,
				"item_id":       messageState.itemID,
				"output_index":  messageState.outputIndex,
				"content_index": 0,
				"delta":         event.Content,
			})
			flusher.Flush()
		}
		if !rawJSONIsEmpty(event.ToolCalls) {
			if reasoningState.itemAdded && !reasoningState.itemDone {
				writeResponsesReasoningDone(w, responseID, reasoningState)
				reasoningState.itemDone = true
			}
			if messageState.itemAdded && !messageState.itemDone {
				writeResponsesMessageDone(w, responseID, messageState, content.String())
				messageState.itemDone = true
			}
			events, nextStates, nextIndex := responsesToolEvents(event.ToolCalls, responseID, toolStates, nextOutputIndex)
			toolStates = nextStates
			nextOutputIndex = nextIndex
			for _, payload := range events {
				writeResponsesEvent(w, stringValue(payload["type"]), payload)
			}
			flusher.Flush()
		}
		if event.Usage != nil {
			usage = event.Usage
		}
	}

	for _, idx := range sortedResponseToolStateIndexes(toolStates) {
		state := toolStates[idx]
		if !state.done {
			writeResponsesEvent(w, "response.function_call_arguments.done", map[string]any{
				"type":         "response.function_call_arguments.done",
				"response_id":  responseID,
				"item_id":      state.itemID,
				"output_index": state.outputIndex,
				"call_id":      state.callID,
				"arguments":    state.arguments.String(),
			})
			writeResponsesEvent(w, "response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"response_id":  responseID,
				"output_index": state.outputIndex,
				"item": map[string]any{
					"id":        state.itemID,
					"type":      "function_call",
					"status":    "completed",
					"arguments": state.arguments.String(),
					"call_id":   state.callID,
					"name":      state.name,
				},
			})
			state.done = true
		}
	}

	if reasoningState.itemAdded && !reasoningState.itemDone {
		writeResponsesReasoningDone(w, responseID, reasoningState)
		reasoningState.itemDone = true
	}

	if messageState.itemAdded && !messageState.itemDone {
		writeResponsesMessageDone(w, responseID, messageState, content.String())
	}

	h.store.PutResponse(responseID, model, appendAssistantResponseMessages(normalized.Messages, qoder.CompleteResponse{
		Content:   content.String(),
		Reasoning: reasoning.String(),
		ToolCalls: aggregatedResponsesToolCalls(toolStates),
	}))

	writeResponsesEvent(w, "response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":                   responseID,
			"object":               "response",
			"created_at":           createdAt,
			"model":                model,
			"status":               "completed",
			"previous_response_id": emptyToNil(original.PreviousResponseID),
			"usage": map[string]any{
				"input_tokens":  usagePromptTokens(usage),
				"output_tokens": usageCompletionTokens(usage),
				"total_tokens":  usageTotalTokens(usage),
				"input_tokens_details": map[string]any{
					"cached_tokens": usageCachedTokens(usage),
				},
				"output_tokens_details": map[string]any{
					"reasoning_tokens": usageReasoningTokens(usage),
				},
			},
		},
	})
	writeResponsesEvent(w, "response.done", map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"id":                   responseID,
			"object":               "response",
			"created_at":           createdAt,
			"model":                model,
			"status":               "completed",
			"previous_response_id": emptyToNil(original.PreviousResponseID),
			"usage": map[string]any{
				"input_tokens":  usagePromptTokens(usage),
				"output_tokens": usageCompletionTokens(usage),
				"total_tokens":  usageTotalTokens(usage),
				"input_tokens_details": map[string]any{
					"cached_tokens": usageCachedTokens(usage),
				},
				"output_tokens_details": map[string]any{
					"reasoning_tokens": usageReasoningTokens(usage),
				},
			},
		},
	})
	flusher.Flush()
}

func appendAssistantResponseMessages(messages []protocol.Message, resp qoder.CompleteResponse) []protocol.Message {
	out := cloneMessages(messages)
	if !rawJSONIsEmpty(resp.ToolCalls) {
		out = append(out, protocol.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: append(json.RawMessage(nil), resp.ToolCalls...),
		})
		return out
	}
	if strings.TrimSpace(resp.Content) != "" {
		out = append(out, protocol.Message{
			Role:    "assistant",
			Content: resp.Content,
		})
	}
	return out
}

func aggregatedResponsesToolCalls(states map[int]responsesToolStreamState) json.RawMessage {
	if len(states) == 0 {
		return nil
	}
	var acc qoder.ToolCallAccumulator
	indexes := make([]int, 0, len(states))
	for idx := range states {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	for _, idx := range indexes {
		state := states[idx]
		acc.AddDelta(qoder.ToolCallDelta{
			Index:             idx,
			ID:                state.callID,
			Type:              "function",
			FunctionName:      state.name,
			ArgumentsFragment: state.arguments.String(),
		})
	}
	return acc.RawJSON()
}

func emptyToNil(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func writeResponsesEvent(w http.ResponseWriter, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}

func collectResponsesOutputText(output []map[string]any) string {
	var parts []string
	for _, item := range output {
		if stringValue(item["type"]) != "message" {
			continue
		}
		content, _ := item["content"].([]map[string]any)
		for _, part := range content {
			if stringValue(part["type"]) == "output_text" {
				parts = append(parts, stringValue(part["text"]))
			}
		}
	}
	return strings.Join(parts, "")
}

func stringValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nestedStringValue(v any, key string) string {
	obj, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return stringValue(obj[key])
}

func normalizeReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low", "minimal":
		return "low"
	case "medium", "auto":
		return "medium"
	case "high":
		return "high"
	case "max", "xhigh", "x-high":
		return "xhigh"
	case "none", "disabled":
		return ""
	default:
		return ""
	}
}

func budgetToReasoningEffort(budget int) string {
	switch {
	case budget <= 0:
		return "medium"
	case budget <= 4000:
		return "low"
	case budget <= 12000:
		return "medium"
	case budget <= 24000:
		return "high"
	default:
		return "xhigh"
	}
}

type responsesToolStreamState struct {
	itemID      string
	callID      string
	name        string
	outputIndex int
	started     bool
	done        bool
	arguments   strings.Builder
}

type responsesReasoningState struct {
	itemID      string
	outputIndex int
	itemAdded   bool
	itemDone    bool
	summary     strings.Builder
}

type responsesMessageState struct {
	itemID       string
	outputIndex  int
	itemAdded    bool
	contentAdded bool
	itemDone     bool
}

func responsesToolEvents(raw json.RawMessage, responseID string, states map[int]responsesToolStreamState, nextOutputIndex int) ([]map[string]any, map[int]responsesToolStreamState, int) {
	deltas := qoder.ParseToolCallDeltas(raw)
	if len(deltas) == 0 {
		return nil, states, nextOutputIndex
	}
	nextStates := make(map[int]responsesToolStreamState, len(states))
	for k, v := range states {
		nextStates[k] = v
	}
	var events []map[string]any
	for _, delta := range deltas {
		state, ok := nextStates[delta.Index]
		if !ok {
			state = responsesToolStreamState{
				itemID:      "fc_" + randomHex(24),
				outputIndex: nextOutputIndex,
			}
			nextOutputIndex++
		}
		if delta.ID != "" {
			state.callID = delta.ID
		}
		if delta.FunctionName != "" {
			state.name = delta.FunctionName
		}
		if !state.started && state.name != "" {
			state.started = true
			events = append(events, map[string]any{
				"type":         "response.output_item.added",
				"response_id":  responseID,
				"output_index": state.outputIndex,
				"item": map[string]any{
					"id":        state.itemID,
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   firstNonBlank(state.callID, "call_"+randomHex(16)),
					"name":      state.name,
					"arguments": "",
				},
			})
		}
		if delta.ArgumentsFragment != "" {
			state.arguments.WriteString(delta.ArgumentsFragment)
			events = append(events, map[string]any{
				"type":         "response.function_call_arguments.delta",
				"response_id":  responseID,
				"item_id":      state.itemID,
				"output_index": state.outputIndex,
				"delta":        delta.ArgumentsFragment,
			})
		}
		nextStates[delta.Index] = state
	}
	return events, nextStates, nextOutputIndex
}

func buildResponsesReasoningItem(itemID, status, summaryText string) map[string]any {
	item := map[string]any{
		"id":      itemID,
		"type":    "reasoning",
		"status":  status,
		"summary": []map[string]any{},
	}
	if strings.TrimSpace(summaryText) != "" {
		item["summary"] = []map[string]any{{
			"type": "summary_text",
			"text": summaryText,
		}}
	}
	return item
}

func writeResponsesReasoningDone(w http.ResponseWriter, responseID string, state responsesReasoningState) {
	fullText := state.summary.String()
	writeResponsesEvent(w, "response.reasoning_summary_text.done", map[string]any{
		"type":          "response.reasoning_summary_text.done",
		"response_id":   responseID,
		"item_id":       state.itemID,
		"output_index":  state.outputIndex,
		"summary_index": 0,
		"text":          fullText,
	})
	writeResponsesEvent(w, "response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"response_id":  responseID,
		"output_index": state.outputIndex,
		"item":         buildResponsesReasoningItem(state.itemID, "completed", fullText),
	})
}

func writeResponsesMessageDone(w http.ResponseWriter, responseID string, state responsesMessageState, fullText string) {
	writeResponsesEvent(w, "response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"response_id":   responseID,
		"item_id":       state.itemID,
		"output_index":  state.outputIndex,
		"content_index": 0,
		"text":          fullText,
		"logprobs":      []any{},
	})
	writeResponsesEvent(w, "response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"response_id":   responseID,
		"item_id":       state.itemID,
		"output_index":  state.outputIndex,
		"content_index": 0,
		"part": map[string]any{
			"type":        "output_text",
			"annotations": []any{},
			"logprobs":    []any{},
			"text":        fullText,
		},
	})
	writeResponsesEvent(w, "response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"response_id":  responseID,
		"output_index": state.outputIndex,
		"item": map[string]any{
			"id":     state.itemID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type":        "output_text",
				"annotations": []any{},
				"logprobs":    []any{},
				"text":        fullText,
			}},
		},
	})
}

func sortedResponseToolStateIndexes(states map[int]responsesToolStreamState) []int {
	indexes := make([]int, 0, len(states))
	for idx := range states {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	return indexes
}
