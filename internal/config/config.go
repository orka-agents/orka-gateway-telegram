package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddress         string
	DatabasePath          string
	TelegramAPIBaseURL    string
	TelegramBotToken      string
	TelegramWebhookSecret string
	TelegramWebhookURL    string
	DropPendingUpdates    bool
	OrkaIngressURL        string
	OrkaInboundToken      string
	OrkaOutboundToken     string
	ConformanceChatID     int64
	RequestTimeout        time.Duration
	ShutdownTimeout       time.Duration
	MaxWebhookBodyBytes   int64
	MaxDeliveryBodyBytes  int64
	AdapterName           string
	AdapterVersion        string
}

func Load() (Config, error) {
	botToken, err := secret("TELEGRAM_BOT_TOKEN")
	if err != nil {
		return Config{}, err
	}
	webhookSecret, err := secret("TELEGRAM_WEBHOOK_SECRET")
	if err != nil {
		return Config{}, err
	}
	inboundToken, err := secret("ORKA_GATEWAY_INBOUND_TOKEN")
	if err != nil {
		return Config{}, err
	}
	outboundToken, err := secret("ORKA_GATEWAY_OUTBOUND_TOKEN")
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := durationEnv("REQUEST_TIMEOUT", 15*time.Second)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := durationEnv("SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return Config{}, err
	}
	chatID, err := int64Env("TELEGRAM_CONFORMANCE_CHAT_ID", 0)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddress:         envOr("LISTEN_ADDRESS", ":8080"),
		DatabasePath:          envOr("DATABASE_PATH", "/data/telegram-adapter.db"),
		TelegramAPIBaseURL:    strings.TrimRight(envOr("TELEGRAM_API_BASE_URL", "https://api.telegram.org"), "/"),
		TelegramBotToken:      botToken,
		TelegramWebhookSecret: webhookSecret,
		TelegramWebhookURL:    strings.TrimSpace(os.Getenv("TELEGRAM_WEBHOOK_URL")),
		DropPendingUpdates:    boolEnv("TELEGRAM_DROP_PENDING_UPDATES", false),
		OrkaIngressURL:        strings.TrimSpace(os.Getenv("ORKA_GATEWAY_INGRESS_URL")),
		OrkaInboundToken:      inboundToken,
		OrkaOutboundToken:     outboundToken,
		ConformanceChatID:     chatID,
		RequestTimeout:        requestTimeout,
		ShutdownTimeout:       shutdownTimeout,
		MaxWebhookBodyBytes:   256 << 10,
		MaxDeliveryBodyBytes:  256 << 10,
		AdapterName:           "orka-gateway-telegram",
		AdapterVersion:        envOr("ADAPTER_VERSION", "dev"),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	for name, value := range map[string]string{
		"TELEGRAM_BOT_TOKEN":          c.TelegramBotToken,
		"TELEGRAM_WEBHOOK_SECRET":     c.TelegramWebhookSecret,
		"ORKA_GATEWAY_INGRESS_URL":    c.OrkaIngressURL,
		"ORKA_GATEWAY_INBOUND_TOKEN":  c.OrkaInboundToken,
		"ORKA_GATEWAY_OUTBOUND_TOKEN": c.OrkaOutboundToken,
		"DATABASE_PATH":               c.DatabasePath,
	} {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	if c.OrkaInboundToken != "" && c.OrkaInboundToken == c.OrkaOutboundToken {
		errs = append(errs, errors.New("orka inbound and outbound tokens must differ"))
	}
	if len(c.TelegramWebhookSecret) < 16 || len(c.TelegramWebhookSecret) > 256 {
		errs = append(errs, errors.New("telegram webhook secret must be 16-256 characters"))
	}
	return errors.Join(errs...)
}

func secret(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	file := strings.TrimSpace(os.Getenv(name + "_FILE"))
	if value != "" && file != "" {
		return "", fmt.Errorf("set only one of %s or %s_FILE", name, name)
	}
	if file == "" {
		return value, nil
	}
	info, err := os.Lstat(file) // #nosec G703 -- operator-owned process configuration path
	if err != nil {
		return "", fmt.Errorf("inspect %s_FILE: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s_FILE must name a regular non-symlink file", name)
	}
	if info.Mode().Perm()&0o027 != 0 {
		return "", fmt.Errorf("%s_FILE must not be group-writable or world-accessible", name)
	}
	data, err := os.ReadFile(file) // #nosec G304,G703 -- operator-owned process configuration validated above
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func boolEnv(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func int64Env(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}
