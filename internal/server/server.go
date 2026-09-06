// Package server 提供 HTTP API,基于 Gin。
//
// 两个核心接口:
//
//	POST /api/create  — 从本地别名池领取一个 Hide My Email 别名
//	GET  /api/inbox   — 读取指定账号(或指定别名)收到的邮件
//
// 辅助接口(用于多账号管理):账号增删查、别名列表、设置 App 密码。
package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

const (
	defaultCreateMinInterval     = 0
	defaultCreateFailureCooldown = 10 * time.Minute
)

type createAttemptState struct {
	inFlight    bool
	nextAllowed time.Time
}

// Server 封装 Gin 引擎和账号管理器。
type Server struct {
	mgr                   *account.Manager
	r                     *gin.Engine
	apiKey                string
	createMu              sync.Mutex
	createAttempts        map[string]*createAttemptState
	now                   func() time.Time
	createMinInterval     time.Duration
	createFailureCooldown time.Duration
	relogin               ReloginConfig
	otpMu                 sync.Mutex
	otpCodes              []otpCodeRecord
	poolMu                sync.Mutex
	poolConfig            AliasPoolConfig
	poolStop              chan struct{}
	poolLogMu             sync.Mutex
	poolLogs              []aliasPoolLogEntry
	inboxCache            inboxCacheStore
	receiverClientFactory func(*account.MailReceiverConfig) (receiverClient, error)
	cleanupMu             sync.Mutex
	cleanupCursors        map[string]string
	pendingCallerStop     chan struct{}
}

// New 创建 Server。debug 为 true 时启用 Gin 调试日志。
func New(mgr *account.Manager, debug bool, apiKeys ...string) *Server {
	if !debug {
		gin.SetMode(gin.ReleaseMode)
	}
	apiKey := ""
	if len(apiKeys) > 0 {
		apiKey = strings.TrimSpace(apiKeys[0])
	}
	s := &Server{
		mgr:                   mgr,
		apiKey:                apiKey,
		createAttempts:        make(map[string]*createAttemptState),
		now:                   time.Now,
		createMinInterval:     defaultCreateMinInterval,
		createFailureCooldown: defaultCreateFailureCooldown,
		relogin:               ReloginConfigFromEnv(),
		poolConfig:            AliasPoolConfigFromEnv(),
		inboxCache:            inboxCacheStore{entries: make(map[string]inboxCacheEntry)},
		receiverClientFactory: newReceiverClient,
		cleanupCursors:        make(map[string]string),
		pendingCallerStop:     make(chan struct{}),
	}
	s.r = gin.Default() // 自带 Logger + Recovery 中间件
	s.register()
	return s
}

// Run 启动 HTTP 服务。
func (s *Server) Run(addr string) error {
	s.startMailReceiverCleanupWorker()
	s.startPendingCallerExpiryWorker()
	return s.r.Run(addr)
}

// Handler 返回底层 gin 引擎(便于测试)。
func (s *Server) Handler() http.Handler { return s.r }

func (s *Server) register() {
	s.r.GET("/_internal/api-key-auth", s.verifyAPIKey)
	s.registerWeb()

	api := s.r.Group("/api")
	{
		// ===== 账号管理 =====
		api.GET("/accounts", s.listAccounts)
		api.POST("/accounts", s.addAccount)
		api.DELETE("/accounts/:id", s.removeAccount)
		api.PUT("/accounts/:id/mail-receiver", s.setMailReceiver)
		api.DELETE("/accounts/:id/mail-receiver", s.clearMailReceiver)
		api.POST("/accounts/:id/mail-receiver/cleanup", s.cleanupMailReceiver)
		api.PUT("/accounts/:id/cookies", s.updateCookies)
		api.PUT("/accounts/:id/enabled", s.setAccountEnabled)
		api.PUT("/accounts/:id/auto-create", s.setAccountAutoCreate)

		// ===== 核心接口 1: 创建邮箱 =====
		api.POST("/create", s.createAlias)

		// ===== 核心接口 2: 读取邮件 =====
		api.GET("/inbox", s.listInbox)
		api.GET("/inbox/message", s.getInboxMessage)

		// ===== 别名管理 =====
		api.GET("/aliases", s.listAliases)
		api.POST("/aliases/:id/deactivate", s.deactivateAlias)
		api.POST("/aliases/:id/reactivate", s.reactivateAlias)
		api.POST("/aliases/:id/callers", s.markAliasCaller)
		api.POST("/aliases/release", s.releaseAliasCaller)
		api.DELETE("/aliases/:id/callers/:caller", s.removeAliasCaller)
		api.DELETE("/aliases/:id", s.deleteAlias)

		// ===== 系统 =====
		api.POST("/reload", s.reloadConfig)
		api.POST("/otp/inbound", s.receiveOTP)
		api.GET("/otp/latest", s.latestOTP)
		api.GET("/relogin/config", s.reloginConfig)
		api.PUT("/relogin/config", s.updateReloginConfig)
		api.GET("/alias-pool/config", s.aliasPoolConfig)
		api.PUT("/alias-pool/config", s.updateAliasPoolConfig)
		api.GET("/alias-pool/logs", s.aliasPoolLogs)
	}
}

