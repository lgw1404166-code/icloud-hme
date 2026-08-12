package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

func TestInboxClientErrorExplainsAppPasswordFallback(t *testing.T) {
	message := inboxClientError(errors.New("账号未设置 App 专用密码"), errors.New("HTTP 400"))
	if !strings.Contains(message, "App Password") || !strings.Contains(message, "钥匙图标") {
		t.Fatalf("message = %q, want App Password guidance", message)
	}
}

func TestAPIKeyAuthEndpoint(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false, "test-api-key")

	for _, tt := range []struct {
		name   string
		key    string
		status int
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "wrong", key: "wrong", status: http.StatusUnauthorized},
		{name: "valid", key: "test-api-key", status: http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/_internal/api-key-auth", nil)
			if tt.key != "" {
				req.Header.Set("X-API-Key", tt.key)
			}
			res := httptest.NewRecorder()
			srv.Handler().ServeHTTP(res, req)
			if res.Code != tt.status {
				t.Fatalf("status = %d, want %d", res.Code, tt.status)
			}
		})
	}
}

func TestReloginConfigEndpointDefaultsToManual(t *testing.T) {
	t.Setenv("ICLOUD_HME_RELOGIN_ENABLED", "")
	t.Setenv("ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN", "")
	t.Setenv("ICLOUD_HME_OTP_WEBHOOK_TOKEN", "")
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodGet, "/api/relogin/config", nil)
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	body := res.Body.String()
	if !strings.Contains(body, `"enabled":false`) || !strings.Contains(body, `"manual_login_url"`) {
		t.Fatalf("unexpected config response: %s", body)
	}
}

func TestAliasPoolConfigEndpointReadsEnvironment(t *testing.T) {
	t.Setenv("ICLOUD_HME_AUTO_CREATE", "false")
	t.Setenv("ICLOUD_HME_AUTO_CREATE_PER_HOUR", "12")
	t.Setenv("ICLOUD_HME_AUTO_CREATE_INTERVAL", "5m")
	t.Setenv("ICLOUD_HME_AUTO_CREATE_MAX_TOTAL", "250")
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/alias-pool/config", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	for _, want := range []string{`"enabled":false`, `"per_hour":12`, `"interval_seconds":300`, `"max_total":250`} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("response %s does not contain %s", res.Body.String(), want)
		}
	}
}

func TestUpdateAliasPoolConfigPersistsAndApplies(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	t.Setenv("ICLOUD_HME_ENV_FILE", envFile)
	t.Setenv("ICLOUD_HME_AUTO_CREATE", "true")
	t.Setenv("ICLOUD_HME_AUTO_CREATE_PER_HOUR", "10")
	t.Setenv("ICLOUD_HME_AUTO_CREATE_INTERVAL", "")
	t.Setenv("ICLOUD_HME_AUTO_CREATE_MAX_TOTAL", "0")
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	body := `{"enabled":false,"per_hour":15,"interval":"4m","max_total":300,"label":"pool","note":"managed"}`
	req := httptest.NewRequest(http.MethodPut, "/api/alias-pool/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	for _, want := range []string{`"enabled":false`, `"per_hour":15`, `"interval_seconds":240`, `"max_total":300`} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("response %s does not contain %s", res.Body.String(), want)
		}
	}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ICLOUD_HME_AUTO_CREATE=false",
		"ICLOUD_HME_AUTO_CREATE_PER_HOUR=15",
		"ICLOUD_HME_AUTO_CREATE_INTERVAL=4m",
		"ICLOUD_HME_AUTO_CREATE_MAX_TOTAL=300",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf(".env %q does not contain %s", raw, want)
		}
	}
}

