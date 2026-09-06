package server

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	defaultAliasPoolPerHour = 10
	defaultAliasPoolNote    = "Created by icloud-hme alias pool"
)

// AliasPoolConfig 控制后台别名池创建节奏。
type AliasPoolConfig struct {
	Enabled  bool
	PerHour  int
	Interval time.Duration
	MaxTotal int
	Label    string
	Note     string
	StartNow bool
}

type publicAliasPoolConfig struct {
	Enabled         bool   `json:"enabled"`
	PerHour         int    `json:"per_hour"`
	IntervalSeconds int64  `json:"interval_seconds"`
	MaxTotal        int    `json:"max_total"`
	Label           string `json:"label"`
	Note            string `json:"note"`
}

type aliasPoolLogEntry struct {
	Time         string `json:"time"`
	Level        string `json:"level"`
	AccountEmail string `json:"account_email,omitempty"`
	Email        string `json:"email,omitempty"`
	Message      string `json:"message"`
}

type aliasPoolConfigUpdateReq struct {
	Enabled  *bool  `json:"enabled"`
	PerHour  *int   `json:"per_hour"`
	Interval string `json:"interval"`
	MaxTotal *int   `json:"max_total"`
	Label    string `json:"label"`
	Note     string `json:"note"`
}

// Normalize 补齐默认值。
func (cfg AliasPoolConfig) Normalize() AliasPoolConfig {
	if cfg.PerHour <= 0 {
		cfg.PerHour = defaultAliasPoolPerHour
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour / time.Duration(cfg.PerHour)
	}
	if cfg.Interval < time.Minute {
		cfg.Interval = time.Minute
	}
	if strings.TrimSpace(cfg.Note) == "" {
		cfg.Note = defaultAliasPoolNote
	}
	return cfg
}

// AliasPoolConfigFromEnv 从环境变量读取后台别名池配置。
func AliasPoolConfigFromEnv() AliasPoolConfig {
	enabled := true
	if raw := strings.TrimSpace(strings.ToLower(os.Getenv("ICLOUD_HME_AUTO_CREATE"))); raw != "" {
		enabled = raw != "0" && raw != "off" && raw != "false" && raw != "no"
	}
	cfg := AliasPoolConfig{
		Enabled:  enabled,
		PerHour:  poolEnvInt("ICLOUD_HME_AUTO_CREATE_PER_HOUR", defaultAliasPoolPerHour),
		MaxTotal: poolEnvInt("ICLOUD_HME_AUTO_CREATE_MAX_TOTAL", 0),
		Label:    strings.TrimSpace(os.Getenv("ICLOUD_HME_AUTO_CREATE_LABEL")),
		Note:     strings.TrimSpace(os.Getenv("ICLOUD_HME_AUTO_CREATE_NOTE")),
		StartNow: true,
	}
	if raw := strings.TrimSpace(os.Getenv("ICLOUD_HME_AUTO_CREATE_INTERVAL")); raw != "" {
		if interval, err := time.ParseDuration(raw); err == nil && interval > 0 {
			cfg.Interval = interval
		}
	}
	return cfg.Normalize()
}

func poolEnvInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func (cfg AliasPoolConfig) Public() publicAliasPoolConfig {
	cfg = cfg.Normalize()
	return publicAliasPoolConfig{
		Enabled:         cfg.Enabled,
		PerHour:         cfg.PerHour,
		IntervalSeconds: int64(cfg.Interval / time.Second),
		MaxTotal:        cfg.MaxTotal,
		Label:           cfg.Label,
		Note:            cfg.Note,
	}
}

func (s *Server) aliasPoolConfig(c *gin.Context) {
	s.poolMu.Lock()
	cfg := s.poolConfig
	s.poolMu.Unlock()
	if cfg.PerHour <= 0 {
		cfg = AliasPoolConfigFromEnv()
	}
	ok(c, cfg.Public())
}

