package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"qoder2api/internal/protocol"
)

func TestUnwrapLegacyAuthArray(t *testing.T) {
	root := []any{
		map[string]any{
			"auth_user_info_raw": map[string]any{
				"id":                 "u1",
				"securityOauthToken": "tok",
				"refreshToken":       "ref",
			},
		},
	}

	node, ok := unwrapLegacyAuth(root)
	if !ok {
		t.Fatal("expected unwrap success")
	}
	if stringValue(node["id"]) != "u1" {
		t.Fatalf("unexpected id: %v", node["id"])
	}
}

func TestFirstNonBlank(t *testing.T) {
	got := firstNonBlank("", " ", "x", "y")
	if got != "x" {
		t.Fatalf("expected x, got %q", got)
	}
}

func TestRawJSONIsEmpty(t *testing.T) {
	if !rawJSONIsEmpty(nil) {
		t.Fatal("nil raw json should be empty")
	}
	if !rawJSONIsEmpty([]byte("[]")) {
		t.Fatal("[] should be empty")
	}
	if rawJSONIsEmpty([]byte(`{"x":1}`)) {
		t.Fatal("object should not be empty")
	}
}

func TestParseToolCallDeltas(t *testing.T) {
	raw := json.RawMessage(`[{"function":{"arguments":"{\"city\": \"Hang","name":"get_weather"},"id":"call_1","index":0,"type":"function"},{"function":{"arguments":"zhou\"}"},"id":"","index":0,"type":"function"}]`)
	deltas := ParseToolCallDeltas(raw)
	if len(deltas) != 2 {
		t.Fatalf("expected 2 deltas, got %d", len(deltas))
	}
	if deltas[0].FunctionName != "get_weather" {
		t.Fatalf("unexpected function name: %q", deltas[0].FunctionName)
	}
	if deltas[1].ArgumentsFragment != `zhou"}` {
		t.Fatalf("unexpected second fragment: %q", deltas[1].ArgumentsFragment)
	}
}

func TestToolCallAccumulator(t *testing.T) {
	var acc ToolCallAccumulator
	acc.AddRaw(json.RawMessage(`[{"function":{"arguments":"","name":"get_weather"},"id":"call_1","index":0,"type":"function"}]`))
	acc.AddRaw(json.RawMessage(`[{"function":{"arguments":"{\"city\": \"Hang"},"id":"","index":0,"type":"function"}]`))
	acc.AddRaw(json.RawMessage(`[{"function":{"arguments":"zhou\"}"},"id":"","index":0,"type":"function"}]`))

	raw := acc.RawJSON()
	if rawJSONIsEmpty(raw) {
		t.Fatal("expected aggregated tool calls")
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal aggregated tool calls: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 call, got %d", len(out))
	}
	fn := out[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("unexpected function name: %v", fn["name"])
	}
	if fn["arguments"] != "{\"city\": \"Hangzhou\"}" {
		t.Fatalf("unexpected arguments: %v", fn["arguments"])
	}
}

func TestNormalizeLegacyContentHandlesImageAndFileParts(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": "https://example.com/a.png"},
		map[string]any{"type": "file", "filename": "a.txt", "file_url": "https://example.com/a.txt", "mime_type": "text/plain"},
	}
	got := normalizeLegacyContent(content)
	if got != "look\n[image] https://example.com/a.png\n[file] name=a.txt mime=text/plain url=https://example.com/a.txt" {
		t.Fatalf("unexpected normalized content: %q", got)
	}
}

func TestNormalizeLegacyPromptContentOmitsImageURLs(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": "data:image/png;base64,ZmFrZQ=="},
		map[string]any{"type": "file", "filename": "a.txt", "file_url": "https://example.com/a.txt", "mime_type": "text/plain"},
	}
	got := normalizeLegacyPromptContent(content)
	want := "@image-1.png look\n[file] name=a.txt mime=text/plain url=https://example.com/a.txt\n--- Content from referenced files ---\nContent from @image-1.png:\nRead image: image-1.png (4B)\n--- End of content ---"
	if got != want {
		t.Fatalf("unexpected normalized prompt content: %q", got)
	}
}

