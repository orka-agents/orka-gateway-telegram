package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		controlTokenEnv, controlTokenEnv + "_FILE", "LISTEN_ADDRESS", "SHUTDOWN_TIMEOUT",
		"TELEGRAM_FAULT_PROXY_BOT_ID", "TELEGRAM_FAULT_PROXY_DEFAULT_RETRY_AFTER",
		"TELEGRAM_FAULT_PROXY_DEFAULT_DELAY", "TELEGRAM_FAULT_PROXY_MAX_REQUEST_BODY_BYTES",
		"TELEGRAM_FAULT_PROXY_AUDIT_CAPACITY",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadConfigFromEnvironment(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv(controlTokenEnv, "environment-control-token")
	t.Setenv("LISTEN_ADDRESS", "127.0.0.1:19090")
	t.Setenv("SHUTDOWN_TIMEOUT", "3s")
	t.Setenv("TELEGRAM_FAULT_PROXY_BOT_ID", "12345")
	t.Setenv("TELEGRAM_FAULT_PROXY_DEFAULT_RETRY_AFTER", "7")
	t.Setenv("TELEGRAM_FAULT_PROXY_DEFAULT_DELAY", "25ms")
	t.Setenv("TELEGRAM_FAULT_PROXY_MAX_REQUEST_BODY_BYTES", "4096")
	t.Setenv("TELEGRAM_FAULT_PROXY_AUDIT_CAPACITY", "25")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "127.0.0.1:19090" || cfg.ShutdownTimeout != 3*time.Second ||
		cfg.Proxy.ControlToken != "environment-control-token" || cfg.Proxy.BotID != 12345 ||
		cfg.Proxy.DefaultRetryAfterSeconds != 7 || cfg.Proxy.DefaultDelay != 25*time.Millisecond ||
		cfg.Proxy.MaxRequestBodyBytes != 4096 || cfg.Proxy.AuditCapacity != 25 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigFromTokenFile(t *testing.T) {
	clearConfigEnvironment(t)
	token := "file-control-token-that-must-not-appear-in-errors"
	path := filepath.Join(t.TempDir(), "control-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(controlTokenEnv+"_FILE", path)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.ControlToken != token || cfg.ListenAddress != defaultAddress || cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("unexpected file config: %+v", cfg)
	}
}

func TestLoadConfigRejectsMissingOrAmbiguousControlToken(t *testing.T) {
	clearConfigEnvironment(t)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected missing control token error")
	}

	secret := "secret-value-must-not-be-in-error"
	path := filepath.Join(t.TempDir(), "control-token")
	if err := os.WriteFile(path, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(controlTokenEnv, secret)
	t.Setenv(controlTokenEnv+"_FILE", path)
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected env/file conflict")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error disclosed control token: %v", err)
	}
}

func TestLoadConfigRejectsInvalidNumericAndDurationValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "SHUTDOWN_TIMEOUT", value: "0s"},
		{name: "TELEGRAM_FAULT_PROXY_BOT_ID", value: "-1"},
		{name: "TELEGRAM_FAULT_PROXY_DEFAULT_RETRY_AFTER", value: "-1"},
		{name: "TELEGRAM_FAULT_PROXY_DEFAULT_DELAY", value: "-1ms"},
		{name: "TELEGRAM_FAULT_PROXY_MAX_REQUEST_BODY_BYTES", value: "-1"},
		{name: "TELEGRAM_FAULT_PROXY_AUDIT_CAPACITY", value: "nope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv(controlTokenEnv, "control-token")
			t.Setenv(test.name, test.value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("expected %s=%q to be rejected", test.name, test.value)
			}
		})
	}
}
