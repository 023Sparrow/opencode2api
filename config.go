package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Listen      string            `json:"listen"`
	ServerKeys  []string          `json:"server_keys"`
	ZenKeys     []string          `json:"zen_keys"`
	GoKeys      []string          `json:"go_keys"`
	Proxies     []string          `json:"proxies"`
	ProxyFile   string            `json:"proxyfile"`
	Upstream    UpstreamConfig    `json:"upstream"`
	Retry       RetryConfig       `json:"retry"`
	Models      ModelsConfig      `json:"models"`
	Performance PerformanceConfig `json:"performance"`
	Logging     LoggingConfig     `json:"logging"`
	Prefer      Tier              `json:"prefer"`
}

type UpstreamConfig struct {
	Zen string `json:"zen"`
	Go  string `json:"go"`
}

type RetryConfig struct {
	MaxAttempts    int `json:"max_attempts"`
	TimeoutSeconds int `json:"timeout_seconds"`
}

type ModelsConfig struct {
	RefreshSeconds int               `json:"refresh_seconds"`
	Protocols      map[string]string `json:"protocols"`
}

type LoggingConfig struct {
	Level string `json:"level"`
}

type PerformanceConfig struct {
	MaxIdleConns           int `json:"max_idle_conns"`
	MaxIdleConnsPerHost    int `json:"max_idle_conns_per_host"`
	MaxConnsPerHost        int `json:"max_conns_per_host"`
	IdleConnTimeoutSeconds int `json:"idle_conn_timeout_seconds"`
	ConnectTimeoutSeconds  int `json:"connect_timeout_seconds"`
	FailureCooldownSeconds int `json:"failure_cooldown_seconds"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	data, err = stripJSONComments(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg := Config{
		Listen:      "127.0.0.1:8080",
		Upstream:    UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go"},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 2048, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 120, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15},
		Logging:     LoggingConfig{Level: "info"},
		Prefer:      TierGo,
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	trimList(&cfg.ServerKeys)
	trimList(&cfg.ZenKeys)
	trimList(&cfg.GoKeys)
	cfg.ProxyFile = strings.TrimSpace(cfg.ProxyFile)
	if err := loadProxyFiles(path, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Prefer != TierZen && cfg.Prefer != TierGo {
		return Config{}, errors.New("prefer must be \"zen\" or \"go\"")
	}
	if cfg.Listen == "" {
		return Config{}, errors.New("listen must not be empty")
	}
	if len(cfg.ServerKeys) == 0 {
		return Config{}, errors.New("server_keys must contain at least one local key")
	}
	if len(cfg.ZenKeys) == 0 && len(cfg.GoKeys) == 0 {
		return Config{}, errors.New("zen_keys or go_keys must contain at least one upstream key")
	}
	if len(cfg.Proxies) == 0 {
		cfg.Proxies = []string{"direct"}
	}
	if cfg.Retry.MaxAttempts < 1 {
		return Config{}, errors.New("retry.max_attempts must be at least 1")
	}
	if cfg.Retry.TimeoutSeconds < 1 {
		return Config{}, errors.New("retry.timeout_seconds must be at least 1")
	}
	if cfg.Models.RefreshSeconds < 1 {
		return Config{}, errors.New("models.refresh_seconds must be at least 1")
	}
	if cfg.Performance.MaxIdleConns < 1 || cfg.Performance.MaxIdleConnsPerHost < 1 || cfg.Performance.MaxConnsPerHost < 0 || cfg.Performance.IdleConnTimeoutSeconds < 1 || cfg.Performance.ConnectTimeoutSeconds < 1 || cfg.Performance.FailureCooldownSeconds < 1 {
		return Config{}, errors.New("performance values must be positive (max_conns_per_host may be zero for unlimited)")
	}
	for _, raw := range cfg.Proxies {
		if raw == "direct" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return Config{}, fmt.Errorf("invalid proxy URL %q", redactURL(raw))
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return Config{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
	for model, protocol := range cfg.Models.Protocols {
		if model == "" || !validProtocol(Protocol(protocol)) {
			return Config{}, fmt.Errorf("models.protocols contains invalid mapping %q: %q", model, protocol)
		}
	}
	return cfg, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// stripJSONComments removes // and /* */ comments without changing newlines,
// so syntax errors still point at the correct line in config.json. Comment
// markers inside JSON strings (for example, https:// URLs) are preserved.
func stripJSONComments(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	lineComment := false
	blockComment := false

	for i := 0; i < len(data); i++ {
		current := data[i]
		if lineComment {
			if current == '\n' || current == '\r' {
				lineComment = false
				out = append(out, current)
			} else {
				out = append(out, ' ')
			}
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(data) && data[i+1] == '/' {
				out = append(out, ' ', ' ')
				i++
				blockComment = false
			} else if current == '\n' || current == '\r' {
				out = append(out, current)
			} else {
				out = append(out, ' ')
			}
			continue
		}
		if inString {
			out = append(out, current)
			if escaped {
				escaped = false
			} else if current == '\\' {
				escaped = true
			} else if current == '"' {
				inString = false
			}
			continue
		}

		switch {
		case current == '"':
			inString = true
			out = append(out, current)
		case current == '/' && i+1 < len(data) && data[i+1] == '/':
			lineComment = true
			out = append(out, ' ', ' ')
			i++
		case current == '/' && i+1 < len(data) && data[i+1] == '*':
			blockComment = true
			out = append(out, ' ', ' ')
			i++
		default:
			out = append(out, current)
		}
	}
	if blockComment {
		return nil, errors.New("unterminated block comment")
	}
	return out, nil
}

func loadProxyFiles(configPath string, cfg *Config) error {
	trimList(&cfg.Proxies)
	if cfg.ProxyFile != "" {
		resolved := cfg.ProxyFile
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(configPath), resolved)
		}
		proxies, err := readProxyFile(resolved)
		if err != nil {
			return fmt.Errorf("load proxy file %s: %w", resolved, err)
		}
		cfg.Proxies = append(cfg.Proxies, proxies...)
	}

	cfg.Proxies = uniqueStrings(cfg.Proxies)
	if len(cfg.Proxies) == 0 {
		cfg.Proxies = []string{"direct"}
	}
	return nil
}

func readProxyFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var proxies []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		value := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		value = strings.TrimSpace(stripProxyLineComment(value))
		if value != "" {
			proxies = append(proxies, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return proxies, nil
}

func stripProxyLineComment(line string) string {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
			continue
		}
		if line[i] == '#' || line[i] == ';' || (line[i] == '/' && i+1 < len(line) && line[i+1] == '/') {
			return line[:i]
		}
	}
	return line
}

func uniqueStrings(items []string) []string {
	out := items[:0]
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func trimList(items *[]string) {
	out := (*items)[:0]
	for _, item := range *items {
		if value := strings.TrimSpace(item); value != "" {
			out = append(out, value)
		}
	}
	*items = out
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}
