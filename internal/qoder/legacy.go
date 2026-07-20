package qoder

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qoder2api/internal/protocol"
)

const (
	legacyCosyVersion  = "0.1.43"
	legacyQoderStream  = "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	legacyPathNoHost   = "/api/v2/service/pro/sse/agent_chat_generation"
	legacyServerPubKey = "-----BEGIN PUBLIC KEY-----\nMIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc\n4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l\n6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17\nXcW+ML9FoCI6AOvOzwIDAQAB\n-----END PUBLIC KEY-----"
)

type legacyBackend struct {
	client   *http.Client
	template map[string]any
	session  legacySession

	modelsMu     sync.Mutex
	modelsCache  []ModelInfo
	modelsLoaded bool
}

type legacyIdentity struct {
	Name             string
	AID              string
	UID              string
	YXUID            string
	OrganizationID   string
	OrganizationName string
	UserType         string
	SecurityOAuth    string
	RefreshToken     string
}

type legacySession struct {
	Identity     legacyIdentity
	TempKey      []byte
	CosyKey      string
	Info         string
	MachineID    string
	MachineToken string
	MachineType  string
}

type legacyStream struct {
	resp   *http.Response
	reader *bufio.Reader
}

func (b *legacyBackend) Capabilities() Capabilities {
	return Capabilities{Legacy: true}
}

func NewLegacyBackend(opts Options, client *http.Client) (Backend, error) {
	identity, err := loadLegacyIdentity(opts.AuthJSON)
	if err != nil {
		return nil, err
	}
	session, err := newLegacySession(identity)
	if err != nil {
		return nil, err
	}
	return &legacyBackend{
		client:   client,
		template: newLegacyTemplate(),
		session:  session,
	}, nil
}

