package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/orka-gateway-telegram/internal/protocol"
	"github.com/sozercan/orka-gateway-telegram/internal/store"
	"github.com/sozercan/orka-gateway-telegram/internal/telegram"
)

const telegramWebhookAuthHeader = "X-Telegram-Bot-Api-Secret-Token"

type TelegramSender interface {
	SendMessage(context.Context, telegram.ReplyTarget, string) (telegram.SendResult, error)
}

type Config struct {
	BotID                int64
	WebhookSecret        string
	OutboundBearerToken  string
	IngressURL           string
	IngressBearerToken   string
	ConformanceChatID    int64
	AdapterName          string
	AdapterVersion       string
	MaxWebhookBodyBytes  int64
	MaxDeliveryBodyBytes int64
	MaxUpstreamBodyBytes int64
	HTTPClient           *http.Client
	Logger               *slog.Logger
}

type Server struct {
	config     Config
	store      *store.Store
	telegram   TelegramSender
	client     *http.Client
	logger     *slog.Logger
	handler    http.Handler
	providerMu sync.Mutex
	chatLocks  [64]sync.Mutex
}

func New(config Config, database *store.Store, telegramClient TelegramSender) (*Server, error) {
	if database == nil {
		return nil, errors.New("store is required")
	}
	if telegramClient == nil {
		return nil, errors.New("telegram client is required")
	}
	if config.BotID <= 0 || strings.TrimSpace(config.WebhookSecret) == "" ||
		strings.TrimSpace(config.OutboundBearerToken) == "" || strings.TrimSpace(config.IngressBearerToken) == "" ||
		strings.TrimSpace(config.IngressURL) == "" {
		return nil, errors.New("adapter configuration is incomplete")
	}
	if config.IngressBearerToken == config.OutboundBearerToken {
		return nil, errors.New("inbound and outbound Orka bearer tokens must differ")
	}
	ingressURL, err := validateIngressURL(config.IngressURL)
	if err != nil {
		return nil, err
	}
	config.IngressURL = ingressURL
	if config.AdapterName == "" {
		config.AdapterName = "orka-gateway-telegram"
	}
	if config.AdapterVersion == "" {
		config.AdapterVersion = "dev"
	}
	if config.MaxWebhookBodyBytes <= 0 {
		config.MaxWebhookBodyBytes = protocol.MaxHTTPBodyBytes
	}
	if config.MaxDeliveryBodyBytes <= 0 {
		config.MaxDeliveryBodyBytes = protocol.MaxHTTPBodyBytes
	}
	if config.MaxUpstreamBodyBytes <= 0 {
		config.MaxUpstreamBodyBytes = protocol.MaxAdapterResponseBytes
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	} else {
		copyClient := *client
		client = &copyClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		client.Transport = transport
	} else if transport, ok := client.Transport.(*http.Transport); ok {
		clone := transport.Clone()
		clone.Proxy = nil
		client.Transport = clone
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{config: config, store: database, telegram: telegramClient, client: client, logger: logger}
	server.handler = server.routes()
	return server, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleKubernetesHealth)
	mux.HandleFunc("GET /readyz", s.handleKubernetesReady)
	mux.HandleFunc("GET /v1/health", s.auth(s.handleHealth))
	mux.HandleFunc("GET /v1/capabilities", s.auth(s.handleCapabilities))
	mux.HandleFunc("POST /v1/deliveries", s.auth(s.handleDelivery))
	mux.HandleFunc("POST /telegram/webhook", s.handleTelegramWebhook)
	mux.HandleFunc("POST /v1/telegram/webhook", s.handleTelegramWebhook)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !protocol.ConstantTimeBearerEqual(protocol.BearerToken(r.Header.Get("Authorization")), s.config.OutboundBearerToken) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleKubernetesHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleKubernetesReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{Status: "ok"})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.CapabilitiesResponse{
		ProtocolVersion: protocol.Version,
		AdapterName:     s.config.AdapterName,
		AdapterVersion:  s.config.AdapterVersion,
		Capabilities: protocol.Capabilities{
			// The live adapter intentionally supports private chats only. Thread
			// transport fields are retained for forward compatibility but are not
			// advertised until group/forum ingress is supported.
			InboundText: true, OutboundText: true, Threads: false, SenderIdentity: true, IdempotentDelivery: true,
		},
	})
}

