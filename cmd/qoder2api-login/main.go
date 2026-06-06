package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"qoder2api/internal/auth"
)

func main() {
	output := flag.String("o", "", "auth JSON 输出路径（默认 ~/.config/qoder2api/auth.json）")
	flag.Parse()

	outPath := *output
	if outPath == "" {
		if env := os.Getenv("QODER_AUTH_JSON"); env != "" {
			outPath = env
		} else {
			outPath = auth.DefaultAuthJSONPath()
		}
	}

	// 确保输出目录存在
	if dir := filepath.Dir(outPath); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Fatalf("创建目录失败: %v", err)
		}
	}

	fmt.Println("[qoder2api-login] 正在创建 Qoder 授权会话...")

	resp, err := auth.StartLogin()
	if err != nil {
		log.Fatalf("创建登录会话失败: %v", err)
	}

	fmt.Println()
	fmt.Println("请在浏览器中打开以下链接并完成授权:")
	fmt.Println()
	fmt.Printf("  %s\n", resp.VerificationURI)
	fmt.Println()
	fmt.Printf("授权超时: %d 秒\n", resp.ExpiresIn)
	fmt.Println()

	if err := auth.OpenBrowser(resp.VerificationURI); err != nil {
		fmt.Printf("自动打开浏览器失败，请手动复制上面的链接: %v\n", err)
	} else {
		fmt.Println("已自动打开浏览器，等待授权...")
	}

	fmt.Println()

	// 带进度提示的轮询
	done := make(chan *auth.LoginResult, 1)
	errCh := make(chan error, 1)

	go func() {
		result, err := auth.CompleteLogin(resp.LoginID)
		if err != nil {
			errCh <- err
			return
		}
		done <- result
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	start := time.Now()

	for {
		select {
		case result := <-done:
			fmt.Println()

			if err := auth.SaveAuthJSON(outPath, result); err != nil {
				log.Fatalf("保存 auth JSON 失败: %v", err)
			}

			email := ""
			if e, ok := result.UserStatus["email"].(string); ok {
				email = e
			}
			name := ""
			if n, ok := result.UserStatus["name"].(string); ok {
				name = n
			}

			fmt.Println("授权成功!")
			fmt.Printf("  用户: %s (%s)\n", name, email)
			fmt.Printf("  已保存: %s\n", outPath)
			fmt.Println()
			fmt.Println("启动方式:")
			fmt.Printf("  QODER_AUTH_JSON=%s go run ./cmd/qoder2api\n", outPath)
			fmt.Println()
			fmt.Println("或直接启动（自动使用默认路径）:")
			fmt.Println("  go run ./cmd/qoder2api")
			return

		case err := <-errCh:
			fmt.Println()
			log.Fatalf("授权失败: %v", err)

		case <-ticker.C:
			elapsed := int(time.Since(start).Seconds())
			remaining := int(resp.ExpiresIn) - elapsed
			if remaining > 0 {
				fmt.Printf("\r等待授权中... 剩余 %d 秒", remaining)
			}
		}
	}
}
