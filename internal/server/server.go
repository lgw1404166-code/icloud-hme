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
	imapMu                sync.Mutex
	imapSessions          map[string]*imapSession
	inboxCache            inboxCacheStore
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
		imapSessions:          make(map[string]*imapSession),
		inboxCache:            inboxCacheStore{entries: make(map[string]inboxCacheEntry)},
	}
	s.r = gin.Default() // 自带 Logger + Recovery 中间件
	s.register()
	return s
}

// Run 启动 HTTP 服务。
func (s *Server) Run(addr string) error {
	s.warmIMAPSessions()
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
		api.POST("/accounts/:id/password", s.setAppPassword)
		api.PUT("/accounts/:id/cookies", s.updateCookies)

		// ===== 核心接口 1: 创建邮箱 =====
		api.POST("/create", s.createAlias)

		// ===== 核心接口 2: 读取邮件 =====
		api.GET("/inbox", s.listInbox)
		api.GET("/inbox/message", s.getInboxMessage)

		// ===== 别名管理 =====
		api.GET("/aliases", s.listAliases)
		api.POST("/aliases/:id/deactivate", s.deactivateAlias)
		api.POST("/aliases/:id/reactivate", s.reactivateAlias)
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
	accountID, status, err := s.resolveCreateAccountID(req.AccountID)
	if err != nil {
		fail(c, status, err.Error())
		return
	}
	req.AccountID = accountID

	caller := account.NormalizeCaller(firstNonEmptyString(req.Caller, req.Client, req.Identity))
	if caller == "" {
		fail(c, http.StatusBadRequest, "参数缺失: caller（调用方身份字符串，例如 chatgpt、moxt）")
		return
	}

	alias, err := s.mgr.AcquireAlias(req.AccountID, caller)
	if err != nil {
		// 本地池没有可领别名时，先同步一次 Apple 侧列表，避免刚启动时本地记录为空。
		if syncErr := s.syncAliases(req.AccountID); syncErr != nil {
			if isSessionError(syncErr.Error()) {
				s.mgr.MarkLoginRequired(req.AccountID, syncErr)
				fail(c, http.StatusUnauthorized, "Apple 登录态已过期，请在管理页面重新登录并更新 Cookie: "+syncErr.Error())
				return
			}
			fail(c, http.StatusConflict, err.Error()+"；同步 Apple 别名列表失败: "+syncErr.Error())
			return
		}
		alias, err = s.mgr.AcquireAlias(req.AccountID, caller)
		if err != nil {
			fail(c, http.StatusConflict, err.Error())
			return
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
		"account_id":  req.AccountID,
	})
}

func (s *Server) syncAliases(accountID string) error {
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
		return accountID, 0, nil
	}
	accounts := s.mgr.ListAccounts()
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
//   认证优先级: IMAP (App Password) 优先 > Web API (Cookie) 回退
//   - IMAP: 支持服务端按收件人搜索 (FindByRecipient)
//   - Web API: 不支持收件人搜索,拉取收件箱后本地按别名过滤 (FindByAlias)
// ====================================================================

