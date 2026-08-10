package server

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	defaultReloginOTPProvider     = "smsgate_webhook"
	defaultReloginManualLoginURL  = "https://account.apple.com/account/manage/section/privacy"
	defaultReloginOTPWebhookPath  = "/api/otp/inbound"
	defaultReloginLatestOTPPath   = "/api/otp/latest"
	defaultReloginOTPWebhookTTL   = 5 * time.Minute
	defaultReloginProtocolTimeout = 3 * time.Minute
)

// ReloginConfig 把“纯协议自动重新登录认证 + Android/SMSGate 验证码接收”配置收在一起。
// Enabled=false 时，管理页面继续弹窗引导用户手动打开 Apple 登录页。
type ReloginConfig struct {
	Enabled         bool
	Mode            string
	ManualLoginURL  string
	ProtocolTimeout time.Duration
	AppleID         string
	ApplePassword   string
	OTP             ReloginOTPConfig
}

type ReloginOTPConfig struct {
	Provider     string
	WebhookToken string
	TTL          time.Duration
	WebhookPath  string
	LatestPath   string
}

type publicReloginConfig struct {
	Enabled                 bool   `json:"enabled"`
	Mode                    string `json:"mode"`
	ManualLoginURL          string `json:"manual_login_url"`
	ProtocolTimeoutSeconds  int64  `json:"protocol_timeout_seconds"`
	AppleIDConfigured       bool   `json:"apple_id_configured"`
	ApplePasswordConfigured bool   `json:"apple_password_configured"`
	OTP                     struct {
		Provider              string `json:"provider"`
		WebhookEnabled        bool   `json:"webhook_enabled"`
		WebhookPath           string `json:"webhook_path"`
		LatestPath            string `json:"latest_path"`
		TTLSeconds            int64  `json:"ttl_seconds"`
		LegacyTokenConfigured bool   `json:"legacy_token_configured"`
	} `json:"otp"`
}

type reloginConfigUpdateReq struct {
	Enabled                *bool  `json:"enabled"`
	Mode                   string `json:"mode"`
	ManualLoginURL         string `json:"manual_login_url"`
	ProtocolTimeout        string `json:"protocol_timeout"`
	ProtocolTimeoutSeconds int64  `json:"protocol_timeout_seconds"`
	AppleID                string `json:"apple_id"`
	ApplePassword          string `json:"apple_password"`
	OTP                    struct {
		Provider     string `json:"provider"`
		WebhookToken string `json:"webhook_token"`
		TTL          string `json:"ttl"`
		TTLSeconds   int64  `json:"ttl_seconds"`
	} `json:"otp"`
}

