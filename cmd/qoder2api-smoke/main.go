package main

import (
	"bytes"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	authJSON := flag.String("auth-json", os.Getenv("QODER_AUTH_JSON"), "Qoder auth JSON path")
	model := flag.String("model", getenvDefault("QODER_MODEL", "auto"), "Qoder model")
	port := flag.Int("port", getenvInt("QODER_PORT", 8963), "local bridge port")
	timeout := flag.Duration("timeout", 5*time.Minute, "timeout for each verifier")
	skipCodex := flag.Bool("skip-codex", false, "skip Codex CLI verification")
	skipClaude := flag.Bool("skip-claude", false, "skip Claude Code CLI verification")
	flag.Parse()

	if strings.TrimSpace(*authJSON) == "" && strings.TrimSpace(os.Getenv("QODER_PAT")) == "" {
		fmt.Fprintln(os.Stderr, "set -auth-json, QODER_AUTH_JSON, or QODER_PAT")
		os.Exit(2)
	}

	files := flag.Args()
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "pass at least one image file")
		os.Exit(2)
	}

	if isPortOpen(*port) {
		fmt.Fprintf(os.Stderr, "port %d is already in use; stop the existing process or pass -port\n", *port)
		os.Exit(1)
	}

	server, err := startBridge(*authJSON, *model, *port)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer stopProcess(server)

	baseURL := "http://127.0.0.1:" + strconv.Itoa(*port)
	if err := waitForBridge(baseURL, 30*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	steps := []struct {
		name string
		args []string
	}{
		{
			name: "protocol verifier",
			args: append([]string{"run", "./cmd/qoder2api-verify", "-base-url", baseURL, "-model", *model}, files...),
		},
	}
	if !*skipCodex {
		steps = append(steps, struct {
			name string
			args []string
		}{
			name: "codex verifier",
			args: append([]string{"run", "./cmd/qoder2api-verify-codex", "-base-url", baseURL + "/v1", "-model", *model, "-timeout", timeout.String()}, files...),
		})
	}
	if !*skipClaude {
		steps = append(steps, struct {
			name string
			args []string
		}{
			name: "claude verifier",
			args: append([]string{"run", "./cmd/qoder2api-verify-claude", "-base-url", baseURL, "-model", *model, "-timeout", timeout.String()}, files...),
		})
	}

	for _, step := range steps {
		fmt.Printf("\n== %s ==\n", step.name)
		if err := runGo(step.args, *timeout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func startBridge(authJSON, model string, port int) (*exec.Cmd, error) {
	cmd := exec.Command("go", "run", "./cmd/qoder2api")
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env,
		"QODER_MODEL="+model,
		"QODER_PORT="+strconv.Itoa(port),
	)
	if strings.TrimSpace(authJSON) != "" {
		abs, err := filepath.Abs(authJSON)
		if err != nil {
			return nil, err
		}
		cmd.Env = append(cmd.Env, "QODER_AUTH_JSON="+abs)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
	}()
	go func() {
		time.Sleep(2 * time.Second)
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			fmt.Fprintf(os.Stderr, "bridge exited early:\n%s\n", output.String())
		}
	}()
	return cmd, nil
}

func waitForBridge(baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/chat/completions", nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("bridge did not become ready at %s", baseURL)
}

func runGo(args []string, timeout time.Duration) error {
	cmd := exec.Command("go", args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
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
		return fmt.Errorf("command timed out: go %s", strings.Join(args, " "))
	}
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return
	}
	_ = cmd.Process.Kill()
}

func isPortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func getenvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}