func (s *Server) aliasPoolLogs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	s.poolLogMu.Lock()
	count := len(s.poolLogs)
	if limit > count {
		limit = count
	}
	logs := make([]aliasPoolLogEntry, 0, limit)
	for index := count - 1; index >= count-limit; index-- {
		logs = append(logs, s.poolLogs[index])
	}
	s.poolLogMu.Unlock()
	ok(c, gin.H{"logs": logs})
}

func (s *Server) recordAliasPoolLog(level, accountID, email, message string) {
	accountEmail := ""
	if accountID != "" {
		if acc, exists := s.mgr.GetAccount(accountID); exists {
			accountEmail = firstNonEmptyString(acc.RealEmail, acc.ID)
		} else {
			accountEmail = accountID
		}
	}
	entry := aliasPoolLogEntry{
		Time:         time.Now().Format(time.RFC3339),
		Level:        level,
		AccountEmail: accountEmail,
		Email:        email,
		Message:      message,
	}
	s.poolLogMu.Lock()
	s.poolLogs = append(s.poolLogs, entry)
	if len(s.poolLogs) > 100 {
		s.poolLogs = append([]aliasPoolLogEntry(nil), s.poolLogs[len(s.poolLogs)-100:]...)
	}
	s.poolLogMu.Unlock()
}

func (s *Server) updateAliasPoolConfig(c *gin.Context) {
	var req aliasPoolConfigUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: 请求体必须是 JSON — "+err.Error())
		return
	}
	current := AliasPoolConfigFromEnv()
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.PerHour != nil {
		if *req.PerHour < 1 || *req.PerHour > 60 {
			fail(c, http.StatusBadRequest, "每小时创建频率必须在 1 到 60 之间")
			return
		}
		current.PerHour = *req.PerHour
	}
	if req.MaxTotal != nil {
		if *req.MaxTotal < 0 {
			fail(c, http.StatusBadRequest, "总量上限不能小于 0")
			return
		}
		current.MaxTotal = *req.MaxTotal
	}
	current.Label = strings.TrimSpace(req.Label)
	current.Note = strings.TrimSpace(req.Note)
	if raw := strings.TrimSpace(req.Interval); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval <= 0 {
			fail(c, http.StatusBadRequest, "间隔格式无效，请使用 6m、1h 等格式")
			return
		}
		current.Interval = interval
	} else {
		current.Interval = 0
	}
	current = current.Normalize()

	updates := map[string]string{
		"ICLOUD_HME_AUTO_CREATE":           strconv.FormatBool(current.Enabled),
		"ICLOUD_HME_AUTO_CREATE_PER_HOUR":  strconv.Itoa(current.PerHour),
		"ICLOUD_HME_AUTO_CREATE_INTERVAL":  strings.TrimSpace(req.Interval),
		"ICLOUD_HME_AUTO_CREATE_MAX_TOTAL": strconv.Itoa(current.MaxTotal),
		"ICLOUD_HME_AUTO_CREATE_LABEL":     current.Label,
		"ICLOUD_HME_AUTO_CREATE_NOTE":      current.Note,
	}
	if err := updateDotEnvFile(relLoginEnvFile(), updates); err != nil {
		fail(c, http.StatusInternalServerError, "保存 .env 失败: "+err.Error())
		return
	}
	for key, value := range updates {
		_ = os.Setenv(key, value)
	}
	current.StartNow = false
	s.StartAliasPoolWorker(current)
	ok(c, current.Public())
}

