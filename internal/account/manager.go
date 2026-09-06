// Package account 实现多账号管理器。
//
// 负责账号 CRUD、Cookie 解析(Header String / JSON)、持久化到 accounts.json,
// 以及创建 HME 客户端和邮件客户端。对应原 Python 项目 account_manager.py。
package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"icloud-hme/internal/hme"
)

// Account 描述一个 iCloud 账号。
type Account struct {
	ID                string                 `json:"id"`
	Name              string                 `json:"name"`
	RealEmail         string                 `json:"real_email"`
	Cookies           map[string]string      `json:"cookies"`
	Host              string                 `json:"host"`
	Proxy             string                 `json:"proxy,omitempty"` // HTTP/SOCKS5 代理
	MailReceiver      *MailReceiverConfig    `json:"mail_receiver,omitempty"`
	HMEClientID       string                 `json:"hme_client_id,omitempty"`
	Status            string                 `json:"status"` // active / error
	Enabled           bool                   `json:"enabled"`
	AutoCreateEnabled bool                   `json:"auto_create_enabled"`
	AliasTotal        int                    `json:"alias_total"`
	AliasActive       int                    `json:"alias_active"`
	AliasUsages       map[string]*AliasUsage `json:"alias_usages,omitempty"`
	RequiresLogin     bool                   `json:"requires_login,omitempty"`
	LastValidated     string                 `json:"last_validated"`
	LastError         string                 `json:"last_error,omitempty"`
	CreatedAt         string                 `json:"created_at"`
}

// MailReceiverConfig stores one account's fixed MoeMail forwarding mailbox.
// APIKey is persisted because the server needs it to read mail, but is
// stripped from every public account response.
type MailReceiverConfig struct {
	BaseURL                 string `json:"base_url,omitempty"`
	Address                 string `json:"address,omitempty"`
	MailboxID               string `json:"mailbox_id,omitempty"`
	APIKey                  string `json:"api_key,omitempty"`
	CleanupEnabled          bool   `json:"cleanup_enabled,omitempty"`
	CleanupRetentionSeconds int    `json:"cleanup_retention_seconds,omitempty"`
}

// AliasUsage 是本项目对某个 HME 别名的本地使用记录。
//
// Apple 只维护别名本身；调用方身份与“是否领取过”的关系由本项目保存。
type AliasUsage struct {
	Email          string                              `json:"email"`
	AnonymousID    string                              `json:"anonymous_id,omitempty"`
	Label          string                              `json:"label,omitempty"`
	Active         bool                                `json:"active"`
	CreatedAt      string                              `json:"created_at,omitempty"`
	CreatedBy      string                              `json:"created_by,omitempty"`
	LastSeenAt     string                              `json:"last_seen_at,omitempty"`
	UsedBy         map[string]string                   `json:"used_by,omitempty"`
	PendingCallers map[string]*PendingCallerAllocation `json:"pending_callers,omitempty"`
}

// PendingCallerAllocation is retained only to read data written by older
// versions. New allocations are permanent immediately and do not create a
// pending caller entry.
type PendingCallerAllocation struct {
	AllocatedAt string `json:"allocated_at"`
	ExpiresAt   string `json:"expires_at"`
	LastReadAt  string `json:"last_read_at,omitempty"`
}

const callerTagConfirmationWindow = 5 * time.Minute

// CallerReadObservation describes the effect of a caller's inbox read on a
// just-allocated alias tag.
type CallerReadObservation struct {
	Confirmed bool
	Cleared   bool
}

// Manager 管理多个 iCloud 账号,线程安全。
type Manager struct {
	mu       sync.Mutex
	accounts map[string]*Account
	dataDir  string
	dataFile string
}

// SessionRefreshResult 描述一个 Apple 账户管理会话的保活结果。
type SessionRefreshResult struct {
	AccountID string
	Err       error
}