func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if !protocol.ConstantTimeBearerEqual(r.Header.Get(telegramWebhookAuthHeader), s.config.WebhookSecret) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	body, err := readBoundedBody(r.Body, s.config.MaxWebhookBodyBytes)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "invalid request body"})
		return
	}
	var update telegram.Update
	if err := decodeSingleJSON(body, &update, false); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Telegram update"})
		return
	}
	event, err := telegram.MapUpdate(s.config.BotID, update)
	if errors.Is(err, telegram.ErrUnsupportedUpdate) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Telegram update"})
		return
	}
	// setWebhook limits Telegram to one in-flight delivery, preserving provider
	// arrival order. This chat-scoped lock additionally serializes authenticated
	// retries and direct conformance traffic within the single-replica adapter.
	chatID, err := strconv.ParseInt(event.ContextID, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Telegram update"})
		return
	}
	lockIndex := chatID % int64(len(s.chatLocks))
	if lockIndex < 0 {
		lockIndex += int64(len(s.chatLocks))
	}
	lock := &s.chatLocks[lockIndex]
	lock.Lock()
	defer lock.Unlock()

	normalized, err := json.Marshal(update)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Telegram update"})
		return
	}
	digest := store.DigestBytes(normalized)
	record, _, err := s.store.CreateUpdate(r.Context(), update.UpdateID, digest)
	if errors.Is(err, store.ErrDigestConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Telegram update identity conflict"})
		return
	}
	if err != nil {
		s.logger.Error("failed to persist Telegram update", "update_id", update.UpdateID, "error", safeError(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}
	if record.HasOrkaResponse() {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	responseBody, ingress, err := s.admitEvent(r.Context(), event)
	if err != nil {
		s.logger.Warn("Orka ingress unavailable", "update_id", update.UpdateID, "error", safeError(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}
	if _, err := s.store.SaveUpdateResponse(r.Context(), update.UpdateID, digest, responseBody); err != nil {
		if errors.Is(err, store.ErrInvalidTransition) {
			stored, getErr := s.store.GetUpdate(r.Context(), update.UpdateID)
			if getErr == nil && stored.HasOrkaResponse() {
				writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
				return
			}
		}
		s.logger.Error("failed to persist Orka acknowledgement", "update_id", update.UpdateID, "event_id", ingress.EventID, "error", safeError(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}
	s.logger.Info("Telegram update admitted", "update_id", update.UpdateID, "event_id", ingress.EventID, "status", ingress.Status)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) admitEvent(ctx context.Context, event *protocol.EventEnvelope) (json.RawMessage, *protocol.IngressResponse, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.IngressURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+s.config.IngressBearerToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, nil, errors.New("orka ingress request failed")
	}
	defer response.Body.Close() //nolint:errcheck
	responseBody, err := readBoundedBody(response.Body, s.config.MaxUpstreamBodyBytes)
	if err != nil {
		return nil, nil, errors.New("orka ingress response was invalid")
	}
	if response.StatusCode != http.StatusAccepted {
		return nil, nil, fmt.Errorf("orka ingress returned HTTP %d", response.StatusCode)
	}
	var ingress protocol.IngressResponse
	if err := decodeSingleJSON(responseBody, &ingress, true); err != nil {
		return nil, nil, errors.New("orka ingress returned an invalid acknowledgement")
	}
	if err := protocol.ValidateIngressResponse(&ingress); err != nil {
		return nil, nil, errors.New("orka ingress returned an invalid acknowledgement")
	}
	return append(json.RawMessage(nil), responseBody...), &ingress, nil
}

func (s *Server) handleDelivery(w http.ResponseWriter, r *http.Request) {
	body, err := readBoundedBody(r.Body, s.config.MaxDeliveryBodyBytes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: "invalid request body"})
		return
	}
	request, err := protocol.DecodeDeliveryRequest(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: "invalid delivery"})
		return
	}
	digest, legacyDigest, err := deliveryRequestDigests(request)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: "invalid delivery"})
		return
	}
	inspected, found, err := s.store.InspectDeliveryWithIdempotency(
		r.Context(), request.DeliveryID, request.IdempotencyID, digest, legacyDigest,
	)
	if errors.Is(err, store.ErrDigestConflict) {
		writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: "delivery identity conflict"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: "delivery store unavailable"})
		return
	}
	if found && inspected.Terminal() {
		// Canonicalize aliases and legacy metadata without claiming provider work.
		record, _, beginErr := s.store.BeginDeliveryWithIdempotency(
			r.Context(), request.DeliveryID, request.IdempotencyID, digest, legacyDigest,
		)
		if beginErr != nil {
			writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: "delivery store unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, replayDelivery(record))
		return
	}

	// Telegram exposes retry_after without identifying a narrower rate-limit
	// scope. This one-bot adapter intentionally permits only one provider call at
	// a time so the durable bot-wide cooldown is exact. Immutable terminal
	// replays above stay outside the gate; sending records enter it so the store's
	// stale-lease recovery remains reachable.
	//
	// Serialize the durable sending claim through provider-result persistence.
	// A request waiting for this gate has no sending record, so a restart cannot
	// misclassify work that never reached Telegram as an ambiguous outcome.
	s.providerMu.Lock()
	defer s.providerMu.Unlock()
	record, shouldSend, err := s.store.BeginDeliveryWithIdempotency(
		r.Context(), request.DeliveryID, request.IdempotencyID, digest, legacyDigest,
	)
	if errors.Is(err, store.ErrDigestConflict) {
		writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: "delivery identity conflict"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: "delivery store unavailable"})
		return
	}
	if !shouldSend {
		writeJSON(w, http.StatusOK, replayDelivery(record))
		return
	}
	canonicalDeliveryID := record.DeliveryID
	target, err := telegram.ParseReplyTarget(request.ReplyTarget, s.config.ConformanceChatID)
	if err == nil && !s.validDeliveryRoute(*request, target) {
		err = errors.New("delivery routing mismatch")
	}
	if err != nil {
		persistCtx, cancel := deliveryPersistenceContext(r.Context())
		defer cancel()
		record, storeErr := s.store.MarkDeliveryPermanent(persistCtx, canonicalDeliveryID, digest, "delivery routing mismatch")
		if storeErr != nil {
			writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: "delivery store unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, replayDelivery(record))
		return
	}
	canonicalRequest := *request
	canonicalRequest.DeliveryID = canonicalDeliveryID
	record, result, err := s.executeTelegramDelivery(r.Context(), canonicalRequest, target, digest)
	if err != nil {
		s.logger.Error("failed to persist Telegram delivery result", "delivery_id", request.DeliveryID, "error", safeError(err))
		writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: "delivery store unavailable"})
		return
	}
	s.logger.Info("Telegram delivery processed", "delivery_id", canonicalDeliveryID, "state", record.State, "truncated", result.Truncated)
	writeJSON(w, http.StatusOK, replayDelivery(record))
}