func (s *Server) verifyAPIKey(c *gin.Context) {
	provided := c.GetHeader("X-API-Key")
	if s.apiKey == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(s.apiKey)) != 1 {
		c.Status(http.StatusUnauthorized)
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- 统一响应 ----

type apiResp struct {
	Success bool                  `json:"success"`
	Message string                `json:"message,omitempty"`
	Data    interface{}           `json:"data,omitempty"`
	Error   *upstreamErrorDetails `json:"error,omitempty"`
}

type upstreamErrorDetails struct {
	UpstreamStatus   int    `json:"upstream_status,omitempty"`
	UpstreamBody     string `json:"upstream_body,omitempty"`
	RetryAfterSecond int64  `json:"retry_after_seconds,omitempty"`
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, apiResp{Success: true, Data: data})
}

func fail(c *gin.Context, code int, msg string) {
	c.JSON(code, apiResp{Success: false, Message: msg})
}

func failCreate(c *gin.Context, code int, msg string, httpErr *hme.HTTPError, retryAfter time.Duration) {
	details := &upstreamErrorDetails{RetryAfterSecond: retryAfterSeconds(retryAfter)}
	if httpErr != nil {
		details.UpstreamStatus = httpErr.StatusCode
		details.UpstreamBody = httpErr.Body
	}
	if details.RetryAfterSecond > 0 {
		c.Header("Retry-After", strconv.FormatInt(details.RetryAfterSecond, 10))
	}
	c.JSON(code, apiResp{Success: false, Message: msg, Error: details})
}

// ====================================================================
// 核心接口 1: 获取邮箱
//   POST /api/create
//   body: {"account_id": "acc_xxx", "caller": "chatgpt"}
//   返回: 本地别名池中尚未被该 caller 领取过的 HME 邮箱地址
//
// account_id is optional. When omitted, the server atomically selects an
// available alias across all enabled accounts and returns its owning account.
// ====================================================================

type createReq struct {
	AccountID string `json:"account_id"`
	Caller    string `json:"caller"`
	Client    string `json:"client"`
	Identity  string `json:"identity"`
	Label     string `json:"label"`
	Note      string `json:"note"`
}

func (s *Server) createAlias(c *gin.Context) {
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: 请求体必须是 JSON — "+err.Error())
		return
	}
	caller := account.NormalizeCaller(firstNonEmptyString(req.Caller, req.Client, req.Identity))
	if caller == "" {
		fail(c, http.StatusBadRequest, "参数缺失: caller（调用方身份字符串，例如 chatgpt、moxt）")
		return
	}
	accountID := strings.TrimSpace(req.AccountID)
	var alias *hme.Alias
	var err error
	if accountID == "" {
		accountID, alias, err = s.acquireAliasAcrossAccounts(caller)
		if err != nil {
			fail(c, http.StatusConflict, err.Error())
			return
		}
	} else {
		var status int
		accountID, status, err = s.resolveCreateAccountID(accountID)
		if err != nil {
			fail(c, status, err.Error())
			return
		}
		alias, err = s.mgr.AcquireAlias(accountID, caller)
		if err != nil {
			// 本地池没有可领别名时，先同步一次 Apple 侧列表，避免刚启动时本地记录为空。
			if syncErr := s.syncAliases(accountID); syncErr != nil {
				if isSessionError(syncErr.Error()) {
					s.mgr.MarkLoginRequired(accountID, syncErr)
					fail(c, http.StatusUnauthorized, "Apple 登录态已过期，请在管理页面重新登录并更新 Cookie: "+syncErr.Error())
					return
				}
				fail(c, http.StatusConflict, err.Error()+"；同步 Apple 别名列表失败: "+syncErr.Error())
				return
			}
			alias, err = s.mgr.AcquireAlias(accountID, caller)
			if err != nil {
				fail(c, http.StatusConflict, err.Error())
				return
			}
		}
	}

	ok(c, gin.H{
		"email":       alias.Email,
		"anonymousId": alias.AnonymousID,
		"label":       alias.Label,
		"created_at":  alias.CreatedAt,
		"caller":      caller,
		"used_by":     alias.UsedBy,
		"protocol":    "local_pool",
		"account_id":  accountID,
	})
}

