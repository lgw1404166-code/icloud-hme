package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvGroupsReloginConfigAndKeepsExistingEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(`
# 纯协议自动重新登录 + OTP
ICLOUD_HME_RELOGIN_ENABLED=false
ICLOUD_HME_RELOGIN_MODE=apple_protocol_sms
ICLOUD_HME_RELOGIN_APPLE_ID="user@example.com"
export ICLOUD_HME_RELOGIN_OTP_TTL=5m
`), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ICLOUD_HME_RELOGIN_ENABLED", "true")
	t.Cleanup(func() {
		_ = os.Unsetenv("ICLOUD_HME_RELOGIN_APPLE_ID")
		_ = os.Unsetenv("ICLOUD_HME_RELOGIN_MODE")
		_ = os.Unsetenv("ICLOUD_HME_RELOGIN_OTP_TTL")
	})
	_ = os.Unsetenv("ICLOUD_HME_RELOGIN_APPLE_ID")
	_ = os.Unsetenv("ICLOUD_HME_RELOGIN_MODE")
	_ = os.Unsetenv("ICLOUD_HME_RELOGIN_OTP_TTL")

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ICLOUD_HME_RELOGIN_ENABLED"); got != "true" {
		t.Fatalf("existing env was overridden: %q", got)
	}
	if got := os.Getenv("ICLOUD_HME_RELOGIN_APPLE_ID"); got != "user@example.com" {
		t.Fatalf("apple id = %q", got)
	}
	if got := os.Getenv("ICLOUD_HME_RELOGIN_OTP_TTL"); got != "5m" {
		t.Fatalf("otp ttl = %q", got)
	}
}