// StartAliasPoolWorker 启动后台别名池。每个 tick 对每个账号尝试补 1 个别名。
func (s *Server) StartAliasPoolWorker(cfg AliasPoolConfig) {
	cfg = cfg.Normalize()
	s.poolMu.Lock()
	if s.poolStop != nil {
		close(s.poolStop)
	}
	stop := make(chan struct{})
	s.poolStop = stop
	s.poolConfig = cfg
	s.poolMu.Unlock()
	if !cfg.Enabled {
		log.Printf("后台别名池已关闭")
		s.recordAliasPoolLog("info", "", "", "自动创建已关闭")
		return
	}
	log.Printf("后台别名池已启用 per_hour=%d interval=%s max_total=%d", cfg.PerHour, cfg.Interval, cfg.MaxTotal)
	s.recordAliasPoolLog("info", "", "", "自动创建已启动，频率为每小时 "+strconv.Itoa(cfg.PerHour)+" 个")
	go func() {
		initialDelay := cfg.Interval
		if cfg.StartNow {
			initialDelay = 0
		}
		timer := time.NewTimer(initialDelay)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
			}
			s.runAliasPoolTick(cfg)
			timer.Reset(cfg.Interval)
		}
	}()
}

func (s *Server) runAliasPoolTick(cfg AliasPoolConfig) {
	for _, acc := range s.mgr.ListAccounts() {
		if acc == nil || !acc.Enabled || acc.RequiresLogin {
			if acc != nil && !acc.Enabled {
				s.recordAliasPoolLog("info", acc.ID, "", "账号已停用，本轮已跳过")
				continue
			}
			if acc != nil && acc.RequiresLogin {
				s.recordAliasPoolLog("warning", acc.ID, "", "账号需要重新登录，本轮已跳过")
			}
			continue
		}
		if !acc.AutoCreateEnabled {
			s.recordAliasPoolLog("info", acc.ID, "", "该账号已关闭自动创建，本轮已跳过")
			continue
		}
		if cfg.MaxTotal > 0 && acc.AliasTotal >= cfg.MaxTotal {
			s.recordAliasPoolLog("info", acc.ID, "", "已达到自动创建总量上限，本轮已跳过")
			continue
		}
		if retryAfter, allowed := s.beginCreate(acc.ID); !allowed {
			log.Printf("后台别名池跳过 account_id=%s retry_after=%s", acc.ID, retryAfter)
			s.recordAliasPoolLog("warning", acc.ID, "", "处于限流冷却，本轮已跳过，剩余 "+retryAfter.Round(time.Second).String())
			continue
		}

		cooldown := time.Duration(0)
		result, err := s.createAliasForPool(acc.ID, cfg)
		if err != nil {
			code, message, httpErr, failureCooldown := s.classifyCreateError(err)
			cooldown = failureCooldown
			if code == 401 {
				s.mgr.MarkLoginRequired(acc.ID, err)
			}
			if httpErr != nil {
				log.Printf("后台别名池创建失败 account_id=%s status=%d cooldown=%s body=%q msg=%s", acc.ID, httpErr.StatusCode, cooldown, httpErr.Body, message)
			} else {
				log.Printf("后台别名池创建失败 account_id=%s cooldown=%s err=%v", acc.ID, cooldown, err)
			}
			s.recordAliasPoolLog("error", acc.ID, "", message)
		} else {
			log.Printf("后台别名池创建成功 account_id=%s email=%s protocol=%s", acc.ID, result.Email, result.Protocol)
			s.recordAliasPoolLog("success", acc.ID, result.Email, "别名创建成功")
		}
		s.finishCreate(acc.ID, cooldown)
	}
}

func (s *Server) createAliasForPool(accountID string, cfg AliasPoolConfig) (*poolCreateResult, error) {
	client, err := s.mgr.HMEClient(accountID, false)
	if err != nil {
		return nil, err
	}
	label := strings.TrimSpace(cfg.Label)
	if label == "" {
		label = "icloud-hme pool " + time.Now().Format("2006-01-02 15:04")
	}
	result, err := client.CreateAliasWithNote(label, cfg.Note, 1)
	_ = s.mgr.SaveCookies(accountID, client.Cookies)
	if err != nil {
		return nil, err
	}
	if err := s.mgr.RegisterCreatedAlias(accountID, result, client.Cookies); err != nil {
		return nil, err
	}
	return &poolCreateResult{Email: result.Email, Protocol: result.Protocol}, nil
}

type poolCreateResult struct {
	Email    string
	Protocol string
}
