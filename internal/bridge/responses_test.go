package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"qoder2api/internal/protocol"
	"qoder2api/internal/qoder"
)

type stubBackend struct {
	completeResp qoder.CompleteResponse
	streamEvents []protocol.DeltaEvent
	caps         qoder.Capabilities
	completeReqs []qoder.CompleteRequest
}

func (s *stubBackend) Complete(_ context.Context, req qoder.CompleteRequest) (qoder.CompleteResponse, error) {
	return s.complete(req)
}

func (s *stubBackend) Stream(context.Context, qoder.CompleteRequest) (qoder.Stream, error) {
	return &stubStream{events: s.streamEvents}, nil
}

func (s *stubBackend) Capabilities() qoder.Capabilities {
	return s.caps
}

func (s *stubBackend) Models(context.Context) ([]qoder.ModelInfo, error) {
	return []qoder.ModelInfo{{ID: "stub-model", DisplayName: "Stub Model"}}, nil
}

func (s *stubBackend) complete(req qoder.CompleteRequest) (qoder.CompleteResponse, error) {
	s.completeReqs = append(s.completeReqs, req)
	return s.completeResp, nil
}

type stubStream struct {
	events []protocol.DeltaEvent
	index  int
}

func (s *stubStream) Next() (protocol.DeltaEvent, error) {
	if s.index >= len(s.events) {
		return protocol.DeltaEvent{Done: true}, nil
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *stubStream) Close() error { return nil }

func TestHandleResponsesSingleIncludesReasoningItem(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			completeResp: qoder.CompleteResponse{
				Reasoning: "Need to verify the result first.",
				Content:   "Final answer.",
				Usage: &protocol.Usage{
					PromptTokens:     11,
					CompletionTokens: 7,
					TotalTokens:      18,
					ReasoningTokens:  5,
				},
			},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qoder-test","input":"hi"}`))
	rec := httptest.NewRecorder()
	handler.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	output, ok := body["output"].([]any)
	if !ok || len(output) != 2 {
		t.Fatalf("unexpected output: %#v", body["output"])
	}

	reasoning, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected reasoning item: %#v", output[0])
	}
	if reasoning["type"] != "reasoning" {
		t.Fatalf("unexpected first item type: %#v", reasoning["type"])
	}
	summary, ok := reasoning["summary"].([]any)
	if !ok || len(summary) != 1 {
		t.Fatalf("unexpected summary: %#v", reasoning["summary"])
	}
	part, _ := summary[0].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != "Need to verify the result first." {
		t.Fatalf("unexpected reasoning summary: %#v", part)
	}

	message, ok := output[1].(map[string]any)
	if !ok || message["type"] != "message" {
		t.Fatalf("unexpected message item: %#v", output[1])
	}
	if body["output_text"] != "Final answer." {
		t.Fatalf("unexpected output_text: %#v", body["output_text"])
	}
}

func TestHandleResponsesStreamReasoningLifecycleAndSequentialIndexes(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			streamEvents: []protocol.DeltaEvent{
				{Reasoning: "think-1"},
				{Reasoning: "think-2"},
				{Content: "answer"},
				{ToolCalls: json.RawMessage(`[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{"}}]`)},
				{ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":"}"}}]`)},
				{Usage: &protocol.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7, ReasoningTokens: 2}},
			},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qoder-test","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	handler.handleResponses(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `event: response.reasoning_summary_text.delta`) {
		t.Fatalf("missing reasoning delta event:\n%s", body)
	}
	if !strings.Contains(body, `"type":"response.reasoning_summary_text.done"`) {
		t.Fatalf("missing reasoning done payload:\n%s", body)
	}
	if !strings.Contains(body, `"item":{"id":"rs_`) {
		t.Fatalf("missing reasoning item payload:\n%s", body)
	}
	if !strings.Contains(body, `"summary":[{"text":"think-1think-2","type":"summary_text"}]`) {
		t.Fatalf("missing reasoning summary text:\n%s", body)
	}
	if !strings.Contains(body, `"output_index":0`) || !strings.Contains(body, `"output_index":1`) || !strings.Contains(body, `"output_index":2`) {
		t.Fatalf("expected sequential output indexes 0/1/2:\n%s", body)
	}

	responseID := extractResponseIDFromSSE(body)
	if responseID == "" {
		t.Fatalf("failed to extract response id from stream:\n%s", body)
	}
	stored, ok := handler.store.GetResponse(responseID)
	if !ok {
		t.Fatalf("missing stored response for %s", responseID)
	}
	if len(stored.Messages) == 0 {
		t.Fatalf("expected stored messages")
	}
	last := stored.Messages[len(stored.Messages)-1]
	if last.Role != "assistant" || last.Content != "answer" {
		t.Fatalf("unexpected stored assistant message: %#v", last)
	}
}