// NewManager 创建管理器。dataDir 用于存放 accounts.json。
func NewManager(dataDir string) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	m := &Manager{
		accounts: make(map[string]*Account),
		dataDir:  dataDir,
		dataFile: filepath.Join(dataDir, "accounts.json"),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// Reload 重新加载 accounts.json 配置文件。
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

func (m *Manager) load() error {
	raw, err := os.ReadFile(m.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var wrapper struct {
		Accounts map[string]*Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return err
	}
	m.accounts = wrapper.Accounts
	if m.accounts == nil {
		m.accounts = make(map[string]*Account)
	}
	var rawWrapper struct {
		Accounts map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &rawWrapper); err != nil {
		return err
	}
	migrated := false
	for id, acc := range m.accounts {
		if ensureHMEClientID(acc) {
			migrated = true
		}
		var rawAccount struct {
			Enabled           *bool   `json:"enabled"`
			AutoCreateEnabled *bool   `json:"auto_create_enabled"`
			ICloudEmail       *string `json:"icloud_email"`
			AppPassword       *string `json:"app_password"`
			MailReceiver      *struct {
				Provider json.RawMessage `json:"provider"`
				Token    json.RawMessage `json:"token"`
			} `json:"mail_receiver"`
		}
		if err := json.Unmarshal(rawWrapper.Accounts[id], &rawAccount); err != nil {
			return err
		}
		if rawAccount.AutoCreateEnabled == nil {
			acc.AutoCreateEnabled = true
			migrated = true
		}
		if rawAccount.Enabled == nil {
			acc.Enabled = true
			migrated = true
		}
		if rawAccount.ICloudEmail != nil || rawAccount.AppPassword != nil {
			migrated = true
		}
		if rawAccount.MailReceiver != nil && (len(rawAccount.MailReceiver.Provider) > 0 || len(rawAccount.MailReceiver.Token) > 0) {
			// Provider-style receiver configurations belonged to the removed
			// multi-provider implementation and must not remain active.
			acc.MailReceiver = nil
			migrated = true
		}
	}
	if migrated {
		return m.save()
	}
	return nil
}

func (m *Manager) save() error {
	wrapper := struct {
		Accounts  map[string]*Account `json:"accounts"`
		UpdatedAt string              `json:"updated_at"`
	}{
		Accounts:  m.accounts,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.dataFile, raw, 0600)
}

// ParseCookieInput 解析 Cookie 输入,支持三种格式:
//   - Header String: "name1=value1; name2=value2; ..."
//   - JSON: {"name1":"value1","name2":"value2"}
//   - 浏览器导出数组: [{"name":"name1","value":"value1"}]
//
// 空输入返回错误。
func ParseCookieInput(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("空白输入 — 请粘贴 Cookie Header String 或 JSON")
	}

	// JSON 格式
	if strings.HasPrefix(raw, "{") {
		var cookies map[string]string
		if err := json.Unmarshal([]byte(raw), &cookies); err == nil && cookies != nil {
			out := make(map[string]string, len(cookies))
			for k, v := range cookies {
				if v != "" {
					out[k] = v
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
	if strings.HasPrefix(raw, "[") {
		var items []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &items); err == nil {
			cookies := make(map[string]string, len(items))
			for _, item := range items {
				if strings.TrimSpace(item.Name) != "" {
					cookies[item.Name] = item.Value
				}
			}
			if len(cookies) > 0 {
				return cookies, nil
			}
		}
	}

	// Header String 格式
	cookies := make(map[string]string)
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		idx := strings.Index(part, "=")
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if name != "" {
			cookies[name] = value
		}
	}
	if len(cookies) == 0 {
		return nil, fmt.Errorf("无法解析 Cookie 输入,请提供 Header String 或 JSON 格式")
	}
	return cookies, nil
}

// AddAccount 添加一个账号。cookieInput 必须包含浏览器导出的 Cookie。
//
// cookieInput 支持 Header String 或 JSON。校验失败仍会保存账号(status=error),
// 方便用户后续修正 Cookie 后重新校验。
func (m *Manager) AddAccount(name, cookieInput, host, proxy string) (*Account, error) {
	if strings.TrimSpace(cookieInput) == "" {
		return nil, fmt.Errorf("cookies 必填,请粘贴浏览器导出的 Cookie")
	}
	var cookies map[string]string
	var err error
	cookies, err = ParseCookieInput(cookieInput)
	if err != nil {
		return nil, err
	}
	if host == "" {
		host = "icloud.com"
	}

	acc := &Account{
		ID:                "acc_" + uuid.New().String()[:8],
		Name:              name,
		Cookies:           cookies,
		Host:              host,
		Proxy:             proxy,
		HMEClientID:       uuid.New().String(),
		Status:            "pending",
		Enabled:           true,
		AutoCreateEnabled: true,
		CreatedAt:         time.Now().Format(time.RFC3339),
	}

	// 有 Cookie 才校验会话
	if len(cookies) > 0 {
		client, err := hme.NewClientWithID(cookies, host, proxy, acc.HMEClientID, false)
		if err != nil {
			return nil, err
		}
		if err := validateHMESession(client); err != nil {
			acc.Status = "error"
			acc.RequiresLogin = true
			acc.LastError = truncate(err.Error(), 300)
		} else {
			acc.Status = "active"
			acc.RequiresLogin = false
			if info := client.AccountInfo(); info != nil {
				acc.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
				acc.Name = firstNonEmpty(acc.RealEmail, acc.Name)
			}
			if aliases, err := client.ListAliases(); err == nil {
				acc.AliasUsages = make(map[string]*AliasUsage, len(aliases))
				acc.AliasTotal = len(aliases)
				for _, a := range aliases {
					ensureAliasUsageLocked(acc, a, "icloud_sync")
					if a.Active {
						acc.AliasActive++
					}
				}
			}
			acc.LastValidated = time.Now().Format(time.RFC3339)
		}
	}

	m.mu.Lock()
	m.accounts[acc.ID] = acc
	saveErr := m.save()
	m.mu.Unlock()
	if saveErr != nil {
		return nil, saveErr
	}
	return acc, nil
}

// RemoveAccount 删除账号。
func (m *Manager) RemoveAccount(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[id]; !ok {
		return false
	}
	delete(m.accounts, id)
	_ = m.save()
	return true
}

// GetAccount 返回账号副本。
func (m *Manager) GetAccount(id string) (*Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return nil, false
	}
	cp := *acc
	cp.Cookies = cloneStringMap(acc.Cookies)
	cp.AliasUsages = cloneAliasUsageMap(acc.AliasUsages)
	cp.MailReceiver = cloneMailReceiverConfig(acc.MailReceiver)
	return &cp, true
}

// ListAccounts 返回所有账号(脱敏,不含 Cookies),按活跃状态排序。
func (m *Manager) ListAccounts() []*Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Account, 0, len(m.accounts))
	for _, acc := range m.accounts {
		cp := *acc
		cp.Cookies = nil
		cp.MailReceiver = publicMailReceiverConfig(acc.MailReceiver)
		cp.HMEClientID = ""
		cp.AliasUsages = nil
		out = append(out, &cp)
	}
	return out
}

