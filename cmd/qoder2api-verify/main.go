package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type result struct {
	Endpoint string
	Text     string
}

func main() {
	baseURL := flag.String("base-url", "http://127.0.0.1:8963", "bridge base URL")
	model := flag.String("model", "auto", "request model")
	prompt := flag.String("prompt", "请只用一句话描述这张图片。", "prompt text")
	timeout := flag.Duration("timeout", 2*time.Minute, "per-request timeout")
	flag.Parse()

	files := flag.Args()
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qoder2api-verify [flags] <image-file> [<image-file>...]")
		os.Exit(2)
	}

	client := &http.Client{Timeout: *timeout}
	for _, file := range files {
		if err := verifyFile(client, strings.TrimRight(*baseURL, "/"), *model, *prompt, file); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, err)
			os.Exit(1)
		}
	}
}

func verifyFile(client *http.Client, baseURL, model, prompt, file string) error {
	dataURL, err := fileToDataURL(file)
	if err != nil {
		return err
	}

	fmt.Printf("FILE %s\n", file)
	results := []result{
		{Endpoint: "/v1/chat/completions"},
		{Endpoint: "/v1/responses"},
		{Endpoint: "/v1/messages"},
	}

	for i := range results {
		text, err := requestEndpoint(client, baseURL, model, prompt, dataURL, results[i].Endpoint)
		if err != nil {
			return fmt.Errorf("%s: %w", results[i].Endpoint, err)
		}
		results[i].Text = text
	}

	for _, item := range results {
		fmt.Printf("  %s\n    %s\n", item.Endpoint, item.Text)
	}
	return nil
}

func requestEndpoint(client *http.Client, baseURL, model, prompt, dataURL, endpoint string) (string, error) {
	payload, err := buildPayload(model, prompt, dataURL, endpoint)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return extractText(endpoint, body)
}

func buildPayload(model, prompt, dataURL, endpoint string) ([]byte, error) {
	var payload any
	switch endpoint {
	case "/v1/chat/completions":
		payload = map[string]any{
			"model": model,
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": prompt},
					{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
				},
			}},
		}
	case "/v1/responses":
		payload = map[string]any{
			"model": model,
			"input": []map[string]any{{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": prompt},
					{"type": "input_image", "image_url": map[string]any{"url": dataURL}},
				},
			}},
		}
	case "/v1/messages":
		payload = map[string]any{
			"model":      model,
			"max_tokens": 512,
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": prompt},
					{"type": "image", "source": map[string]any{"type": "base64", "media_type": detectMIMEType(fileExt(dataURL)), "data": dataOnly(dataURL)}},
				},
			}},
		}
	default:
		return nil, fmt.Errorf("unsupported endpoint %s", endpoint)
	}
	return json.Marshal(payload)
}

func extractText(endpoint string, body []byte) (string, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", err
	}

	switch endpoint {
	case "/v1/chat/completions":
		choices, _ := doc["choices"].([]any)
		if len(choices) == 0 {
			return "", fmt.Errorf("missing choices")
		}
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		return stringValue(message["content"]), nil
	case "/v1/responses":
		if text := stringValue(doc["output_text"]); text != "" {
			return text, nil
		}
		return "", fmt.Errorf("missing output_text")
	case "/v1/messages":
		content, _ := doc["content"].([]any)
		var parts []string
		for _, item := range content {
			obj, _ := item.(map[string]any)
			switch stringValue(obj["type"]) {
			case "text":
				if text := stringValue(obj["text"]); text != "" {
					parts = append(parts, text)
				}
			case "thinking":
				if text := stringValue(obj["thinking"]); text != "" {
					parts = append(parts, "[thinking] "+text)
				}
			}
		}
		if len(parts) == 0 {
			return "", fmt.Errorf("missing content text")
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", fmt.Errorf("unsupported endpoint %s", endpoint)
	}
}

func fileToDataURL(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mimeType := detectMIMEType(filepath.Ext(path))
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

func detectMIMEType(ext string) string {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func fileExt(dataURL string) string {
	switch {
	case strings.HasPrefix(dataURL, "data:image/jpeg;"):
		return ".jpg"
	case strings.HasPrefix(dataURL, "data:image/gif;"):
		return ".gif"
	case strings.HasPrefix(dataURL, "data:image/webp;"):
		return ".webp"
	default:
		return ".png"
	}
}

func dataOnly(dataURL string) string {
	const marker = ";base64,"
	idx := strings.Index(dataURL, marker)
	if idx < 0 {
		return dataURL
	}
	return dataURL[idx+len(marker):]
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}