func (s *Server) acquireAliasAcrossAccounts(caller string) (string, *hme.Alias, error) {
	accountID, alias, err := s.mgr.AcquireAliasAny(caller)
	if err == nil {
		return accountID, alias, nil
	}

	// `/api/create` is intentionally a local-pool allocation endpoint.  Do not
	// synchronously refresh every Apple account here when the pool is empty:
	// those upstream calls can take tens of seconds, exceed callers' request
	// deadlines, and may still reserve an alias after the caller has given up.
	// The alias-pool worker is the only component that talks to Apple to refill
	// inventory, so an empty pool is returned promptly as a normal 409 response.
	return "", nil, err
}

func (s *Server) syncAliases(accountID string) error {
	if err := s.mgr.EnsureEnabled(accountID); err != nil {
		return err
	}
	client, err := s.mgr.HMEClient(accountID, false)
	if err != nil {
		return err
	}
	aliases, err := client.ListAliases()
	if err != nil {
		_ = s.mgr.SaveCookies(accountID, client.Cookies)
		return err
	}
	return s.mgr.SaveAliasStats(accountID, aliases, client.Cookies)
}

func (s *Server) resolveCreateAccountID(accountID string) (string, int, error) {
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		if acc, ok := s.mgr.GetAccount(accountID); ok && !acc.Enabled {
			return "", http.StatusConflict, fmt.Errorf("账号已停用，无法领取别名: %s", accountID)
		}
		return accountID, 0, nil
	}
	allAccounts := s.mgr.ListAccounts()
	accounts := make([]*account.Account, 0, len(allAccounts))
	for _, acc := range allAccounts {
		if acc != nil && acc.Enabled {
			accounts = append(accounts, acc)
		}
	}
	switch len(accounts) {
	case 0:
		return "", http.StatusNotFound, errors.New("没有可用的 iCloud 账号")
	case 1:
		return accounts[0].ID, 0, nil
	default:
		return "", http.StatusBadRequest, errors.New("存在多个 iCloud 账号，请显式提供 account_id")
	}
}

func (s *Server) beginCreate(accountID string) (time.Duration, bool) {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	now := s.now()
	state := s.createAttempts[accountID]
	if state == nil {
		state = &createAttemptState{}
		s.createAttempts[accountID] = state
	}
	if state.inFlight {
		retryAfter := state.nextAllowed.Sub(now)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return retryAfter, false
	}
	if now.Before(state.nextAllowed) {
		return state.nextAllowed.Sub(now), false
	}
	state.inFlight = true
	state.nextAllowed = now.Add(s.createMinInterval)
	return 0, true
}

func (s *Server) finishCreate(accountID string, failureCooldown time.Duration) {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	state := s.createAttempts[accountID]
	if state == nil {
		return
	}
	state.inFlight = false
	if failureCooldown > 0 {
		nextAllowed := s.now().Add(failureCooldown)
		if nextAllowed.After(state.nextAllowed) {
			state.nextAllowed = nextAllowed
		}
	}
}

func (s *Server) classifyCreateError(err error) (int, string, *hme.HTTPError, time.Duration) {
	var httpErr *hme.HTTPError
	if errors.As(err, &httpErr) {
		body := strings.ToLower(httpErr.Body)
		if strings.Contains(body, "rate_limit_exceeded") || strings.Contains(body, "too many") {
			cooldown := maxDuration(s.createFailureCooldown, httpErr.RetryAfter)
			return http.StatusTooManyRequests, "Apple 暂时限制了自动创建，请在冷却结束后重试: " + err.Error(), httpErr, cooldown
		}
		switch httpErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return http.StatusUnauthorized, "iCloud 会话失效，请更新 Cookie: " + err.Error(), httpErr, 0
		case http.StatusTooManyRequests:
			cooldown := maxDuration(s.createFailureCooldown, httpErr.RetryAfter)
			return http.StatusTooManyRequests, "Apple 暂时限制了自动创建，请在冷却结束后重试: " + err.Error(), httpErr, cooldown
		default:
			return http.StatusFailedDependency, "Apple 创建接口失败: " + err.Error(), httpErr, s.createFailureCooldown
		}
	}
	if isSessionError(err.Error()) {
		return http.StatusUnauthorized, "iCloud 会话失效，请更新 Cookie: " + err.Error(), nil, 0
	}
	return http.StatusFailedDependency, "Apple 创建接口失败: " + err.Error(), nil, s.createFailureCooldown
}

