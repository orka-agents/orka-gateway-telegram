package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBotToken() string      { return "123456:" + strings.Repeat("t", 24) }
func testWebhookSecret() string { return strings.Repeat("w", 32) }
func testInboundToken() string  { return strings.Repeat("i", 32) }
func testOutboundToken() string { return strings.Repeat("o", 32) }

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("TELEGRAM_BOT_TOKEN", testBotToken())
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", testWebhookSecret())
	t.Setenv("ORKA_GATEWAY_INGRESS_URL", "http://orka/api/v1/gateways/ns/gw/events")
	t.Setenv("ORKA_GATEWAY_INBOUND_TOKEN", testInboundToken())
	t.Setenv("ORKA_GATEWAY_OUTBOUND_TOKEN", testOutboundToken())
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "adapter.db"))
}

func TestLoad(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != ":8080" || cfg.TelegramBotToken != testBotToken() {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadSecretFile(t *testing.T) {
	setRequired(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testBotToken()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN_FILE", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramBotToken != testBotToken() {
		t.Fatalf("token = %q", cfg.TelegramBotToken)
	}
}

func TestLoadRejectsSharedOrkaToken(t *testing.T) {
	setRequired(t)
	t.Setenv("ORKA_GATEWAY_OUTBOUND_TOKEN", testInboundToken())
	if _, err := Load(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadAcceptsGroupReadableProjectedSecret(t *testing.T) {
	setRequired(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testBotToken()), 0o440); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN_FILE", path)
	if _, err := Load(); err != nil {
		t.Fatalf("group-readable projected secret rejected: %v", err)
	}
}

func TestLoadRejectsWorldReadableSecretFile(t *testing.T) {
	setRequired(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testBotToken()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN_FILE", path)
	if _, err := Load(); err == nil {
		t.Fatal("expected world-readable secret file rejection")
	}
}

func TestLoadRejectsSecretFileSymlink(t *testing.T) {
	setRequired(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(testBotToken()), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN_FILE", link)
	if _, err := Load(); err == nil {
		t.Fatal("expected symlink secret file rejection")
	}
}