// ReloginConfigFromEnv 从环境变量读取统一配置。
//
// 新变量:
//   - ICLOUD_HME_RELOGIN_ENABLED
//   - ICLOUD_HME_RELOGIN_MODE
//   - ICLOUD_HME_RELOGIN_MANUAL_URL
//   - ICLOUD_HME_RELOGIN_PROTOCOL_TIMEOUT
//   - ICLOUD_HME_RELOGIN_APPLE_ID
//   - ICLOUD_HME_RELOGIN_APPLE_PASSWORD
//   - ICLOUD_HME_RELOGIN_OTP_PROVIDER
//   - ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN
//   - ICLOUD_HME_RELOGIN_OTP_TTL
//
// 兼容旧变量:
//   - ICLOUD_HME_OTP_WEBHOOK_TOKEN
//   - ICLOUD_HME_OTP_TTL
func ReloginConfigFromEnv() ReloginConfig {
	cfg := ReloginConfig{
		Enabled:         envBool("ICLOUD_HME_RELOGIN_ENABLED", false),
		Mode:            envString("ICLOUD_HME_RELOGIN_MODE", "apple_protocol_sms"),
		ManualLoginURL:  envString("ICLOUD_HME_RELOGIN_MANUAL_URL", defaultReloginManualLoginURL),
		ProtocolTimeout: envDuration("ICLOUD_HME_RELOGIN_PROTOCOL_TIMEOUT", defaultReloginProtocolTimeout),
		AppleID:         strings.TrimSpace(os.Getenv("ICLOUD_HME_RELOGIN_APPLE_ID")),
		ApplePassword:   strings.TrimSpace(os.Getenv("ICLOUD_HME_RELOGIN_APPLE_PASSWORD")),
		OTP: ReloginOTPConfig{
			Provider:     envString("ICLOUD_HME_RELOGIN_OTP_PROVIDER", defaultReloginOTPProvider),
			WebhookToken: firstEnv("ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN", "ICLOUD_HME_OTP_WEBHOOK_TOKEN"),
			TTL:          envDurationFallback("ICLOUD_HME_RELOGIN_OTP_TTL", "ICLOUD_HME_OTP_TTL", defaultReloginOTPWebhookTTL),
			WebhookPath:  envString("ICLOUD_HME_RELOGIN_OTP_WEBHOOK_PATH", defaultReloginOTPWebhookPath),
			LatestPath:   envString("ICLOUD_HME_RELOGIN_OTP_LATEST_PATH", defaultReloginLatestOTPPath),
		},
	}
	if cfg.ManualLoginURL == "" {
		cfg.ManualLoginURL = defaultReloginManualLoginURL
	}
	if cfg.OTP.Provider == "" {
		cfg.OTP.Provider = defaultReloginOTPProvider
	}
	if cfg.OTP.TTL <= 0 {
		cfg.OTP.TTL = defaultReloginOTPWebhookTTL
	}
	return cfg
}

func (s *Server) reloginConfig(c *gin.Context) {
	ok(c, s.relogin.Public())
}

func (s *Server) updateReloginConfig(c *gin.Context) {
	var req reloginConfigUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: 请求体必须是 JSON — "+err.Error())
		return
	}

	updates := map[string]string{}
	if req.Enabled != nil {
		updates["ICLOUD_HME_RELOGIN_ENABLED"] = strconv.FormatBool(*req.Enabled)
	}
	if mode := strings.TrimSpace(req.Mode); mode != "" {
		updates["ICLOUD_HME_RELOGIN_MODE"] = mode
	}
	if manualURL := strings.TrimSpace(req.ManualLoginURL); manualURL != "" {
		updates["ICLOUD_HME_RELOGIN_MANUAL_URL"] = manualURL
	}
	if timeout := durationStringFromUpdate(req.ProtocolTimeout, req.ProtocolTimeoutSeconds); timeout != "" {
		updates["ICLOUD_HME_RELOGIN_PROTOCOL_TIMEOUT"] = timeout
	}
	if appleID := strings.TrimSpace(req.AppleID); appleID != "" {
		updates["ICLOUD_HME_RELOGIN_APPLE_ID"] = appleID
	}
	if req.ApplePassword != "" {
		updates["ICLOUD_HME_RELOGIN_APPLE_PASSWORD"] = strings.TrimSpace(req.ApplePassword)
	}
	if provider := strings.TrimSpace(req.OTP.Provider); provider != "" {
		updates["ICLOUD_HME_RELOGIN_OTP_PROVIDER"] = provider
	}
	if req.OTP.WebhookToken != "" {
		updates["ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN"] = strings.TrimSpace(req.OTP.WebhookToken)
	}
	if ttl := durationStringFromUpdate(req.OTP.TTL, req.OTP.TTLSeconds); ttl != "" {
		updates["ICLOUD_HME_RELOGIN_OTP_TTL"] = ttl
	}

	if len(updates) == 0 {
		ok(c, s.relogin.Public())
		return
	}
	if err := updateDotEnvFile(relLoginEnvFile(), updates); err != nil {
		fail(c, http.StatusInternalServerError, "保存 .env 失败: "+err.Error())
		return
	}
	for key, value := range updates {
		_ = os.Setenv(key, value)
	}
	s.relogin = ReloginConfigFromEnv()
	ok(c, s.relogin.Public())
}