func retryAfterSeconds(delay time.Duration) int64 {
	if delay <= 0 {
		return 0
	}
	return int64((delay + time.Second - 1) / time.Second)
}

func maxDuration(left, right time.Duration) time.Duration {
	if right > left {
		return right
	}
	return left
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// ====================================================================
// 核心接口 2: 读取邮件
//   GET /api/inbox?account_id=acc_xxx[&alias=xxx@icloud.com][&folder=all][&limit=20][&days=7]
//
//   - folder: all(默认) / inbox / junk
//   - 不传 alias: 返回指定邮件夹最近邮件
//   - 传 alias:   在指定邮件夹中查找发给该 HME 别名的邮件
//
//   读取方式: 账号配置的 MoeMail 转发收件箱 API。
//   Apple Cookie 只管理 HME 别名，不再用于读取 iCloud Mail。
// ====================================================================

func (s *Server) listInbox(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return
	}
	alias := strings.TrimSpace(c.Query("alias"))
	caller := account.NormalizeCaller(firstNonEmptyString(c.Query("caller"), c.Query("client"), c.Query("identity")))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit
	days, _ := strconv.Atoi(c.DefaultQuery("days", "0"))
	folder, err := mail.NormalizeFolder(c.DefaultQuery("folder", mail.FolderAll))
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if alias == "" {
		fail(c, http.StatusBadRequest, "参数缺失: alias；共享转发收件箱必须指定 iCloud 别名")
		return
	}
	if err := s.mgr.EnsureEnabled(accountID); err != nil {
		fail(c, accountErrorStatus(err), err.Error())
		return
	}
	if folder == mail.FolderJunk {
		fail(c, http.StatusBadRequest, "MoeMail 收件 API 不区分垃圾邮件文件夹，请使用 folder=all 或 inbox")
		return
	}
	cacheKey := ""
	cacheKey = inboxCacheKey(accountID, alias, folder, limit, page, days)
	forceRefresh := c.Query("refresh") == "1" || strings.EqualFold(c.Query("refresh"), "true")
	if !forceRefresh {
		if cachedMessages, cachedTotal, found := s.getInboxCache(cacheKey); found {
			s.observeCallerInboxRead(accountID, alias, caller, len(cachedMessages) > 0)
			ok(c, gin.H{
				"account_id":  accountID,
				"alias":       alias,
				"folder":      folder,
				"page":        page,
				"per_page":    limit,
				"count":       len(cachedMessages),
				"total":       cachedTotal,
				"total_pages": totalPages(cachedTotal, limit),
				"messages":    cachedMessages,
				"method":      "receiver_api_cache",
			})
			return
		}
	}
	receiverConfig, err := s.mgr.MailReceiver(accountID)
	if err != nil {
		fail(c, http.StatusFailedDependency, err.Error())
		return
	}
	receiver, err := s.receiverClientFactory(normalizeMailReceiver(receiverConfig))
	if err != nil {
		fail(c, http.StatusFailedDependency, err.Error())
		return
	}
	messages, total, err := receiver.List(c.Request.Context(), alias, limit, offset, days)
	if err != nil {
		fail(c, http.StatusFailedDependency, "读取转发收件箱失败: "+err.Error())
		return
	}
	s.setInboxCache(cacheKey, messages, total)
	s.observeCallerInboxRead(accountID, alias, caller, len(messages) > 0)
	ok(c, gin.H{
		"account_id": accountID, "alias": alias, "folder": folder, "page": page,
		"per_page": limit, "count": len(messages), "total": total,
		"total_pages": totalPages(total, limit), "messages": messages,
		"method": "moemail_api",
	})
}

