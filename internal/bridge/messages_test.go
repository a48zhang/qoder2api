package bridge

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"qoder2api/internal/protocol"
	"qoder2api/internal/qoder"
)

func TestAnthropicTranscriptMatchesChatToolRoundtrip(t *testing.T) {
	chat := protocol.ChatCompletionRequest{
		Messages: []protocol.Message{
			{
				Role:    "user",
				Content: "You must call the function get_weather with city=Hangzhou. Do not answer directly.",
			},
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: json.RawMessage(`[
					{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Hangzhou\"}"}}
				]`),
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content:    `{"city":"Hangzhou","weather":"sunny","temperature_c":26}`,
			},
		},
	}

	anthropic := protocol.AnthropicMessagesRequest{
		Messages: []protocol.Message{
			{
				Role:    "user",
				Content: "You must call the function get_weather with city=Hangzhou. Do not answer directly.",
			},
			{
				Role: "assistant",
				Content: []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "call_1",
						"name":  "get_weather",
						"input": map[string]any{"city": "Hangzhou"},
					},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "call_1",
						"content":     `{"city":"Hangzhou","weather":"sunny","temperature_c":26}`,
					},
				},
			},
		},
	}

	got := anthropicMessagesToTranscript(anthropic)
	if len(got) != len(chat.Messages) {
		t.Fatalf("message count mismatch: got %d want %d\n got=%#v", len(got), len(chat.Messages), got)
	}
	for i := range got {
		if got[i].Role != chat.Messages[i].Role {
			t.Fatalf("message[%d] role mismatch: got %q want %q", i, got[i].Role, chat.Messages[i].Role)
		}
		if got[i].ToolCallID != chat.Messages[i].ToolCallID {
			t.Fatalf("message[%d] tool_call_id mismatch: got %q want %q", i, got[i].ToolCallID, chat.Messages[i].ToolCallID)
		}
		if !rawJSONEqual(got[i].ToolCalls, chat.Messages[i].ToolCalls) {
			t.Fatalf("message[%d] tool_calls mismatch:\n got=%s\nwant=%s", i, got[i].ToolCalls, chat.Messages[i].ToolCalls)
		}
		if got[i].Content != chat.Messages[i].Content {
			t.Fatalf("message[%d] content mismatch: got %#v want %#v", i, got[i].Content, chat.Messages[i].Content)
		}
	}
}

func rawJSONEqual(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	var va any
	var vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func TestNormalizeAnthropicMessagesRequestCollectsImages(t *testing.T) {
	req := protocol.AnthropicMessagesRequest{
		Model: "claude-sonnet-4.5",
		Messages: []protocol.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "image/png",
						"data":       "ZmFrZQ==",
					},
				},
			},
		}},
	}

	got, err := normalizeAnthropicMessagesRequest(req)
	if err != nil {
		t.Fatalf("normalizeAnthropicMessagesRequest: %v", err)
	}
	if len(got.ImageURLs) != 1 {
		t.Fatalf("expected 1 image url, got %#v", got.ImageURLs)
	}
	if got.ImageURLs[0] != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected image url: %q", got.ImageURLs[0])
	}
	parts, ok := got.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("unexpected message content: %#v", got.Messages[0].Content)
	}
	if parts[1]["type"] != "file" {
		t.Fatalf("expected image to normalize to file part, got %#v", parts[1])
	}
	if parts[1]["filename"] != "image.png" {
		t.Fatalf("unexpected normalized filename: %#v", parts[1])
	}
}