func TestReloginConfigEndpointReadsUnifiedOTPSettings(t *testing.T) {
	t.Setenv("ICLOUD_HME_RELOGIN_ENABLED", "1")
	t.Setenv("ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN", "secret")
	t.Setenv("ICLOUD_HME_RELOGIN_OTP_TTL", "7m")
	t.Setenv("ICLOUD_HME_RELOGIN_APPLE_ID", "user@example.com")
	t.Setenv("ICLOUD_HME_RELOGIN_APPLE_PASSWORD", "password")
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodGet, "/api/relogin/config", nil)
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	body := res.Body.String()
	for _, want := range []string{`"enabled":true`, `"webhook_enabled":true`, `"ttl_seconds":420`, `"apple_id_configured":true`, `"apple_password_configured":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response %s does not contain %s", body, want)
		}
	}
	if strings.Contains(body, "secret") || strings.Contains(body, "user@example.com") || strings.Contains(body, `"password"`) {
		t.Fatalf("response leaked relogin secrets: %s", body)
	}
}

func TestUpdateReloginConfigEndpointPersistsDotEnv(t *testing.T) {
	envFile := t.TempDir() + "/.env"
	t.Setenv("ICLOUD_HME_ENV_FILE", envFile)
	t.Setenv("ICLOUD_HME_RELOGIN_ENABLED", "")
	t.Setenv("ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN", "")
	t.Setenv("ICLOUD_HME_RELOGIN_APPLE_ID", "")
	t.Setenv("ICLOUD_HME_RELOGIN_APPLE_PASSWORD", "")
	t.Setenv("ICLOUD_HME_OTP_WEBHOOK_TOKEN", "")
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)

	body := `{
		"enabled": true,
		"mode": "apple_protocol_sms",
		"manual_login_url": "https://account.apple.com/account/manage/section/privacy",
		"protocol_timeout": "4m",
		"apple_id": "user@example.com",
		"apple_password": "secret-password",
		"otp": {
			"provider": "smsgate_webhook",
			"webhook_token": "webhook-secret",
			"ttl": "6m"
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/relogin/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	responseBody := res.Body.String()
	for _, leaked := range []string{"secret-password", "webhook-secret", "user@example.com"} {
		if strings.Contains(responseBody, leaked) {
			t.Fatalf("response leaked secret/config value %q: %s", leaked, responseBody)
		}
	}
	for _, want := range []string{`"enabled":true`, `"apple_id_configured":true`, `"apple_password_configured":true`, `"webhook_enabled":true`, `"ttl_seconds":360`} {
		if !strings.Contains(responseBody, want) {
			t.Fatalf("response %s does not contain %s", responseBody, want)
		}
	}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	envText := string(raw)
	for _, want := range []string{
		"ICLOUD_HME_RELOGIN_ENABLED=true",
		"ICLOUD_HME_RELOGIN_APPLE_ID=user@example.com",
		"ICLOUD_HME_RELOGIN_APPLE_PASSWORD=secret-password",
		"ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN=webhook-secret",
		"ICLOUD_HME_RELOGIN_OTP_TTL=6m0s",
	} {
		if !strings.Contains(envText, want) {
			t.Fatalf(".env %q does not contain %s", envText, want)
		}
	}
}

func TestWebConsoleAndAPI(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)

	tests := []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/", contentType: "text/html", contains: "iCloud HME"},
		{path: "/assets/app.css", contentType: "text/css", contains: ".app-shell"},
		{path: "/assets/app.js", contentType: "text/javascript", contains: "refreshAccounts"},
		{path: "/assets/lucide.min.js", contentType: "text/javascript", contains: "createIcons"},
		{path: "/api/accounts", contentType: "application/json", contains: `"success":true`},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			res := httptest.NewRecorder()
			srv.Handler().ServeHTTP(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.Code)
			}
			if !strings.Contains(res.Header().Get("Content-Type"), tt.contentType) {
				t.Fatalf("content-type = %q, want %q", res.Header().Get("Content-Type"), tt.contentType)
			}
			if !strings.Contains(res.Body.String(), tt.contains) {
				t.Fatalf("body does not contain %q", tt.contains)
			}
		})
	}
}