func (s *Server) getInboxMessage(c *gin.Context) {
	accountID := c.Query("account_id")
	messageID := strings.TrimSpace(c.Query("id"))
	alias := strings.TrimSpace(c.Query("alias"))
	caller := account.NormalizeCaller(firstNonEmptyString(c.Query("caller"), c.Query("client"), c.Query("identity")))
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return
	}
	if messageID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: id")
		return
	}
	if alias == "" {
		fail(c, http.StatusBadRequest, "参数缺失: alias")
		return
	}
	if err := s.mgr.EnsureEnabled(accountID); err != nil {
		fail(c, accountErrorStatus(err), err.Error())
		return
	}
	receiverConfig, err := s.mgr.MailReceiver(accountID)
	if err != nil {
		fail(c, http.StatusFailedDependency, err.Error())
		return
	}
	receiver, err := s.receiverClientFactory(normalizeMailReceiver(receiverConfig))
	if err != nil {
		fail(c, http.StatusFailedDependency, err.Error())
		return
	}
	full, err := receiver.Get(c.Request.Context(), alias, messageID)
	if err != nil {
		fail(c, http.StatusFailedDependency, "读取转发收件箱邮件正文失败: "+err.Error())
		return
	}
	s.observeCallerInboxRead(accountID, alias, caller, full != nil)
	ok(c, gin.H{"account_id": accountID, "message": full, "method": "moemail_api"})
}

// observeCallerInboxRead deliberately never makes a mail read fail: the
// caller's response is already valid, while confirmation bookkeeping can be
// retried by the next caller request or expiry sweep.
func (s *Server) observeCallerInboxRead(accountID, alias, caller string, hasMail bool) {
	if caller == "" || alias == "" {
		return
	}
	if _, err := s.mgr.ObserveCallerInboxRead(accountID, alias, caller, hasMail); err != nil {
		log.Printf("caller tag observation failed account_id=%s alias=%s caller=%s: %v", accountID, alias, caller, err)
	}
}

func totalPages(total, perPage int) int {
	if total <= 0 || perPage <= 0 {
		return 1
	}
	return (total + perPage - 1) / perPage
}

// ====================================================================
// 辅助接口
// ====================================================================

func (s *Server) listAccounts(c *gin.Context) {
	accounts := s.mgr.ListAccounts()
	includeDisabled := c.Query("include_disabled") == "1" || strings.EqualFold(c.Query("include_disabled"), "true")
	if includeDisabled {
		ok(c, accounts)
		return
	}
	enabledAccounts := make([]*account.Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc != nil && acc.Enabled {
			enabledAccounts = append(enabledAccounts, acc)
		}
	}
	ok(c, enabledAccounts)
}

type addAccountReq struct {
	Name    string `json:"name"`
	Cookies string `json:"cookies" binding:"required"`
	Host    string `json:"host"`
	Proxy   string `json:"proxy"` // HTTP/SOCKS5 代理
}

func (s *Server) addAccount(c *gin.Context) {
	var req addAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: cookies 必填 — "+err.Error())
		return
	}
	acc, err := s.mgr.AddAccount(req.Name, req.Cookies, req.Host, req.Proxy)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	// 返回脱敏副本，不能修改 Manager 持有的账号对象。
	publicAccount := *acc
	publicAccount.Cookies = nil
	publicAccount.MailReceiver = nil
	publicAccount.HMEClientID = ""
	c.JSON(http.StatusCreated, apiResp{Success: true, Data: &publicAccount})
}

func (s *Server) removeAccount(c *gin.Context) {
	id := c.Param("id")
	if !s.mgr.RemoveAccount(id) {
		fail(c, http.StatusNotFound, "账号不存在")
		return
	}
	s.clearInboxCache(id)
	ok(c, gin.H{"id": id})
}

type setMailReceiverReq struct {
	Provider                string `json:"provider"`
	BaseURL                 string `json:"base_url"`
	Address                 string `json:"address" binding:"required"`
	MailboxID               string `json:"mailbox_id"`
	APIKey                  string `json:"api_key" binding:"required"`
	CleanupEnabled          *bool  `json:"cleanup_enabled"`
	CleanupRetentionMinutes int    `json:"cleanup_retention_minutes"`
}

