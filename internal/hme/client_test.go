package hme

import (
	"encoding/json"
	"io"
	stdhttp "net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

type recordedRequest struct {
	Method string
	URL    string
	Header http.Header
	Body   string
}

type scriptedHTTPClient struct {
	requests   []recordedRequest
	responses  []*http.Response
	jarCookies []*http.Cookie
}

func (m *scriptedHTTPClient) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	m.requests = append(m.requests, recordedRequest{
		Method: req.Method,
		URL:    req.URL.String(),
		Header: req.Header,
		Body:   string(body),
	})
	response := m.responses[0]
	m.responses = m.responses[1:]
	return response, nil
}

func (m *scriptedHTTPClient) GetCookies(*url.URL) []*http.Cookie { return m.jarCookies }

func jsonResponse(body, scnt string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	if scnt != "" {
		header.Set("scnt", scnt)
	}
	return &http.Response{
		StatusCode: stdhttp.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestUsesAccountAPI(t *testing.T) {
	client := &Client{Cookies: map[string]string{"X-APPLE-WEBAUTH-TOKEN": "icloud"}}
	if client.UsesAccountAPI() {
		t.Fatal("iCloud Web Cookie 被误判为 Apple 账户会话")
	}
	client.Cookies["caw-at"] = "account-token"
	if !client.UsesAccountAPI() {
		t.Fatal("Apple 账户会话 Cookie 未启用官网协议")
	}
}

func TestBuildCookieHeaderKeepsAccountValuesUnquoted(t *testing.T) {
	cookies := map[string]string{
		"caw-at":                   "header.payload.signature",
		"myacinfo":                 "opaque-value",
		accountSCNTCookieKey:       "rolling-scnt",
		accountAPIKeyCookieKey:     "api-key",
		"X-Apple-I-FD-Client-Info": `{"U":"test"}`,
	}
	got := buildCookieHeader(cookies, false)
	if got != "caw-at=header.payload.signature; myacinfo=opaque-value" {
		t.Fatalf("account Cookie header = %q", got)
	}
	legacy := buildCookieHeader(map[string]string{"session": "value"}, true)
	if legacy != `session="value"` {
		t.Fatalf("legacy Cookie header = %q", legacy)
	}
}

func TestAccountCreatePayloadMatchesOfficialFlow(t *testing.T) {
	payload := accountCreatePayload{
		EmailAddress: "candidate@icloud.com",
		Label:        "shopping",
		Note:         "note",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"emailAddress":"candidate@icloud.com","label":"shopping","note":"note"}`; got != want {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestDefaultAccountFDClientInfo(t *testing.T) {
	var info accountFDClientInfo
	if err := json.Unmarshal([]byte(defaultAccountFDClientInfo()), &info); err != nil {
		t.Fatal(err)
	}
	if info.UserAgent != browserUA || info.Language != "zh-CN" || info.Version != "1.1" || !strings.HasPrefix(info.TimeZone, "GMT") {
		t.Fatalf("FD client info = %#v", info)
	}
}

func TestAccountRequestSyncsCookieJar(t *testing.T) {
	httpc := &scriptedHTTPClient{
		responses: []*http.Response{jsonResponse(`{"ok":true}`, "")},
		jarCookies: []*http.Cookie{
			{Name: "caw-at", Value: "rolling-caw-at"},
			{Name: "awat", Value: "rolling-awat"},
		},
	}
	client := &Client{
		Cookies: map[string]string{"myacinfo": "account-cookie"},
		httpc:   httpc,
	}
	if _, err := client.accountRequest("GET", "/account/manage/gs/ws/token", nil, false); err != nil {
		t.Fatal(err)
	}
	if client.Cookies["caw-at"] != "rolling-caw-at" || client.Cookies["awat"] != "rolling-awat" {
		t.Fatalf("CookieJar tokens were not synchronized: names=%v", []string{"caw-at", "awat"})
	}
}

func TestCreateAccountAliasMatchesOfficialRequestSequence(t *testing.T) {
	httpc := &scriptedHTTPClient{responses: []*http.Response{
		jsonResponse(`{"timeOutInterval":15}`, "scnt-token"),
		jsonResponse(`{"apiKey":"api-key"}`, "scnt-manage"),
		jsonResponse(`{"emailAddress":"candidate@icloud.com"}`, "scnt-generate"),
		jsonResponse(`{"emailAddress":"candidate@icloud.com","id":"alias-id","label":"test","note":"note"}`, "scnt-complete"),
		jsonResponse(`{"emailAddress":"candidate@icloud.com","id":"alias-id","label":"test","note":"note","active":true}`, "scnt-detail"),
	}}
	client := &Client{
		Cookies: map[string]string{
			"myacinfo": "account-cookie",
		},
		httpc: httpc,
	}

	result, err := client.CreateAliasWithNote("test", "note", 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Protocol != "apple_account" || result.Email != "candidate@icloud.com" || result.Note != "note" {
		t.Fatalf("result = %#v", result)
	}
	if len(httpc.requests) != 5 {
		t.Fatalf("request count = %d, want 5", len(httpc.requests))
	}
	want := []struct {
		method string
		path   string
		body   string
	}{
		{"GET", "/account/manage/gs/ws/token", ""},
		{"GET", "/account/manage", ""},
		{"POST", "/account/manage/email/private/add", `{}`},
		{"PUT", "/account/manage/email/private/add/complete", `{"emailAddress":"candidate@icloud.com","label":"test","note":"note"}`},
		{"GET", "/account/manage/email/private/alias-id.em", ""},
	}
	for i, request := range httpc.requests {
		parsed, err := url.Parse(request.URL)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != want[i].method || parsed.Path != want[i].path || request.Body != want[i].body {
			t.Fatalf("request[%d] = %s %s %q", i, request.Method, parsed.Path, request.Body)
		}
	}
	generate := httpc.requests[2]
	if generate.Header.Get("X-Apple-Api-Key") != "api-key" || generate.Header.Get("scnt") != "scnt-manage" {
		t.Fatalf("generate headers missing session state: %#v", generate.Header)
	}
	if generate.Header.Get("Origin") != appleIDOrigin || generate.Header.Get("X-Apple-I-Request-Context") != "ca" {
		t.Fatalf("generate headers do not match account.apple.com: %#v", generate.Header)
	}
	if got := generate.Header.Get("Cookie"); got != "myacinfo=account-cookie" {
		t.Fatalf("account Cookie = %q", got)
	}
	if client.Cookies[accountSCNTCookieKey] != "scnt-detail" || client.Cookies[accountAPIKeyCookieKey] != "api-key" {
		t.Fatalf("session metadata was not persisted: %#v", client.Cookies)
	}
}

func TestNewClientWithIDCopiesCookies(t *testing.T) {
	clientID := uuid.New().String()
	source := map[string]string{"session": "original"}
	client, err := NewClientWithID(source, "icloud.com", "", clientID, false)
	if err != nil {
		t.Fatal(err)
	}
	if client.ClientID() != clientID {
		t.Fatalf("client ID = %q, want %q", client.ClientID(), clientID)
	}
	client.Cookies["session"] = "client-value"
	if source["session"] != "original" {
		t.Fatal("client retained the caller's Cookie map")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 5, 0, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("120", now); got != 2*time.Minute {
		t.Fatalf("numeric Retry-After = %s, want 2m", got)
	}
	date := now.Add(5 * time.Minute).Format(stdhttp.TimeFormat)
	if got := parseRetryAfter(date, now); got != 5*time.Minute {
		t.Fatalf("date Retry-After = %s, want 5m", got)
	}
}

func TestSanitizeResponseBody(t *testing.T) {
	got := sanitizeResponseBody([]byte("  {\n  \"error\": \"limited\"\n}  "))
	if got != `{"error":"limited"}` {
		t.Fatalf("sanitized body = %q", got)
	}
	long := sanitizeResponseBody([]byte(strings.Repeat("x", 1200)))
	if len(long) != 1003 || !strings.HasSuffix(long, "...") {
		t.Fatalf("long body was not truncated: len=%d", len(long))
	}
}
