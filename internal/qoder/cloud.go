package qoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"qoder2api/internal/protocol"
)

type cloudBackend struct {
	baseURL       string
	pat           string
	defaultModel  string
	workspaceRoot string
	client        *http.Client

	mu            sync.Mutex
	environmentID string
	agentID       string
}

type cloudListResponse[T any] struct {
	Data []T `json:"data"`
}

type cloudEnvironment struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type cloudAgent struct {
	ID string `json:"id"`
}

type cloudSession struct {
	ID string `json:"id"`
}

type cloudEventStream struct {
	resp     *http.Response
	scanner  *bufio.Scanner
	lastType string
}

func (b *cloudBackend) Capabilities() Capabilities {
	return Capabilities{}
}

func (b *cloudBackend) Models(_ context.Context) ([]ModelInfo, error) {
	return nil, fmt.Errorf("models listing not supported in PAT mode")
}

func NewCloudBackend(opts Options, client *http.Client) Backend {
	return &cloudBackend{
		baseURL:       strings.TrimRight(opts.BaseURL, "/") + "/api/v1/cloud",
		pat:           opts.PAT,
		defaultModel:  opts.DefaultModel,
		workspaceRoot: opts.WorkspaceRoot,
		client:        client,
	}
}

func (b *cloudBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	stream, err := b.Stream(ctx, req)
	if err != nil {
		return CompleteResponse{}, err
	}
	defer stream.Close()

	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls json.RawMessage
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
			toolCalls = event.ToolCalls
		}
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	return CompleteResponse{
		Content:   content.String(),
		Reasoning: reasoning.String(),
		ToolCalls: toolCalls,
		Usage:     usage,
	}, nil
}

func (b *cloudBackend) Stream(ctx context.Context, req CompleteRequest) (Stream, error) {
	if err := b.ensureResources(ctx, req.Model); err != nil {
		return nil, err
	}

	sessionID, err := b.createSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.sendMessage(ctx, sessionID, req); err != nil {
		return nil, err
	}
	return b.openStream(ctx, sessionID)
}

func (b *cloudBackend) ensureResources(ctx context.Context, model string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.environmentID == "" {
		envID, err := b.ensureEnvironment(ctx)
		if err != nil {
			return err
		}
		b.environmentID = envID
	}
	if b.agentID == "" {
		agentID, err := b.createAgent(ctx, model)
		if err != nil {
			return err
		}
		b.agentID = agentID
	}
	return nil
}