func (s *Server) setMailReceiver(c *gin.Context) {
	id := c.Param("id")
	var req setMailReceiverReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: address、api_key 必填 — "+err.Error())
		return
	}
	if provider := strings.TrimSpace(req.Provider); provider != "" && !strings.EqualFold(provider, "moemail") {
		fail(c, http.StatusBadRequest, "转发收件箱仅支持 MoeMail，不接受 provider="+provider)
		return
	}
	previous, _ := s.mgr.MailReceiver(id)
	receiver := normalizeMailReceiver(&account.MailReceiverConfig{
		BaseURL: req.BaseURL, Address: req.Address,
		MailboxID: req.MailboxID, APIKey: req.APIKey,
	})
	if previous != nil {
		receiver.CleanupEnabled = previous.CleanupEnabled
		receiver.CleanupRetentionSeconds = previous.CleanupRetentionSeconds
	}
	if req.CleanupEnabled != nil {
		receiver.CleanupEnabled = *req.CleanupEnabled
	}
	if req.CleanupRetentionMinutes > 0 {
		receiver.CleanupRetentionSeconds = req.CleanupRetentionMinutes * 60
	}
	if err := validateMailReceiver(receiver); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	client, err := s.receiverClientFactory(receiver)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := client.Validate(c.Request.Context()); err != nil {
		fail(c, http.StatusFailedDependency, "转发收件箱验证失败，配置未保存: "+err.Error())
		return
	}
	if err := s.mgr.SetMailReceiver(id, receiver); err != nil {
		if strings.Contains(err.Error(), "账号不存在") {
			fail(c, http.StatusNotFound, err.Error())
		} else {
			fail(c, http.StatusInternalServerError, "保存转发收件箱配置失败: "+err.Error())
		}
		return
	}
	s.clearInboxCache(id)
	public := *receiver
	public.APIKey = ""
	ok(c, gin.H{"id": id, "mail_receiver": public})
}

func (s *Server) cleanupMailReceiver(c *gin.Context) {
	id := c.Param("id")
	receiverConfig, err := s.mgr.MailReceiver(id)
	if err != nil {
		fail(c, accountErrorStatus(err), err.Error())
		return
	}
	receiverConfig = normalizeMailReceiver(receiverConfig)
	if err := validateMailReceiver(receiverConfig); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	retention := time.Duration(receiverConfig.CleanupRetentionSeconds) * time.Second
	if retention <= 0 {
		fail(c, http.StatusConflict, "转发收件箱未启用自动清理或未设置保留期")
		return
	}
	receiver, err := s.receiverClientFactory(receiverConfig)
	if err != nil {
		fail(c, http.StatusFailedDependency, err.Error())
		return
	}
	cursor := s.cleanupCursor(id)
	result, err := cleanupMoEmailReceiver(c.Request.Context(), receiver, retention, cursor, 25)
	if err != nil {
		fail(c, http.StatusFailedDependency, "清理 MoeMail 历史邮件失败: "+err.Error())
		return
	}
	s.setCleanupCursor(id, result.NextCursor)
	s.clearInboxCache(id)
	ok(c, gin.H{
		"id": id, "deleted": result.Deleted, "scanned": result.Scanned,
		"next_cursor": result.NextCursor, "retention_seconds": int64(retention / time.Second),
	})
}

func (s *Server) clearMailReceiver(c *gin.Context) {
	id := c.Param("id")
	if err := s.mgr.SetMailReceiver(id, nil); err != nil {
		if strings.Contains(err.Error(), "账号不存在") {
			fail(c, http.StatusNotFound, err.Error())
		} else {
			fail(c, http.StatusInternalServerError, "清除转发收件箱配置失败: "+err.Error())
		}
		return
	}
	s.clearInboxCache(id)
	ok(c, gin.H{"id": id, "mail_receiver": nil})
}

type updateCookiesReq struct {
	Cookies map[string]string `json:"cookies" binding:"required"`
}

func (s *Server) updateCookies(c *gin.Context) {
	id := c.Param("id")
	var req updateCookiesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: cookies 必填 — "+err.Error())
		return
	}
	if err := s.mgr.UpdateCookies(id, req.Cookies); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, gin.H{"id": id, "cookies_count": len(req.Cookies)})
}

type accountAutoCreateReq struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

func (s *Server) setAccountEnabled(c *gin.Context) {
	id := c.Param("id")
	var req accountAutoCreateReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		fail(c, http.StatusBadRequest, "参数错误: enabled 必须是布尔值")
		return
	}
	if err := s.mgr.SetEnabled(id, *req.Enabled); err != nil {
		if strings.Contains(err.Error(), "账号不存在") {
			fail(c, http.StatusNotFound, err.Error())
		} else {
			fail(c, http.StatusInternalServerError, "保存账号启用开关失败: "+err.Error())
		}
		return
	}
	if !*req.Enabled {
		s.clearInboxCache(id)
	}
	ok(c, gin.H{"id": id, "enabled": *req.Enabled})
}