func TestHandleAnthropicMessagesStreamFallsBackForLegacyImagesWithoutTools(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			caps: qoder.Capabilities{Legacy: true},
			completeResp: qoder.CompleteResponse{
				Reasoning: "inspect image metadata and result",
				Content:   "图片里是一只橙猫。",
				Usage: &protocol.Usage{
					PromptTokens:     12,
					CompletionTokens: 9,
					TotalTokens:      21,
					ReasoningTokens:  4,
				},
			},
			streamEvents: []protocol.DeltaEvent{
				{Reasoning: "bad live stream reasoning"},
				{Content: "wrong streamed answer"},
			},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"qoder-test",
		"stream":true,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZmFrZQ=="}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	handler.handleAnthropicMessages(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `"thinking"`) || strings.Contains(body, "inspect image metadata") {
		t.Fatalf("unexpected thinking leaked without explicit thinking request:\n%s", body)
	}
	if !strings.Contains(body, `"text":"图片里是一只橙猫。"`) {
		t.Fatalf("expected fallback complete content in SSE:\n%s", body)
	}
	if strings.Contains(body, "wrong streamed answer") || strings.Contains(body, "bad live stream reasoning") {
		t.Fatalf("unexpected live stream payload leaked through fallback:\n%s", body)
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("expected end_turn stop reason:\n%s", body)
	}
}

func TestHandleAnthropicMessagesSingleHidesThinkingByDefault(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			completeResp: qoder.CompleteResponse{
				Reasoning: "private chain of thought",
				Content:   "Final answer.",
			},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"qoder-test",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	rec := httptest.NewRecorder()
	handler.handleAnthropicMessages(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "private chain of thought") || strings.Contains(body, `"thinking"`) {
		t.Fatalf("unexpected thinking leaked by default:\n%s", body)
	}
	if !strings.Contains(body, `"text":"Final answer."`) {
		t.Fatalf("expected final text:\n%s", body)
	}
}

func TestHandleAnthropicMessagesSingleExposesThinkingWhenRequested(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			completeResp: qoder.CompleteResponse{
				Reasoning: "visible thinking",
				Content:   "Final answer.",
			},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"qoder-test",
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	rec := httptest.NewRecorder()
	handler.handleAnthropicMessages(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"thinking":"visible thinking"`) {
		t.Fatalf("expected requested thinking block:\n%s", body)
	}
	if !strings.Contains(body, `"text":"Final answer."`) {
		t.Fatalf("expected final text:\n%s", body)
	}
}

func TestHandleAnthropicMessagesStreamFallbackExposesThinkingWhenRequested(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			caps: qoder.Capabilities{Legacy: true},
			completeResp: qoder.CompleteResponse{
				Reasoning: "inspect image metadata and result",
				Content:   "图片里是一只橙猫。",
			},
			streamEvents: []protocol.DeltaEvent{
				{Reasoning: "bad live stream reasoning"},
				{Content: "wrong streamed answer"},
			},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"qoder-test",
		"stream":true,
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZmFrZQ=="}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	handler.handleAnthropicMessages(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"thinking":"inspect image metadata and result"`) {
		t.Fatalf("expected fallback complete reasoning in SSE:\n%s", body)
	}
	if strings.Contains(body, "wrong streamed answer") || strings.Contains(body, "bad live stream reasoning") {
		t.Fatalf("unexpected live stream payload leaked through fallback:\n%s", body)
	}
}