func (s *Server) listInbox(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return
	}
	alias := strings.TrimSpace(c.Query("alias"))
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
	cacheKey := ""
	if alias != "" {
		cacheKey = inboxCacheKey(accountID, alias, folder, limit, page, days)
		forceRefresh := c.Query("refresh") == "1" || strings.EqualFold(c.Query("refresh"), "true")
		if !forceRefresh {
			if cachedMessages, cachedTotal, found := s.getInboxCache(cacheKey); found {
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
					"method":      "imap_cache",
				})
				return
			}
		}
	}

	// 优先使用 IMAP (App Password 认证)
	var messages []mail.Message
	var total int
	var imapErr error
	if alias != "" && folder == mail.FolderAll {
		messages, total, imapErr = s.findAliasInAllFolders(accountID, alias, limit, offset, days)
	} else {
		slot := "default"
		if alias != "" {
			slot = folder
		}
		imapErr = s.withMailClientSlot(accountID, slot, func(mc *mail.Client) error {
			if alias != "" {
				var queryErr error
				messages, total, queryErr = mc.FindByRecipientPage(alias, folder, limit, offset, days)
				return queryErr
			}
			fetched, queryErr := mc.ListMessages(folder, offset+limit, days)
			if queryErr != nil {
				return queryErr
			}
			total = len(fetched)
			messages = pageMessages(fetched, offset, limit)
			return nil
		})
	}
	if imapErr == nil {
		if cacheKey != "" {
			s.setInboxCache(cacheKey, messages, total)
		}
		ok(c, gin.H{
			"account_id":  accountID,
			"alias":       alias,
			"folder":      folder,
			"page":        page,
			"per_page":    limit,
			"count":       len(messages),
			"total":       total,
			"total_pages": totalPages(total, limit),
			"messages":    messages,
			"method":      "imap",
		})
		return
	}
	if folder != mail.FolderInbox {
		message := "查询全部邮件和垃圾邮件需要可用的 iCloud IMAP App Password"
		if imapErr != nil {
			message += ": " + imapErr.Error()
		}
		fail(c, http.StatusFailedDependency, message)
		return
	}

	// Web API 只支持 INBOX，因此仅在 folder=inbox 时回退。
	wmc, err := s.mgr.WebMailClient(accountID)
	if err != nil {
		fail(c, http.StatusFailedDependency, inboxClientError(imapErr, err))
		return
	}

	if alias != "" {
		messages, err := wmc.FindByAlias(alias, offset+limit)
		if err != nil {
			fail(c, http.StatusFailedDependency, inboxClientError(imapErr, err))
			return
		}
		setMessagesFolder(messages, mail.FolderInbox)
		total := len(messages)
		messages = pageMessages(messages, offset, limit)
		ok(c, gin.H{
			"account_id":  accountID,
			"alias":       alias,
			"folder":      folder,
			"page":        page,
			"per_page":    limit,
			"count":       len(messages),
			"total":       total,
			"total_pages": totalPages(total, limit),
			"messages":    messages,
			"method":      "web_api",
		})
	} else {
		messages, err := wmc.ListInbox(offset + limit)
		if err != nil {
			fail(c, http.StatusFailedDependency, inboxClientError(imapErr, err))
			return
		}
		setMessagesFolder(messages, mail.FolderInbox)
		total := len(messages)
		messages = pageMessages(messages, offset, limit)
		ok(c, gin.H{
			"account_id":  accountID,
			"folder":      folder,
			"page":        page,
			"per_page":    limit,
			"count":       len(messages),
			"total":       total,
			"total_pages": totalPages(total, limit),
			"messages":    messages,
			"method":      "web_api",
		})
	}
}

func (s *Server) getInboxMessage(c *gin.Context) {
	accountID := c.Query("account_id")
	messageID := strings.TrimSpace(c.Query("id"))
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return
	}
	if messageID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: id")
		return
	}

	var full *mail.FullMessage
	slot := strings.ToLower(strings.TrimSpace(strings.SplitN(messageID, ":", 2)[0]))
	if slot != mail.FolderInbox && slot != mail.FolderJunk {
		slot = "default"
	}
	imapErr := s.withMailClientSlot(accountID, slot, func(mc *mail.Client) error {
		var fetchErr error
		full, fetchErr = mc.GetFullByID(messageID)
		return fetchErr
	})
	if imapErr == nil {
		ok(c, gin.H{
			"account_id": accountID,
			"message":    full,
			"method":     "imap",
		})
		return
	}

	wmc, webErr := s.mgr.WebMailClient(accountID)
	if webErr == nil {
		full, err := wmc.GetFull(messageID)
		if err == nil {
			ok(c, gin.H{
				"account_id": accountID,
				"message":    full,
				"method":     "web_api",
			})
			return
		}
		webErr = err
	}
	fail(c, http.StatusFailedDependency, inboxClientError(imapErr, webErr))
}

