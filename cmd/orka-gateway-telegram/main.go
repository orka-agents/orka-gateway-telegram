package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sozercan/orka-gateway-telegram/internal/adapter"
	"github.com/sozercan/orka-gateway-telegram/internal/config"
	"github.com/sozercan/orka-gateway-telegram/internal/store"
	"github.com/sozercan/orka-gateway-telegram/internal/telegram"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("adapter stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.AdapterVersion == "dev" && version != "" {
		cfg.AdapterVersion = version
	}
	database, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck
	if err := pruneStore(context.Background(), logger, database, cfg.RecordRetention, cfg.RequestTimeout); err != nil {
		logger.Warn("initial terminal-record pruning failed; continuing startup", "error", err.Error())
	}

	telegramClient, err := telegram.NewClient(cfg.TelegramAPIBaseURL, cfg.TelegramBotToken, telegram.WithHTTPClient(&http.Client{Timeout: cfg.RequestTimeout}))
	if err != nil {
		return err
	}
	getMeCtx, getMeCancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
	bot, err := telegramClient.GetMe(getMeCtx)
	getMeCancel()
	if err != nil {
		return err
	}

	server, err := adapter.New(adapter.Config{
		BotID: bot.ID, WebhookSecret: cfg.TelegramWebhookSecret,
		OutboundBearerToken: cfg.OrkaOutboundToken, IngressURL: cfg.OrkaIngressURL,
		IngressBearerToken: cfg.OrkaInboundToken, ConformanceChatID: cfg.ConformanceChatID,
		AdapterName: cfg.AdapterName, AdapterVersion: cfg.AdapterVersion,
		MaxWebhookBodyBytes: cfg.MaxWebhookBodyBytes, MaxDeliveryBodyBytes: cfg.MaxDeliveryBodyBytes,
		HTTPClient: &http.Client{Timeout: cfg.RequestTimeout}, Logger: logger,
	}, database, telegramClient)
	if err != nil {
		return err
	}
	if cfg.TelegramWebhookURL != "" {
		webhookCtx, webhookCancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
		err = telegramClient.SetWebhook(webhookCtx, cfg.TelegramWebhookURL, cfg.TelegramWebhookSecret, cfg.DropPendingUpdates)
		webhookCancel()
		if err != nil {
			return err
		}
	}

	httpServer := &http.Server{
		Addr: cfg.ListenAddress, Handler: server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.RequestTimeout,
		WriteTimeout:      cfg.RequestTimeout + 15*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("starting Telegram gateway adapter", "address", cfg.ListenAddress, "bot_id", bot.ID, "version", cfg.AdapterVersion)
		serverErr <- httpServer.ListenAndServe()
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cleanupTicker := time.NewTicker(cfg.CleanupInterval)
	defer cleanupTicker.Stop()
	for {
		select {
		case <-signalCtx.Done():
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer shutdownCancel()
			return httpServer.Shutdown(shutdownCtx)
		case err := <-serverErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-cleanupTicker.C:
			if err := pruneStore(signalCtx, logger, database, cfg.RecordRetention, cfg.RequestTimeout); err != nil && signalCtx.Err() == nil {
				logger.Error("failed to prune terminal gateway records", "error", err.Error())
			}
		}
	}
}

func pruneStore(parent context.Context, logger *slog.Logger, database *store.Store, retention, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	result, err := database.PruneTerminalBefore(ctx, time.Now().UTC().Add(-retention))
	if err != nil {
		return err
	}
	if result.Updates > 0 || result.Deliveries > 0 {
		logger.Info("pruned terminal gateway records", "updates", result.Updates, "deliveries", result.Deliveries)
	}
	return nil
}
