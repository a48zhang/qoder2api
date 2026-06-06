package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type streamEvent struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
	Result    string         `json:"result,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Usage     map[string]any `json:"usage,omitempty"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	} `json:"message,omitempty"`
}

func main() {
	claudePath := flag.String("claude", "", "Claude Code CLI path")
	baseURL := flag.String("base-url", "http://127.0.0.1:8963", "Anthropic-compatible base URL")
	model := flag.String("model", "auto", "Claude Code model argument")
	prompt := flag.String("prompt", "请只用一句话描述这张图片。", "prompt text")
	timeout := flag.Duration("timeout", 3*time.Minute, "per-file timeout")
	flag.Parse()

	files := flag.Args()
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qoder2api-verify-claude [flags] <image-file> [<image-file>...]")
		os.Exit(2)
	}

	path, err := resolveClaudePath(*claudePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for _, file := range files {
		result, err := verifyFile(path, *baseURL, *model, *prompt, *timeout, file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, err)
			os.Exit(1)
		}
		fmt.Printf("FILE %s\n", file)
		fmt.Printf("  result: %s\n", result.Result)
		if result.SessionID != "" {
			fmt.Printf("  session: %s\n", result.SessionID)
		}
		if len(result.Usage) > 0 {
			if data, err := json.Marshal(result.Usage); err == nil {
				fmt.Printf("  usage: %s\n", data)
			}
		}
	}
}

func resolveClaudePath(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	if path, err := exec.LookPath("claude"); err == nil {
		return path, nil
	}
	return "", errors.New("Claude Code CLI not found; pass -claude /path/to/claude")
}

func verifyFile(claudePath, baseURL, model, prompt string, timeout time.Duration, file string) (streamEvent, error) {
	input, err := buildInput(file, prompt)
	if err != nil {
		return streamEvent{}, err
	}

	cmd := exec.Command(claudePath,
		"--bare",
		"-p",
		"--verbose",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--setting-sources", "local",
		"--tools", "",
		"--model", model,
		"--no-session-persistence",
	)
	cmd.Env = claudeEnv(baseURL)
	cmd.Stdin = bytes.NewReader(input)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := runWithTimeout(cmd, timeout); err != nil {
		return streamEvent{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if strings.TrimSpace(stderr.String()) != "" {
		fmt.Fprintf(os.Stderr, "claude stderr: %s\n", strings.TrimSpace(stderr.String()))
	}

	result, err := parseResult(stdout.Bytes())
	if err != nil {
		return streamEvent{}, err
	}
	if result.IsError {
		return streamEvent{}, fmt.Errorf("claude returned error result: %s", result.Result)
	}
	return result, nil
}

func buildInput(file, prompt string) ([]byte, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": prompt},
				{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": detectMIMEType(filepath.Ext(file)),
						"data":       base64.StdEncoding.EncodeToString(raw),
					},
				},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func claudeEnv(baseURL string) []string {
	home := os.Getenv("HOME")
	path := os.Getenv("PATH")
	term := os.Getenv("TERM")
	if term == "" {
		term = "xterm-256color"
	}
	return []string{
		"HOME=" + home,
		"PATH=" + path,
		"TERM=" + term,
		"ANTHROPIC_BASE_URL=" + strings.TrimRight(baseURL, "/"),
		"ANTHROPIC_API_KEY=test-key",
		"ANTHROPIC_AUTH_TOKEN=test-key",
		"ANTHROPIC_MODEL=auto",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=auto",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=auto",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=auto",
		"DISABLE_TELEMETRY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
}

func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return fmt.Errorf("timed out after %s", timeout)
	}
}

func parseResult(data []byte) (streamEvent, error) {
	var lastAssistant streamEvent
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return streamEvent{}, fmt.Errorf("parse claude stream-json: %w", err)
		}
		switch event.Type {
		case "assistant":
			lastAssistant = event
		case "result":
			if strings.TrimSpace(event.Result) == "" {
				event.Result = assistantText(lastAssistant)
			}
			return event, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return streamEvent{}, err
	}
	if text := assistantText(lastAssistant); text != "" {
		lastAssistant.Result = text
		return lastAssistant, nil
	}
	return streamEvent{}, errors.New("missing claude result event")
}

func assistantText(event streamEvent) string {
	var parts []string
	for _, part := range event.Message.Content {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
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
