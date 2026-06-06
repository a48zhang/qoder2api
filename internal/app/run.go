package app

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"qoder2api/internal/bridge"
	"qoder2api/internal/qoder"
)

func Run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	transport, err := newHTTPTransport(cfg)
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.RequestTimeoutSec) * time.Second,
	}

	backend, err := qoder.NewBackend(qoder.Options{
		PAT:           cfg.QoderPAT,
		AuthJSON:      cfg.QoderAuthJSON,
		BaseURL:       cfg.QoderBaseURL,
		WorkspaceRoot: cfg.QoderWorkspaceRoot,
		DefaultModel:  cfg.QoderModel,
	}, client)
	if err != nil {
		return err
	}

	handler := bridge.NewHandler(backend, cfg.QoderModel)
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	fmt.Printf("[qoder2api] listening on http://%s/v1/chat/completions\n", addr)
	return http.ListenAndServe(addr, handler)
}

func newHTTPTransport(cfg Config) (*http.Transport, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
	if !cfg.InsecureSkipVerify {
		pool, err := LoadCertPool(cfg.ExtraCAFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.RootCAs = pool
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	if cfg.ProxyURL != "" {
		proxyURL, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return transport, nil
}