func (cfg ReloginConfig) Public() publicReloginConfig {
	out := publicReloginConfig{
		Enabled:                 cfg.Enabled,
		Mode:                    cfg.Mode,
		ManualLoginURL:          cfg.ManualLoginURL,
		ProtocolTimeoutSeconds:  int64(cfg.ProtocolTimeout / time.Second),
		AppleIDConfigured:       cfg.AppleID != "",
		ApplePasswordConfigured: cfg.ApplePassword != "",
	}
	out.OTP.Provider = cfg.OTP.Provider
	out.OTP.WebhookEnabled = cfg.OTP.WebhookToken != ""
	out.OTP.WebhookPath = cfg.OTP.WebhookPath
	out.OTP.LatestPath = cfg.OTP.LatestPath
	out.OTP.TTLSeconds = int64(cfg.OTP.TTL / time.Second)
	out.OTP.LegacyTokenConfigured = strings.TrimSpace(os.Getenv("ICLOUD_HME_OTP_WEBHOOK_TOKEN")) != ""
	return out
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envDurationFallback(primary, legacy string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(os.Getenv(primary)) != "" {
		return envDuration(primary, fallback)
	}
	return envDuration(legacy, fallback)
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func durationStringFromUpdate(raw string, seconds int64) string {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			return parsed.String()
		}
		return raw
	}
	if seconds > 0 {
		return (time.Duration(seconds) * time.Second).String()
	}
	return ""
}

func relLoginEnvFile() string {
	if configured := strings.TrimSpace(os.Getenv("ICLOUD_HME_ENV_FILE")); configured != "" {
		return configured
	}
	return ".env"
}

func updateDotEnvFile(path string, updates map[string]string) error {
	if path == "" {
		path = ".env"
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	var lines []string
	file, err := os.Open(path)
	if err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		closeErr := file.Close()
		if scannerErr := scanner.Err(); scannerErr != nil {
			return scannerErr
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	seen := make(map[string]bool, len(updates))
	for i, line := range lines {
		key, ok := dotenvLineKey(line)
		if !ok {
			continue
		}
		value, exists := updates[key]
		if !exists {
			continue
		}
		lines[i] = formatDotenvLine(key, value)
		seen[key] = true
	}

	if len(updates) > 0 && len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	for _, key := range sortedUpdateKeys(updates) {
		if seen[key] {
			continue
		}
		lines = append(lines, formatDotenvLine(key, updates[key]))
	}
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0600)
}

func dotenvLineKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	sep := strings.Index(trimmed, "=")
	if sep <= 0 {
		return "", false
	}
	key := strings.TrimSpace(trimmed[:sep])
	if key == "" {
		return "", false
	}
	return key, true
}

func sortedUpdateKeys(values map[string]string) []string {
	preferred := []string{
		"ICLOUD_HME_RELOGIN_ENABLED",
		"ICLOUD_HME_RELOGIN_MODE",
		"ICLOUD_HME_RELOGIN_APPLE_ID",
		"ICLOUD_HME_RELOGIN_APPLE_PASSWORD",
		"ICLOUD_HME_RELOGIN_OTP_PROVIDER",
		"ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN",
		"ICLOUD_HME_RELOGIN_OTP_TTL",
		"ICLOUD_HME_RELOGIN_MANUAL_URL",
		"ICLOUD_HME_RELOGIN_PROTOCOL_TIMEOUT",
	}
	out := make([]string, 0, len(values))
	used := make(map[string]bool, len(values))
	for _, key := range preferred {
		if _, ok := values[key]; ok {
			out = append(out, key)
			used[key] = true
		}
	}
	for key := range values {
		if !used[key] {
			out = append(out, key)
		}
	}
	return out
}

func formatDotenvLine(key, value string) string {
	return fmt.Sprintf("%s=%s", key, quoteDotenvValue(value))
}

func quoteDotenvValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\r\n#'\"") {
		return strconv.Quote(value)
	}
	return value
}