// HMEClient 为指定账号创建一个新的 HME 客户端。
// 必须有有效的 Cookie 才能使用 HME 功能。
func (m *Manager) HMEClient(id string, verbose bool) (*hme.Client, error) {
	m.mu.Lock()
	acc, ok := m.accounts[id]
	var cookies map[string]string
	var host, proxy, clientID string
	if ok {
		cookies = cloneStringMap(acc.Cookies)
		host = acc.Host
		proxy = acc.Proxy
		clientID = acc.HMEClientID
	}
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	if !acc.Enabled {
		return nil, fmt.Errorf("账号已停用，当前功能不可用: %s", id)
	}
	if len(cookies) == 0 {
		return nil, fmt.Errorf("账号未配置 Cookie，无法使用 HME 功能")
	}
	return hme.NewClientWithID(cookies, host, proxy, clientID, verbose)
}

// SetMailReceiver replaces the account's forwarding mailbox configuration.
// Passing nil removes it. Credentials are never exposed through ListAccounts.
func (m *Manager) SetMailReceiver(id string, receiver *MailReceiverConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.MailReceiver = cloneMailReceiverConfig(receiver)
	return m.save()
}

// MailReceiver returns a private copy for the server-side provider client.
func (m *Manager) MailReceiver(id string) (*MailReceiverConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	if acc.MailReceiver == nil {
		return nil, fmt.Errorf("账号未配置转发收件箱提供商")
	}
	return cloneMailReceiverConfig(acc.MailReceiver), nil
}

// SaveCookies 保存指定账号的最新 Cookie（HMEClient 操作后刷新的 token）。
// 用于客户端 validate/操作过程中从 Set-Cookie 获取了新 token 后持久化。
func (m *Manager) SaveCookies(id string, cookies map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.Cookies = cloneStringMap(cookies)
	return m.save()
}

// SetAutoCreateEnabled controls whether the background alias pool may create aliases for an account.
func (m *Manager) SetAutoCreateEnabled(id string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.AutoCreateEnabled = enabled
	return m.save()
}

// SetEnabled controls whether an account participates in any iCloud-HME operation.
// Cookie and forwarding mailbox configuration remain available while disabled.
func (m *Manager) SetEnabled(id string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.Enabled = enabled
	return m.save()
}

// EnsureEnabled verifies that an account exists and is available for business operations.
func (m *Manager) EnsureEnabled(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if !acc.Enabled {
		return fmt.Errorf("账号已停用，当前功能不可用: %s", id)
	}
	return nil
}

// MarkLoginRequired 标记账号需要重新登录。前端会据此弹窗并打开 Apple 登录页。
func (m *Manager) MarkLoginRequired(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return
	}
	acc.Status = "error"
	acc.RequiresLogin = true
	if err != nil {
		acc.LastError = truncate(err.Error(), 300)
	} else {
		acc.LastError = "Apple 登录态已过期"
	}
	_ = m.save()
}