func (b *legacyBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	if b.shouldUseLegacyVisionLoop(req) {
		return b.completeWithVisionLoop(ctx, req)
	}

	stream, err := b.Stream(ctx, req)
	if err != nil {
		return CompleteResponse{}, err
	}
	defer stream.Close()

	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls ToolCallAccumulator
	var usage *protocol.Usage
	for {
		event, err := stream.Next()
		if err != nil {
			return CompleteResponse{}, err
		}
		if event.Done {
			break
		}
		if event.Content != "" {
			content.WriteString(event.Content)
		}
		if event.Reasoning != "" {
			reasoning.WriteString(event.Reasoning)
		}
		if !rawJSONIsEmpty(event.ToolCalls) {
			toolCalls.AddRaw(event.ToolCalls)
		}
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	return CompleteResponse{
		Content:   content.String(),
		Reasoning: reasoning.String(),
		ToolCalls: toolCalls.RawJSON(),
		Usage:     usage,
	}, nil
}

func (b *legacyBackend) shouldUseLegacyVisionLoop(req CompleteRequest) bool {
	if len(req.ImageParts) == 0 {
		return false
	}
	if !rawJSONIsEmpty(req.Tools) {
		return false
	}
	return true
}

func (b *legacyBackend) completeWithVisionLoop(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	originalPrompt := latestPrompt(req.Messages)
	scope := legacyVisionScope()
	loopReq := req
	loopReq.LegacySessionID = scope.SessionID
	loopReq.LegacyRequestSetID = scope.RequestSetID
	loopReq.LegacyChatRecordID = scope.ChatRecordID
	loopReq.LegacyRequestID = scope.RequestID
	loopReq.LegacyBusinessID = scope.BusinessID
	loopReq.LegacyBusinessBeginAt = scope.BusinessBeginAt

	currentReq := loopReq
	var lastResp CompleteResponse
	for iteration := 0; iteration < 4; iteration++ {
		resp, err := b.completeOnce(ctx, currentReq)
		if err != nil {
			if iteration == 0 {
				return CompleteResponse{}, err
			}
			return lastResp, nil
		}
		lastResp = resp

		readCall, ok := findLegacyReadCall(resp.ToolCalls)
		if !ok {
			if iteration == 0 {
				readCall, ok = synthesizeLegacyReadCall(req.ImageParts)
				if !ok {
					return resp, nil
				}
			} else if legacyVisionResponseLooksBroken(resp) && iteration < 3 {
				continue
			} else {
				return resp, nil
			}
		}

		injected, ok := buildLegacyVisionFollowupMessages(currentReq.Messages, req.ImageParts, readCall)
		if !ok {
			return resp, nil
		}

		currentReq.Messages = injected
		currentReq.ImageURLs = nil
		currentReq.OriginalImageURL = ""
		currentReq.LegacyPromptOverride = originalPrompt
		currentReq.LegacyAllowDefaultTools = true
	}
	return lastResp, nil
}

func (b *legacyBackend) completeOnce(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	stream, err := b.Stream(ctx, req)
	if err != nil {
		return CompleteResponse{}, err
	}
	defer stream.Close()

	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls ToolCallAccumulator
	var usage *protocol.Usage
	for {
		event, err := stream.Next()
		if err != nil {
			return CompleteResponse{}, err
		}
		if event.Done {
			break
		}
		if event.Content != "" {
			content.WriteString(event.Content)
		}
		if event.Reasoning != "" {
			reasoning.WriteString(event.Reasoning)
		}
		if !rawJSONIsEmpty(event.ToolCalls) {
			toolCalls.AddRaw(event.ToolCalls)
		}
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	return CompleteResponse{
		Content:   content.String(),
		Reasoning: reasoning.String(),
		ToolCalls: toolCalls.RawJSON(),
		Usage:     usage,
	}, nil
}

func (b *legacyBackend) Stream(ctx context.Context, req CompleteRequest) (Stream, error) {
	req.Model = b.resolveModelKey(ctx, req.Model)
	body, err := b.buildLegacyRequest(req)
	if err != nil {
		return nil, err
	}
	plainPayload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	payload := []byte(legacyEncode(plainPayload))

	cosyDate := fmt.Sprintf("%d", time.Now().Unix())
	payloadB64, err := legacyBuildPayloadB64(b.session.Info)
	if err != nil {
		return nil, err
	}
	sig := legacySignRequest(payloadB64, b.session.CosyKey, cosyDate, string(payload), legacyPathNoHost)
	reqUpstream, err := http.NewRequestWithContext(ctx, http.MethodPost, legacyQoderStream, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	reqUpstream.Header.Set("cache-control", "no-cache")
	reqUpstream.Header.Set("cosy-data-policy", "AGREE")
	reqUpstream.Header.Set("accept", "text/event-stream")
	reqUpstream.Header.Set("accept-encoding", "identity")
	reqUpstream.Header.Set("authorization", "Bearer COSY."+payloadB64+"."+sig)
	reqUpstream.Header.Set("content-type", "application/json")
	reqUpstream.Header.Set("cosy-clienttype", "5")
	reqUpstream.Header.Set("cosy-clientip", "169.254.198.161")
	reqUpstream.Header.Set("cosy-date", cosyDate)
	reqUpstream.Header.Set("cosy-key", b.session.CosyKey)
	reqUpstream.Header.Set("cosy-machineid", b.session.MachineID)
	reqUpstream.Header.Set("cosy-machinetoken", b.session.MachineToken)
	reqUpstream.Header.Set("cosy-machinetype", b.session.MachineType)
	reqUpstream.Header.Set("cosy-user", b.session.Identity.UID)
	reqUpstream.Header.Set("cosy-version", legacyCosyVersion)
	reqUpstream.Header.Set("login-version", "v2")
	reqUpstream.Header.Set("user-agent", "Go-http-client/2.0")
	reqUpstream.Header.Set("x-model-key", req.Model)
	reqUpstream.Header.Set("x-model-source", "system")

	resp, err := b.client.Do(reqUpstream)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("legacy upstream http %d: %s", resp.StatusCode, string(data))
	}
	return &legacyStream{
		resp:   resp,
		reader: bufio.NewReader(resp.Body),
	}, nil
}

// cosySignedRequest 构造一个带 COSY 签名头的请求（签名使用空 body）。
func (b *legacyBackend) cosySignedRequest(ctx context.Context, method, rawURL, signPath string) (*http.Request, error) {
	cosyDate := fmt.Sprintf("%d", time.Now().Unix())
	payloadB64, err := legacyBuildPayloadB64(b.session.Info)
	if err != nil {
		return nil, err
	}
	sig := legacySignRequest(payloadB64, b.session.CosyKey, cosyDate, "", signPath)
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("cosy-data-policy", "AGREE")
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("authorization", "Bearer COSY."+payloadB64+"."+sig)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("cosy-clienttype", "5")
	req.Header.Set("cosy-clientip", "169.254.198.161")
	req.Header.Set("cosy-date", cosyDate)
	req.Header.Set("cosy-key", b.session.CosyKey)
	req.Header.Set("cosy-machineid", b.session.MachineID)
	req.Header.Set("cosy-machinetoken", b.session.MachineToken)
	req.Header.Set("cosy-machinetype", b.session.MachineType)
	req.Header.Set("cosy-user", b.session.Identity.UID)
	req.Header.Set("cosy-version", legacyCosyVersion)
	req.Header.Set("login-version", "v2")
	req.Header.Set("user-agent", "Go-http-client/2.0")
	return req, nil
}

// Models 返回聊天桥可用的模型列表（Qoder chat 分组），结果按进程缓存。
func (b *legacyBackend) Models(ctx context.Context) ([]ModelInfo, error) {
	return b.loadModels(ctx)
}

func (b *legacyBackend) loadModels(ctx context.Context) ([]ModelInfo, error) {
	b.modelsMu.Lock()
	defer b.modelsMu.Unlock()
	if b.modelsLoaded {
		return b.modelsCache, nil
	}
	models, err := b.fetchModels(ctx)
	if err != nil {
		return nil, err
	}
	b.modelsCache = models
	b.modelsLoaded = true
	return models, nil
}

// fetchModels 拉取聊天桥可用模型：chat 分组优先，其后合并 byok
// （团队/企业自带 key，如 Peach）。按展示名去重，同名以靠前分组为准。
func (b *legacyBackend) fetchModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := b.cosySignedRequest(ctx, http.MethodGet,
		"https://api3.qoder.sh/algo/api/v2/model/list", "/api/v2/model/list")
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("model list http %d: %s", resp.StatusCode, string(data))
	}

	type legacyModelEntry struct {
		Key         string `json:"key"`
		DisplayName string `json:"display_name"`
	}
	var groups map[string][]legacyModelEntry
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return nil, err
	}

	chat := groups["chat"]
	if len(chat) == 0 {
		chat = groups["assistant"]
	}
	seen := map[string]struct{}{}
	models := make([]ModelInfo, 0, 24)
	for _, list := range [][]legacyModelEntry{chat, groups["byok_teams"], groups["byok_enterprise"]} {
		for _, m := range list {
			key := strings.TrimSpace(m.Key)
			name := strings.TrimSpace(m.DisplayName)
			if key == "" || name == "" {
				continue
			}
			dedup := strings.ToLower(name)
			if _, ok := seen[dedup]; ok {
				continue
			}
			seen[dedup] = struct{}{}
			models = append(models, ModelInfo{ID: key, DisplayName: name})
		}
	}
	return models, nil
}