func TestResponsesPreviousResponseIDPreservesTranscriptWithReasoning(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			completeResp: qoder.CompleteResponse{
				Reasoning: "first-pass reasoning",
				Content:   "first-pass answer",
			},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qoder-test","input":"first"}`))
	firstRec := httptest.NewRecorder()
	handler.handleResponses(firstRec, firstReq)

	var firstBody map[string]any
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	respID, _ := firstBody["id"].(string)
	if respID == "" {
		t.Fatalf("missing response id: %#v", firstBody)
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qoder-test","previous_response_id":"`+respID+`","input":"second"}`))
	secondRec := httptest.NewRecorder()
	handler.handleResponses(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("unexpected second status: %d body=%s", secondRec.Code, secondRec.Body.String())
	}
}

func TestResponsesPreviousResponseIDPreservesToolCallRoundtrip(t *testing.T) {
	backend := &stubBackend{
		completeResp: qoder.CompleteResponse{
			ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"qoder\"}"}}]`),
		},
	}
	handler := &Handler{
		backend:      backend,
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qoder-test","input":"lookup qoder"}`))
	firstRec := httptest.NewRecorder()
	handler.handleResponses(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("unexpected first status: %d body=%s", firstRec.Code, firstRec.Body.String())
	}

	var firstBody map[string]any
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	respID, _ := firstBody["id"].(string)
	if respID == "" {
		t.Fatalf("missing response id: %#v", firstBody)
	}

	backend.completeResp = qoder.CompleteResponse{Content: "tool result consumed"}
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"qoder-test",
		"previous_response_id":"`+respID+`",
		"input":[{
			"type":"function_call_output",
			"call_id":"call_1",
			"output":"{\"result\":\"found\"}"
		}]
	}`))
	secondRec := httptest.NewRecorder()
	handler.handleResponses(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("unexpected second status: %d body=%s", secondRec.Code, secondRec.Body.String())
	}

	if len(backend.completeReqs) != 2 {
		t.Fatalf("expected 2 backend requests, got %d", len(backend.completeReqs))
	}
	secondMessages := backend.completeReqs[1].Messages
	if len(secondMessages) != 3 {
		t.Fatalf("expected user, assistant tool call, tool output transcript; got %#v", secondMessages)
	}
	if secondMessages[0].Role != "user" || secondMessages[0].Content != "lookup qoder" {
		t.Fatalf("unexpected first transcript message: %#v", secondMessages[0])
	}
	if secondMessages[1].Role != "assistant" || rawJSONIsEmpty(secondMessages[1].ToolCalls) {
		t.Fatalf("expected stored assistant tool call, got %#v", secondMessages[1])
	}
	if secondMessages[2].Role != "tool" || secondMessages[2].ToolCallID != "call_1" || secondMessages[2].Content != `{"result":"found"}` {
		t.Fatalf("unexpected tool output message: %#v", secondMessages[2])
	}
}

func TestResponsesPreviousResponseIDPreservesStreamedToolCallRoundtrip(t *testing.T) {
	backend := &stubBackend{
		streamEvents: []protocol.DeltaEvent{
			{ToolCalls: json.RawMessage(`[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"qo"}}]`)},
			{ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":"der\"}"}}]`)},
		},
	}
	handler := &Handler{
		backend:      backend,
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qoder-test","input":"lookup qoder","stream":true}`))
	firstRec := httptest.NewRecorder()
	handler.handleResponses(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("unexpected first status: %d body=%s", firstRec.Code, firstRec.Body.String())
	}
	respID := extractResponseIDFromSSE(firstRec.Body.String())
	if respID == "" {
		t.Fatalf("missing streamed response id:\n%s", firstRec.Body.String())
	}

	backend.completeResp = qoder.CompleteResponse{Content: "tool result consumed"}
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"qoder-test",
		"previous_response_id":"`+respID+`",
		"input":[{
			"type":"function_call_output",
			"call_id":"call_1",
			"output":"{\"result\":\"found\"}"
		}]
	}`))
	secondRec := httptest.NewRecorder()
	handler.handleResponses(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("unexpected second status: %d body=%s", secondRec.Code, secondRec.Body.String())
	}

	if len(backend.completeReqs) != 1 {
		t.Fatalf("expected 1 complete request after streamed first response, got %d", len(backend.completeReqs))
	}
	secondMessages := backend.completeReqs[0].Messages
	if len(secondMessages) != 3 {
		t.Fatalf("expected user, assistant streamed tool call, tool output transcript; got %#v", secondMessages)
	}
	if secondMessages[1].Role != "assistant" || rawJSONIsEmpty(secondMessages[1].ToolCalls) {
		t.Fatalf("expected stored streamed assistant tool call, got %#v", secondMessages[1])
	}
	var toolCalls []map[string]any
	if err := json.Unmarshal(secondMessages[1].ToolCalls, &toolCalls); err != nil {
		t.Fatalf("decode stored tool calls: %v", err)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 stored tool call, got %#v", toolCalls)
	}
	fn, _ := toolCalls[0]["function"].(map[string]any)
	if toolCalls[0]["id"] != "call_1" || fn["name"] != "lookup" || fn["arguments"] != `{"query":"qoder"}` {
		t.Fatalf("unexpected stored streamed tool call: %#v", toolCalls[0])
	}
	if secondMessages[2].Role != "tool" || secondMessages[2].ToolCallID != "call_1" || secondMessages[2].Content != `{"result":"found"}` {
		t.Fatalf("unexpected streamed tool output message: %#v", secondMessages[2])
	}
}

func TestResponsesPreviousResponseIDPassesToolOutputImagesToBackend(t *testing.T) {
	backend := &stubBackend{
		completeResp: qoder.CompleteResponse{
			ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"screenshot","arguments":"{}"}}]`),
		},
	}
	handler := &Handler{
		backend:      backend,
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qoder-test","input":"take screenshot"}`))
	firstRec := httptest.NewRecorder()
	handler.handleResponses(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("unexpected first status: %d body=%s", firstRec.Code, firstRec.Body.String())
	}
	var firstBody map[string]any
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	respID, _ := firstBody["id"].(string)
	if respID == "" {
		t.Fatalf("missing response id: %#v", firstBody)
	}

	backend.completeResp = qoder.CompleteResponse{Content: "screenshot inspected"}
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"qoder-test",
		"previous_response_id":"`+respID+`",
		"input":[{
			"type":"function_call_output",
			"call_id":"call_1",
			"output":[
				{"type":"output_text","text":"screenshot result"},
				{"type":"input_image","image_url":{"url":"data:image/png;base64,ZmFrZQ=="}}
			]
		}]
	}`))
	secondRec := httptest.NewRecorder()
	handler.handleResponses(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("unexpected second status: %d body=%s", secondRec.Code, secondRec.Body.String())
	}
	if len(backend.completeReqs) != 2 {
		t.Fatalf("expected 2 complete requests, got %d", len(backend.completeReqs))
	}
	second := backend.completeReqs[1]
	if len(second.ImageURLs) != 1 || second.ImageURLs[0] != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected image urls: %#v", second.ImageURLs)
	}
	if len(second.ImageParts) != 1 || second.ImageParts[0].Type != "image_url" || second.ImageParts[0].ImageURL != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected image parts: %#v", second.ImageParts)
	}
	if len(second.Messages) != 3 {
		t.Fatalf("expected previous transcript plus tool output, got %#v", second.Messages)
	}
	toolParts, ok := second.Messages[2].Content.([]map[string]any)
	if !ok || len(toolParts) != 2 {
		t.Fatalf("expected structured tool image output, got %#v", second.Messages[2].Content)
	}
	if toolParts[1]["type"] != "image_url" {
		t.Fatalf("unexpected tool output content: %#v", toolParts)
	}
}

func extractResponseIDFromSSE(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			continue
		}
		if response, ok := data["response"].(map[string]any); ok {
			if id, _ := response["id"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

func TestHandleResponsesStreamWritesBody(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			streamEvents: []protocol.DeltaEvent{{Content: "ok"}},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"ping","stream":true}`))
	rec := httptest.NewRecorder()
	handler.handleResponses(rec, req)

	if _, err := io.ReadAll(strings.NewReader(rec.Body.String())); err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "response.done") {
		t.Fatalf("expected response.done event")
	}
}

