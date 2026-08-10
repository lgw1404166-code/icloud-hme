package account

import (
	"encoding/json"
	"os"
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
		ID:          "acc_test",
		Name:        "Test",
		Cookies:     map[string]string{"session": "cookie-secret"},
		AppPassword: "app-password-secret",
		HMEClientID: uuid.New().String(),
	}

	accounts := mgr.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("len(accounts) = %d, want 1", len(accounts))
	}
	if accounts[0].Cookies != nil {
		t.Fatal("ListAccounts returned cookies")
	}
	if accounts[0].AppPassword != "" {
		t.Fatal("ListAccounts returned the app password")
	}
	if accounts[0].HMEClientID != "" {
		t.Fatal("ListAccounts returned the HME client ID")
	}
	if mgr.accounts["acc_test"].Cookies["session"] != "cookie-secret" {
		t.Fatal("ListAccounts modified stored cookies")
	}
	if mgr.accounts["acc_test"].AppPassword != "app-password-secret" {
		t.Fatal("ListAccounts modified the stored app password")
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
}

func TestSaveCookiesCopiesInput(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{ID: "acc_test"}
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
		ID: "acc_test",
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
	mgr.accounts["acc_test"] = &Account{ID: "acc_test"}

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

func TestAcquireAliasIsCaseInsensitivePerCaller(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.accounts["acc_test"] = &Account{
		ID: "acc_test",
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

func TestAddAccountRequiresCookies(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddAccount("without-cookie", "", "", ""); err == nil {
		t.Fatal("AddAccount accepted an empty Cookie input")
	}
}