// resolveModelKey 将客户端传入的模型名（展示名或 key，大小写不敏感）解析为
// Qoder 内部 key。未命中时退回 normalizeLegacyModelKey 处理 auto/lite/别名。
func (b *legacyBackend) resolveModelKey(ctx context.Context, model string) string {
	resolved := normalizeLegacyModelKey(model)
	if name := strings.TrimSpace(model); name != "" {
		if models, err := b.loadModels(ctx); err == nil {
			lower := strings.ToLower(name)
			for _, m := range models {
				if strings.ToLower(m.DisplayName) == lower || strings.ToLower(m.ID) == lower {
					resolved = m.ID
					break
				}
			}
		}
	}
	log.Printf("[q2a] model=%q -> key=%q", model, resolved)
	return resolved
}

func (s *legacyStream) Next() (protocol.DeltaEvent, error) {
	idleMs := 0
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF {
				if strings.TrimSpace(line) == "" {
					return protocol.DeltaEvent{Done: true}, nil
				}
			} else {
				return protocol.DeltaEvent{}, err
			}
		}
		if strings.TrimSpace(line) == "" {
			idleMs += 500
			if idleMs >= 3000 || err == io.EOF {
				return protocol.DeltaEvent{Done: true}, nil
			}
			continue
		}
		idleMs = 0
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			if err == io.EOF {
				return protocol.DeltaEvent{Done: true}, nil
			}
			continue
		}
		dataLine := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		event, ok := parseLegacyLine(dataLine)
		if ok {
			return event, nil
		}
		if err == io.EOF {
			return protocol.DeltaEvent{Done: true}, nil
		}
	}
}

func (s *legacyStream) readLine() (string, error) {
	var buf bytes.Buffer
	for {
		frag, err := s.reader.ReadString('\n')
		buf.WriteString(frag)
		if err == nil {
			return buf.String(), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf.String(), err
	}
}

func (s *legacyStream) Close() error {
	return s.resp.Body.Close()
}

func parseLegacyLine(dataLine string) (protocol.DeltaEvent, bool) {
	if dataLine == "" || dataLine == "[DONE]" {
		return protocol.DeltaEvent{}, false
	}
	var envelope struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(dataLine), &envelope); err != nil || envelope.Body == "" {
		return protocol.DeltaEvent{}, false
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string          `json:"content"`
				Reasoning string          `json:"reasoning_content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int `json:"prompt_tokens"`
			CompletionTokens        int `json:"completion_tokens"`
			TotalTokens             int `json:"total_tokens"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(envelope.Body), &chunk); err != nil {
		return protocol.DeltaEvent{}, false
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" || choice.Delta.Reasoning != "" || !rawJSONIsEmpty(choice.Delta.ToolCalls) {
			return protocol.DeltaEvent{
				Content:   choice.Delta.Content,
				Reasoning: choice.Delta.Reasoning,
				ToolCalls: choice.Delta.ToolCalls,
			}, true
		}
	}
	if chunk.Usage != nil {
		usage := &protocol.Usage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
			InputTokens:      chunk.Usage.PromptTokens,
			OutputTokens:     chunk.Usage.CompletionTokens,
		}
		if chunk.Usage.CompletionTokensDetails != nil {
			usage.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
		}
		if chunk.Usage.PromptTokensDetails != nil {
			usage.CachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		return protocol.DeltaEvent{Usage: usage}, true
	}
	return protocol.DeltaEvent{}, false
}

func (b *legacyBackend) buildLegacyRequest(req CompleteRequest) (map[string]any, error) {
	body := deepCopyMap(b.template)
	requestID := firstNonBlank(req.LegacyRequestID, randomUUID())
	chatRecordID := firstNonBlank(req.LegacyChatRecordID, requestID)
	requestSetID := firstNonBlank(req.LegacyRequestSetID, randomUUID())
	sessionID := firstNonBlank(req.LegacySessionID, randomUUID())
	body["request_id"] = requestID
	body["chat_record_id"] = chatRecordID
	body["request_set_id"] = requestSetID
	body["session_id"] = sessionID
	body["stream"] = true
	body["aliyun_user_type"] = b.session.Identity.UserType

	modelKey := normalizeLegacyModelKey(req.Model)
	if mc, ok := body["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
		mc["is_reasoning"] = legacyReasoningEnabled(req)
		mc["is_vl"] = len(req.ImageURLs) > 0
	}
	if biz, ok := body["business"].(map[string]any); ok {
		biz["id"] = firstNonBlank(req.LegacyBusinessID, randomUUID())
		beginAt := req.LegacyBusinessBeginAt
		if beginAt == 0 {
			beginAt = time.Now().UnixMilli()
		}
		biz["begin_at"] = beginAt
		prompt := latestPrompt(req.Messages)
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}
	prompt := latestPrompt(req.Messages)
	if strings.TrimSpace(req.LegacyPromptOverride) != "" {
		prompt = req.LegacyPromptOverride
	}
	if ctx, ok := body["chat_context"].(map[string]any); ok {
		if text, ok := ctx["text"].(map[string]any); ok {
			text["text"] = prompt
		}
		if extra, ok := ctx["extra"].(map[string]any); ok {
			if original, ok := extra["originalContent"].(map[string]any); ok {
				original["text"] = prompt
			}
			if modelConfig, ok := extra["modelConfig"].(map[string]any); ok {
				modelConfig["key"] = modelKey
				modelConfig["is_reasoning"] = legacyReasoningEnabled(req)
				modelConfig["is_vl"] = len(req.ImageURLs) > 0
			}
		}
		if len(req.ImageURLs) > 0 {
			ctx["imageUrls"] = req.ImageURLs
		}
	}
	if len(req.ImageURLs) > 0 {
		body["image_urls"] = req.ImageURLs
	}
	if params, ok := body["parameters"].(map[string]any); ok {
		applyLegacyReasoningParameters(params, req)
	}
	body["messages"] = legacyMessages(req.Messages)
	if rawJSONIsEmpty(req.Tools) {
		if req.LegacyAllowDefaultTools || len(req.ImageParts) > 0 || len(req.ImageURLs) > 0 {
			return body, nil
		}
		body["tools"] = []any{}
	} else {
		var tools any
		if err := json.Unmarshal(req.Tools, &tools); err == nil {
			body["tools"] = tools
		}
	}
	return body, nil
}

