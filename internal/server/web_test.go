package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
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

func TestCreateGateSerializesAndAppliesCooldown(t *testing.T) {
	now := time.Date(2026, time.August, 5, 0, 0, 0, 0, time.UTC)
	srv := &Server{
		createAttempts:        make(map[string]*createAttemptState),
		now:                   func() time.Time { return now },
		createMinInterval:     30 * time.Second,
		createFailureCooldown: 10 * time.Minute,
	}

	if _, allowed := srv.beginCreate("acc_test"); !allowed {
		t.Fatal("first create attempt was rejected")
	}
	if retry, allowed := srv.beginCreate("acc_test"); allowed || retry != 30*time.Second {
		t.Fatalf("concurrent attempt allowed=%v retry=%s", allowed, retry)
	}
	srv.finishCreate("acc_test", 0)
	if retry, allowed := srv.beginCreate("acc_test"); allowed || retry != 30*time.Second {
		t.Fatalf("minimum interval allowed=%v retry=%s", allowed, retry)
	}

	now = now.Add(31 * time.Second)
	if _, allowed := srv.beginCreate("acc_test"); !allowed {
		t.Fatal("attempt after minimum interval was rejected")
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
