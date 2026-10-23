package server

import (
	"encoding/json"
	"icloud-hme/internal/account"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalAliasesAvailableAfterReloadWithoutAppleCredentials(t *testing.T) {
	dir := t.TempDir()
	data := `{"accounts":{"a":{"id":"a","enabled":true,"real_email":"owner@example.test","cookies":{"session":"cookie-secret"},"mail_receiver":{"address":"receiver@example.test","api_key":"mail-secret"},"alias_usages":{"old@icloud.com":{"email":"old@icloud.com","anonymous_id":"alias-old","label":"Stored","active":false,"used_by":{"lovart":"2026-01-02T00:00:00Z"}},"active@icloud.com":{"email":"active@icloud.com","anonymous_id":"alias-active","active":true},"empty":null}},"disabled":{"id":"disabled","enabled":false,"alias_usages":{"hidden@icloud.com":{"email":"hidden@icloud.com","active":true}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		mgr, err := account.NewManager(dir)
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		New(mgr, false, "test-key").Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/aliases/local", nil))
		if res.Code != http.StatusOK {
			t.Fatalf("status: %d", res.Code)
		}
		if res.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
		var response struct {
			Success bool `json:"success"`
			Data    struct {
				Count   int                  `json:"count"`
				Source  string               `json:"source"`
				Aliases []account.LocalAlias `json:"aliases"`
			} `json:"data"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !response.Success || response.Data.Count != 2 || response.Data.Source != "local" {
			t.Fatalf("unexpected: %s", res.Body.String())
		}
		aliases := response.Data.Aliases
		if len(aliases) != 2 || aliases[0].Email != "active@icloud.com" || aliases[1].Active || aliases[1].AccountID != "a" || aliases[1].AccountEmail != "owner@example.test" || len(aliases[1].UsedBy) != 1 || aliases[1].UsedBy[0] != "lovart" {
			t.Fatalf("aliases: %#v", aliases)
		}
		for _, secret := range []string{"cookie-secret", "mail-secret", "cookies", "mail_receiver", "hidden@icloud.com"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatalf("leaked %s", secret)
			}
		}
		aliases[1].UsedBy[0] = "mutated"
		if mgr.ListLocalAliases()[1].UsedBy[0] != "lovart" {
			t.Fatal("shares mutable storage")
		}
	}
}

func TestLocalAliasesEmptyListIsArray(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	New(mgr, false).Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/aliases/local", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"aliases":[]`) {
		t.Fatalf("unexpected: %s", res.Body.String())
	}
}