func latestPrompt(messages []protocol.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Role) != "user" {
			continue
		}
		text := normalizeLegacyPromptContent(messages[i].Content)
		if strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func legacyMessages(messages []protocol.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		role := firstNonBlank(message.Role, "user")
		switch role {
		case "user":
			contents := legacyUserContents(message.Content)
			if len(contents) == 0 {
				continue
			}
			out = append(out, map[string]any{
				"role":     "user",
				"content":  "",
				"contents": contents,
				"response_meta": map[string]any{
					"id": "",
					"usage": map[string]any{
						"prompt_tokens":     0,
						"completion_tokens": 0,
						"total_tokens":      0,
					},
				},
				"reasoning_content_signature": "",
			})
		case "assistant":
			entry := map[string]any{
				"role":    "assistant",
				"content": normalizeLegacyContent(message.Content),
			}
			if !rawJSONIsEmpty(message.ToolCalls) {
				entry["tool_calls"] = rawJSONToAny(message.ToolCalls)
			}
			if strings.TrimSpace(stringValue(entry["content"])) == "" && entry["tool_calls"] == nil {
				continue
			}
			out = append(out, entry)
		case "tool":
			text := normalizeLegacyContent(message.Content)
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": message.ToolCallID,
				"content":      text,
			})
		default:
			text := normalizeLegacyContent(message.Content)
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, map[string]any{
				"role":    role,
				"content": text,
			})
		}
	}
	return out
}

func legacyUserContents(content any) []map[string]any {
	switch v := content.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []map[string]any{{
			"type": "text",
			"text": v,
		}}
	case []map[string]any:
		items := make([]any, 0, len(v))
		for _, item := range v {
			items = append(items, item)
		}
		return legacyUserContents(items)
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringValue(part["type"]) {
			case "text":
				text := stringValue(part["text"])
				if strings.TrimSpace(text) == "" {
					continue
				}
				out = append(out, map[string]any{
					"type": "text",
					"text": text,
				})
			case "image_url":
				imageURL := firstNonBlank(stringValue(part["image_url"]), nestedStringValue(part["image_url"], "url"))
				if strings.TrimSpace(imageURL) == "" {
					continue
				}
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageURL},
				})
			case "file":
				fileURL := firstNonBlank(stringValue(part["file_url"]), stringValue(part["data"]))
				if strings.TrimSpace(fileURL) == "" {
					continue
				}
				entry := map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": fileURL},
				}
				out = append(out, entry)
			}
		}
		if len(out) > 0 {
			return out
		}
		text := normalizeLegacyPromptContent(content)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []map[string]any{{
			"type": "text",
			"text": text,
		}}
	default:
		text := normalizeLegacyPromptContent(content)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []map[string]any{{
			"type": "text",
			"text": text,
		}}
	}
}

func normalizeLegacyContent(content any) string {
	return normalizeLegacyContentWithImages(content, true)
}

func normalizeLegacyPromptContent(content any) string {
	return normalizeLegacyPrompt(content)
}

func normalizeLegacyContentWithImages(content any, includeImages bool) string {
	switch v := content.(type) {
	case string:
		return v
	case []map[string]any:
		items := make([]any, 0, len(v))
		for _, item := range v {
			items = append(items, item)
		}
		return normalizeLegacyContentWithImages(items, includeImages)
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringValue(m["type"]) {
			case "text":
				parts = append(parts, stringValue(m["text"]))
			case "image_url":
				if !includeImages {
					continue
				}
				if imageURL := firstNonBlank(stringValue(m["image_url"]), nestedStringValue(m["image_url"], "url")); imageURL != "" {
					parts = append(parts, "[image] "+imageURL)
				}
			case "file":
				parts = append(parts, renderLegacyFilePart(m))
			}
		}
		return strings.Join(parts, "\n")
	default:
		buf, _ := json.Marshal(v)
		return string(buf)
	}
}

type legacyAttachmentRef struct {
	name      string
	mimeType  string
	imageURL  string
	sizeBytes int
}

type legacyVisionRequestScope struct {
	SessionID       string
	RequestSetID    string
	ChatRecordID    string
	RequestID       string
	BusinessID      string
	BusinessBeginAt int64
}

func normalizeLegacyPrompt(content any) string {
	textParts, attachments := legacyPromptParts(content)
	body := strings.Join(textParts, "\n")
	if len(attachments) == 0 {
		return body
	}

	refs := make([]string, 0, len(attachments))
	block := make([]string, 0, 2+len(attachments)*2)
	block = append(block, "--- Content from referenced files ---")
	for _, attachment := range attachments {
		refs = append(refs, "@"+attachment.name)
		block = append(block, "Content from @"+attachment.name+":")
		block = append(block, renderLegacyAttachmentReadLine(attachment))
	}
	block = append(block, "--- End of content ---")

	prefix := strings.Join(refs, " ")
	switch {
	case strings.TrimSpace(body) == "":
		return prefix + "\n" + strings.Join(block, "\n")
	default:
		return prefix + " " + body + "\n" + strings.Join(block, "\n")
	}
}

func legacyVisionScope() legacyVisionRequestScope {
	requestID := randomUUID()
	return legacyVisionRequestScope{
		SessionID:       randomUUID(),
		RequestSetID:    randomUUID(),
		ChatRecordID:    requestID,
		RequestID:       requestID,
		BusinessID:      randomUUID(),
		BusinessBeginAt: time.Now().UnixMilli(),
	}
}

