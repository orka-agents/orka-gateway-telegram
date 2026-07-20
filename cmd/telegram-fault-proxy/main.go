package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sozercan/orka-gateway-telegram/internal/faultproxy"
)

const (
	controlTokenEnv = "TELEGRAM_FAULT_PROXY_CONTROL_TOKEN"
	defaultAddress  = ":8081"
)

type appConfig struct {
	ListenAddress   string
	ShutdownTimeout time.Duration
	Proxy           faultproxy.Config
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("Telegram fault proxy stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	proxy, err := faultproxy.New(cfg.Proxy)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           proxy.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Suppress net/http's free-form connection diagnostics so a malformed
		// token-bearing request target can never reach process logs.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("starting Telegram fault proxy", "address", cfg.ListenAddress)
		serverErr <- httpServer.ListenAndServe()
	}()

	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownContext)
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func loadConfig() (appConfig, error) {
	controlToken, err := loadSecret(controlTokenEnv)
	if err != nil {
		return appConfig{}, err
	}
	shutdownTimeout, err := positiveDurationEnv("SHUTDOWN_TIMEOUT", 10*time.Second)
	if err != nil {
		return appConfig{}, err
	}
	botID, err := nonNegativeInt64Env("TELEGRAM_FAULT_PROXY_BOT_ID", 0)
	if err != nil {
		return appConfig{}, err
	}
	defaultRetryAfter, err := nonNegativeIntEnv("TELEGRAM_FAULT_PROXY_DEFAULT_RETRY_AFTER", 0)
	if err != nil {
		return appConfig{}, err
	}
	defaultDelay, err := nonNegativeDurationEnv("TELEGRAM_FAULT_PROXY_DEFAULT_DELAY", 0)
	if err != nil {
		return appConfig{}, err
	}
	maxBodyBytes, err := nonNegativeInt64Env("TELEGRAM_FAULT_PROXY_MAX_REQUEST_BODY_BYTES", 0)
	if err != nil {
		return appConfig{}, err
	}
	auditCapacity, err := nonNegativeIntEnv("TELEGRAM_FAULT_PROXY_AUDIT_CAPACITY", 0)
	if err != nil {
		return appConfig{}, err
	}

	address := strings.TrimSpace(os.Getenv("LISTEN_ADDRESS"))
	if address == "" {
		address = defaultAddress
	}
	return appConfig{
		ListenAddress:   address,
		ShutdownTimeout: shutdownTimeout,
		Proxy: faultproxy.Config{
			ControlToken:             controlToken,
			BotID:                    botID,
			DefaultRetryAfterSeconds: defaultRetryAfter,
			DefaultDelay:             defaultDelay,
			MaxRequestBodyBytes:      maxBodyBytes,
			AuditCapacity:            auditCapacity,
		},
	}, nil
}

func loadSecret(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	filePath := strings.TrimSpace(os.Getenv(name + "_FILE"))
	if value != "" && filePath != "" {
		return "", fmt.Errorf("set only one of %s or %s_FILE", name, name)
	}
	if filePath != "" {
		contents, err := os.ReadFile(filePath) // #nosec G304 -- operator-supplied secret file path
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", name, err)
		}
		value = strings.TrimSpace(string(contents))
	}
	if value == "" {
		return "", fmt.Errorf("%s or %s_FILE is required", name, name)
	}
	return value, nil
}

func positiveDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
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

func nonNegativeDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration", name)
	}
	return parsed, nil
}

func nonNegativeIntEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func nonNegativeInt64Env(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}
