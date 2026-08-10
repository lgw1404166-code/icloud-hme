package server

import (
	"log"
	"strings"
	"time"
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

// StartAliasPoolWorker 启动后台别名池。每个 tick 对每个账号尝试补 1 个别名。
func (s *Server) StartAliasPoolWorker(cfg AliasPoolConfig) {
	cfg = cfg.Normalize()
	if !cfg.Enabled {
		log.Printf("后台别名池已关闭")
		return
	}
	log.Printf("后台别名池已启用 per_hour=%d interval=%s max_total=%d", cfg.PerHour, cfg.Interval, cfg.MaxTotal)
	go func() {
		initialDelay := cfg.Interval
		if cfg.StartNow {
			initialDelay = 0
		}
		timer := time.NewTimer(initialDelay)
		defer timer.Stop()
		for {
			<-timer.C
			s.runAliasPoolTick(cfg)
			timer.Reset(cfg.Interval)
		}
	}()
}

func (s *Server) runAliasPoolTick(cfg AliasPoolConfig) {
	for _, acc := range s.mgr.ListAccounts() {
		if acc == nil || acc.RequiresLogin {
			continue
		}
		if cfg.MaxTotal > 0 && acc.AliasTotal >= cfg.MaxTotal {
			continue
		}
		if retryAfter, allowed := s.beginCreate(acc.ID); !allowed {
			log.Printf("后台别名池跳过 account_id=%s retry_after=%s", acc.ID, retryAfter)
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
		} else {
			log.Printf("后台别名池创建成功 account_id=%s email=%s protocol=%s", acc.ID, result.Email, result.Protocol)
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