func (s *Server) setAccountAutoCreate(c *gin.Context) {
	id := c.Param("id")
	var req accountAutoCreateReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		fail(c, http.StatusBadRequest, "参数错误: enabled 必须是布尔值")
		return
	}
	if err := s.mgr.SetAutoCreateEnabled(id, *req.Enabled); err != nil {
		if strings.Contains(err.Error(), "账号不存在") {
			fail(c, http.StatusNotFound, err.Error())
		} else {
			fail(c, http.StatusInternalServerError, "保存账号自动创建开关失败: "+err.Error())
		}
		return
	}
	ok(c, gin.H{"id": id, "enabled": *req.Enabled})
}

func (s *Server) listAliases(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return
	}
	if err := s.mgr.EnsureEnabled(accountID); err != nil {
		fail(c, accountErrorStatus(err), err.Error())
		return
	}
	client, err := s.mgr.HMEClient(accountID, false)
	if err != nil {
		fail(c, accountErrorStatus(err), err.Error())
		return
	}
	aliases, err := client.ListAliases()
	if err != nil {
		_ = s.mgr.SaveCookies(accountID, client.Cookies)
		if isSessionError(err.Error()) {
			s.mgr.MarkLoginRequired(accountID, err)
			fail(c, http.StatusUnauthorized, "iCloud 会话失效,请更新 Cookie: "+err.Error())
		} else {
			fail(c, http.StatusBadGateway, err.Error())
		}
		return
	}
	if err := s.mgr.SaveAliasStats(accountID, aliases, client.Cookies); err != nil {
		fail(c, http.StatusInternalServerError, "保存别名统计失败: "+err.Error())
		return
	}
	aliases = s.mgr.DecorateAliases(accountID, aliases)
	ok(c, gin.H{
		"account_id": accountID,
		"count":      len(aliases),
		"aliases":    aliases,
	})
}

type aliasActionReq struct {
	AccountID string `json:"account_id" binding:"required"`
}

type aliasCallerReleaseReq struct {
	AccountID   string `json:"account_id" binding:"required"`
	AnonymousID string `json:"anonymous_id" binding:"required"`
	Caller      string `json:"caller" binding:"required"`
}

type aliasCallerMarkReq struct {
	AccountID string `json:"account_id" binding:"required"`
	Caller    string `json:"caller" binding:"required"`
}

// markAliasCaller repairs caller bookkeeping for an alias that was already
// registered outside the current allocation flow. It does not change or
// delete the Apple alias; it only persists the local caller tag.
func (s *Server) markAliasCaller(c *gin.Context) {
	anonymousID := strings.TrimSpace(c.Param("id"))
	var req aliasCallerMarkReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 和 caller 必填 — "+err.Error())
		return
	}
	caller := account.NormalizeCaller(req.Caller)
	if anonymousID == "" || strings.TrimSpace(req.AccountID) == "" || caller == "" {
		fail(c, http.StatusBadRequest, "参数错误: account_id、anonymousId 和 caller 必填")
		return
	}
	alias, err := s.mgr.MarkAliasCaller(req.AccountID, anonymousID, caller)
	if err != nil {
		if strings.Contains(err.Error(), "不存在") {
			fail(c, http.StatusNotFound, err.Error())
		} else {
			fail(c, http.StatusInternalServerError, "更新调用方标签失败: "+err.Error())
		}
		return
	}
	ok(c, gin.H{
		"account_id":  strings.TrimSpace(req.AccountID),
		"anonymousId": alias.AnonymousID,
		"email":       alias.Email,
		"caller":      caller,
		"used_by":     alias.UsedBy,
	})
}

// releaseAliasCaller is the generic caller-facing release endpoint. A caller
// uses it after a definite business failure to make a permanently claimed
// alias eligible for that same caller again.
func (s *Server) releaseAliasCaller(c *gin.Context) {
	var req aliasCallerReleaseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id、anonymous_id 和 caller 必填 — "+err.Error())
		return
	}
	s.respondAliasCallerRelease(c, req.AccountID, req.AnonymousID, req.Caller)
}

func (s *Server) removeAliasCaller(c *gin.Context) {
	anonymousID := strings.TrimSpace(c.Param("id"))
	caller := account.NormalizeCaller(c.Param("caller"))
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}
	if anonymousID == "" || caller == "" {
		fail(c, http.StatusBadRequest, "参数错误: anonymousId 和 caller 必填")
		return
	}
	s.respondAliasCallerRelease(c, req.AccountID, anonymousID, caller)
}