func (s *Server) executeTelegramDelivery(
	ctx context.Context,
	request protocol.DeliveryRequest,
	target telegram.ReplyTarget,
	digest string,
) (store.DeliveryRecord, telegram.SendResult, error) {
	result, sendErr := s.telegram.SendMessage(ctx, target, request.Text)
	persistCtx, cancelPersist := deliveryPersistenceContext(ctx)
	defer cancelPersist()
	record, err := s.persistSendResult(persistCtx, request, digest, result, sendErr)
	return record, result, err
}

func (s *Server) validDeliveryRoute(request protocol.DeliveryRequest, target telegram.ReplyTarget) bool {
	if request.ReplyTarget == telegram.ConformanceReplyTarget {
		return request.AccountID == telegram.ConformanceReplyTarget &&
			request.ContextID == telegram.ConformanceReplyTarget &&
			request.ThreadID == "" &&
			target.ChatID == s.config.ConformanceChatID &&
			target.ThreadID == 0 && target.MessageID == 0
	}
	expectedThreadID := ""
	if target.ThreadID != 0 {
		expectedThreadID = strconv.FormatInt(target.ThreadID, 10)
	}
	return request.AccountID == strconv.FormatInt(s.config.BotID, 10) &&
		request.ContextID == strconv.FormatInt(target.ChatID, 10) &&
		request.ThreadID == expectedThreadID
}

