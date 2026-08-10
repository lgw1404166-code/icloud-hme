// Command icloud-hme 启动 iCloud Hide My Email 多账号管理平台。
//
// 两个核心 HTTP 接口:
//
//	POST /api/create  — 从本地别名池领取隐私邮箱别名
//	GET  /api/inbox   — 读取邮件
//
// 用法:
//
//	./icloud-hme                    # 默认 :8081
//	./icloud-hme -addr :9000        # 指定端口
//	./icloud-hme -data ./data       # 指定数据目录
//	./icloud-hme -debug             # 调试模式
//	./icloud-hme -log-level debug   # 日志级别 (debug/info/warn/error)
package main

import (
	"bufio"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/server"
)

const defaultAccountSessionRefreshInterval = 10 * time.Minute

func main() {
	if err := loadDotEnv(".env"); err != nil && !os.IsNotExist(err) {
		log.Printf("警告: 读取 .env 失败: %v", err)
	}

	addr := flag.String("addr", ":8081", "HTTP 监听地址")
	dataDir := flag.String("data", "./data", "数据目录 (accounts.json 存放位置)")
	debug := flag.Bool("debug", false, "调试模式 (启用 Gin 调试日志)")
	flag.Parse()

	log.Printf("iCloud Hide My Email 服务启动 addr=%s", *addr)

	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("数据目录路径错误: %v", err)
	}

	mgr, err := account.NewManager(abs)
	if err != nil {
		log.Fatalf("初始化账号管理器失败: %v", err)
	}
	count := len(mgr.ListAccounts())
	log.Printf("账号加载完成 count=%d data_dir=%s", count, abs)

	apiKey := strings.TrimSpace(os.Getenv("ICLOUD_HME_API_KEY"))
	if apiKey == "" {
		log.Printf("警告: ICLOUD_HME_API_KEY 未配置，API Key 鉴权不可用")
	}
	srv := server.New(mgr, *debug, apiKey)
	startAccountSessionKeepAlive(mgr, accountSessionRefreshInterval())
	srv.StartAliasPoolWorker(aliasPoolConfigFromEnv())

	log.Printf("HTTP 服务就绪 addr=%s", *addr)
	if err := srv.Run(*addr); err != nil {
		log.Fatalf("服务启动失败: %v", err)
	}
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		sep := strings.Index(line, "=")
		if sep <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:sep])
		value := strings.TrimSpace(line[sep+1:])
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		value = decodeDotEnvValue(value)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func decodeDotEnvValue(value string) string {
	if len(value) >= 2 {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	return strings.Trim(value, `"'`)
}

func aliasPoolConfigFromEnv() server.AliasPoolConfig {
	enabledRaw := strings.TrimSpace(os.Getenv("ICLOUD_HME_AUTO_CREATE"))
	enabled := true
	if enabledRaw != "" {
		enabled = !(enabledRaw == "0" || strings.EqualFold(enabledRaw, "off") || strings.EqualFold(enabledRaw, "false"))
	}
	cfg := server.AliasPoolConfig{
		Enabled:  enabled,
		PerHour:  envInt("ICLOUD_HME_AUTO_CREATE_PER_HOUR", 10),
		MaxTotal: envInt("ICLOUD_HME_AUTO_CREATE_MAX_TOTAL", 0),
		Label:    strings.TrimSpace(os.Getenv("ICLOUD_HME_AUTO_CREATE_LABEL")),
		Note:     strings.TrimSpace(os.Getenv("ICLOUD_HME_AUTO_CREATE_NOTE")),
		StartNow: true,
	}
	if raw := strings.TrimSpace(os.Getenv("ICLOUD_HME_AUTO_CREATE_INTERVAL")); raw != "" {
		if interval, err := time.ParseDuration(raw); err == nil && interval > 0 {
			cfg.Interval = interval
		} else {
			log.Printf("警告: ICLOUD_HME_AUTO_CREATE_INTERVAL=%q 无效，使用按小时速率计算的间隔", raw)
		}
	}
	return cfg
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("警告: %s=%q 无效，使用默认值 %d", name, raw, fallback)
		return fallback
	}
	return value
}

func accountSessionRefreshInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("ICLOUD_HME_ACCOUNT_REFRESH_INTERVAL"))
	if raw == "" {
		return defaultAccountSessionRefreshInterval
	}
	if raw == "0" || strings.EqualFold(raw, "off") {
		return 0
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval <= 0 {
		log.Printf("警告: ICLOUD_HME_ACCOUNT_REFRESH_INTERVAL=%q 无效，使用默认值 %s", raw, defaultAccountSessionRefreshInterval)
		return defaultAccountSessionRefreshInterval
	}
	return interval
}

func startAccountSessionKeepAlive(mgr *account.Manager, interval time.Duration) {
	if interval <= 0 {
		log.Printf("Apple 账户会话保活已关闭")
		return
	}
	log.Printf("Apple 账户会话保活已启用 interval=%s", interval)
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			<-timer.C
			for _, result := range mgr.RefreshAccountSessions() {
				if result.Err != nil {
					log.Printf("Apple 账户会话保活失败 account_id=%s err=%v", result.AccountID, result.Err)
				} else {
					log.Printf("Apple 账户会话保活成功 account_id=%s", result.AccountID)
				}
			}
			timer.Reset(interval)
		}
	}()
}
