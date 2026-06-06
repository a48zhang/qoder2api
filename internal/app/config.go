package app

import (
	"crypto/x509"
	"errors"
	"os"
	"strconv"
	"strings"

	"qoder2api/internal/auth"
)

type Config struct {
	Host               string
	Port               int
	QoderPAT           string
	QoderAuthJSON      string
	QoderBaseURL       string
	QoderWorkspaceRoot string
	QoderModel         string
	RequestTimeoutSec  int
	ProxyURL           string
	ExtraCAFile        string
	InsecureSkipVerify bool
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Host:               getenvDefault("QODER_HOST", "127.0.0.1"),
		Port:               getenvInt("QODER_PORT", 8963),
		QoderPAT:           strings.TrimSpace(os.Getenv("QODER_PAT")),
		QoderAuthJSON:      strings.TrimSpace(os.Getenv("QODER_AUTH_JSON")),
		QoderBaseURL:       getenvDefault("QODER_BASE_URL", "https://api.qoder.com"),
		QoderWorkspaceRoot: strings.TrimSpace(os.Getenv("QODER_WORKSPACE_ROOT")),
		QoderModel:         getenvDefault("QODER_MODEL", "auto"),
		RequestTimeoutSec:  getenvInt("QODER_TIMEOUT_SEC", 300),
		ProxyURL:           firstNonBlankEnv("QODER_PROXY_URL", "HTTPS_PROXY", "HTTP_PROXY"),
		ExtraCAFile:        strings.TrimSpace(os.Getenv("QODER_CA_FILE")),
		InsecureSkipVerify: getenvBool("QODER_INSECURE_SKIP_VERIFY", false),
	}

	if cfg.QoderPAT == "" && cfg.QoderAuthJSON == "" {
		// 尝试使用 login 命令保存的默认路径
		defaultPath := auth.DefaultAuthJSONPath()
		if _, err := os.Stat(defaultPath); err == nil {
			cfg.QoderAuthJSON = defaultPath
		} else {
			return Config{}, errors.New("set QODER_PAT or QODER_AUTH_JSON (or run: go run ./cmd/qoder2api-login)")
		}
	}
	if cfg.ExtraCAFile != "" {
		if _, err := os.Stat(cfg.ExtraCAFile); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
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

func getenvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return fallback
	default:
		return fallback
	}
}

func firstNonBlankEnv(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func LoadCertPool(extraCAFile string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if strings.TrimSpace(extraCAFile) == "" {
		return pool, nil
	}
	pemData, err := os.ReadFile(extraCAFile)
	if err != nil {
		return nil, err
	}
	if ok := pool.AppendCertsFromPEM(pemData); !ok {
		return nil, errors.New("failed to append certificates from QODER_CA_FILE")
	}
	return pool, nil
}
