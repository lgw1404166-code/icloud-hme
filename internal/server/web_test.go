package server

import (
	"bytes"
	"fmt"
	"net"
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

func TestWebConsoleHasRemovableAliasCallerTags(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)

	for _, tt := range []struct {
		path string
		want string
	}{
		{path: "/assets/app.js", want: `data-action="remove-alias-caller"`},
		{path: "/assets/app.js", want: `data-action="toggle-account-enabled"`},
		{path: "/assets/app.js", want: `/api/accounts?include_disabled=true`},
		{path: "/assets/app.js", want: `data-action="toggle-account-auto-create"`},
		{path: "/assets/app.js", want: `data-action="set-mail-receiver"`},
		{path: "/assets/app.js", want: `/mail-receiver`},
		{path: "/assets/app.css", want: ".alias-caller-tag"},
	} {
		res := httptest.NewRecorder()
		srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, tt.path, nil))
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), tt.want) {
			t.Fatalf("GET %s status = %d, body missing %q", tt.path, res.Code, tt.want)
		}
	}
}

func TestWebConsoleDoesNotExposeLegacyAppPasswordControl(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	for _, forbidden := range []string{`data-action="set-password"`, `openPasswordEditor`, `/accounts/${encodeURIComponent(account.id)}/password`} {
		if strings.Contains(res.Body.String(), forbidden) {
			t.Fatalf("legacy App Password UI remained: %q", forbidden)
		}
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
	for _, want := range []string{`"account_email":"login@example.com"`, `"email":"new@icloud.com"`, `"message":"second"`} {
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
	      "mail_receiver": {"address":"relay@example.com","api_key":"api-secret"},
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
	if strings.Contains(res.Body.String(), "cookie-secret") || strings.Contains(res.Body.String(), "api-secret") || strings.Contains(res.Body.String(), "00000000-0000-4000-8000-000000000001") {
		t.Fatalf("response leaked credentials: %s", res.Body.String())
	}
}

func TestAccountsEndpointExcludesDisabledAccountsByDefault(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(`{
  "accounts": {
    "acc_enabled": {"id":"acc_enabled", "name":"Enabled", "enabled":true},
    "acc_disabled": {"id":"acc_disabled", "name":"Disabled", "enabled":false}
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)

	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "acc_enabled") || strings.Contains(res.Body.String(), "acc_disabled") {
		t.Fatalf("default accounts status = %d, body = %s", res.Code, res.Body.String())
	}

	res = httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/accounts?include_disabled=true", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "acc_enabled") || !strings.Contains(res.Body.String(), "acc_disabled") {
		t.Fatalf("management accounts status = %d, body = %s", res.Code, res.Body.String())
	}
}

func TestAccountAutoCreateEndpoint(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(`{
  "accounts": {
    "acc_test": {"id": "acc_test", "name": "Test"}
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)

	req := httptest.NewRequest(http.MethodPut, "/api/accounts/acc_test/auto-create", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"enabled":false`) {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}

	res = httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"auto_create_enabled":false`) {
		t.Fatalf("accounts status = %d, body = %s", res.Code, res.Body.String())
	}
}

func TestAccountEnabledEndpointAndCreateResolution(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(`{
  "accounts": {
    "acc_test": {"id":"acc_test", "enabled":false, "alias_usages":{"one@icloud.com":{"email":"one@icloud.com","active":true}}}
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	if _, status, err := srv.resolveCreateAccountID(""); err == nil || status != http.StatusNotFound {
		t.Fatalf("disabled default resolution status=%d err=%v", status, err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/create", strings.NewReader(`{"account_id":"acc_test","caller":"chatgpt"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "账号已停用") {
		t.Fatalf("disabled create status = %d, body = %s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/accounts/acc_test/enabled", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"enabled":true`) {
		t.Fatalf("enable status = %d, body = %s", res.Code, res.Body.String())
	}
	if got, status, err := srv.resolveCreateAccountID(""); err != nil || status != 0 || got != "acc_test" {
		t.Fatalf("enabled default resolution got=%q status=%d err=%v", got, status, err)
	}
}

func TestAliasPoolSkipsDisabledAccount(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(`{
  "accounts": {
    "acc_test": {"id": "acc_test", "auto_create_enabled": false}
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	srv.runAliasPoolTick(AliasPoolConfig{Enabled: true, PerHour: 1})
	if _, exists := srv.createAttempts["acc_test"]; exists {
		t.Fatal("disabled account entered the create gate")
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

func TestCreateEndpointAllocatesAcrossEnabledAccounts(t *testing.T) {
	dataDir := t.TempDir()
	raw := `{"accounts":{
  "acc_exhausted":{"id":"acc_exhausted","enabled":true,"alias_usages":{"used@icloud.com":{"email":"used@icloud.com","active":true,"used_by":{"lovart":"2026-09-01T00:00:00Z"}}}},
  "acc_available":{"id":"acc_available","enabled":true,"alias_usages":{"available@icloud.com":{"email":"available@icloud.com","active":true}}},
  "acc_disabled":{"id":"acc_disabled","enabled":false,"alias_usages":{"disabled@icloud.com":{"email":"disabled@icloud.com","active":true}}}
}}`
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodPost, "/api/create", bytes.NewBufferString(`{"caller":"lovart"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	for _, want := range []string{`"account_id":"acc_available"`, `"email":"available@icloud.com"`} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("response %s does not contain %s", res.Body.String(), want)
		}
	}
}

func TestCreateEndpointReturnsPromptlyWhenGlobalPoolIsEmpty(t *testing.T) {
	// A blocked proxy makes an accidental Apple-side refresh deterministic. The
	// allocation endpoint must still return immediately when the local pool has
	// no alias for its caller.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	releaseProxy := make(chan struct{})
	defer close(releaseProxy)
	proxyConnected := make(chan struct{}, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		proxyConnected <- struct{}{}
		<-releaseProxy
	}()

	dataDir := t.TempDir()
	raw := fmt.Sprintf(`{"accounts":{"acc_test":{"id":"acc_test","enabled":true,"proxy":%q,"cookies":{"session":"test"},"alias_usages":{"used@icloud.com":{"email":"used@icloud.com","active":true,"used_by":{"lovart":"2026-09-01T00:00:00Z"}}}}}}`, "http://"+listener.Addr().String())
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	started := time.Now()
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/create", bytes.NewBufferString(`{"caller":"lovart"}`)))
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("empty-pool response took %s; must not wait for Apple sync", elapsed)
	}
	select {
	case <-proxyConnected:
		t.Fatal("empty-pool request unexpectedly attempted an Apple-side refresh")
	default:
	}
}

func TestInboxCallerReadKeepsPermanentAliasTag(t *testing.T) {
	dataDir := t.TempDir()
	raw := `{"accounts":{"acc_test":{"id":"acc_test","enabled":true,"mail_receiver":{"address":"relay@example.test","api_key":"test-key"},"alias_usages":{"one@icloud.com":{"email":"one@icloud.com","active":true}}}}}`
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := mgr.AcquireAlias("acc_test", "lovart")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	srv.receiverClientFactory = func(*account.MailReceiverConfig) (receiverClient, error) { return receiverValidationStub{}, nil }

	// Legacy reads without caller stay compatible and cannot change a label.
	legacy := httptest.NewRequest(http.MethodGet, "/api/inbox?account_id=acc_test&alias="+alias.Email, nil)
	legacyRes := httptest.NewRecorder()
	srv.Handler().ServeHTTP(legacyRes, legacy)
	if legacyRes.Code != http.StatusOK {
		t.Fatalf("legacy read status=%d body=%s", legacyRes.Code, legacyRes.Body.String())
	}
	stored, ok := mgr.GetAccount("acc_test")
	if !ok {
		t.Fatal("stored account not found")
	}
	if _, exists := stored.AliasUsages[alias.Email].UsedBy["lovart"]; !exists {
		t.Fatal("legacy read unexpectedly removed caller tag")
	}

	confirmedRead := httptest.NewRequest(http.MethodGet, "/api/inbox?account_id=acc_test&alias="+alias.Email+"&caller=LOVART", nil)
	confirmedRes := httptest.NewRecorder()
	srv.Handler().ServeHTTP(confirmedRes, confirmedRead)
	if confirmedRes.Code != http.StatusOK {
		t.Fatalf("caller read status=%d body=%s", confirmedRes.Code, confirmedRes.Body.String())
	}
	stored, ok = mgr.GetAccount("acc_test")
	if !ok {
		t.Fatal("stored account not found after caller read")
	}
	usage := stored.AliasUsages[alias.Email]
	if _, exists := usage.UsedBy["lovart"]; !exists || len(usage.PendingCallers) != 0 {
		t.Fatalf("caller read changed permanent tag: %#v", usage)
	}
}

func TestRemoveAliasCallerEndpoint(t *testing.T) {
	dataDir := t.TempDir()
	raw := `{"accounts":{"acc_test":{"id":"acc_test","hme_client_id":"00000000-0000-4000-8000-000000000001","alias_usages":{"one@icloud.com":{"email":"one@icloud.com","anonymous_id":"alias_one","active":true,"used_by":{"moxt":"2026-08-12T00:00:00Z"}}}}}}`
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)

	req := httptest.NewRequest(http.MethodDelete, "/api/aliases/alias_one/callers/MOXT", strings.NewReader(`{"account_id":"acc_test"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	for _, want := range []string{`"anonymousId":"alias_one"`, `"caller":"moxt"`, `"used_by":null`} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("response %s does not contain %s", res.Body.String(), want)
		}
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/aliases/alias_one/callers/moxt", strings.NewReader(`{"account_id":"acc_test"}`))
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", res.Code)
	}
}

func TestMarkAliasCallerEndpoint(t *testing.T) {
	dataDir := t.TempDir()
	raw := `{"accounts":{"acc_test":{"id":"acc_test","enabled":true,"hme_client_id":"00000000-0000-4000-8000-000000000001","alias_usages":{"one@icloud.com":{"email":"one@icloud.com","anonymous_id":"alias_one","active":true}}}}}`
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodPost, "/api/aliases/alias_one/callers", strings.NewReader(`{"account_id":"acc_test","caller":"Lovart"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"caller":"lovart"`) || !strings.Contains(res.Body.String(), `"used_by":["lovart"]`) {
		t.Fatalf("response = %s", res.Body.String())
	}
}

func TestReleaseAliasCallerEndpoint(t *testing.T) {
	dataDir := t.TempDir()
	raw := `{"accounts":{"acc_test":{"id":"acc_test","hme_client_id":"00000000-0000-4000-8000-000000000001","alias_usages":{"one@icloud.com":{"email":"one@icloud.com","anonymous_id":"alias_one","active":true,"used_by":{"lovart":"2026-09-02T00:00:00Z","other":"2026-09-02T00:00:01Z"}}}}}}`
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodPost, "/api/aliases/release", strings.NewReader(`{"account_id":"acc_test","anonymous_id":"alias_one","caller":"Lovart"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"caller":"lovart"`) || !strings.Contains(res.Body.String(), `"used_by":["other"]`) {
		t.Fatalf("unexpected response: %s", res.Body.String())
	}
	stored, ok := mgr.GetAccount("acc_test")
	if !ok {
		t.Fatal("stored account not found")
	}
	if _, exists := stored.AliasUsages["one@icloud.com"].UsedBy["lovart"]; exists {
		t.Fatal("release endpoint retained the caller label")
	}
}

func TestRemoveAliasCallerAllowsDisabledAccount(t *testing.T) {
	dataDir := t.TempDir()
	raw := `{"accounts":{"acc_disabled":{"id":"acc_disabled","enabled":false,"hme_client_id":"00000000-0000-4000-8000-000000000001","alias_usages":{"one@icloud.com":{"email":"one@icloud.com","anonymous_id":"alias_one","active":true,"used_by":{"lovart":"2026-09-01T00:00:00Z"}}}}}}`
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodDelete, "/api/aliases/alias_one/callers/lovart", strings.NewReader(`{"account_id":"acc_disabled"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if err := mgr.EnsureEnabled("acc_disabled"); err == nil {
		t.Fatal("caller cleanup unexpectedly enabled the account")
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
