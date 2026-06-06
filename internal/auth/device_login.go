package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	loginBaseURL    = "https://qoder.com/device/selectAccounts"
	openAPIBaseURL  = "https://openapi.qoder.sh"
	clientID        = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	challengeMethod = "S256"
	pollPath        = "/api/v1/deviceToken/poll"
	userInfoPath    = "/api/v1/userinfo"
	userStatusPath  = "/api/v3/user/status"
	userPlanPath    = "/api/v2/user/plan"
	creditUsagePath = "/api/v2/quota/usage"
	pollInterval    = time.Second
	pollTimeout     = 600 * time.Second
)

// LoginResponse 登录会话信息
type LoginResponse struct {
	LoginID         string
	VerificationURI string
	ExpiresIn       int64
}

// DeviceTokenResult deviceToken/poll 返回的 token 数据
type DeviceTokenResult struct {
	Token                 string `json:"token"`
	UserID                string `json:"user_id"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresAt             string `json:"expires_at"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
}

// LoginResult 完整登录结果，用于保存 auth JSON
type LoginResult struct {
	DeviceToken *DeviceTokenResult
	UserInfo    map[string]any
	UserStatus  map[string]any
	UserPlan    map[string]any
}

// StartLogin 创建 Device Login 会话，返回验证 URL
func StartLogin() (*LoginResponse, error) {
	nonce, err := generateNonce()
	if err != nil {
		return nil, fmt.Errorf("生成 nonce 失败: %w", err)
	}
	verifier, err := generatePKCEVerifier()
	if err != nil {
		return nil, fmt.Errorf("生成 PKCE verifier 失败: %w", err)
	}
	challenge := generatePKCEChallenge(verifier)

	loginID, err := generateUUID()
	if err != nil {
		return nil, fmt.Errorf("生成 login_id 失败: %w", err)
	}

	machineID := readQoderMachineID()

	verificationURI := buildVerificationURL(nonce, challenge, machineID)

	// 存储 pending 状态供 CompleteLogin 使用
	pendingMu.Lock()
	pending = &pendingState{
		loginID:         loginID,
		nonce:           nonce,
		verifier:        verifier,
		challengeMethod: challengeMethod,
		machineToken:    readQoderMachineToken(),
		machineType:     readQoderMachineType(),
		expiresAt:       time.Now().Add(pollTimeout),
	}
	pendingMu.Unlock()

	return &LoginResponse{
		LoginID:         loginID,
		VerificationURI: verificationURI,
		ExpiresIn:       int64(pollTimeout.Seconds()),
	}, nil
}

// CompleteLogin 轮询等待用户在浏览器中完成授权，返回完整登录结果
func CompleteLogin(loginID string) (*LoginResult, error) {
	client := &http.Client{Timeout: 20 * time.Second}

	var lastErr error

	for {
		pendingMu.Lock()
		state := pending
		pendingMu.Unlock()

		if state == nil {
			return nil, fmt.Errorf("没有进行中的登录会话")
		}
		if state.loginID != loginID {
			return nil, fmt.Errorf("登录会话已变更，请重新发起")
		}
		if time.Now().After(state.expiresAt) {
			clearPending()
			msg := "登录已超时，请重试"
			if lastErr != nil {
				msg = lastErr.Error()
			}
			return nil, fmt.Errorf(msg)
		}

		tokenResult, err := pollDeviceToken(client, state.nonce, state.verifier, state.challengeMethod)
		if err != nil {
			lastErr = err
			time.Sleep(pollInterval)
			continue
		}
		if tokenResult == nil {
			time.Sleep(pollInterval)
			continue
		}

		// 成功获取 token，清理 pending 状态
		clearPending()

		// 获取用户详情
		userInfo, _ := fetchJSON(client, openAPIBaseURL+userInfoPath, map[string]string{
			"Authorization": "Bearer " + tokenResult.Token,
		})

		// 获取用户状态（含 Cosy-* 头）
		statusHeaders := buildStatusHeaders(tokenResult.Token, state.machineToken, state.machineType)
		userStatus, err := fetchJSON(client, openAPIBaseURL+userStatusPath, statusHeaders)
		if err != nil {
			return nil, fmt.Errorf("获取用户状态失败: %w", err)
		}

		// 检查白名单状态
		if err := checkWhitelistStatus(userStatus); err != nil {
			return nil, err
		}

		// 获取用户计划（可选）
		userPlan, _ := fetchJSON(client, openAPIBaseURL+userPlanPath, map[string]string{
			"Authorization": "Bearer " + tokenResult.Token,
		})

		return &LoginResult{
			DeviceToken: tokenResult,
			UserInfo:    userInfo,
			UserStatus:  userStatus,
			UserPlan:    userPlan,
		}, nil
	}
}