func (b *cloudBackend) ensureEnvironment(ctx context.Context) (string, error) {
	var list cloudListResponse[cloudEnvironment]
	if err := b.getJSON(ctx, "/environments", &list); err != nil {
		return "", err
	}
	for _, env := range list.Data {
		if env.Status == "" || env.Status == "ready" {
			return env.ID, nil
		}
	}

	body := map[string]any{
		"name": "qoder2api-default",
		"config": map[string]any{
			"type": "cloud",
			"networking": map[string]any{
				"type": "unrestricted",
			},
		},
	}
	var created cloudEnvironment
	if err := b.postJSON(ctx, "/environments", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (b *cloudBackend) createAgent(ctx context.Context, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		model = b.defaultModel
	}

	instructions := strings.TrimSpace(`
You are an efficient programming assistant.
Reply concisely and correctly.
When the user supplies chat history, treat the provided transcript as authoritative context.
If the task is coding-related, prefer concrete patches, commands, and implementation details.
`)

	body := map[string]any{
		"name":   "qoder2api-go-bridge",
		"model":  model,
		"system": instructions,
		"tools": []map[string]any{
			{"type": "bash_20250124"},
			{"type": "text_editor_20250124"},
		},
	}
	var created cloudAgent
	if err := b.postJSON(ctx, "/agents", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (b *cloudBackend) createSession(ctx context.Context) (string, error) {
	body := map[string]any{
		"agent":          b.agentID,
		"environment_id": b.environmentID,
	}
	if b.workspaceRoot != "" {
		body["metadata"] = map[string]any{
			"workspace_root": b.workspaceRoot,
		}
	}
	var created cloudSession
	if err := b.postJSON(ctx, "/sessions", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (b *cloudBackend) sendMessage(ctx context.Context, sessionID string, req CompleteRequest) error {
	body := map[string]any{
		"events": []map[string]any{{
			"type":    "user.message",
			"content": renderPrompt(req),
		}},
	}
	return b.postJSON(ctx, "/sessions/"+sessionID+"/events", body, nil)
}

func (b *cloudBackend) openStream(ctx context.Context, sessionID string) (Stream, error) {
	url := b.baseURL + "/sessions/" + sessionID + "/events/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.pat)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("open stream http %d: %s", resp.StatusCode, string(data))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	return &cloudEventStream{resp: resp, scanner: scanner}, nil
}

func (s *cloudEventStream) Next() (protocol.DeltaEvent, error) {
	var eventType string
	var dataLine string
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			s.lastType = eventType
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			continue
		}
		if line == "" && (eventType != "" || dataLine != "") {
			return parseCloudEvent(eventType, dataLine)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return protocol.DeltaEvent{}, err
	}
	return protocol.DeltaEvent{Done: true}, nil
}

func (s *cloudEventStream) Close() error {
	return s.resp.Body.Close()
}

func parseCloudEvent(eventType, dataLine string) (protocol.DeltaEvent, error) {
	if eventType == "session.status_idle" || eventType == "terminated" {
		return protocol.DeltaEvent{Done: true}, nil
	}
	if eventType == "session.error" {
		return protocol.DeltaEvent{}, fmt.Errorf("session error: %s", dataLine)
	}
	if eventType != "agent.message" && eventType != "agent.tool_use" {
		return protocol.DeltaEvent{}, nil
	}

	var payload map[string]any
	if dataLine != "" && dataLine != "{}" {
		_ = json.Unmarshal([]byte(dataLine), &payload)
	}
	switch eventType {
	case "agent.message":
		return protocol.DeltaEvent{
			Content: normalizeContent(payload["content"]),
		}, nil
	case "agent.tool_use":
		name := stringValue(payload["name"])
		input := payload["input"]
		call := []map[string]any{{
			"id":   "call_" + randomHex(12),
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": mustJSON(input),
			},
		}}
		raw, _ := json.Marshal(call)
		return protocol.DeltaEvent{ToolCalls: raw}, nil
	default:
		return protocol.DeltaEvent{}, nil
	}
}

func (b *cloudBackend) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.pat)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("get %s http %d: %s", path, resp.StatusCode, string(data))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (b *cloudBackend) postJSON(ctx context.Context, path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.pat)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post %s http %d: %s", path, resp.StatusCode, string(data))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func renderPrompt(req CompleteRequest) string {
	var sb strings.Builder
	sb.WriteString("You are handling an OpenAI-style chat completion request.\n")
	sb.WriteString("Conversation transcript:\n")
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		sb.WriteString("\n[")
		sb.WriteString(role)
		sb.WriteString("]\n")
		sb.WriteString(normalizeContent(msg.Content))
		sb.WriteString("\n")
	}
	if !rawJSONIsEmpty(req.Tools) {
		sb.WriteString("\nAvailable tool schema from the caller:\n")
		sb.Write(req.Tools)
		sb.WriteString("\nIf tool use is necessary, emit structured tool-use actions when supported by the runtime.\n")
	}
	sb.WriteString("\nRespond to the latest user request using the transcript above.")
	return sb.String()
}

func normalizeContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(m["type"]) == "text" {
				parts = append(parts, stringValue(m["text"]))
			}
		}
		return strings.Join(parts, "\n")
	default:
		buf, _ := json.Marshal(v)
		return string(buf)
	}
}

func mustJSON(v any) string {
	if v == nil {
		return "{}"
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(buf)
}
