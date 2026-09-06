package account

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"icloud-hme/internal/hme"
)

func TestParseCookieInputSupportsBrowserArray(t *testing.T) {
	cookies, err := ParseCookieInput(`[{"name":"session","value":"cookie-value"},{"name":"empty","value":""}]`)
	if err != nil {
		t.Fatal(err)
	}
	if cookies["session"] != "cookie-value" || cookies["empty"] != "" {
		t.Fatalf("unexpected cookies: %#v", cookies)
	}
}

func TestListAccountsRedactsCredentials(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	mgr.accounts["acc_test"] = &Account{
		ID:           "acc_test",
		Name:         "Test",
		Cookies:      map[string]string{"session": "cookie-secret"},
		HMEClientID:  uuid.New().String(),
		MailReceiver: &MailReceiverConfig{Address: "relay@example.test", APIKey: "api-secret"},
	}

	accounts := mgr.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("len(accounts) = %d, want 1", len(accounts))
	}
	if accounts[0].Cookies != nil {
		t.Fatal("ListAccounts returned cookies")
	}
	if accounts[0].MailReceiver == nil || accounts[0].MailReceiver.APIKey != "" {
		t.Fatal("ListAccounts returned receiver credentials")
	}
	if accounts[0].HMEClientID != "" {
		t.Fatal("ListAccounts returned the HME client ID")
	}
	if mgr.accounts["acc_test"].Cookies["session"] != "cookie-secret" {
		t.Fatal("ListAccounts modified stored cookies")
	}
	if mgr.accounts["acc_test"].MailReceiver.APIKey != "api-secret" {
		t.Fatal("ListAccounts modified stored receiver credentials")
	}
}