func legacyPromptParts(content any) ([]string, []legacyAttachmentRef) {
	switch v := content.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []string{v}, nil
	case []map[string]any:
		items := make([]any, 0, len(v))
		for _, item := range v {
			items = append(items, item)
		}
		return legacyPromptParts(items)
	case []any:
		textParts := make([]string, 0, len(v))
		attachments := make([]legacyAttachmentRef, 0, len(v))
		imageIndex := 0
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringValue(m["type"]) {
			case "text":
				if text := stringValue(m["text"]); strings.TrimSpace(text) != "" {
					textParts = append(textParts, text)
				}
			case "image_url":
				imageIndex++
				attachments = append(attachments, legacyImageAttachmentFromImagePart(m, imageIndex))
			case "file":
				if attachment, ok := legacyImageAttachmentFromFilePart(m, imageIndex+1); ok {
					imageIndex++
					attachments = append(attachments, attachment)
					continue
				}
				textParts = append(textParts, renderLegacyFilePart(m))
			}
		}
		return textParts, attachments
	default:
		buf, _ := json.Marshal(v)
		return []string{string(buf)}, nil
	}
}

func legacyImageAttachmentFromImagePart(part map[string]any, index int) legacyAttachmentRef {
	imageURL := firstNonBlank(stringValue(part["image_url"]), nestedStringValue(part["image_url"], "url"))
	name := firstNonBlank(
		stringValue(part["filename"]),
		stringValue(part["file_name"]),
		legacyAttachmentNameFromURL(imageURL),
		legacyGeneratedAttachmentName(index, legacyImageExtensionFromURL(imageURL)),
	)
	return legacyAttachmentRef{
		name:      name,
		mimeType:  legacyImageMimeType(imageURL, stringValue(part["mime_type"])),
		imageURL:  imageURL,
		sizeBytes: legacyAttachmentSize(imageURL, ""),
	}
}

func legacyImageAttachmentFromFilePart(part map[string]any, index int) (legacyAttachmentRef, bool) {
	mimeType := firstNonBlank(stringValue(part["mime_type"]), legacyMimeTypeFromData(stringValue(part["data"])))
	name := firstNonBlank(
		stringValue(part["filename"]),
		stringValue(part["file_name"]),
		legacyAttachmentNameFromURL(stringValue(part["file_url"])),
	)
	if !legacyIsImageAttachment(name, mimeType, stringValue(part["data"]), stringValue(part["file_url"])) {
		return legacyAttachmentRef{}, false
	}
	if name == "" {
		name = legacyGeneratedAttachmentName(index, legacyImageExtensionFromDataOrURL(stringValue(part["data"]), stringValue(part["file_url"]), mimeType))
	}
	return legacyAttachmentRef{
		name:      name,
		mimeType:  legacyImageMimeType(firstNonBlank(stringValue(part["data"]), stringValue(part["file_url"])), mimeType),
		imageURL:  firstNonBlank(stringValue(part["data"]), stringValue(part["file_url"])),
		sizeBytes: legacyAttachmentSize(stringValue(part["data"]), stringValue(part["file_url"])),
	}, true
}

func renderLegacyAttachmentReadLine(attachment legacyAttachmentRef) string {
	size := legacyFormatAttachmentSize(attachment.sizeBytes)
	if size == "" {
		return "Read image: " + attachment.name
	}
	return "Read image: " + attachment.name + " (" + size + ")"
}

func findLegacyReadCall(raw json.RawMessage) (ToolCall, bool) {
	var acc ToolCallAccumulator
	acc.AddRaw(raw)
	for _, call := range acc.Calls() {
		if strings.EqualFold(strings.TrimSpace(call.Name), "Read") {
			return call, true
		}
	}
	return ToolCall{}, false
}

func buildLegacyVisionFollowupMessages(messages []protocol.Message, imageParts []protocol.ContentPart, readCall ToolCall) ([]protocol.Message, bool) {
	imagePart, attachment, ok := matchLegacyVisionImage(imageParts, readCall.Arguments)
	if !ok {
		return nil, false
	}
	if requestedPath := extractLegacyReadPath(readCall.Arguments); strings.TrimSpace(requestedPath) != "" {
		if requestedName := filepath.Base(strings.ReplaceAll(requestedPath, "\\", "/")); strings.TrimSpace(requestedName) != "" {
			attachment.name = requestedName
		}
	}

	out := cloneProtocolMessages(messages)
	out = append(out, protocol.Message{
		Role:      "assistant",
		Content:   "",
		ToolCalls: singleLegacyToolCallJSON(readCall),
	})
	out = append(out, protocol.Message{
		Role:       "tool",
		ToolCallID: readCall.ID,
		Content:    renderLegacyAttachmentReadLine(attachment),
	})

	imageURL := firstNonBlank(imagePart.ImageURL, imagePart.Data, imagePart.FileURL)
	if strings.TrimSpace(imageURL) == "" {
		return nil, false
	}

	followupContent := []map[string]any{
		{
			"type":      "image_url",
			"image_url": map[string]any{"url": imageURL},
		},
	}
	if attachment.name != "" {
		followupContent = append(followupContent, map[string]any{
			"type": "text",
			"text": "[Image: source: " + attachment.name + "]",
		})
	}
	out = append(out, protocol.Message{
		Role:    "user",
		Content: followupContent,
	})
	return out, true
}

func matchLegacyVisionImage(imageParts []protocol.ContentPart, arguments string) (protocol.ContentPart, legacyAttachmentRef, bool) {
	requestedPath := extractLegacyReadPath(arguments)
	if strings.TrimSpace(requestedPath) != "" {
		requestedName := filepath.Base(strings.ReplaceAll(requestedPath, "\\", "/"))
		for idx, part := range imageParts {
			ref := legacyAttachmentFromContentPart(part, idx+1)
			if strings.EqualFold(ref.name, requestedName) {
				return part, ref, true
			}
		}
	}
	if len(imageParts) == 0 {
		return protocol.ContentPart{}, legacyAttachmentRef{}, false
	}
	ref := legacyAttachmentFromContentPart(imageParts[0], 1)
	return imageParts[0], ref, true
}

