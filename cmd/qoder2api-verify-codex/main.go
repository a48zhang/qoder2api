package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type codexEvent struct {
	Type  string          `json:"type"`
	Item  json.RawMessage `json:"item,omitempty"`
	Usage map[string]any  `json:"usage,omitempty"`
}

type codexItem struct {
	ID               string `json:"id,omitempty"`
	Type             string `json:"type"`
	Text             string `json:"text,omitempty"`
	Command          string `json:"command,omitempty"`
	AggregatedOutput string `json:"aggregated_output,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	Status           string `json:"status,omitempty"`
}

type codexRunResult struct {
	LastMessage       string
	Usage             map[string]any
	CommandExecutions []codexItem
}

func main() {
	codexPath := flag.String("codex", "", "Codex CLI path")
	baseURL := flag.String("base-url", "http://127.0.0.1:8963/v1", "OpenAI-compatible base URL; keep /v1 because Codex appends /responses")
	model := flag.String("model", "auto", "Codex model argument")
	cwd := flag.String("cwd", "", "working directory for Codex")
	timeout := flag.Duration("timeout", 3*time.Minute, "per-check timeout")
	flag.Parse()

	path, err := resolveCodexPath(*codexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	workdir := strings.TrimSpace(*cwd)
	if workdir == "" {
		workdir, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	if err := runTextCheck(path, *baseURL, *model, workdir, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "text: %v\n", err)
		os.Exit(1)
	}
	if err := runToolCheck(path, *baseURL, *model, workdir, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "tool: %v\n", err)
		os.Exit(1)
	}

	for _, file := range flag.Args() {
		if err := runImageCheck(path, *baseURL, *model, workdir, *timeout, file); err != nil {
			fmt.Fprintf(os.Stderr, "image %s: %v\n", file, err)
			os.Exit(1)
		}
	}
}

func resolveCodexPath(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	if path, err := exec.LookPath("codex"); err == nil {
		return path, nil
	}
	return "", errors.New("Codex CLI not found; pass -codex /path/to/codex")
}

func runTextCheck(codexPath, baseURL, model, cwd string, timeout time.Duration) error {
	result, err := runCodex(codexPath, baseURL, model, cwd, timeout, nil, false, "Reply exactly CODEX_LOCAL_OK")
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.LastMessage) != "CODEX_LOCAL_OK" {
		return fmt.Errorf("unexpected final message %q", result.LastMessage)
	}
	fmt.Println("TEXT")
	fmt.Printf("  result: %s\n", result.LastMessage)
	printUsage(result.Usage)
	return nil
}

func runToolCheck(codexPath, baseURL, model, cwd string, timeout time.Duration) error {
	result, err := runCodex(codexPath, baseURL, model, cwd, timeout, nil, true, "Run printf QODER2API_CODEX_TOOL_OK using the shell, then answer with exactly the command output.")
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.LastMessage) != "QODER2API_CODEX_TOOL_OK" {
		return fmt.Errorf("unexpected final message %q", result.LastMessage)
	}
	if len(result.CommandExecutions) == 0 {
		return errors.New("missing command_execution event")
	}
	fmt.Println("TOOL")
	for _, item := range result.CommandExecutions {
		exitCode := "nil"
		if item.ExitCode != nil {
			exitCode = fmt.Sprintf("%d", *item.ExitCode)
		}
		fmt.Printf("  command: %s\n", item.Command)
		fmt.Printf("  output: %s\n", strings.TrimSpace(item.AggregatedOutput))
		fmt.Printf("  exit: %s\n", exitCode)
	}
	fmt.Printf("  result: %s\n", result.LastMessage)
	printUsage(result.Usage)
	return nil
}

func runImageCheck(codexPath, baseURL, model, cwd string, timeout time.Duration, file string) error {
	result, err := runCodex(codexPath, baseURL, model, cwd, timeout, []string{file}, false, "请只用一句话说明这张图的主要内容。")
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.LastMessage) == "" {
		return errors.New("missing final image description")
	}
	fmt.Printf("IMAGE %s\n", file)
	fmt.Printf("  result: %s\n", result.LastMessage)
	printUsage(result.Usage)
	return nil
}

func runCodex(codexPath, baseURL, model, cwd string, timeout time.Duration, images []string, allowTools bool, prompt string) (codexRunResult, error) {
	args := []string{}
	if allowTools {
		args = append(args, "--ask-for-approval", "never")
	}
	args = append(args,
		"exec",
	)
	for _, image := range images {
		args = append(args, "-i", image)
	}
	args = append(args,
		"--json",
		"--ephemeral",
		"--ignore-rules",
		"--skip-git-repo-check",
		"-C", cwd,
		"-c", `model_provider="local-qoder"`,
		"-c", `model="`+escapeTomlString(model)+`"`,
		"-c", `model_providers.local-qoder.name="local-qoder"`,
		"-c", `model_providers.local-qoder.wire_api="responses"`,
		"-c", `model_providers.local-qoder.requires_openai_auth=true`,
		"-c", `model_providers.local-qoder.base_url="`+escapeTomlString(strings.TrimRight(baseURL, "/"))+`"`,
	)
	if allowTools {
		args = append(args, "-s", "danger-full-access")
	}
	args = append(args, prompt)

	cmd := exec.Command(codexPath, args...)
	cmd.Env = codexEnv()
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(nil)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := runWithTimeout(cmd, timeout); err != nil {
		return codexRunResult{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if strings.TrimSpace(stderr.String()) != "" {
		fmt.Fprintf(os.Stderr, "codex stderr: %s\n", strings.TrimSpace(stderr.String()))
	}
	return parseCodexResult(stdout.Bytes())
}

func codexEnv() []string {
	env := os.Environ()
	hasOpenAIKey := false
	for _, item := range env {
		if strings.HasPrefix(item, "OPENAI_API_KEY=") {
			hasOpenAIKey = true
			break
		}
	}
	if !hasOpenAIKey {
		env = append(env, "OPENAI_API_KEY=test-key")
	}
	return env
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

func parseCodexResult(data []byte) (codexRunResult, error) {
	var result codexRunResult
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return codexRunResult{}, fmt.Errorf("parse codex jsonl: %w", err)
		}
		switch event.Type {
		case "item.completed":
			var item codexItem
			if err := json.Unmarshal(event.Item, &item); err != nil {
				return codexRunResult{}, fmt.Errorf("parse codex item: %w", err)
			}
			switch item.Type {
			case "agent_message":
				result.LastMessage = item.Text
			case "command_execution":
				result.CommandExecutions = append(result.CommandExecutions, item)
			}
		case "turn.completed":
			result.Usage = event.Usage
		}
	}
	if err := scanner.Err(); err != nil {
		return codexRunResult{}, err
	}
	if strings.TrimSpace(result.LastMessage) == "" {
		return codexRunResult{}, errors.New("missing Codex agent_message")
	}
	return result, nil
}

func printUsage(usage map[string]any) {
	if len(usage) == 0 {
		return
	}
	if data, err := json.Marshal(usage); err == nil {
		fmt.Printf("  usage: %s\n", data)
	}
}

func escapeTomlString(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}