func TestNormalizeResponsesRequestCollectsImages(t *testing.T) {
	req := protocol.ResponsesRequest{
		Model: "claude-sonnet-4.5",
		Input: []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "look"},
					map[string]any{"type": "input_image", "image_url": "https://example.com/a.png"},
					map[string]any{"type": "input_file", "filename": "a.txt", "file_url": "https://example.com/a.txt", "mime_type": "text/plain"},
				},
			},
		},
	}

	got, err := normalizeResponsesRequest(req)
	if err != nil {
		t.Fatalf("normalizeResponsesRequest: %v", err)
	}
	if len(got.ImageURLs) != 1 || got.ImageURLs[0] != "https://example.com/a.png" {
		t.Fatalf("unexpected image urls: %#v", got.ImageURLs)
	}
}

func TestNormalizeResponsesRequestContentConcreteType(t *testing.T) {
	req := protocol.ResponsesRequest{
		Model: "claude-sonnet-4.5",
		Input: []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "look"},
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64,ZmFrZQ=="},
				},
			},
		},
	}

	got, err := normalizeResponsesRequest(req)
	if err != nil {
		t.Fatalf("normalizeResponsesRequest: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("unexpected messages: %#v", got.Messages)
	}
	if reflect.TypeOf(got.Messages[0].Content).String() != "[]map[string]interface {}" {
		t.Fatalf("unexpected content type: %T", got.Messages[0].Content)
	}
}