func extractLegacyReadPath(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(arguments), &obj); err == nil {
		return firstNonBlank(stringValue(obj["file_path"]), stringValue(obj["path"]))
	}
	return ""
}

func synthesizeLegacyReadCall(imageParts []protocol.ContentPart) (ToolCall, bool) {
	if len(imageParts) == 0 {
		return ToolCall{}, false
	}
	ref := legacyAttachmentFromContentPart(imageParts[0], 1)
	if strings.TrimSpace(ref.name) == "" {
		return ToolCall{}, false
	}
	args, err := json.Marshal(map[string]any{
		"file_path": "/root/" + ref.name,
	})
	if err != nil {
		return ToolCall{}, false
	}
	return ToolCall{
		Index:     0,
		ID:        "call_read_" + randomHex(8),
		Type:      "function",
		Name:      "Read",
		Arguments: string(args),
	}, true
}

func nestedStringValue(v any, key string) string {
	obj, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return stringValue(obj[key])
}

func legacyAttachmentFromContentPart(part protocol.ContentPart, index int) legacyAttachmentRef {
	imageURL := firstNonBlank(part.ImageURL, part.Data, part.FileURL)
	name := firstNonBlank(
		part.FileName,
		legacyAttachmentNameFromURL(imageURL),
		legacyGeneratedAttachmentName(index, legacyImageExtensionFromDataOrURL(part.Data, imageURL, firstNonBlank(part.MIMEType, part.MediaType))),
	)
	return legacyAttachmentRef{
		name:      name,
		mimeType:  firstNonBlank(part.MIMEType, part.MediaType, legacyImageMimeType(imageURL, "")),
		imageURL:  imageURL,
		sizeBytes: legacyAttachmentSize(part.Data, imageURL),
	}
}

func singleLegacyToolCallJSON(call ToolCall) json.RawMessage {
	buf, err := json.Marshal([]map[string]any{{
		"id":    call.ID,
		"index": call.Index,
		"type":  firstNonBlank(call.Type, "function"),
		"function": map[string]any{
			"name":      call.Name,
			"arguments": call.Arguments,
		},
	}})
	if err != nil {
		return nil
	}
	return buf
}

func cloneProtocolMessages(messages []protocol.Message) []protocol.Message {
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

func legacyVisionResponseLooksBroken(resp CompleteResponse) bool {
	content := strings.ToLower(strings.TrimSpace(resp.Content))
	reasoning := strings.ToLower(strings.TrimSpace(resp.Reasoning))
	if content == "" && reasoning == "" {
		return true
	}
	patterns := []string{
		"无法看到",
		"无法查看",
		"无法描述",
		"未能成功加载",
		"图片未能成功加载",
		"我无法看到",
		"i cannot see the image",
		"cannot see the image",
		"cannot view the image",
		"failed to load",
		"read image:",
	}
	for _, pattern := range patterns {
		if strings.Contains(content, pattern) || strings.Contains(reasoning, pattern) {
			return true
		}
	}
	return false
}

func legacyIsImageAttachment(name, mimeType, data, rawURL string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/") {
		return true
	}
	if ext := strings.ToLower(filepath.Ext(strings.TrimSpace(name))); ext != "" {
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg":
			return true
		}
	}
	kind := strings.ToLower(strings.TrimSpace(legacyMimeTypeFromData(firstNonBlank(data, rawURL))))
	return strings.HasPrefix(kind, "image/")
}

func legacyGeneratedAttachmentName(index int, ext string) string {
	if strings.TrimSpace(ext) == "" {
		ext = ".png"
	}
	return fmt.Sprintf("image-%d%s", index, ext)
}

func legacyAttachmentNameFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.TrimSpace(u.Path) == "" {
		return ""
	}
	name := pathBase(u.Path)
	if name == "." || name == "/" {
		return ""
	}
	return name
}

func pathBase(rawPath string) string {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return ""
	}
	return filepath.Base(strings.ReplaceAll(rawPath, "\\", "/"))
}

func legacyAttachmentSize(primary, fallback string) int {
	if size := legacyDataSize(primary); size > 0 {
		return size
	}
	return legacyDataSize(fallback)
}

func legacyDataSize(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if strings.HasPrefix(raw, "data:") {
		comma := strings.IndexByte(raw, ',')
		if comma < 0 || comma == len(raw)-1 {
			return 0
		}
		payload := raw[comma+1:]
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return 0
		}
		return len(data)
	}
	return 0
}

func legacyFormatAttachmentSize(sizeBytes int) string {
	if sizeBytes <= 0 {
		return ""
	}
	if sizeBytes < 1024 {
		return fmt.Sprintf("%dB", sizeBytes)
	}
	if sizeBytes < 1024*1024 {
		return fmt.Sprintf("%dKB", sizeBytes/1024)
	}
	return fmt.Sprintf("%.1fMB", math.Floor((float64(sizeBytes)/1024.0/1024.0)*10)/10)
}

func legacyImageMimeType(raw, fallback string) string {
	if mimeType := legacyMimeTypeFromData(raw); mimeType != "" {
		return mimeType
	}
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	switch strings.ToLower(legacyImageExtensionFromURL(raw)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	default:
		return "image/png"
	}
}

func legacyMimeTypeFromData(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		return ""
	}
	semi := strings.IndexByte(raw, ';')
	comma := strings.IndexByte(raw, ',')
	if semi < 0 || (comma >= 0 && comma < semi) {
		semi = comma
	}
	if semi <= len("data:") {
		return ""
	}
	return raw[len("data:"):semi]
}