func (s *Server) persistSendResult(
	ctx context.Context,
	request protocol.DeliveryRequest,
	digest string,
	result telegram.SendResult,
	sendErr error,
) (store.DeliveryRecord, error) {
	message := result.Description
	if sendErr != nil && message == "" {
		message = safeError(sendErr)
	}
	switch result.Classification {
	case telegram.ResultDelivered:
		return s.store.MarkDeliveryDelivered(ctx, request.DeliveryID, digest, result.ProviderMessageID)
	case telegram.ResultRetryable:
		if result.RetryAfter > 0 {
			return s.store.MarkDeliveryRetryableNotBefore(
				ctx, request.DeliveryID, digest, message, time.Now().UTC().Add(result.RetryAfter),
			)
		}
		return s.store.MarkDeliveryRetryable(ctx, request.DeliveryID, digest, message)
	case telegram.ResultNonRetryable:
		return s.store.MarkDeliveryPermanent(ctx, request.DeliveryID, digest, message)
	case telegram.ResultAmbiguous:
		return s.store.MarkDeliveryUnknown(ctx, request.DeliveryID, digest, message)
	default:
		return s.store.MarkDeliveryUnknown(ctx, request.DeliveryID, digest, "Telegram delivery outcome is unknown")
	}
}

func deliveryPersistenceContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
}

func replayDelivery(record store.DeliveryRecord) protocol.DeliveryResponse {
	switch record.State {
	case store.DeliveryStateDelivered:
		return protocol.DeliveryResponse{Status: protocol.DeliveryStatusDelivered, ProviderMessageID: record.ProviderMessageID}
	case store.DeliveryStateRetryable, store.DeliveryStateSending:
		message := record.SafeMessage
		if message == "" {
			message = "delivery is already in progress"
		}
		return protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: message}
	case store.DeliveryStatePermanent, store.DeliveryStateUnknown:
		message := record.SafeMessage
		if message == "" {
			message = "delivery could not be completed"
		}
		return protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: message}
	default:
		return protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: "delivery state is unavailable"}
	}
}

func validateIngressURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("orka ingress URL must be absolute")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("orka ingress URL must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("orka ingress URL must not contain credentials, query, or fragment")
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != 6 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "gateways" || parts[3] == "" || parts[4] == "" || parts[5] != "events" {
		return "", errors.New("orka ingress URL path is invalid")
	}
	if parsed.Scheme == "http" && !isTrustedPlaintextIngressHost(parsed.Hostname()) {
		return "", errors.New("plaintext Orka ingress is allowed only for loopback or Kubernetes Service DNS")
	}
	return parsed.String(), nil
}

func isTrustedPlaintextIngressHost(host string) bool {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func readBoundedBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("body exceeds limit")
	}
	return body, nil
}

func decodeSingleJSON(body []byte, target any, disallowUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return protocol.SanitizeMessage(err.Error(), 512)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
