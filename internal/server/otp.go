package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var (
	appleOTPProviderRe = regexp.MustCompile(`(?i)(apple|apple\s*id|apple\s*account|apple\s*账户|apple\s*帐户|苹果)`)
	sixDigitCodeRe     = regexp.MustCompile(`(?:^|[^\d])(\d{6})(?:[^\d]|$)`)
)

type otpCodeRecord struct {
	Provider   string `json:"provider"`
	Code       string `json:"code"`
	Sender     string `json:"sender,omitempty"`
	ReceivedAt string `json:"received_at,omitempty"`
	StoredAt   string `json:"stored_at"`
	ExpiresAt  string `json:"expires_at"`
}

func (s *Server) receiveOTP(c *gin.Context) {
	if !s.verifyOTPToken(c) {
		return
	}
	raw, err := c.GetRawData()
	if err != nil || len(raw) == 0 {
		fail(c, http.StatusBadRequest, "请求体为空")
		return
	}

	event := strings.ToLower(strings.TrimSpace(gjson.GetBytes(raw, "event").String()))
	if event != "" && event != "sms:received" {
		ok(c, gin.H{"accepted": false, "reason": "ignored_event", "event": event})
		return
	}

	message := firstGJSON(raw,
		"payload.message",
		"payload.body",
		"payload.text",
		"message",
		"text",
		"body",
		"sms.message",
		"data.message",
	)
	sender := firstGJSON(raw,
		"payload.sender",
		"payload.phoneNumber",
		"sender",
		"from",
		"phoneNumber",
		"sms.sender",
		"data.sender",
	)
	receivedAt := firstGJSON(raw,
		"payload.receivedAt",
		"received_at",
		"receivedAt",
		"timestamp",
	)

	code, okApple := extractAppleOTP(sender, message)
	if !okApple {
		ok(c, gin.H{"accepted": false, "reason": "not_apple_otp"})
		return
	}
	record := s.storeOTP("apple", code, sender, receivedAt)
	ok(c, gin.H{
		"accepted":    true,
		"provider":    record.Provider,
		"received_at": record.ReceivedAt,
		"expires_at":  record.ExpiresAt,
	})
}

func (s *Server) latestOTP(c *gin.Context) {
	if !s.verifyOTPToken(c) {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(c.DefaultQuery("provider", "apple")))
	record, found := s.latestOTPRecord(provider)
	if !found {
		fail(c, http.StatusNotFound, "没有可用验证码")
		return
	}
	ok(c, gin.H{"otp": record})
}

func (s *Server) verifyOTPToken(c *gin.Context) bool {
	if s.relogin.OTP.WebhookToken == "" {
		fail(c, http.StatusServiceUnavailable, "未配置 ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN")
		return false
	}
	provided := strings.TrimSpace(c.Query("token"))
	if provided == "" {
		provided = strings.TrimSpace(c.GetHeader("X-OTP-Token"))
	}
	if provided == "" {
		provided = strings.TrimSpace(c.GetHeader("X-Webhook-Token"))
	}
	if provided == "" {
		auth := strings.TrimSpace(c.GetHeader("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			provided = strings.TrimSpace(auth[7:])
		}
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(s.relogin.OTP.WebhookToken)) != 1 {
		fail(c, http.StatusUnauthorized, "OTP webhook token 无效")
		return false
	}
	return true
}

func firstGJSON(raw []byte, paths ...string) string {
	for _, path := range paths {
		value := strings.TrimSpace(gjson.GetBytes(raw, path).String())
		if value != "" {
			return value
		}
	}
	return ""
}

func extractAppleOTP(sender, message string) (string, bool) {
	combined := strings.TrimSpace(sender + " " + message)
	if !appleOTPProviderRe.MatchString(combined) {
		return "", false
	}
	matches := sixDigitCodeRe.FindStringSubmatch(message)
	if len(matches) < 2 {
		matches = sixDigitCodeRe.FindStringSubmatch(combined)
	}
	if len(matches) < 2 {
		return "", false
	}
	return matches[1], true
}

func (s *Server) storeOTP(provider, code, sender, receivedAt string) otpCodeRecord {
	now := s.now()
	expiresAt := now.Add(s.relogin.OTP.TTL)
	record := otpCodeRecord{
		Provider:   strings.ToLower(provider),
		Code:       code,
		Sender:     sender,
		ReceivedAt: normalizeReceivedAt(receivedAt, now),
		StoredAt:   now.Format(time.RFC3339),
		ExpiresAt:  expiresAt.Format(time.RFC3339),
	}

	s.otpMu.Lock()
	defer s.otpMu.Unlock()
	s.pruneOTPLocked(now)
	s.otpCodes = append(s.otpCodes, record)
	if len(s.otpCodes) > 20 {
		s.otpCodes = s.otpCodes[len(s.otpCodes)-20:]
	}
	return record
}

func (s *Server) latestOTPRecord(provider string) (otpCodeRecord, bool) {
	now := s.now()
	s.otpMu.Lock()
	defer s.otpMu.Unlock()
	s.pruneOTPLocked(now)
	for i := len(s.otpCodes) - 1; i >= 0; i-- {
		record := s.otpCodes[i]
		if provider == "" || record.Provider == provider {
			return record, true
		}
	}
	return otpCodeRecord{}, false
}

func (s *Server) pruneOTPLocked(now time.Time) {
	if len(s.otpCodes) == 0 {
		return
	}
	kept := s.otpCodes[:0]
	for _, record := range s.otpCodes {
		expiresAt, err := time.Parse(time.RFC3339, record.ExpiresAt)
		if err == nil && now.After(expiresAt) {
			continue
		}
		kept = append(kept, record)
	}
	s.otpCodes = kept
}

func normalizeReceivedAt(value string, fallback time.Time) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback.Format(time.RFC3339)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.RFC1123Z, time.RFC1123} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format(time.RFC3339)
		}
	}
	var unixMillis int64
	if _, err := fmt.Sscanf(value, "%d", &unixMillis); err == nil && unixMillis > 0 {
		if unixMillis > 1_000_000_000_000 {
			return time.UnixMilli(unixMillis).Format(time.RFC3339)
		}
		return time.Unix(unixMillis, 0).Format(time.RFC3339)
	}
	return value
}