func legacyImageExtensionFromDataOrURL(data, rawURL, mimeType string) string {
	if ext := legacyExtensionFromMime(firstNonBlank(legacyMimeTypeFromData(data), mimeType)); ext != "" {
		return ext
	}
	return legacyImageExtensionFromURL(rawURL)
}

func legacyImageExtensionFromURL(raw string) string {
	name := legacyAttachmentNameFromURL(raw)
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return ""
	}
	return ext
}

func legacyExtensionFromMime(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	case "image/png":
		return ".png"
	default:
		return ""
	}
}

func renderLegacyFilePart(part map[string]any) string {
	name := firstNonBlank(stringValue(part["filename"]), stringValue(part["file_name"]), stringValue(part["file_id"]), stringValue(part["file_url"]))
	mime := firstNonBlank(stringValue(part["mime_type"]), "application/octet-stream")
	var fields []string
	fields = append(fields, "[file]")
	if name != "" {
		fields = append(fields, "name="+name)
	}
	if mime != "" {
		fields = append(fields, "mime="+mime)
	}
	if url := stringValue(part["file_url"]); url != "" {
		fields = append(fields, "url="+url)
	}
	if data := stringValue(part["data"]); data != "" {
		fields = append(fields, "inline_data=present")
	}
	return strings.Join(fields, " ")
}

func removeLegacyToolByName(body map[string]any, toolName string) {
	rawTools, ok := body["tools"].([]any)
	if !ok || strings.TrimSpace(toolName) == "" {
		return
	}
	filtered := make([]any, 0, len(rawTools))
	for _, item := range rawTools {
		obj, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if fn, ok := obj["function"].(map[string]any); ok {
			if stringValue(fn["name"]) == toolName {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	body["tools"] = filtered
}

func legacyReasoningEnabled(req CompleteRequest) bool {
	return normalizeLegacyReasoningEffort(req.ReasoningEffort) != "" || strings.TrimSpace(req.ThinkingType) == "enabled" || strings.TrimSpace(req.ThinkingType) == "adaptive"
}

func normalizeLegacyModelKey(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	switch normalized {
	case "", "auto", "default":
		return "auto"
	case "lite":
		return "lite"
	case "claude-sonnet-4.5", "claude_sonnet_4_5", "sonnet", "claude":
		return "auto"
	default:
		return strings.TrimSpace(model)
	}
}

func applyLegacyReasoningParameters(params map[string]any, req CompleteRequest) {
	effort := normalizeLegacyReasoningEffort(req.ReasoningEffort)
	if effort == "" && legacyReasoningEnabled(req) {
		effort = budgetToLegacyReasoningEffort(req.ThinkingBudget)
	}
	if effort == "" {
		return
	}
	params["reasoning_effort"] = effort
	if _, ok := params["max_tokens"]; !ok {
		params["max_tokens"] = legacyMaxTokensForEffort(effort)
		return
	}
	if current, ok := params["max_tokens"].(float64); ok {
		target := legacyMaxTokensForEffort(effort)
		if int(current) < target {
			params["max_tokens"] = target
		}
	}
}

func normalizeLegacyReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "max"
	default:
		return ""
	}
}

func budgetToLegacyReasoningEffort(budget int) string {
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
		return "max"
	}
}

func legacyMaxTokensForEffort(effort string) int {
	switch effort {
	case "low":
		return 8192
	case "medium":
		return 16384
	case "high":
		return 24576
	case "max":
		return 32768
	default:
		return 32768
	}
}

