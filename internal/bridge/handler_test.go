package bridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qoder2api/internal/qoder"
)

func TestHandleChatCompletionsPassesBase64ImageToBackend(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"qoder-test",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"请看图"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,ZmFrZQ=="}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	handler.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(backend.completeReqs) != 1 {
		t.Fatalf("expected 1 backend request, got %d", len(backend.completeReqs))
	}
	got := backend.completeReqs[0]
	if len(got.ImageURLs) != 1 || got.ImageURLs[0] != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected image urls: %#v", got.ImageURLs)
	}
	if got.OriginalImageURL != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected original image url: %q", got.OriginalImageURL)
	}
	if len(got.ImageParts) != 1 || got.ImageParts[0].Type != "image_url" || got.ImageParts[0].ImageURL != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected image parts: %#v", got.ImageParts)
	}
	parts, ok := got.Messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("expected structured message content, got %#v", got.Messages[0].Content)
	}
	imagePart, _ := parts[1].(map[string]any)
	imageURL, _ := imagePart["image_url"].(map[string]any)
	if imagePart["type"] != "image_url" || imageURL["url"] != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected image content part: %#v", imagePart)
	}
}