// SaveAuthJSON 保存兼容 loadLegacyIdentity 格式的 auth JSON 文件
func SaveAuthJSON(path string, result *LoginResult) error {
	userInfoRaw := buildUserInfoRaw(result)

	wrapper := map[string]any{
		"auth_user_info_raw": userInfoRaw,
	}

	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 auth JSON 失败: %w", err)
	}

	if err := atomicWrite(path, data); err != nil {
		return fmt.Errorf("写入 auth JSON 文件失败: %w", err)
	}

	return nil
}

// OpenBrowser 自动打开浏览器
func OpenBrowser(uri string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", uri)
	case "linux":
		cmd = exec.Command("xdg-open", uri)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", uri)
	default:
		return fmt.Errorf("不支持的操作系统: %s，请手动打开链接", runtime.GOOS)
	}
	return cmd.Start()
}

// --- 内部实现 ---

type pendingState struct {
	loginID         string
	nonce           string
	verifier        string
	challengeMethod string
	machineToken    string
	machineType     string
	expiresAt       time.Time
}

var (
	pending   *pendingState
	pendingMu = &sync.Mutex{}
)

func clearPending() {
	pendingMu.Lock()
	pending = nil
	pendingMu.Unlock()
}

func buildVerificationURL(nonce, challenge, machineID string) string {
	u, _ := url.Parse(loginBaseURL)
	q := u.Query()
	q.Set("nonce", nonce)
	q.Set("challenge", challenge)
	q.Set("challenge_method", challengeMethod)
	q.Set("client_id", clientID)
	if machineID != "" {
		q.Set("machine_id", machineID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func pollDeviceToken(client *http.Client, nonce, verifier, challengeMethod string) (*DeviceTokenResult, error) {
	u, _ := url.Parse(openAPIBaseURL + pollPath)
	q := u.Query()
	q.Set("nonce", nonce)
	q.Set("verifier", verifier)
	q.Set("challenge_method", challengeMethod)
	u.RawQuery = q.Encode()

	resp, err := client.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("轮询 device token 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // 未授权，继续轮询
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("轮询 device token 失败: status=%d, body=%s", resp.StatusCode, string(body))
	}

	var result DeviceTokenResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 device token 响应失败: %w", err)
	}
	if result.Token == "" {
		return nil, nil
	}
	return &result, nil
}

func fetchJSON(client *http.Client, url string, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d, body=%s", resp.StatusCode, string(body))
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func buildStatusHeaders(token, machineToken, machineType string) map[string]string {
	headers := map[string]string{
		"Authorization": "Bearer " + token,
		"Cosy-ClientType": "0",
		"Cosy-MachineOS":  buildMachineOS(),
	}
	if machineToken != "" {
		headers["Cosy-MachineToken"] = machineToken
	}
	if machineType != "" {
		headers["Cosy-MachineType"] = machineType
	}
	return headers
}

func buildMachineOS() string {
	arch := runtime.GOARCH
	if arch == "arm64" {
		arch = "aarch64"
	}
	return arch + "_" + runtime.GOOS
}

func checkWhitelistStatus(userStatus map[string]any) error {
	id, _ := userStatus["id"].(string)
	if id == "" {
		return fmt.Errorf("用户状态缺少 id，无法确认登录身份")
	}

	status, _ := userStatus["whitelistStatus"].(string)
	switch status {
	case "NoIpPermission":
		return fmt.Errorf("企业设置了 IP 白名单，当前 IP 无法登录")
	case "AppDisable":
		return fmt.Errorf("Qoder 应用已被停用，无法登录")
	case "LoginExpire":
		return fmt.Errorf("Qoder 登录已失效，请重试")
	case "NotAllow", "NOT_ALLOW":
		return fmt.Errorf("当前账号暂无 Qoder 使用权限")
	}
	return nil
}

func buildUserInfoRaw(result *LoginResult) map[string]any {
	userInfo := make(map[string]any)

	// 从 device token 获取核心字段
	if result.DeviceToken != nil {
		dt := result.DeviceToken
		userInfo["id"] = dt.UserID
		userInfo["uid"] = dt.UserID
		userInfo["userId"] = dt.UserID
		userInfo["token"] = dt.Token
		userInfo["securityOauthToken"] = dt.Token
		userInfo["accessToken"] = dt.Token
		userInfo["refreshToken"] = dt.RefreshToken
		userInfo["expireTime"] = normalizeExpireTimestamp(dt.ExpiresAt)
		userInfo["refreshTokenExpireTime"] = normalizeExpireTimestamp(dt.RefreshTokenExpiresAt)
	}

	// 从 userinfo API 合并用户详情
	if result.UserInfo != nil {
		copyField(result.UserInfo, userInfo, "name")
		copyField(result.UserInfo, userInfo, "email")
		copyField(result.UserInfo, userInfo, "avatarUrl")
	}

	// 从 user/status API 合并用户状态
	if result.UserStatus != nil {
		copyField(result.UserStatus, userInfo, "id")
		copyField(result.UserStatus, userInfo, "name")
		copyField(result.UserStatus, userInfo, "email")
		copyField(result.UserStatus, userInfo, "userType")
		copyField(result.UserStatus, userInfo, "userTag")
		copyField(result.UserStatus, userInfo, "orgId")
		copyField(result.UserStatus, userInfo, "orgName")
		copyField(result.UserStatus, userInfo, "yxUid")
		copyField(result.UserStatus, userInfo, "staffId")
		copyField(result.UserStatus, userInfo, "cloudType")
		copyField(result.UserStatus, userInfo, "quota")
		copyField(result.UserStatus, userInfo, "isQuotaExceeded")

		// 更新 uid 以匹配 status 返回的 id
		if statusID, ok := result.UserStatus["id"].(string); ok && statusID != "" {
			userInfo["uid"] = statusID
			userInfo["userId"] = statusID
		}

		// 计算认证状态码
		status, whitelist := calculateAuthStatus(result.UserStatus)
		userInfo["status"] = status
		userInfo["whitelist"] = whitelist
	}

	// 从 user/plan 合并计划信息
	if result.UserPlan != nil {
		userInfo["userPlan"] = result.UserPlan
	}

	return userInfo
}

func calculateAuthStatus(userStatus map[string]any) (int, int) {
	const (
		authStatusAuthorized = 2
		authStatusLoginExp   = 3
		authStatusIPBanned   = 6
		authStatusAppDisable = 7
		whitelistPass        = 3
		whitelistNot         = 1
		whitelistWait        = 2
		whitelistNoLicense   = 5
		whitelistOrgExpired  = 6
		whitelistNotAllow    = 7
	)

	status, _ := userStatus["whitelistStatus"].(string)
	switch status {
	case "NoIpPermission":
		return authStatusIPBanned, whitelistNot
	case "AppDisable":
		return authStatusAppDisable, whitelistNot
	case "LoginExpire":
		return authStatusLoginExp, whitelistNot
	case "PASS":
		return authStatusAuthorized, whitelistPass
	case "WAIT":
		return authStatusAuthorized, whitelistWait
	case "NoLicense":
		return authStatusAuthorized, whitelistNoLicense
	case "NoQuota", "EXPIRED":
		return authStatusAuthorized, whitelistOrgExpired
	case "NotAllow", "NOT_ALLOW":
		return authStatusAuthorized, whitelistNotAllow
	default:
		return authStatusAuthorized, whitelistNot
	}
}

func copyField(from, to map[string]any, key string) {
	if v, ok := from[key]; ok && v != nil {
		to[key] = v
	}
}

func normalizeExpireTimestamp(raw string) string {
	if raw == "" {
		return ""
	}
	// 尝试解析为数字
	var num int64
	if _, err := fmt.Sscanf(raw, "%d", &num); err == nil {
		// 如果是秒级时间戳，转为毫秒
		if num < 1_000_000_000_000 {
			num *= 1000
		}
		return fmt.Sprintf("%d", num)
	}
	return raw
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := writeFile(tmp, data); err != nil {
		return err
	}
	return renameFile(tmp, path)
}

// --- 加密工具 ---

func generatePKCEVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func generatePKCEChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateNonce() (string, error) {
	return generateSimpleUUID()
}

func generateUUID() (string, error) {
	return generateSimpleUUID()
}

// generateSimpleUUID 生成 32 位小写 hex UUID（与 cockpit-tools Uuid::new_v4().simple() 一致）
func generateSimpleUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x%04x%04x%04x%012x",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]),
		uint16(b[6])<<8|uint16(b[7]),
		uint16(b[8])<<8|uint16(b[9]),
		uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]),
	), nil
}