func TestNewManagerMigratesAndReusesHMEClientID(t *testing.T) {
	dataDir := t.TempDir()
	dataFile := dataDir + "/accounts.json"
	if err := os.WriteFile(dataFile, []byte(`{
  "accounts": {
    "acc_test": {
      "id": "acc_test",
      "name": "Test",
      "cookies": {"session": "original"},
      "host": "icloud.com"
    }
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	account, ok := mgr.GetAccount("acc_test")
	if !ok {
		t.Fatal("migrated account not found")
	}
	if _, err := uuid.Parse(account.HMEClientID); err != nil {
		t.Fatalf("HMEClientID = %q, want UUID", account.HMEClientID)
	}

	client, err := mgr.HMEClient("acc_test", false)
	if err != nil {
		t.Fatal(err)
	}
	if client.ClientID() != account.HMEClientID {
		t.Fatalf("client ID = %q, want %q", client.ClientID(), account.HMEClientID)
	}
	client.Cookies["session"] = "client-rotated"
	stored, _ := mgr.GetAccount("acc_test")
	if stored.Cookies["session"] != "original" {
		t.Fatal("HME client mutated the stored Cookie map")
	}

	reloaded, err := NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	reloadedAccount, _ := reloaded.GetAccount("acc_test")
	if reloadedAccount.HMEClientID != account.HMEClientID {
		t.Fatalf("client ID changed after reload: %q != %q", reloadedAccount.HMEClientID, account.HMEClientID)
	}

	raw, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		Accounts map[string]Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Accounts["acc_test"].HMEClientID != account.HMEClientID {
		t.Fatal("migrated client ID was not persisted")
	}
	if !persisted.Accounts["acc_test"].AutoCreateEnabled {
		t.Fatal("legacy account did not default auto creation to enabled")
	}
	if !persisted.Accounts["acc_test"].Enabled {
		t.Fatal("legacy account did not default enabled state to true")
	}
}

func TestNewManagerRemovesLegacyICloudMailCredentials(t *testing.T) {
	dataDir := t.TempDir()
	dataFile := dataDir + "/accounts.json"
	if err := os.WriteFile(dataFile, []byte(`{
  "accounts": {
    "acc_test": {
      "id": "acc_test",
      "icloud_email": "old@icloud.com",
      "app_password": "old-secret",
      "mail_receiver": {"provider":"legacy","address":"relay@example.test","api_key":"legacy-secret"}
    }
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(dataDir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "old-secret") || strings.Contains(string(raw), "app_password") || strings.Contains(string(raw), "icloud_email") {
		t.Fatalf("legacy iCloud mail credentials were retained: %s", raw)
	}
	if strings.Contains(string(raw), "legacy-secret") || strings.Contains(string(raw), `"provider"`) {
		t.Fatalf("legacy provider receiver was retained: %s", raw)
	}
}

func TestSetAutoCreateEnabledPersists(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(dataDir+"/accounts.json", []byte(`{
  "accounts": {
    "acc_test": {"id": "acc_test", "auto_create_enabled": true}
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetAutoCreateEnabled("acc_test", false); err != nil {
		t.Fatal(err)
	}
	account, ok := mgr.GetAccount("acc_test")
	if !ok || account.AutoCreateEnabled {
		t.Fatalf("account auto creation = %v, want false", account.AutoCreateEnabled)
	}
	reloaded, err := NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	account, ok = reloaded.GetAccount("acc_test")
	if !ok || account.AutoCreateEnabled {
		t.Fatalf("reloaded account auto creation = %v, want false", account.AutoCreateEnabled)
	}
}

func TestSetEnabledPersistsAndBlocksAliasAcquisition(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID:      "acc_test",
		Enabled: true,
		AliasUsages: map[string]*AliasUsage{
			"one@icloud.com": {Email: "one@icloud.com", Active: true},
		},
	}
	if _, err := mgr.AcquireAlias("acc_test", "caller"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetEnabled("acc_test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AcquireAlias("acc_test", "another-caller"); err == nil || !strings.Contains(err.Error(), "账号已停用") {
		t.Fatalf("disabled acquisition error = %v", err)
	}
	reloaded, err := NewManager(mgr.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	account, ok := reloaded.GetAccount("acc_test")
	if !ok || account.Enabled {
		t.Fatalf("reloaded enabled = %v, want false", account.Enabled)
	}
}

func TestSaveCookiesCopiesInput(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{ID: "acc_test", Enabled: true}
	cookies := map[string]string{"session": "saved"}
	if err := mgr.SaveCookies("acc_test", cookies); err != nil {
		t.Fatal(err)
	}
	cookies["session"] = "mutated-after-save"
	stored, _ := mgr.GetAccount("acc_test")
	if stored.Cookies["session"] != "saved" {
		t.Fatal("SaveCookies retained the caller's mutable map")
	}
}

func TestMergeRefreshedCookiesPreservesConcurrentChanges(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID:      "acc_test",
		Enabled: true,
		Cookies: map[string]string{
			"caw-at":     "old-token",
			"concurrent": "newer-business-value",
		},
	}
	before := map[string]string{
		"caw-at":     "old-token",
		"concurrent": "stale-snapshot",
	}
	refreshed := map[string]string{
		"caw-at":     "rotated-token",
		"concurrent": "stale-snapshot",
	}
	if err := mgr.mergeRefreshedCookies("acc_test", before, refreshed); err != nil {
		t.Fatal(err)
	}
	stored, _ := mgr.GetAccount("acc_test")
	if stored.Cookies["caw-at"] != "rotated-token" {
		t.Fatal("rotated token was not merged")
	}
	if stored.Cookies["concurrent"] != "newer-business-value" {
		t.Fatal("concurrent Cookie update was overwritten")
	}
}

func TestSaveAliasStats(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{ID: "acc_test", Enabled: true}

	aliases := []hme.Alias{
		{Email: "one@icloud.com", Active: true},
		{Email: "two@icloud.com", Active: false},
		{Email: "three@icloud.com", Active: true},
	}
	if err := mgr.SaveAliasStats("acc_test", aliases, map[string]string{"session": "rotated"}); err != nil {
		t.Fatal(err)
	}
	account, ok := mgr.GetAccount("acc_test")
	if !ok {
		t.Fatal("account was not saved")
	}
	if account.AliasTotal != 3 || account.AliasActive != 2 {
		t.Fatalf("alias stats = %d/%d, want 2/3 active/total", account.AliasActive, account.AliasTotal)
	}
	if account.Cookies["session"] != "rotated" {
		t.Fatalf("cookies were not saved: %#v", account.Cookies)
	}
}

func TestRegisterCreatedAliasPreservesAppleSourcedTotal(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID:          "acc_test",
		Enabled:     true,
		AliasTotal:  2,
		AliasActive: 1,
		AliasUsages: map[string]*AliasUsage{
			"active@icloud.com":  {Email: "active@icloud.com", AnonymousID: "active-id", Active: true},
			"deleted@icloud.com": {Email: "deleted@icloud.com", AnonymousID: "deleted-id", Active: false},
		},
	}

	err = mgr.RegisterCreatedAlias("acc_test", &hme.CreateResult{
		Email:       "new@icloud.com",
		AnonymousID: "new-id",
		CreatedAt:   "2026-09-05T00:00:00Z",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := mgr.GetAccount("acc_test")
	if stored.AliasTotal != 3 || stored.AliasActive != 2 {
		t.Fatalf("alias stats after create = %d/%d, want 2/3 active/total", stored.AliasActive, stored.AliasTotal)
	}
}

func TestAliasStateUpdatesCounters(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID:          "acc_test",
		Enabled:     true,
		AliasTotal:  2,
		AliasActive: 2,
		AliasUsages: map[string]*AliasUsage{
			"one@icloud.com": {Email: "one@icloud.com", AnonymousID: "one-id", Active: true},
			"two@icloud.com": {Email: "two@icloud.com", AnonymousID: "two-id", Active: true},
		},
	}

	if err := mgr.SetAliasActive("acc_test", "one-id", false); err != nil {
		t.Fatal(err)
	}
	stored, _ := mgr.GetAccount("acc_test")
	if stored.AliasTotal != 2 || stored.AliasActive != 1 {
		t.Fatalf("alias stats after deactivate = %d/%d, want 1/2 active/total", stored.AliasActive, stored.AliasTotal)
	}
	if err := mgr.SetAliasActive("acc_test", "one-id", true); err != nil {
		t.Fatal(err)
	}
	if err := mgr.MarkAliasDeleted("acc_test", "one-id"); err != nil {
		t.Fatal(err)
	}
	stored, _ = mgr.GetAccount("acc_test")
	if stored.AliasTotal != 1 || stored.AliasActive != 1 {
		t.Fatalf("alias stats after delete = %d/%d, want 1/1 active/total", stored.AliasActive, stored.AliasTotal)
	}
}

func TestAcquireAliasIsCaseInsensitivePerCaller(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID:      "acc_test",
		Enabled: true,
		AliasUsages: map[string]*AliasUsage{
			"one@icloud.com": {Email: "one@icloud.com", Active: true, UsedBy: map[string]string{}},
			"two@icloud.com": {Email: "two@icloud.com", Active: true, UsedBy: map[string]string{}},
		},
	}

	first, err := mgr.AcquireAlias("acc_test", "ChatGPT")
	if err != nil {
		t.Fatal(err)
	}
	second, err := mgr.AcquireAlias("acc_test", "chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	if first.Email == second.Email {
		t.Fatalf("same caller received duplicate alias: %s", first.Email)
	}
	other, err := mgr.AcquireAlias("acc_test", "MOXT")
	if err != nil {
		t.Fatal(err)
	}
	if other.Email == "" {
		t.Fatal("other caller did not receive an alias")
	}
}

func TestCallerTagIsPermanentAtAllocation(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID: "acc_test", Enabled: true,
		AliasUsages: map[string]*AliasUsage{
			"one@icloud.com": {Email: "one@icloud.com", Active: true, UsedBy: map[string]string{}},
		},
	}

	allocated, err := mgr.AcquireAlias("acc_test", "Lovart")
	if err != nil {
		t.Fatal(err)
	}
	usage := mgr.accounts["acc_test"].AliasUsages[allocated.Email]
	if _, ok := usage.UsedBy["lovart"]; !ok {
		t.Fatal("allocation did not immediately expose the caller tag")
	}
	if len(usage.PendingCallers) != 0 {
		t.Fatalf("allocation created an unexpected pending tag: %#v", usage.PendingCallers)
	}

	result, err := mgr.ObserveCallerInboxRead("acc_test", allocated.Email, "LOVART", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Confirmed || result.Cleared {
		t.Fatalf("observation=%#v, want no state change", result)
	}
	if _, ok := usage.UsedBy["lovart"]; !ok {
		t.Fatal("mail observation removed permanent caller tag")
	}
}

func TestMarkAliasCallerRepairsHistoricalAllocation(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID: "acc_test", Enabled: true,
		AliasUsages: map[string]*AliasUsage{
			"one@icloud.com": {Email: "one@icloud.com", AnonymousID: "alias_one", Active: true, UsedBy: map[string]string{}},
		},
	}

	marked, err := mgr.MarkAliasCaller("acc_test", "alias_one", "Lovart")
	if err != nil {
		t.Fatal(err)
	}
	if marked.Email != "one@icloud.com" || len(marked.UsedBy) != 1 || marked.UsedBy[0] != "lovart" {
		t.Fatalf("marked alias = %#v", marked)
	}
	if _, err := mgr.MarkAliasCaller("acc_test", "alias_one", "lovart"); err != nil {
		t.Fatalf("idempotent repair failed: %v", err)
	}
	stored, ok := mgr.GetAccount("acc_test")
	if !ok || len(stored.AliasUsages["one@icloud.com"].UsedBy) != 1 {
		t.Fatalf("stored usage = %#v", stored)
	}
}

func TestCallerTagIsNotReleasedByExpiry(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID: "acc_test", Enabled: true,
		AliasUsages: map[string]*AliasUsage{
			"one@icloud.com": {Email: "one@icloud.com", Active: true, UsedBy: map[string]string{}},
			"two@icloud.com": {Email: "two@icloud.com", Active: true, UsedBy: map[string]string{}},
		},
	}

	first, err := mgr.AcquireAlias("acc_test", "lovart")
	if err != nil {
		t.Fatal(err)
	}
	firstUsage := mgr.accounts["acc_test"].AliasUsages[first.Email]
	cleared, err := mgr.ExpirePendingCallerAllocations()
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 0 {
		t.Fatalf("expired claims cleared=%d, want 0", cleared)
	}
	if _, exists := firstUsage.UsedBy["lovart"]; !exists {
		t.Fatalf("expiry released permanent allocation: %#v", firstUsage)
	}
}

func TestAcquireAliasAnySkipsExhaustedAndDisabledAccounts(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_exhausted"] = &Account{
		ID: "acc_exhausted", Enabled: true,
		AliasUsages: map[string]*AliasUsage{
			"used@icloud.com": {Email: "used@icloud.com", Active: true, UsedBy: map[string]string{"lovart": "2026-09-01T00:00:00Z"}},
		},
	}
	mgr.accounts["acc_available"] = &Account{
		ID: "acc_available", Enabled: true,
		AliasUsages: map[string]*AliasUsage{
			"available@icloud.com": {Email: "available@icloud.com", Active: true, UsedBy: map[string]string{}},
		},
	}
	mgr.accounts["acc_disabled"] = &Account{
		ID: "acc_disabled", Enabled: false,
		AliasUsages: map[string]*AliasUsage{
			"disabled@icloud.com": {Email: "disabled@icloud.com", Active: true, UsedBy: map[string]string{}},
		},
	}

	accountID, alias, err := mgr.AcquireAliasAny("Lovart")
	if err != nil {
		t.Fatal(err)
	}
	if accountID != "acc_available" || alias.Email != "available@icloud.com" {
		t.Fatalf("global allocation = %q/%q, want acc_available/available@icloud.com", accountID, alias.Email)
	}
	if _, _, err := mgr.AcquireAliasAny("lovart"); err == nil {
		t.Fatal("expected no second global alias for lovart")
	}
	if _, ok := mgr.accounts["acc_disabled"].AliasUsages["disabled@icloud.com"].UsedBy["lovart"]; ok {
		t.Fatal("global allocation used a disabled account")
	}
}

func TestRemoveAliasCallerIsPreciseAndCaseInsensitive(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID:      "acc_test",
		Enabled: true,
		AliasUsages: map[string]*AliasUsage{
			"one@icloud.com": {
				Email:       "one@icloud.com",
				AnonymousID: "alias_one",
				Active:      true,
				UsedBy:      map[string]string{"MoXT": "2026-08-12T00:00:00Z", "chatgpt": "2026-08-12T01:00:00Z"},
			},
			"two@icloud.com": {
				Email:       "two@icloud.com",
				AnonymousID: "alias_two",
				Active:      true,
				UsedBy:      map[string]string{"moxt": "2026-08-12T02:00:00Z"},
			},
		},
	}

	alias, err := mgr.RemoveAliasCaller("acc_test", "alias_one", "MOXT")
	if err != nil {
		t.Fatal(err)
	}
	if len(alias.UsedBy) != 1 || alias.UsedBy[0] != "chatgpt" {
		t.Fatalf("remaining callers = %#v, want chatgpt", alias.UsedBy)
	}
	if _, exists := mgr.accounts["acc_test"].AliasUsages["one@icloud.com"]; !exists {
		t.Fatal("removing a caller deleted the alias")
	}
	if _, exists := mgr.accounts["acc_test"].AliasUsages["two@icloud.com"].UsedBy["moxt"]; !exists {
		t.Fatal("removing a caller changed another alias")
	}
	if _, err := mgr.RemoveAliasCaller("acc_test", "alias_one", "missing"); err == nil {
		t.Fatal("removing an unknown caller succeeded")
	}
}

func TestAddAccountRequiresCookies(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddAccount("without-cookie", "", "", ""); err == nil {
		t.Fatal("AddAccount accepted an empty Cookie input")
	}
}
