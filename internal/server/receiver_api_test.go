package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
)

type receiverValidationStub struct{}

func (receiverValidationStub) Validate(context.Context) error { return nil }
func (receiverValidationStub) List(context.Context, string, int, int, int) ([]mail.Message, int, error) {
	return nil, 0, nil
}
func (receiverValidationStub) Get(context.Context, string, string) (*mail.FullMessage, error) {
	return nil, nil
}

func TestMoEmailReceiverResolvesMailboxAndFiltersAlias(t *testing.T) {
	const alias = "alias@icloud.com"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "mo-key" {
			http.Error(w, "missing api key", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/emails":
			_, _ = w.Write([]byte(`{"emails":[{"id":"box-1","address":"relay@moemail.test"}],"total":1}`))
		case "/api/emails/box-1":
			_, _ = w.Write([]byte(`{"messages":[{"id":"one","to_address":"alias@icloud.com","from_address":"sender@example.com","subject":"Matched","content":"Preview","received_at":1788134400000},{"id":"two","to_address":"else@icloud.com","subject":"Other","received_at":1788134401000}],"total":2}`))
		case "/api/emails/box-1/one":
			_, _ = w.Write([]byte(`{"message":{"id":"one","to_address":"alias@icloud.com","from_address":"sender@example.com","subject":"Matched","content":"Full content","html":"<p>Full content</p>","received_at":1788134400000}}`))
		case "/api/emails/box-1/two":
			_, _ = w.Write([]byte(`{"message":{"id":"two","to_address":"else@icloud.com","subject":"Other","received_at":1788134401000}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	receiver := &apiMailReceiver{config: &account.MailReceiverConfig{
		BaseURL: server.URL, Address: "relay@moemail.test", APIKey: "mo-key",
	}, client: server.Client()}
	messages, total, err := receiver.List(context.Background(), alias, 20, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(messages) != 1 || messages[0].ID != "one" {
		t.Fatalf("messages=%#v total=%d", messages, total)
	}
	full, err := receiver.Get(context.Background(), alias, "one")
	if err != nil || full.Body != "Full content" || full.To != alias {
		t.Fatalf("full=%#v err=%v", full, err)
	}
}

func TestMoEmailReceiverRejectsMailboxWithoutOriginalRecipient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "mo-key" {
			http.Error(w, "missing api key", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/emails":
			_, _ = w.Write([]byte(`{"emails":[{"id":"box-1","address":"relay@moemail.test"}]}`))
		case "/api/emails/box-1":
			_, _ = w.Write([]byte(`{"messages":[{"id":"one","to_address":"relay@moemail.test","subject":"Forwarded"}]}`))
		case "/api/emails/box-1/one":
			_, _ = w.Write([]byte(`{"message":{"id":"one","to_address":"relay@moemail.test","subject":"Forwarded"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	receiver := &apiMailReceiver{config: &account.MailReceiverConfig{BaseURL: server.URL, Address: "relay@moemail.test", APIKey: "mo-key"}, client: server.Client()}
	_, _, err := receiver.List(context.Background(), "alias@icloud.com", 20, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "未保留原始收件人") {
		t.Fatalf("error = %v", err)
	}
}

func TestMoEmailReceiverSkipsExplicitRecipientsWithoutDetailFetch(t *testing.T) {
	detailRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "mo-key" {
			http.Error(w, "missing api key", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/emails":
			_, _ = w.Write([]byte(`{"emails":[{"id":"box-1","address":"relay@moemail.test"}]}`))
		case "/api/emails/box-1":
			_, _ = w.Write([]byte(`{"messages":[{"id":"history","to_address":"relay@moemail.test","subject":"Old forwarded message"},{"id":"other","to_address":"other@icloud.com","subject":"Another alias"}]}`))
		default:
			detailRequested = true
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	receiver := &apiMailReceiver{config: &account.MailReceiverConfig{BaseURL: server.URL, Address: "relay@moemail.test", APIKey: "mo-key"}, client: server.Client()}
	messages, total, err := receiver.List(context.Background(), "alias@icloud.com", 20, 0, 0)
	if err != nil || len(messages) != 0 || total != 0 {
		t.Fatalf("messages=%#v total=%d err=%v", messages, total, err)
	}
	if detailRequested {
		t.Fatal("fixed forwarding history triggered a detail request")
	}
}

func TestMoEmailReceiverSharesRecentMailboxSnapshot(t *testing.T) {
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "mo-key" {
			http.Error(w, "missing api key", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/emails/box-1":
			listCalls++
			_, _ = w.Write([]byte(`{"messages":[{"id":"one","to_address":"alias@icloud.com","subject":"Matched"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	receiver := &apiMailReceiver{config: &account.MailReceiverConfig{BaseURL: server.URL, MailboxID: "box-1", Address: "relay@moemail.test", APIKey: "mo-key"}, client: server.Client()}
	for range 2 {
		messages, total, err := receiver.List(context.Background(), "alias@icloud.com", 20, 0, 0)
		if err != nil || total != 1 || len(messages) != 1 {
			t.Fatalf("messages=%#v total=%d err=%v", messages, total, err)
		}
	}
	if listCalls != 1 {
		t.Fatalf("mailbox list calls = %d, want 1", listCalls)
	}
}

func TestMoEmailReceiverRetriesTruncatedJSON(t *testing.T) {
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "mo-key" {
			http.Error(w, "missing api key", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/emails/box-1" {
			http.NotFound(w, r)
			return
		}
		listCalls++
		if listCalls == 1 {
			_, _ = w.Write([]byte(`{"messages":[`))
			return
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"one","to_address":"alias@icloud.com","subject":"Matched"}]}`))
	}))
	defer server.Close()
	receiver := &apiMailReceiver{config: &account.MailReceiverConfig{BaseURL: server.URL, MailboxID: "box-1", Address: "relay@moemail.test", APIKey: "mo-key"}, client: server.Client()}
	messages, total, err := receiver.List(context.Background(), "alias@icloud.com", 20, 0, 0)
	if err != nil || total != 1 || len(messages) != 1 {
		t.Fatalf("messages=%#v total=%d err=%v", messages, total, err)
	}
	if listCalls != 2 {
		t.Fatalf("mailbox list calls = %d, want 2", listCalls)
	}
}

func TestMailReceiverEndpointStoresCredentialsButNeverReturnsThem(t *testing.T) {
	dataDir := t.TempDir()
	mgr, err := account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "accounts.json"), []byte(`{"accounts":{"acc_test":{"id":"acc_test","name":"Test","enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err = account.NewManager(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	srv.receiverClientFactory = func(*account.MailReceiverConfig) (receiverClient, error) {
		return receiverValidationStub{}, nil
	}
	req := httptest.NewRequest(http.MethodPut, "/api/accounts/acc_test/mail-receiver", strings.NewReader(`{"address":"relay@example.test","api_key":"mo-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "mo-secret") {
		t.Fatalf("update response=%d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/accounts?include_disabled=true", nil))
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "mo-secret") || !strings.Contains(res.Body.String(), "relay@example.test") {
		t.Fatalf("list response=%d %s", res.Code, res.Body.String())
	}
	private, err := mgr.MailReceiver("acc_test")
	if err != nil || private.APIKey != "mo-secret" {
		t.Fatalf("stored receiver=%#v err=%v", private, err)
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/accounts/acc_test/mail-receiver", nil)
	res = httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("clear response=%d %s", res.Code, res.Body.String())
	}
	if _, err := mgr.MailReceiver("acc_test"); err == nil {
		t.Fatal("receiver credentials remained after clear")
	}
}

func TestMailReceiverEndpointRejectsNonMoeMailProvider(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, false)
	req := httptest.NewRequest(http.MethodPut, "/api/accounts/acc_test/mail-receiver", strings.NewReader(`{"provider":"other","address":"relay@example.test","api_key":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "仅支持 MoeMail") {
		t.Fatalf("response=%d %s", res.Code, res.Body.String())
	}
}