func TestHandleAnthropicMessagesStreamDoesNotFallbackWhenToolsProvided(t *testing.T) {
	handler := &Handler{
		backend: &stubBackend{
			caps: qoder.Capabilities{Legacy: true},
			completeResp: qoder.CompleteResponse{
				Content: "should not use complete path",
			},
			streamEvents: []protocol.DeltaEvent{
				{Content: "streamed tool path"},
			},
		},
		defaultModel: "qoder-test",
		store:        newConversationStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"qoder-test",
		"stream":true,
		"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZmFrZQ=="}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	handler.handleAnthropicMessages(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"text":"streamed tool path"`) {
		t.Fatalf("expected normal streaming path when explicit tools exist:\n%s", body)
	}
	if strings.Contains(body, "should not use complete path") {
		t.Fatalf("unexpected fallback complete path used:\n%s", body)
	}
}

func TestHandleAnthropicMessagesPassesStructuredImageContentToBackend(t *testing.T) {
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

	imagePayload := base64.StdEncoding.EncodeToString([]byte("fake"))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"qoder-test",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"请看图"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+imagePayload+`"}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	handler.handleAnthropicMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(backend.completeReqs) != 1 {
		t.Fatalf("expected 1 backend request, got %d", len(backend.completeReqs))
	}
	got := backend.completeReqs[0]
	if len(got.ImageURLs) != 1 || got.ImageURLs[0] != "data:image/png;base64,"+imagePayload {
		t.Fatalf("unexpected image urls: %#v", got.ImageURLs)
	}
	if got.OriginalImageURL != "data:image/png;base64,"+imagePayload {
		t.Fatalf("unexpected original image url: %q", got.OriginalImageURL)
	}
	if len(got.ImageParts) != 1 {
		t.Fatalf("expected 1 image part, got %#v", got.ImageParts)
	}
	if got.ImageParts[0].Type != "file" || got.ImageParts[0].MIMEType != "image/png" || got.ImageParts[0].Data != "data:image/png;base64,"+imagePayload {
		t.Fatalf("unexpected image part: %#v", got.ImageParts[0])
	}
	parts, ok := got.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("expected structured message content, got %#v", got.Messages[0].Content)
	}
	if parts[0]["type"] != "text" || parts[1]["type"] != "file" {
		t.Fatalf("unexpected normalized content parts: %#v", parts)
	}
}

func TestNormalizeAnthropicMessagesKeepsToolResultMediaOutOfUserContent(t *testing.T) {
	req := protocol.AnthropicMessagesRequest{
		Model: "claude-sonnet-4.5",
		Messages: []protocol.Message{
			{
				Role: "assistant",
				Content: []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_1",
						"name":  "inspect",
						"input": map[string]any{"target": "page"},
					},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "text", "text": "continue"},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "dXNlci1pbWFnZQ==",
						},
					},
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_1",
						"content": []any{
							map[string]any{"type": "text", "text": "tool saw a chart"},
							map[string]any{
								"type": "image",
								"source": map[string]any{
									"type":       "base64",
									"media_type": "image/png",
									"data":       "dG9vbC1pbWFnZQ==",
								},
							},
						},
					},
				},
			},
		},
	}

	got, err := normalizeAnthropicMessagesRequest(req)
	if err != nil {
		t.Fatalf("normalizeAnthropicMessagesRequest: %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("expected assistant, user, tool messages; got %#v", got.Messages)
	}
	user := got.Messages[1]
	if user.Role != "user" {
		t.Fatalf("expected second message to be user, got %#v", user)
	}
	userParts, ok := user.Content.([]map[string]any)
	if !ok || len(userParts) != 2 {
		t.Fatalf("expected user content to keep only text and user image, got %#v", user.Content)
	}
	for _, part := range userParts {
		if part["type"] == "tool_result" {
			t.Fatalf("tool_result leaked into user content: %#v", userParts)
		}
	}
	tool := got.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "toolu_1" {
		t.Fatalf("unexpected tool message: %#v", tool)
	}
	toolParts, ok := tool.Content.([]map[string]any)
	if !ok || len(toolParts) != 2 {
		t.Fatalf("expected structured tool_result content, got %#v", tool.Content)
	}
	if toolParts[0]["type"] != "text" || toolParts[1]["type"] != "file" {
		t.Fatalf("unexpected tool_result parts: %#v", toolParts)
	}
	if len(got.ImageURLs) != 2 {
		t.Fatalf("expected user and tool images to be collected, got %#v", got.ImageURLs)
	}
	if got.ImageURLs[0] != "data:image/png;base64,dXNlci1pbWFnZQ==" || got.ImageURLs[1] != "data:image/png;base64,dG9vbC1pbWFnZQ==" {
		t.Fatalf("unexpected collected image urls: %#v", got.ImageURLs)
	}
}