func (s *Server) respondAliasCallerRelease(c *gin.Context, accountID, anonymousID, caller string) {
	accountID = strings.TrimSpace(accountID)
	anonymousID = strings.TrimSpace(anonymousID)
	caller = account.NormalizeCaller(caller)
	if accountID == "" || anonymousID == "" || caller == "" {
		fail(c, http.StatusBadRequest, "参数错误: account_id、anonymous_id 和 caller 必填")
		return
	}
	alias, err := s.mgr.RemoveAliasCaller(accountID, anonymousID, caller)
	if err != nil {
		if strings.Contains(err.Error(), "不存在") || strings.Contains(err.Error(), "没有调用方记录") {
			fail(c, http.StatusNotFound, err.Error())
		} else if strings.Contains(err.Error(), "账号已停用") {
			fail(c, http.StatusConflict, err.Error())
		} else {
			fail(c, http.StatusInternalServerError, "删除调用方记录失败: "+err.Error())
		}
		return
	}
	ok(c, gin.H{
		"account_id":  accountID,
		"anonymousId": alias.AnonymousID,
		"email":       alias.Email,
		"caller":      caller,
		"used_by":     alias.UsedBy,
	})
}

func (s *Server) deactivateAlias(c *gin.Context) {
	anonymousID := c.Param("id")
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}

	client, err := s.mgr.HMEClient(req.AccountID, false)
	if err != nil {
		fail(c, accountErrorStatus(err), err.Error())
		return
	}

	success, err := client.DeactivateHME(anonymousID)
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
	if err != nil {
		fail(c, http.StatusBadGateway, "停用失败: "+err.Error())
		return
	}
	if success {
		if statsErr := s.mgr.SetAliasActive(req.AccountID, anonymousID, false); statsErr != nil {
			log.Printf("保存别名停用状态失败 account_id=%s anonymous_id=%s: %v", req.AccountID, anonymousID, statsErr)
		}
	}
	ok(c, gin.H{"anonymous_id": anonymousID, "success": success})
}

func (s *Server) reactivateAlias(c *gin.Context) {
	anonymousID := c.Param("id")
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}

	client, err := s.mgr.HMEClient(req.AccountID, false)
	if err != nil {
		fail(c, accountErrorStatus(err), err.Error())
		return
	}

	success, err := client.ReactivateHME(anonymousID)
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
	if err != nil {
		fail(c, http.StatusBadGateway, "激活失败: "+err.Error())
		return
	}
	if success {
		if statsErr := s.mgr.SetAliasActive(req.AccountID, anonymousID, true); statsErr != nil {
			log.Printf("保存别名激活状态失败 account_id=%s anonymous_id=%s: %v", req.AccountID, anonymousID, statsErr)
		}
	}
	ok(c, gin.H{"anonymous_id": anonymousID, "success": success})
}

func (s *Server) deleteAlias(c *gin.Context) {
	anonymousID := c.Param("id")
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}

	client, err := s.mgr.HMEClient(req.AccountID, false)
	if err != nil {
		fail(c, accountErrorStatus(err), err.Error())
		return
	}

	if err := client.Delete(anonymousID); err != nil {
		_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
		fail(c, http.StatusBadGateway, "删除失败: "+err.Error())
		return
	}
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
	if statsErr := s.mgr.MarkAliasDeleted(req.AccountID, anonymousID); statsErr != nil {
		log.Printf("保存别名删除状态失败 account_id=%s anonymous_id=%s: %v", req.AccountID, anonymousID, statsErr)
	}
	ok(c, gin.H{"anonymous_id": anonymousID})
}

// isSessionError 判断错误是否由会话失效引起。
func isSessionError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "401") || strings.Contains(m, "403") ||
		strings.Contains(m, "session") || strings.Contains(m, "cookie") ||
		strings.Contains(m, "unauthorized") || strings.Contains(m, "认证") ||
		strings.Contains(m, "会话校验失败")
}

func accountErrorStatus(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	if strings.Contains(err.Error(), "账号不存在") {
		return http.StatusNotFound
	}
	if strings.Contains(err.Error(), "账号已停用") {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// reloadConfig 重新加载 accounts.json 配置文件。
func (s *Server) reloadConfig(c *gin.Context) {
	if err := s.mgr.Reload(); err != nil {
		fail(c, http.StatusInternalServerError, "重新加载配置失败: "+err.Error())
		return
	}
	s.clearInboxCache("")
	ok(c, gin.H{"message": "配置已重新加载"})
}