func TestHandleResponsesPassesNestedInputImageToBackend(t *testing.T) {
	backend := &stubBackend{
		completeResp: qoder.CompleteResponse{
			Content: "ok",
		},
	}
	handler := &Handler{
		backend:      backend,
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"qoder-test",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_text","text":"请看图"},
				{"type":"input_image","image_url":{"url":"https://example.com/demo.png"}}
			]
		}]
	}`))
	rec := httptest.NewRecorder()
	handler.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(backend.completeReqs) != 1 {
		t.Fatalf("expected 1 backend request, got %d", len(backend.completeReqs))
	}
	got := backend.completeReqs[0]
	if len(got.ImageURLs) != 1 || got.ImageURLs[0] != "https://example.com/demo.png" {
		t.Fatalf("unexpected image urls: %#v", got.ImageURLs)
	}
	if got.OriginalImageURL != "https://example.com/demo.png" {
		t.Fatalf("unexpected original image url: %q", got.OriginalImageURL)
	}
	if len(got.ImageParts) != 1 || got.ImageParts[0].Type != "image_url" || got.ImageParts[0].ImageURL != "https://example.com/demo.png" {
		t.Fatalf("unexpected image parts: %#v", got.ImageParts)
	}
	parts, ok := got.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("expected structured message content, got %#v", got.Messages[0].Content)
	}
	if parts[1]["type"] != "image_url" || parts[1]["image_url"] != "https://example.com/demo.png" {
		t.Fatalf("unexpected normalized image content: %#v", parts[1])
	}
}

func TestNormalizeResponsesFunctionCallOutputPreservesStructuredMedia(t *testing.T) {
	req := protocol.ResponsesRequest{
		Model: "qoder-test",
		Input: []any{
			map[string]any{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "inspect_page",
				"arguments": `{"url":"https://example.com"}`,
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output": []any{
					map[string]any{"type": "output_text", "text": "tool saw a page"},
					map[string]any{"type": "input_image", "image_url": map[string]any{"url": "data:image/png;base64,ZmFrZQ=="}},
				},
			},
		},
	}

	got, err := normalizeResponsesRequest(req)
	if err != nil {
		t.Fatalf("normalizeResponsesRequest: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("expected assistant and tool messages, got %#v", got.Messages)
	}
	tool := got.Messages[1]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" {
		t.Fatalf("unexpected tool message: %#v", tool)
	}
	parts, ok := tool.Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("expected structured tool output, got %#v", tool.Content)
	}
	if parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Fatalf("unexpected normalized tool output parts: %#v", parts)
	}
	if len(got.ImageURLs) != 1 || got.ImageURLs[0] != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected collected image urls: %#v", got.ImageURLs)
	}
}