func TestWebConsoleUsesUnifiedThreeColumnInbox(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	body := res.Body.String()
	for _, want := range []string{`id="mailbox-list"`, `id="mail-list"`, `id="mail-detail"`, `id="mail-pagination"`, `id="inbox-email-search"`, `id="alias-pool-config-panel"`, `<th>所属账号</th>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("web console does not contain %s", want)
		}
	}
	if strings.Contains(body, `id="global-account-select"`) {
		t.Fatal("web console still contains the global account selector")
	}
	if strings.Contains(body, `id="inbox-filter-form"`) || strings.Contains(body, `data-inbox-folder`) {
		t.Fatal("web console still contains inbox filter controls")
	}
}

func TestInboxPaginationHelpers(t *testing.T) {
	messages := []mail.Message{{ID: "1"}, {ID: "2"}, {ID: "3"}}
	page := pageMessages(messages, 2, 20)
	if len(page) != 1 || page[0].ID != "3" {
		t.Fatalf("unexpected page: %#v", page)
	}
	for _, test := range []struct{ total, perPage, want int }{
		{0, 20, 1}, {1, 20, 1}, {20, 20, 1}, {21, 20, 2}, {41, 20, 3},
	} {
		if got := totalPages(test.total, test.perPage); got != test.want {
			t.Fatalf("totalPages(%d, %d) = %d, want %d", test.total, test.perPage, got, test.want)
		}
	}
}

func TestInboxCacheCopiesAndClearsByAccount(t *testing.T) {
	srv := &Server{inboxCache: inboxCacheStore{entries: make(map[string]inboxCacheEntry)}}
	key := inboxCacheKey("acc_one", "Alias@icloud.com", mail.FolderAll, 20, 1, 0)
	srv.setInboxCache(key, []mail.Message{{ID: "inbox:1"}}, 3)
	messages, total, found := srv.getInboxCache(key)
	if !found || total != 3 || len(messages) != 1 || messages[0].ID != "inbox:1" {
		t.Fatalf("unexpected cache result found=%v total=%d messages=%#v", found, total, messages)
	}
	messages[0].ID = "changed"
	cachedAgain, _, _ := srv.getInboxCache(key)
	if cachedAgain[0].ID != "inbox:1" {
		t.Fatal("cache returned its internal message slice")
	}
	srv.clearInboxCache("acc_one")
	if _, _, found := srv.getInboxCache(key); found {
		t.Fatal("account cache was not cleared")
	}
}

func TestAliasPoolLogsUsePrimaryEmailAndNewestFirst(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(`{
  "accounts": {
    "acc_test": {
      "id": "acc_test",
      "icloud_email": "primary@icloud.com",
      "real_email": "login@example.com"
    }
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	srv.recordAliasPoolLog("error", "acc_test", "", "first")
	srv.recordAliasPoolLog("success", "acc_test", "new@icloud.com", "second")

	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/alias-pool/logs?limit=1", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, want := range []string{`"account_email":"primary@icloud.com"`, `"email":"new@icloud.com"`, `"message":"second"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response %s does not contain %s", body, want)
		}
	}
	if strings.Contains(body, `"message":"first"`) {
		t.Fatalf("limit was not applied: %s", body)
	}
}

