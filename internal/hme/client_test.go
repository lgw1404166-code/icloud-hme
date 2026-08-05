package hme

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

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
	date := now.Add(5 * time.Minute).Format(http.TimeFormat)
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