func TestBuildLegacyRequestPropagatesImageURLs(t *testing.T) {
	backend := &legacyBackend{
		template: map[string]any{
			"business": map[string]any{},
			"chat_context": map[string]any{
				"text": map[string]any{"type": "text", "text": ""},
				"extra": map[string]any{
					"originalContent": map[string]any{"type": "text", "text": ""},
					"modelConfig":     map[string]any{},
				},
			},
			"parameters":   map[string]any{"max_tokens": 1024},
			"model_config": map[string]any{},
			"tools": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "Read"}},
				map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
			},
		},
		session: legacySession{
			Identity: legacyIdentity{UserType: "personal_standard"},
		},
	}

	body, err := backend.buildLegacyRequest(CompleteRequest{
		Model:     "claude-sonnet-4.5",
		ImageURLs: []string{"https://example.com/a.png"},
		Messages: []protocol.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image_url", "image_url": "https://example.com/a.png"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("buildLegacyRequest: %v", err)
	}

	imageURLs, ok := body["image_urls"].([]string)
	if !ok || len(imageURLs) != 1 || imageURLs[0] != "https://example.com/a.png" {
		t.Fatalf("unexpected image_urls: %#v", body["image_urls"])
	}

	chatContext, _ := body["chat_context"].(map[string]any)
	if got, ok := chatContext["imageUrls"].([]string); !ok || len(got) != 1 || got[0] != "https://example.com/a.png" {
		t.Fatalf("unexpected chat_context.imageUrls: %#v", chatContext["imageUrls"])
	}
	wantPrompt := "@a.png look\n--- Content from referenced files ---\nContent from @a.png:\nRead image: a.png\n--- End of content ---"
	if text, _ := chatContext["text"].(map[string]any); text["text"] != wantPrompt {
		t.Fatalf("unexpected chat_context.text: %#v", text)
	}
	if extra, _ := chatContext["extra"].(map[string]any); extra != nil {
		if original, _ := extra["originalContent"].(map[string]any); original["text"] != wantPrompt {
			t.Fatalf("unexpected originalContent: %#v", original)
		}
		if modelCfg, _ := extra["modelConfig"].(map[string]any); modelCfg["is_vl"] != true || modelCfg["key"] != "auto" {
			t.Fatalf("expected chat_context.extra.modelConfig.is_vl=true, got %#v", modelCfg)
		}
	}
	if modelCfg, _ := body["model_config"].(map[string]any); modelCfg["is_vl"] != true || modelCfg["key"] != "auto" {
		t.Fatalf("expected model_config.is_vl=true, got %#v", modelCfg)
	}

	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("expected default legacy tools to be preserved for image requests, got %#v", body["tools"])
	}
}

func TestNormalizeLegacyPromptContentMatchesQoderAttachmentStyle(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "请只用一句话描述这张图片。"},
		map[string]any{"type": "image_url", "image_url": "https://example.com/test.png"},
	}
	got := normalizeLegacyPromptContent(content)
	want := "@test.png 请只用一句话描述这张图片。\n--- Content from referenced files ---\nContent from @test.png:\nRead image: test.png\n--- End of content ---"
	if got != want {
		t.Fatalf("unexpected normalized prompt content:\n got=%q\nwant=%q", got, want)
	}
}

func TestBuildLegacyRequestPreservesExplicitTools(t *testing.T) {
	backend := &legacyBackend{
		template: map[string]any{
			"business": map[string]any{},
			"chat_context": map[string]any{
				"text": map[string]any{"type": "text", "text": ""},
				"extra": map[string]any{
					"originalContent": map[string]any{"type": "text", "text": ""},
					"modelConfig":     map[string]any{},
				},
			},
			"parameters":   map[string]any{"max_tokens": 1024},
			"model_config": map[string]any{},
			"tools": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "Read"}},
			},
		},
		session: legacySession{
			Identity: legacyIdentity{UserType: "personal_standard"},
		},
	}

	body, err := backend.buildLegacyRequest(CompleteRequest{
		Model: "auto",
		Messages: []protocol.Message{{
			Role:    "user",
			Content: "hello",
		}},
		Tools: json.RawMessage(`[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]`),
	})
	if err != nil {
		t.Fatalf("buildLegacyRequest: %v", err)
	}

	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("unexpected explicit tools: %#v", body["tools"])
	}
	toolObj, _ := tools[0].(map[string]any)
	fn, _ := toolObj["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("expected explicit tool to survive, got %#v", body["tools"])
	}
}