func TestAccountsEndpointRedactsCredentials(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(dataDir+"/accounts.json", []byte(`{
  "accounts": {
    "acc_test": {
      "id": "acc_test",
      "name": "Test",
      "cookies": {"session": "cookie-secret"},
      "app_password": "app-password-secret",
      "hme_client_id": "00000000-0000-4000-8000-000000000001"
    }
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	srv := New(mgr, false)
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if strings.Contains(res.Body.String(), "cookie-secret") || strings.Contains(res.Body.String(), "app-password-secret") || strings.Contains(res.Body.String(), "00000000-0000-4000-8000-000000000001") {
		t.Fatalf("response leaked credentials: %s", res.Body.String())
	}
}

func TestCreateGateSerializesAndAppliesFailureCooldown(t *testing.T) {
	now := time.Date(2026, time.August, 5, 0, 0, 0, 0, time.UTC)
	srv := &Server{
		createAttempts:        make(map[string]*createAttemptState),
		now:                   func() time.Time { return now },
		createMinInterval:     0,
		createFailureCooldown: 10 * time.Minute,
	}

	if _, allowed := srv.beginCreate("acc_test"); !allowed {
		t.Fatal("first create attempt was rejected")
	}
	if retry, allowed := srv.beginCreate("acc_test"); allowed || retry != time.Second {
		t.Fatalf("concurrent attempt allowed=%v retry=%s", allowed, retry)
	}
	srv.finishCreate("acc_test", 0)
	if _, allowed := srv.beginCreate("acc_test"); !allowed {
		t.Fatal("attempt after successful completion was rejected")
	}
	srv.finishCreate("acc_test", 10*time.Minute)
	if retry, allowed := srv.beginCreate("acc_test"); allowed || retry != 10*time.Minute {
		t.Fatalf("failure cooldown allowed=%v retry=%s", allowed, retry)
	}
}

func TestClassifyCreateErrorPreservesUpstreamStatus(t *testing.T) {
	srv := &Server{createFailureCooldown: 10 * time.Minute}

	upstream := &hme.HTTPError{StatusCode: http.StatusTooManyRequests, Body: `{"error":"limited"}`, RetryAfter: 15 * time.Minute}
	code, message, gotHTTPError, cooldown := srv.classifyCreateError(fmt.Errorf("generate: %w", upstream))
	if code != http.StatusTooManyRequests || gotHTTPError != upstream || cooldown != 15*time.Minute {
		t.Fatalf("429 mapping = code %d cooldown %s error %#v", code, cooldown, gotHTTPError)
	}
	if !strings.Contains(message, "HTTP 429") {
		t.Fatalf("429 message = %q", message)
	}

	upstream = &hme.HTTPError{StatusCode: http.StatusServiceUnavailable, Body: "unavailable"}
	code, _, gotHTTPError, cooldown = srv.classifyCreateError(fmt.Errorf("reserve: %w", upstream))
	if code != http.StatusFailedDependency || gotHTTPError != upstream || cooldown != 10*time.Minute {
		t.Fatalf("503 mapping = code %d cooldown %s error %#v", code, cooldown, gotHTTPError)
	}

	upstream = &hme.HTTPError{StatusCode: http.StatusUnauthorized, Body: "unauthorized"}
	code, _, _, cooldown = srv.classifyCreateError(fmt.Errorf("validate: %w", upstream))
	if code != http.StatusUnauthorized || cooldown != 0 {
		t.Fatalf("401 mapping = code %d cooldown %s", code, cooldown)
	}

	upstream = &hme.HTTPError{StatusCode: http.StatusPreconditionFailed, Body: `{"ineligibilityReason":"rate_limit_exceeded"}`}
	code, _, gotHTTPError, cooldown = srv.classifyCreateError(fmt.Errorf("account add: %w", upstream))
	if code != http.StatusTooManyRequests || gotHTTPError != upstream || cooldown != 10*time.Minute {
		t.Fatalf("412 rate limit mapping = code %d cooldown %s error %#v", code, cooldown, gotHTTPError)
	}
}

func TestAddAccountRequiresCookies(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewBufferString(`{"name":"without-cookie"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

func TestCreateEndpointRequiresCaller(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodPost, "/api/create", bytes.NewBufferString(`{"account_id":"acc_test"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	if !strings.Contains(res.Body.String(), "caller") {
		t.Fatalf("response does not mention caller: %s", res.Body.String())
	}
}

func TestResolveCreateAccountID(t *testing.T) {
	emptyManager, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	emptyServer := New(emptyManager, false)
	if _, status, err := emptyServer.resolveCreateAccountID(""); err == nil || status != http.StatusNotFound {
		t.Fatalf("empty account resolution status=%d err=%v", status, err)
	}
	if got, status, err := emptyServer.resolveCreateAccountID(" explicit "); err != nil || status != 0 || got != "explicit" {
		t.Fatalf("explicit account resolution got=%q status=%d err=%v", got, status, err)
	}

	dataDir := t.TempDir()
	if err := os.WriteFile(dataDir+"/accounts.json", []byte(`{
  "accounts": {
    "acc_only": {"id":"acc_only","name":"Only","cookies":{"session":"value"}}
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	oneManager, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	oneServer := New(oneManager, false)
	if got, status, err := oneServer.resolveCreateAccountID(""); err != nil || status != 0 || got != "acc_only" {
		t.Fatalf("single account resolution got=%q status=%d err=%v", got, status, err)
	}
}

func TestInboxRejectsUnsupportedFolder(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox?account_id=acc_test&folder=archive", nil)
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	if !strings.Contains(res.Body.String(), "all、inbox、junk") {
		t.Fatalf("response does not explain supported folders: %s", res.Body.String())
	}
}