func rawJSONToAny(raw json.RawMessage) any {
	if rawJSONIsEmpty(raw) {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func loadLegacyIdentity(path string) (legacyIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return legacyIdentity{}, err
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return legacyIdentity{}, err
	}
	node, ok := unwrapLegacyAuth(root)
	if !ok {
		return legacyIdentity{}, fmt.Errorf("unable to locate auth payload in %s", path)
	}
	id := legacyIdentity{
		UID:              firstNonBlank(stringValue(node["uid"]), stringValue(node["userId"]), stringValue(node["id"])),
		Name:             firstNonBlank(stringValue(node["name"]), stringValue(node["displayName"]), stringValue(node["email"])),
		UserType:         firstNonBlank(stringValue(node["userType"]), stringValue(node["user_type"]), "personal_standard"),
		SecurityOAuth:    firstNonBlank(stringValue(node["securityOauthToken"]), stringValue(node["security_oauth_token"]), stringValue(node["token"])),
		RefreshToken:     firstNonBlank(stringValue(node["refreshToken"]), stringValue(node["refresh_token"])),
		OrganizationID:   firstNonBlank(stringValue(node["orgId"]), stringValue(node["organizationId"]), stringValue(node["organization_id"])),
		OrganizationName: firstNonBlank(stringValue(node["orgName"]), stringValue(node["organizationName"]), stringValue(node["organization_name"])),
		YXUID:            firstNonBlank(stringValue(node["yxUid"]), stringValue(node["yx_uid"])),
	}
	id.AID = firstNonBlank(stringValue(node["aid"]), id.UID)
	if id.Name == "" {
		id.Name = id.UID
	}
	if id.UID == "" || id.SecurityOAuth == "" || id.RefreshToken == "" {
		return legacyIdentity{}, fmt.Errorf("missing required auth fields in %s", path)
	}
	return id, nil
}

func unwrapLegacyAuth(root any) (map[string]any, bool) {
	switch v := root.(type) {
	case []any:
		if len(v) == 0 {
			return nil, false
		}
		return unwrapLegacyAuth(v[0])
	case map[string]any:
		if inner, ok := v["auth_user_info_raw"].(map[string]any); ok {
			return inner, true
		}
		if user, ok := v["user"].(map[string]any); ok {
			if inner, ok := user["auth_user_info_raw"].(map[string]any); ok {
				return inner, true
			}
		}
		return v, true
	default:
		return nil, false
	}
}

func newLegacySession(identity legacyIdentity) (legacySession, error) {
	tempKey := []byte(randomHex(16))
	cosyKey, err := legacyRSAEncrypt(tempKey)
	if err != nil {
		return legacySession{}, err
	}
	info, err := legacyEncryptInfo(identity, tempKey)
	if err != nil {
		return legacySession{}, err
	}
	return legacySession{
		Identity:     identity,
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         info,
		MachineID:    randomUUID(),
		MachineToken: randomMachineToken(),
		MachineType:  randomHex(18),
	}, nil
}

func legacyEncryptInfo(identity legacyIdentity, key []byte) (string, error) {
	payload := map[string]string{
		"name":                 identity.Name,
		"aid":                  identity.AID,
		"uid":                  identity.UID,
		"yx_uid":               identity.YXUID,
		"organization_id":      identity.OrganizationID,
		"organization_name":    identity.OrganizationName,
		"user_type":            identity.UserType,
		"security_oauth_token": identity.SecurityOAuth,
		"refresh_token":        identity.RefreshToken,
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	plain = pkcs5Pad(plain, block.BlockSize())
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, plain)
	return base64.StdEncoding.EncodeToString(out), nil
}

func legacyRSAEncrypt(plain []byte) (string, error) {
	block, _ := pem.Decode([]byte(legacyServerPubKey))
	if block == nil {
		return "", fmt.Errorf("invalid public key")
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("unexpected public key type")
	}
	out, err := rsa.EncryptPKCS1v15(rand.Reader, pub, plain)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

func legacyBuildPayloadB64(info string) (string, error) {
	payload := struct {
		CosyVersion string `json:"cosyVersion"`
		IdeVersion  string `json:"ideVersion"`
		Info        string `json:"info"`
		RequestID   string `json:"requestId"`
		Version     string `json:"version"`
	}{
		CosyVersion: legacyCosyVersion,
		IdeVersion:  "",
		Info:        info,
		RequestID:   randomUUID(),
		Version:     "v1",
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func legacySignRequest(payloadB64, cosyKey, cosyDate, body, path string) string {
	sum := md5.Sum([]byte(payloadB64 + "\n" + cosyKey + "\n" + cosyDate + "\n" + body + "\n" + path))
	return fmt.Sprintf("%x", sum[:])
}

func legacyEncode(plaintext []byte) string {
	const stdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	const customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	const customPad = '$'

	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]

	lookup := make(map[byte]byte, 65)
	for i := 0; i < 64; i++ {
		lookup[stdAlphabet[i]] = customAlphabet[i]
	}
	lookup['='] = customPad

	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = lookup[rearranged[i]]
	}
	return string(out)
}

func newLegacyTemplate() map[string]any {
	return map[string]any{
		"request_id":     randomUUID(),
		"request_set_id": randomUUID(),
		"chat_record_id": randomUUID(),
		"stream":         true,
		"chat_task":      "FREE_INPUT",
		"chat_context": map[string]any{
			"chatPrompt": "",
			"extra": map[string]any{
				"context": []any{},
				"modelConfig": map[string]any{
					"is_reasoning": false,
					"key":          "auto",
				},
				"originalContent": map[string]any{
					"type": "text",
					"text": "",
				},
			},
			"features":  []any{},
			"imageUrls": nil,
			"text": map[string]any{
				"type": "text",
				"text": "",
			},
		},
		"image_urls":       nil,
		"is_reply":         true,
		"is_retry":         false,
		"session_id":       randomUUID(),
		"code_language":    "",
		"source":           1,
		"version":          "3",
		"chat_prompt":      "",
		"parameters":       map[string]any{"max_tokens": 32768},
		"aliyun_user_type": "personal_standard",
		"session_type":     "qodercli",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"model_config": map[string]any{
			"key":              "auto",
			"display_name":     "Auto",
			"model":            "",
			"format":           "openai",
			"is_vl":            false,
			"is_reasoning":     false,
			"api_key":          "",
			"url":              "",
			"source":           "system",
			"max_input_tokens": 180000,
		},
		"business": map[string]any{
			"id":          randomUUID(),
			"begin_at":    time.Now().UnixMilli(),
			"scene":       "chat",
			"type":        "chat",
			"sub_type":    "free_input",
			"name":        "",
			"language":    "",
			"parent_type": "",
		},
		"messages": []any{},
		"tools":    defaultLegacyTools(),
	}
}

func defaultLegacyTools() []any {
	return []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "Read",
				"description": "Read a local file or image attachment referenced in the conversation.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_path": map[string]any{
							"type":        "string",
							"description": "Path of the file to read.",
						},
					},
					"required": []any{"file_path"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "Bash",
				"description": "Execute a shell command when the client allows command execution.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "Command to execute.",
						},
					},
					"required": []any{"command"},
				},
			},
		},
	}
}

func deepCopyMap(in map[string]any) map[string]any {
	buf, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(buf, &out)
	return out
}

func pkcs5Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	out := make([]byte, 0, len(data)+padding)
	out = append(out, data...)
	for i := 0; i < padding; i++ {
		out = append(out, byte(padding))
	}
	return out
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytesToUint32(b[0:4]),
		bytesToUint16(b[4:6]),
		bytesToUint16(b[6:8]),
		bytesToUint16(b[8:10]),
		bytesToUint48(b[10:16]),
	)
}

func randomMachineToken() string {
	raw := randomUUID() + randomUUID()
	if len(raw) > 50 {
		raw = raw[:50]
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func bytesToUint16(b []byte) uint16 {
	return uint16(b[0])<<8 | uint16(b[1])
}

func bytesToUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func bytesToUint48(b []byte) uint64 {
	var out uint64
	for _, x := range b {
		out = (out << 8) | uint64(x)
	}
	return out
}