func TestBuildLegacyRequestCanKeepDefaultToolsAndPromptOverride(t *testing.T) {
	backend := &legacyBackend{
		template: map[string]any{
			"chat_context": map[string]any{
				"text": map[string]any{"type": "text", "text": ""},
				"extra": map[string]any{
					"originalContent": map[string]any{"type": "text", "text": ""},
					"modelConfig":     map[string]any{},
				},
			},
			"parameters":   map[string]any{"max_tokens": 1024},
			"model_config": map[string]any{},
			"tools": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "Read"}},
				map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
			},
		},
		session: legacySession{
			Identity: legacyIdentity{UserType: "personal_standard"},
		},
	}

	body, err := backend.buildLegacyRequest(CompleteRequest{
		Model:                   "auto",
		LegacyPromptOverride:    "@test.png 请只用一句话描述这张图片。\n--- Content from referenced files ---\nContent from @test.png:\nRead image: test.png (618KB)\n--- End of content ---",
		LegacyAllowDefaultTools: true,
		LegacySessionID:         "session-1",
		LegacyRequestSetID:      "request-set-1",
		LegacyChatRecordID:      "chat-record-1",
		LegacyRequestID:         "request-1",
		LegacyBusinessID:        "business-1",
		LegacyBusinessBeginAt:   1234567890,
		Messages: []protocol.Message{
			{Role: "user", Content: "ignored"},
			{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"call_read_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/workspace/test.png\"}"}}]`)},
			{Role: "tool", ToolCallID: "call_read_1", Content: "Read image: test.png (618KB)"},
			{Role: "user", Content: []map[string]any{
				{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,ZmFrZQ=="}},
				{"type": "text", "text": "[Image: source: test.png]"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("buildLegacyRequest: %v", err)
	}

	chatContext, _ := body["chat_context"].(map[string]any)
	text, _ := chatContext["text"].(map[string]any)
	if text["text"] != "@test.png 请只用一句话描述这张图片。\n--- Content from referenced files ---\nContent from @test.png:\nRead image: test.png (618KB)\n--- End of content ---" {
		t.Fatalf("expected prompt override to survive, got %#v", text["text"])
	}
	if body["session_id"] != "session-1" || body["request_set_id"] != "request-set-1" || body["chat_record_id"] != "chat-record-1" || body["request_id"] != "request-1" {
		t.Fatalf("expected legacy ids to be reused, got %#v", body)
	}
	if biz, _ := body["business"].(map[string]any); biz != nil {
		if biz["id"] != "business-1" || biz["begin_at"] != int64(1234567890) {
			t.Fatalf("expected business scope to be reused, got %#v", biz)
		}
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("expected default tools to be preserved, got %#v", body["tools"])
	}
	messages, ok := body["messages"].([]map[string]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("expected 4 legacy messages, got %#v", body["messages"])
	}
	contents, ok := messages[3]["contents"].([]map[string]any)
	if !ok || len(contents) != 2 {
		t.Fatalf("expected structured followup contents, got %#v", messages[3]["contents"])
	}
	imageObj, _ := contents[0]["image_url"].(map[string]any)
	if imageObj["url"] != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("expected nested image_url to survive, got %#v", contents[0])
	}
}

func TestNormalizeLegacyModelKey(t *testing.T) {
	tests := map[string]string{
		"":                  "auto",
		"auto":              "auto",
		"default":           "auto",
		"lite":              "lite",
		"claude-sonnet-4.5": "auto",
		"sonnet":            "auto",
		"qwen-max":          "qwen-max",
	}
	for input, want := range tests {
		if got := normalizeLegacyModelKey(input); got != want {
			t.Fatalf("normalizeLegacyModelKey(%q)=%q want %q", input, got, want)
		}
	}
}

func TestBuildLegacyVisionFollowupMessages(t *testing.T) {
	messages := []protocol.Message{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": "请描述图片"},
			map[string]any{
				"type":      "file",
				"filename":  "test.png",
				"mime_type": "image/png",
				"data":      "data:image/png;base64,ZmFrZQ==",
			},
		},
	}}
	imageParts := []protocol.ContentPart{{
		Type:     "file",
		FileName: "test.png",
		MIMEType: "image/png",
		Data:     "data:image/png;base64,ZmFrZQ==",
	}}
	readCall := ToolCall{
		Index:     0,
		ID:        "call_read_1",
		Type:      "function",
		Name:      "Read",
		Arguments: `{"file_path":"/workspace/test.png"}`,
	}

	got, ok := buildLegacyVisionFollowupMessages(messages, imageParts, readCall)
	if !ok {
		t.Fatal("expected followup messages")
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(got))
	}
	if got[1].Role != "assistant" || rawJSONIsEmpty(got[1].ToolCalls) {
		t.Fatalf("expected assistant tool call message, got %#v", got[1])
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "call_read_1" {
		t.Fatalf("expected tool result message, got %#v", got[2])
	}
	if got[2].Content != "Read image: test.png (4B)" {
		t.Fatalf("unexpected tool content: %#v", got[2].Content)
	}
	if got[3].Role != "user" {
		t.Fatalf("expected followup user message, got %#v", got[3])
	}
	parts, ok := got[3].Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("unexpected followup user content: %#v", got[3].Content)
	}
	imageObj, _ := parts[0]["image_url"].(map[string]any)
	if imageObj["url"] != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("unexpected followup image payload: %#v", parts[0])
	}
	if parts[1]["text"] != "[Image: source: test.png]" {
		t.Fatalf("unexpected followup text payload: %#v", parts[1])
	}
}

func TestLegacyVisionResponseLooksBroken(t *testing.T) {
	cases := []struct {
		name string
		resp CompleteResponse
		want bool
	}{
		{
			name: "good answer",
			resp: CompleteResponse{Content: "一位身着精美铠甲、头戴金冠的孙悟空角色扮演者手持金箍棒凝视前方。"},
			want: false,
		},
		{
			name: "empty answer",
			resp: CompleteResponse{},
			want: true,
		},
		{
			name: "cannot see content",
			resp: CompleteResponse{Content: "抱歉，我无法看到您上传的图片内容，因此无法为您进行描述。"},
			want: true,
		},
		{
			name: "read image leaked in reasoning",
			resp: CompleteResponse{Reasoning: "the prompt only shows Read image: image.png (617KB)"},
			want: true,
		},
	}

	for _, tc := range cases {
		if got := legacyVisionResponseLooksBroken(tc.resp); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeLegacyPromptSupportsNestedImageURL(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "look again"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,ZmFrZQ=="}},
	}
	got := normalizeLegacyPromptContent(content)
	want := "@image-1.png look again\n--- Content from referenced files ---\nContent from @image-1.png:\nRead image: image-1.png (4B)\n--- End of content ---"
	if got != want {
		t.Fatalf("unexpected normalized nested image content:\n got=%q\nwant=%q", got, want)
	}
}

func TestSynthesizeLegacyReadCall(t *testing.T) {
	imageParts := []protocol.ContentPart{{
		Type:     "file",
		FileName: "image.png",
		MIMEType: "image/png",
		Data:     "data:image/png;base64,ZmFrZQ==",
	}}
	call, ok := synthesizeLegacyReadCall(imageParts)
	if !ok {
		t.Fatal("expected synthetic read call")
	}
	if call.Name != "Read" || call.Type != "function" || call.ID == "" {
		t.Fatalf("unexpected synthetic call: %#v", call)
	}
	if path := extractLegacyReadPath(call.Arguments); path != "/root/image.png" {
		t.Fatalf("unexpected synthetic file path: %q", path)
	}
}

func TestLegacyVisionScopeProducesStableNonEmptyIDs(t *testing.T) {
	scope := legacyVisionScope()
	if scope.SessionID == "" || scope.RequestSetID == "" || scope.ChatRecordID == "" || scope.RequestID == "" || scope.BusinessID == "" {
		t.Fatalf("expected non-empty scope ids: %#v", scope)
	}
	if scope.ChatRecordID != scope.RequestID {
		t.Fatalf("expected chat_record_id to align with request_id, got %#v", scope)
	}
	if scope.BusinessBeginAt == 0 {
		t.Fatalf("expected business begin_at to be set, got %#v", scope)
	}
}

func TestCloneProtocolMessagesCopiesToolCalls(t *testing.T) {
	raw := json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"}}]`)
	cloned := cloneProtocolMessages([]protocol.Message{{
		Role:      "assistant",
		ToolCalls: raw,
	}})
	if len(cloned) != 1 {
		t.Fatalf("unexpected cloned messages: %#v", cloned)
	}
	cloned[0].ToolCalls[0] = 'x'
	if strings.HasPrefix(string(raw), "x") {
		t.Fatal("expected tool call raw json to be deep-copied")
	}
}