func setMessagesFolder(messages []mail.Message, folder string) {
	for i := range messages {
		messages[i].Folder = folder
	}
}

func pageMessages(messages []mail.Message, offset, limit int) []mail.Message {
	if offset >= len(messages) || limit <= 0 {
		return []mail.Message{}
	}
	end := offset + limit
	if end > len(messages) {
		end = len(messages)
	}
	return messages[offset:end]
}

func totalPages(total, perPage int) int {
	if total <= 0 || perPage <= 0 {
		return 1
	}
	return (total + perPage - 1) / perPage
}

func inboxClientError(imapErr, webErr error) string {
	if imapErr != nil {
		message := strings.ToLower(imapErr.Error())
		if strings.Contains(message, "app") && strings.Contains(message, "密码") {
			return "当前账号未设置或未能使用 App 专用密码，且 Cookie Web 邮件接口不可用。请在账号列表点击钥匙图标设置 App Password 后重试。"
		}
	}
	if webErr != nil {
		return "读取邮件失败: " + webErr.Error()
	}
	if imapErr != nil {
		return "读取邮件失败: " + imapErr.Error()
	}
	return "读取邮件失败: 未知错误"
}

// ====================================================================
// 辅助接口
// ====================================================================

func (s *Server) listAccounts(c *gin.Context) {
	ok(c, s.mgr.ListAccounts())
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
	publicAccount.AppPassword = ""
	publicAccount.HMEClientID = ""
	c.JSON(http.StatusCreated, apiResp{Success: true, Data: &publicAccount})
}

func (s *Server) removeAccount(c *gin.Context) {
	id := c.Param("id")
	if !s.mgr.RemoveAccount(id) {
		fail(c, http.StatusNotFound, "账号不存在")
		return
	}
	s.closeIMAPSession(id)
	ok(c, gin.H{"id": id})
}

type setPwdReq struct {
	ICloudEmail string `json:"icloud_email" binding:"required"`
	AppPassword string `json:"app_password" binding:"required"`
}

func (s *Server) setAppPassword(c *gin.Context) {
	id := c.Param("id")
	var req setPwdReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: icloud_email, app_password 必填 — "+err.Error())
		return
	}
	if err := s.mgr.SetAppPassword(id, req.ICloudEmail, req.AppPassword); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	s.closeIMAPSession(id)
	ok(c, gin.H{"id": id, "icloud_email": req.ICloudEmail})
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

func (s *Server) listAliases(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return
	}
	client, err := s.mgr.HMEClient(accountID, false)
	if err != nil {
		fail(c, http.StatusNotFound, err.Error())
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

func (s *Server) deactivateAlias(c *gin.Context) {
	anonymousID := c.Param("id")
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: account_id 必填 — "+err.Error())
		return
	}

	client, err := s.mgr.HMEClient(req.AccountID, false)
	if err != nil {
		fail(c, http.StatusNotFound, err.Error())
		return
	}

	success, err := client.DeactivateHME(anonymousID)
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
	if err != nil {
		fail(c, http.StatusBadGateway, "停用失败: "+err.Error())
		return
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
		fail(c, http.StatusNotFound, err.Error())
		return
	}

	success, err := client.ReactivateHME(anonymousID)
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
	if err != nil {
		fail(c, http.StatusBadGateway, "激活失败: "+err.Error())
		return
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
		fail(c, http.StatusNotFound, err.Error())
		return
	}

	if err := client.Delete(anonymousID); err != nil {
		_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
		fail(c, http.StatusBadGateway, "删除失败: "+err.Error())
		return
	}
	_ = s.mgr.SaveCookies(req.AccountID, client.Cookies)
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

// reloadConfig 重新加载 accounts.json 配置文件。
func (s *Server) reloadConfig(c *gin.Context) {
	if err := s.mgr.Reload(); err != nil {
		fail(c, http.StatusInternalServerError, "重新加载配置失败: "+err.Error())
		return
	}
	s.closeAllIMAPSessions()
	ok(c, gin.H{"message": "配置已重新加载"})
}