// --- Qoder 官方客户端 machine token 读取 ---

func readQoderMachineID() string {
	// 尝试读取官方客户端缓存的 machine ID
	path := qoderSharedClientCachePath() + "/cache/id"
	data, err := readFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readQoderMachineToken() string {
	path := qoderSharedClientCachePath() + "/cache/machine_token.json"
	data, err := readFile(path)
	if err != nil {
		return ""
	}
	var cache struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return ""
	}
	return cache.Token
}

func readQoderMachineType() string {
	path := qoderSharedClientCachePath() + "/cache/machine_token.json"
	data, err := readFile(path)
	if err != nil {
		return ""
	}
	var cache struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return ""
	}
	return cache.Type
}

// DefaultAuthJSONPath 返回默认的 auth JSON 文件路径
func DefaultAuthJSONPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "auth.json"
	}
	switch runtime.GOOS {
	case "windows":
		return home + "\\AppData\\Local\\qoder2api\\auth.json"
	default:
		return home + "/.config/qoder2api/auth.json"
	}
}

func qoderSharedClientCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return home + "/Library/Application Support/Qoder/SharedClientCache"
	case "linux":
		return home + "/.config/Qoder/SharedClientCache"
	case "windows":
		return home + "\\AppData\\Roaming\\Qoder\\SharedClientCache"
	}
	return ""
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0600)
}

func renameFile(old, new string) error {
	return os.Rename(old, new)
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