// RegisterCreatedAlias 保存后台定时创建出来的别名到本地池。
func (m *Manager) RegisterCreatedAlias(id string, created *hme.CreateResult, cookies map[string]string) error {
	if created == nil || strings.TrimSpace(created.Email) == "" {
		return nil
	}
	alias := hme.Alias{
		Email:       strings.ToLower(strings.TrimSpace(created.Email)),
		AnonymousID: created.AnonymousID,
		Label:       created.Label,
		Active:      true,
		CreatedAt:   created.CreatedAt,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if !acc.Enabled {
		return fmt.Errorf("账号已停用，当前功能不可用: %s", id)
	}
	if cookies != nil {
		acc.Cookies = cloneStringMap(cookies)
	}
	key := aliasKey(alias.Email)
	existing := acc.AliasUsages[key]
	wasActive := existing != nil && existing.Active
	ensureAliasUsageLocked(acc, alias, "auto_pool")
	// AliasUsages intentionally retains historical records for aliases that
	// were deleted upstream. Recounting the map here would therefore turn
	// deleted aliases into phantom aliases and trip the pool max_total gate.
	// Keep the Apple-sourced counters and account only for this new alias.
	if existing == nil {
		acc.AliasTotal++
	}
	if !wasActive && alias.Active {
		acc.AliasActive++
	}
	acc.Status = "active"
	acc.RequiresLogin = false
	acc.LastError = ""
	return m.save()
}

// SetAliasActive updates the local counters after a successful upstream
// activate/deactivate operation. AliasTotal includes inactive aliases that
// still exist upstream; AliasActive tracks only currently active aliases.
func (m *Manager) SetAliasActive(id, anonymousID string, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	for _, usage := range acc.AliasUsages {
		if usage == nil || usage.AnonymousID != anonymousID || usage.Active == active {
			continue
		}
		usage.Active = active
		if active {
			acc.AliasActive++
		} else if acc.AliasActive > 0 {
			acc.AliasActive--
		}
		return m.save()
	}
	return nil
}

// MarkAliasDeleted removes a successfully deleted alias from the local pool
// and counters. Caller usage history for a permanently deleted alias is no
// longer allocatable and is intentionally discarded.
func (m *Manager) MarkAliasDeleted(id, anonymousID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	for key, usage := range acc.AliasUsages {
		if usage == nil || usage.AnonymousID != anonymousID {
			continue
		}
		delete(acc.AliasUsages, key)
		if usage.Active && acc.AliasActive > 0 {
			acc.AliasActive--
		}
		if acc.AliasTotal > 0 {
			acc.AliasTotal--
		}
		return m.save()
	}
	return nil
}

// RefreshAccountSessions 刷新所有使用 Apple 账户管理协议的会话。
// 保存时只合并本次请求实际发生变化的 Cookie,避免覆盖并发业务请求的新值。
func (m *Manager) RefreshAccountSessions() []SessionRefreshResult {
	type candidate struct {
		id       string
		cookies  map[string]string
		host     string
		proxy    string
		clientID string
	}

	m.mu.Lock()
	candidates := make([]candidate, 0, len(m.accounts))
	for id, acc := range m.accounts {
		if !acc.Enabled {
			continue
		}
		if len(acc.Cookies) == 0 {
			continue
		}
		candidates = append(candidates, candidate{
			id:       id,
			cookies:  cloneStringMap(acc.Cookies),
			host:     acc.Host,
			proxy:    acc.Proxy,
			clientID: acc.HMEClientID,
		})
	}
	m.mu.Unlock()

	results := make([]SessionRefreshResult, 0, len(candidates))
	for _, item := range candidates {
		client, err := hme.NewClientWithID(item.cookies, item.host, item.proxy, item.clientID, false)
		if err != nil {
			results = append(results, SessionRefreshResult{AccountID: item.id, Err: err})
			continue
		}
		if !client.UsesAccountAPI() {
			continue
		}
		if err := client.ValidateAccountSession(); err != nil {
			m.MarkLoginRequired(item.id, err)
			results = append(results, SessionRefreshResult{AccountID: item.id, Err: err})
			continue
		}
		err = m.mergeRefreshedCookies(item.id, item.cookies, client.Cookies)
		results = append(results, SessionRefreshResult{AccountID: item.id, Err: err})
	}
	return results
}

func (m *Manager) mergeRefreshedCookies(id string, before, refreshed map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if acc.Cookies == nil {
		acc.Cookies = make(map[string]string)
	}
	for name, value := range refreshed {
		if value == "" || before[name] == value {
			continue
		}
		acc.Cookies[name] = value
	}
	acc.Status = "active"
	acc.RequiresLogin = false
	acc.LastError = ""
	acc.LastValidated = time.Now().Format(time.RFC3339)
	return m.save()
}

// SaveAliasStats 保存指定账号的 Cookie 和最新别名统计。
// 别名列表是 iCloud 的事实来源，账号列表中的汇总字段由此同步。
func (m *Manager) SaveAliasStats(id string, aliases []hme.Alias, cookies map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if !acc.Enabled {
		return fmt.Errorf("账号已停用，当前功能不可用: %s", id)
	}
	if cookies != nil {
		acc.Cookies = cloneStringMap(cookies)
	}
	for _, usage := range acc.AliasUsages {
		if usage != nil {
			usage.Active = false
		}
	}
	acc.AliasTotal = len(aliases)
	acc.AliasActive = 0
	for _, alias := range aliases {
		if alias.Active {
			acc.AliasActive++
		}
		ensureAliasUsageLocked(acc, alias, "icloud_sync")
	}
	acc.Status = "active"
	acc.RequiresLogin = false
	acc.LastError = ""
	return m.save()
}

// DecorateAliases 把本地调用方使用记录附加到别名列表，供管理页面展示。
func (m *Manager) DecorateAliases(id string, aliases []hme.Alias) []hme.Alias {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok || len(aliases) == 0 {
		return aliases
	}
	out := make([]hme.Alias, len(aliases))
	copy(out, aliases)
	for i := range out {
		key := aliasKey(out[i].Email)
		usage := acc.AliasUsages[key]
		if usage == nil {
			continue
		}
		out[i].UsedBy = sortedUsageCallers(usage.UsedBy)
		out[i].UsedByCount = len(out[i].UsedBy)
		out[i].LastUsedAt = latestUsageAt(usage.UsedBy)
		out[i].PendingCallers = sortedPendingCallers(usage.PendingCallers)
	}
	return out
}

// AcquireAlias 为某个调用方从本地别名池领取一个尚未被该调用方领取过的别名。
// A successful claim is permanent until that caller explicitly releases it.
func (m *Manager) AcquireAlias(id, caller string) (*hme.Alias, error) {
	caller = NormalizeCaller(caller)
	if caller == "" {
		return nil, fmt.Errorf("调用方身份 caller 必填")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	if !acc.Enabled {
		return nil, fmt.Errorf("账号已停用，当前功能不可用: %s", id)
	}
	if len(acc.AliasUsages) == 0 {
		return nil, fmt.Errorf("当前账号本地别名池为空，请等待后台定时创建或先刷新别名列表")
	}

	now := time.Now()
	changed := expirePendingAliasCallersLocked(acc, now)
	var selected *AliasUsage
	for _, usage := range acc.AliasUsages {
		if usage == nil || !usage.Active || usage.Email == "" {
			continue
		}
		if _, used := usage.UsedBy[caller]; used {
			continue
		}
		if selected == nil || betterAliasCandidate(usage, selected) {
			selected = usage
		}
	}
	if selected == nil {
		if changed {
			if err := m.save(); err != nil {
				return nil, err
			}
		}
		return nil, fmt.Errorf("该调用方暂无可领取的新别名，请等待后台定时创建")
	}
	markCallerAllocationLocked(selected, caller, now)
	if err := m.save(); err != nil {
		return nil, err
	}
	alias := hme.Alias{
		Email:          selected.Email,
		AnonymousID:    selected.AnonymousID,
		Label:          selected.Label,
		Active:         selected.Active,
		CreatedAt:      selected.CreatedAt,
		UsedBy:         sortedUsageCallers(selected.UsedBy),
		UsedByCount:    len(selected.UsedBy),
		LastUsedAt:     latestUsageAt(selected.UsedBy),
		PendingCallers: sortedPendingCallers(selected.PendingCallers),
	}
	return &alias, nil
}

// AcquireAliasAny atomically allocates an active alias from any enabled
// account which has not already allocated that alias to caller. It is used by
// callers that do not care which Apple account owns the alias, but need a
// usable address without retrying exhausted accounts themselves.
func (m *Manager) AcquireAliasAny(caller string) (string, *hme.Alias, error) {
	caller = NormalizeCaller(caller)
	if caller == "" {
		return "", nil, fmt.Errorf("调用方身份 caller 必填")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	changed := false
	var selected *AliasUsage
	selectedAccountID := ""
	for accountID, acc := range m.accounts {
		if acc == nil || !acc.Enabled {
			continue
		}
		if expirePendingAliasCallersLocked(acc, time.Now()) {
			changed = true
		}
		for _, usage := range acc.AliasUsages {
			if usage == nil || !usage.Active || usage.Email == "" {
				continue
			}
			if _, used := usage.UsedBy[caller]; used {
				continue
			}
			if selected == nil || betterAliasCandidate(usage, selected) ||
				(!betterAliasCandidate(selected, usage) && accountID < selectedAccountID) {
				selected = usage
				selectedAccountID = accountID
			}
		}
	}
	if selected == nil {
		if changed {
			if saveErr := m.save(); saveErr != nil {
				return "", nil, saveErr
			}
		}
		return "", nil, fmt.Errorf("所有启用账号中，该调用方暂无可领取的新别名，请等待后台定时创建")
	}
	markCallerAllocationLocked(selected, caller, time.Now())
	if err := m.save(); err != nil {
		return "", nil, err
	}
	return selectedAccountID, &hme.Alias{
		Email:          selected.Email,
		AnonymousID:    selected.AnonymousID,
		Label:          selected.Label,
		Active:         selected.Active,
		CreatedAt:      selected.CreatedAt,
		UsedBy:         sortedUsageCallers(selected.UsedBy),
		UsedByCount:    len(selected.UsedBy),
		LastUsedAt:     latestUsageAt(selected.UsedBy),
		PendingCallers: sortedPendingCallers(selected.PendingCallers),
	}, nil
}

// MarkAliasCaller records that a caller already owns/uses an alias without
// allocating a different alias. This is used when an external consumer finds
// an alias that was registered before caller-tag tracking was enabled.
// The operation is idempotent for the same caller and alias.
func (m *Manager) MarkAliasCaller(id, anonymousID, caller string) (*hme.Alias, error) {
	anonymousID = strings.TrimSpace(anonymousID)
	caller = NormalizeCaller(caller)
	if anonymousID == "" {
		return nil, fmt.Errorf("别名 anonymousId 必填")
	}
	if caller == "" {
		return nil, fmt.Errorf("调用方身份 caller 必填")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[strings.TrimSpace(id)]
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	var selected *AliasUsage
	for _, usage := range acc.AliasUsages {
		if usage != nil && usage.AnonymousID == anonymousID {
			selected = usage
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("别名不存在: %s", anonymousID)
	}
	markCallerAllocationLocked(selected, caller, time.Now())
	if err := m.save(); err != nil {
		return nil, err
	}
	return &hme.Alias{
		Email:          selected.Email,
		AnonymousID:    selected.AnonymousID,
		Label:          selected.Label,
		Active:         selected.Active,
		CreatedAt:      selected.CreatedAt,
		UsedBy:         sortedUsageCallers(selected.UsedBy),
		UsedByCount:    len(selected.UsedBy),
		LastUsedAt:     latestUsageAt(selected.UsedBy),
		PendingCallers: sortedPendingCallers(selected.PendingCallers),
	}, nil
}

// RemoveAliasCaller deletes one local caller allocation without changing the Apple alias.
func (m *Manager) RemoveAliasCaller(id, anonymousID, caller string) (*hme.Alias, error) {
	anonymousID = strings.TrimSpace(anonymousID)
	caller = NormalizeCaller(caller)
	if anonymousID == "" {
		return nil, fmt.Errorf("别名 anonymousId 必填")
	}
	if caller == "" {
		return nil, fmt.Errorf("调用方身份 caller 必填")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	var selected *AliasUsage
	for _, usage := range acc.AliasUsages {
		if usage != nil && usage.AnonymousID == anonymousID {
			selected = usage
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("别名不存在: %s", anonymousID)
	}
	removed := false
	for storedCaller := range selected.UsedBy {
		if NormalizeCaller(storedCaller) == caller {
			delete(selected.UsedBy, storedCaller)
			removed = true
		}
	}
	for storedCaller := range selected.PendingCallers {
		if NormalizeCaller(storedCaller) == caller {
			delete(selected.PendingCallers, storedCaller)
			removed = true
		}
	}
	if !removed {
		return nil, fmt.Errorf("该别名没有调用方记录: %s", caller)
	}
	if err := m.save(); err != nil {
		return nil, err
	}
	return &hme.Alias{
		Email:          selected.Email,
		AnonymousID:    selected.AnonymousID,
		Label:          selected.Label,
		Active:         selected.Active,
		CreatedAt:      selected.CreatedAt,
		UsedBy:         sortedUsageCallers(selected.UsedBy),
		UsedByCount:    len(selected.UsedBy),
		LastUsedAt:     latestUsageAt(selected.UsedBy),
		PendingCallers: sortedPendingCallers(selected.PendingCallers),
	}, nil
}

// ObserveCallerInboxRead is kept for compatibility with existing inbox
// callers. Claims are permanent at allocation time, so reading mail no longer
// changes a caller label.
func (m *Manager) ObserveCallerInboxRead(id, email, caller string, hasMail bool) (CallerReadObservation, error) {
	return CallerReadObservation{}, nil
}

// ExpirePendingCallerAllocations is retained as a no-op for backwards
// compatibility. Caller labels are no longer released by time-based expiry.
func (m *Manager) ExpirePendingCallerAllocations() (int, error) {
	return 0, nil
}

// UpdateCookies 更新指定账号的 Cookie,并自动校验会话有效性。
func (m *Manager) UpdateCookies(id string, cookies map[string]string) error {
	if len(cookies) == 0 {
		return fmt.Errorf("cookies 不能为空")
	}
	m.mu.Lock()
	acc, ok := m.accounts[id]
	var host, proxy, clientID string
	if ok {
		host = acc.Host
		proxy = acc.Proxy
		clientID = acc.HMEClientID
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}

	// 自动校验 Cookie 是否有效
	if host == "" {
		host = "icloud.com"
	}
	client, err := hme.NewClientWithID(cookies, host, proxy, clientID, false)
	if err != nil {
		m.mu.Lock()
		acc, ok = m.accounts[id]
		if !ok {
			m.mu.Unlock()
			return fmt.Errorf("账号不存在: %s", id)
		}
		acc.Cookies = cloneStringMap(cookies)
		acc.Status = "error"
		acc.RequiresLogin = true
		acc.LastError = "创建客户端失败: " + err.Error()
		m.accounts[id] = acc
		_ = m.save()
		m.mu.Unlock()
		return err
	}

	status := "active"
	lastError := ""
	lastValidated := time.Now().Format(time.RFC3339)
	var realEmail string
	var aliases []hme.Alias
	aliasesLoaded := false
	if err := validateHMESession(client); err != nil {
		status = "error"
		lastError = "Cookie 校验失败: " + err.Error()
	} else {
		if info := client.AccountInfo(); info != nil {
			realEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
		}
		if listed, listErr := client.ListAliases(); listErr == nil {
			aliases = listed
			aliasesLoaded = true
		}
	}

	m.mu.Lock()
	acc, ok = m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.Cookies = cloneStringMap(client.Cookies)
	acc.Status = status
	acc.LastError = lastError
	if status == "active" {
		acc.RequiresLogin = false
		acc.LastValidated = lastValidated
		if realEmail != "" {
			acc.RealEmail = realEmail
		}
	}
	if status == "error" {
		acc.RequiresLogin = true
	}
	if aliasesLoaded {
		for _, usage := range acc.AliasUsages {
			if usage != nil {
				usage.Active = false
			}
		}
		if acc.AliasUsages == nil {
			acc.AliasUsages = make(map[string]*AliasUsage, len(aliases))
		}
		acc.AliasTotal = len(aliases)
		acc.AliasActive = 0
		for _, alias := range aliases {
			if alias.Active {
				acc.AliasActive++
			}
			ensureAliasUsageLocked(acc, alias, "icloud_sync")
		}
	}
	m.accounts[id] = acc
	saveErr := m.save()
	m.mu.Unlock()
	return saveErr
}

func validateHMESession(client *hme.Client) error {
	legacyErr := client.ValidateSession()
	if legacyErr == nil {
		return nil
	}
	if !client.UsesAccountAPI() {
		return legacyErr
	}
	if accountErr := client.ValidateAccountSession(); accountErr != nil {
		return fmt.Errorf("iCloud Web 校验失败: %v; Apple 账户校验失败: %w", legacyErr, accountErr)
	}
	return nil
}

// ---- 辅助函数 ----

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func ensureHMEClientID(acc *Account) bool {
	if acc == nil {
		return false
	}
	if _, err := uuid.Parse(acc.HMEClientID); err == nil {
		return false
	}
	acc.HMEClientID = uuid.New().String()
	return true
}

// NormalizeCaller 规范化调用方身份，大小写视为同一个调用方。
func NormalizeCaller(caller string) string {
	return strings.ToLower(strings.TrimSpace(caller))
}

func aliasKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func ensureAliasUsageLocked(acc *Account, alias hme.Alias, source string) *AliasUsage {
	if acc.AliasUsages == nil {
		acc.AliasUsages = make(map[string]*AliasUsage)
	}
	key := aliasKey(alias.Email)
	if key == "" {
		return nil
	}
	now := time.Now().Format(time.RFC3339)
	usage := acc.AliasUsages[key]
	if usage == nil {
		usage = &AliasUsage{
			Email:      key,
			Active:     alias.Active,
			CreatedAt:  alias.CreatedAt,
			CreatedBy:  source,
			LastSeenAt: now,
			UsedBy:     make(map[string]string),
		}
		acc.AliasUsages[key] = usage
	}
	usage.Email = key
	if alias.AnonymousID != "" {
		usage.AnonymousID = alias.AnonymousID
	}
	if alias.Label != "" {
		usage.Label = alias.Label
	}
	usage.Active = alias.Active
	if alias.CreatedAt != "" {
		usage.CreatedAt = alias.CreatedAt
	}
	if usage.CreatedBy == "" {
		usage.CreatedBy = source
	}
	usage.LastSeenAt = now
	if usage.UsedBy == nil {
		usage.UsedBy = make(map[string]string)
	}
	if usage.PendingCallers == nil {
		usage.PendingCallers = make(map[string]*PendingCallerAllocation)
	}
	return usage
}

func markCallerAllocationLocked(usage *AliasUsage, caller string, now time.Time) {
	if usage == nil {
		return
	}
	caller = NormalizeCaller(caller)
	if caller == "" {
		return
	}
	if usage.UsedBy == nil {
		usage.UsedBy = make(map[string]string)
	}
	usage.UsedBy[caller] = now.Format(time.RFC3339Nano)
}

func pendingCallerFor(values map[string]*PendingCallerAllocation, caller string) (string, *PendingCallerAllocation) {
	for storedCaller, allocation := range values {
		if NormalizeCaller(storedCaller) == caller {
			return storedCaller, allocation
		}
	}
	return "", nil
}

func deleteUsageCaller(values map[string]string, caller string) {
	for storedCaller := range values {
		if NormalizeCaller(storedCaller) == caller {
			delete(values, storedCaller)
		}
	}
}

func expirePendingAliasCallersLocked(acc *Account, now time.Time) bool {
	return expirePendingAliasCallersLockedCount(acc, now) > 0
}

func expirePendingAliasCallersLockedCount(acc *Account, now time.Time) int {
	return 0
}

func parsePendingExpiry(value *PendingCallerAllocation) (time.Time, bool) {
	if value == nil {
		return time.Time{}, false
	}
	if expiresAt, ok := parseAnyTime(value.ExpiresAt); ok {
		return expiresAt, true
	}
	if allocatedAt, ok := parseAnyTime(value.AllocatedAt); ok {
		return allocatedAt.Add(callerTagConfirmationWindow), true
	}
	return time.Time{}, false
}

func countAliasUsages(acc *Account, activeOnly bool) int {
	if acc == nil {
		return 0
	}
	count := 0
	for _, usage := range acc.AliasUsages {
		if usage == nil || usage.Email == "" {
			continue
		}
		if activeOnly && !usage.Active {
			continue
		}
		count++
	}
	return count
}

func betterAliasCandidate(candidate, current *AliasUsage) bool {
	candidateUsed := len(candidate.UsedBy)
	currentUsed := len(current.UsedBy)
	if candidateUsed != currentUsed {
		return candidateUsed < currentUsed
	}
	candidateTime, candidateOK := parseAnyTime(candidate.CreatedAt)
	currentTime, currentOK := parseAnyTime(current.CreatedAt)
	if candidateOK && currentOK && !candidateTime.Equal(currentTime) {
		return candidateTime.Before(currentTime)
	}
	if candidateOK != currentOK {
		return candidateOK
	}
	return candidate.Email < current.Email
}

func sortedUsageCallers(used map[string]string) []string {
	if len(used) == 0 {
		return nil
	}
	callers := make([]string, 0, len(used))
	for caller := range used {
		callers = append(callers, caller)
	}
	sort.Strings(callers)
	return callers
}

func sortedPendingCallers(pending map[string]*PendingCallerAllocation) []string {
	if len(pending) == 0 {
		return nil
	}
	callers := make([]string, 0, len(pending))
	for caller := range pending {
		if normalized := NormalizeCaller(caller); normalized != "" {
			callers = append(callers, normalized)
		}
	}
	sort.Strings(callers)
	return callers
}

func latestUsageAt(used map[string]string) string {
	var latest string
	var latestTime time.Time
	for _, value := range used {
		parsed, ok := parseAnyTime(value)
		if ok {
			if latest == "" || parsed.After(latestTime) {
				latest = value
				latestTime = parsed
			}
			continue
		}
		if value > latest {
			latest = value
		}
	}
	return latest
}

func parseAnyTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	if ms, err := parseInt64(value); err == nil && ms > 0 {
		if ms > 1_000_000_000_000 {
			return time.UnixMilli(ms), true
		}
		return time.Unix(ms, 0), true
	}
	return time.Time{}, false
}

func parseInt64(value string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(value, "%d", &n)
	return n, err
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneAliasUsageMap(values map[string]*AliasUsage) map[string]*AliasUsage {
	if values == nil {
		return nil
	}
	cloned := make(map[string]*AliasUsage, len(values))
	for key, value := range values {
		if value == nil {
			continue
		}
		cp := *value
		cp.UsedBy = cloneStringMap(value.UsedBy)
		cp.PendingCallers = clonePendingCallerMap(value.PendingCallers)
		cloned[key] = &cp
	}
	return cloned
}

func clonePendingCallerMap(values map[string]*PendingCallerAllocation) map[string]*PendingCallerAllocation {
	if values == nil {
		return nil
	}
	cloned := make(map[string]*PendingCallerAllocation, len(values))
	for key, value := range values {
		if value == nil {
			continue
		}
		cp := *value
		cloned[key] = &cp
	}
	return cloned
}

func cloneMailReceiverConfig(value *MailReceiverConfig) *MailReceiverConfig {
	if value == nil {
		return nil
	}
	cp := *value
	return &cp
}

func publicMailReceiverConfig(value *MailReceiverConfig) *MailReceiverConfig {
	cp := cloneMailReceiverConfig(value)
	if cp == nil {
		return nil
	}
	cp.APIKey = ""
	return cp
}
