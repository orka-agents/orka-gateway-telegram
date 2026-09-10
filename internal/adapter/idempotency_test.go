package adapter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/orka-gateway-telegram/internal/protocol"
	"github.com/sozercan/orka-gateway-telegram/internal/store"
	"github.com/sozercan/orka-gateway-telegram/internal/telegram"
)

type sequenceTelegram struct {
	mu      sync.Mutex
	results []telegram.SendResult
	count   int
}

func TestDeliveryRequestDigestsSeparateLogicalIdentityFromAttempt(t *testing.T) {
	first := protocol.DeliveryRequest{
		ProtocolVersion: protocol.Version, DeliveryID: "delivery-first", IdempotencyID: "stable-key",
		OriginatingEvent: "event", Kind: protocol.DeliveryKindFinal, AccountID: "42", ContextID: "100",
		ReplyTarget: "tg:v1:100:0:9", Text: "reply",
	}
	second := first
	second.DeliveryID = "delivery-second"
	firstLogical, firstLegacy, err := deliveryRequestDigests(&first)
	if err != nil {
		t.Fatal(err)
	}
	secondLogical, secondLegacy, err := deliveryRequestDigests(&second)
	if err != nil {
		t.Fatal(err)
	}
	if firstLogical != secondLogical {
		t.Fatalf("logical digests differ: %s != %s", firstLogical, secondLogical)
	}
	if firstLegacy == secondLegacy {
		t.Fatal("legacy attempt digests unexpectedly match")
	}
}

func (s *sequenceTelegram) SendMessage(_ context.Context, _ telegram.ReplyTarget, _ string) (telegram.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	if len(s.results) == 0 {
		return telegram.SendResult{Classification: telegram.ResultAmbiguous, Description: "unexpected send"}, nil
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func TestDeliveryIdempotencyIDSuppressesNewDeliveryID(t *testing.T) {
	sender := &fakeTelegram{result: telegram.SendResult{
		Classification: telegram.ResultDelivered, ProviderMessageID: "telegram:100:123", MessageID: 123,
	}}
	server, _ := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), sender)

	responses := make([]protocol.DeliveryResponse, 0, 2)
	for _, deliveryID := range []string{"delivery-idempotency-first", "delivery-idempotency-retry"} {
		body, err := json.Marshal(protocol.DeliveryRequest{
			ProtocolVersion:  protocol.Version,
			DeliveryID:       deliveryID,
			IdempotencyID:    "stable-idempotency-key",
			OriginatingEvent: "event-idempotency",
			Kind:             protocol.DeliveryKindFinal,
			AccountID:        "42",
			ContextID:        "100",
			ReplyTarget:      "tg:v1:100:0:9",
			Text:             "reply",
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/deliveries", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testOutboundToken())
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("delivery %q status = %d body=%s", deliveryID, response.Code, response.Body.String())
		}
		var decoded protocol.DeliveryResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, decoded)
	}

	if sender.count != 1 {
		t.Fatalf("Telegram sends = %d, want 1", sender.count)
	}
	if responses[0].Status != protocol.DeliveryStatusDelivered || responses[1] != responses[0] {
		t.Fatalf("responses = %#v, want identical delivered replay", responses)
	}
}

func TestDeliveryIdempotencyIDClaimsRetryWithNewDeliveryIDOnce(t *testing.T) {
	sender := &sequenceTelegram{results: []telegram.SendResult{
		{Classification: telegram.ResultRetryable, Description: "temporary provider failure"},
		{Classification: telegram.ResultDelivered, ProviderMessageID: "telegram:100:456", MessageID: 456},
	}}
	server, _ := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), sender)

	post := func(deliveryID string) protocol.DeliveryResponse {
		t.Helper()
		body, err := json.Marshal(protocol.DeliveryRequest{
			ProtocolVersion: protocol.Version,
			DeliveryID:      deliveryID, IdempotencyID: "stable-retry-key", OriginatingEvent: "event-retry",
			Kind: protocol.DeliveryKindFinal, AccountID: "42", ContextID: "100",
			ReplyTarget: "tg:v1:100:0:9", Text: "reply",
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/deliveries", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testOutboundToken())
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("delivery %q status = %d body=%s", deliveryID, response.Code, response.Body.String())
		}
		var decoded protocol.DeliveryResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}

	first := post("delivery-retry-first")
	second := post("delivery-retry-second")
	third := post("delivery-retry-third")
	if first.Status != protocol.DeliveryStatusRetryableError {
		t.Fatalf("first response = %+v", first)
	}
	if second.Status != protocol.DeliveryStatusDelivered || third != second {
		t.Fatalf("delivered responses = second:%+v third:%+v", second, third)
	}
	if sender.count != 2 {
		t.Fatalf("Telegram sends = %d, want 2", sender.count)
	}
}

func TestLegacyDeliveryReplayMigratesWithoutResending(t *testing.T) {
	for _, test := range []struct {
		name               string
		distinctStableID   bool
		changeFirstAttempt bool
	}{
		{name: "equal IDs"},
		{name: "equal IDs with a new first attempt", changeFirstAttempt: true},
		{name: "distinct IDs replay the original attempt", distinctStableID: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := protocol.DeliveryRequest{
				ProtocolVersion: protocol.Version, DeliveryID: "legacy-delivery", IdempotencyID: "legacy-delivery",
				OriginatingEvent: "event", Kind: protocol.DeliveryKindFinal, AccountID: "42", ContextID: "100",
				ReplyTarget: "tg:v1:100:0:9", Text: "reply",
			}
			if test.distinctStableID {
				request.IdempotencyID = "legacy-stable-id"
			}
			path := filepath.Join(t.TempDir(), "adapter.db")
			seedLegacyDeliveredRequest(t, path, request)
			originalID := request.DeliveryID
			if test.changeFirstAttempt {
				request.DeliveryID = "first-upgraded-attempt"
			}
			sender := &fakeTelegram{}
			for restart := 0; restart < 2; restart++ {
				database, err := store.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = database.Close() })
				server, err := New(Config{
					BotID: 42, WebhookSecret: testWebhookSecret(), OutboundBearerToken: testOutboundToken(),
					IngressURL: "http://localhost/api/v1/gateways/test/telegram/events", IngressBearerToken: testInboundToken(),
				}, database, sender)
				if err != nil {
					t.Fatal(err)
				}
				response := postTestDelivery(t, server, context.Background(), request)
				if response.Status != protocol.DeliveryStatusDelivered || response.ProviderMessageID != "telegram:100:123" {
					t.Fatalf("legacy replay after %d restarts = %+v", restart, response)
				}
				for _, identity := range []string{originalID, request.DeliveryID, request.IdempotencyID} {
					record, err := database.GetDelivery(context.Background(), identity)
					if err != nil || record.DeliveryID != originalID || record.IdempotencyID != request.IdempotencyID || record.DigestVersion != 2 {
						t.Fatalf("migrated identity %q = (%+v, %v)", identity, record, err)
					}
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
				request.DeliveryID = "new-attempt-after-restart"
			}
			if sender.count != 0 {
				t.Fatalf("legacy replay contacted Telegram %d times", sender.count)
			}
		})
	}
}

func seedLegacyDeliveredRequest(t *testing.T, path string, request protocol.DeliveryRequest) {
	t.Helper()
	// Use the released schema and its full-request digest to exercise migration
	// through the HTTP handler, including the adapter's JSON canonicalization.
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close() //nolint:errcheck
	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at_ms INTEGER NOT NULL) STRICT;
		INSERT INTO schema_migrations(version, applied_at_ms) VALUES (1, 1);
		CREATE TABLE telegram_updates (
			update_id INTEGER PRIMARY KEY,
			digest TEXT NOT NULL CHECK(length(digest) = 64),
			orka_response BLOB,
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL
		) STRICT;
		CREATE TABLE deliveries (
			delivery_id TEXT PRIMARY KEY,
			request_digest TEXT NOT NULL CHECK(length(request_digest) = 64),
			state TEXT NOT NULL CHECK(state IN ('sending', 'retryable', 'delivered', 'permanent', 'unknown')),
			provider_message_id TEXT NOT NULL DEFAULT '',
			safe_message TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL
		) STRICT;
	`); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO deliveries(
		delivery_id, request_digest, state, provider_message_id, safe_message, created_at_ms, updated_at_ms
	) VALUES (?, ?, 'delivered', 'telegram:100:123', '', 1, 1)`, request.DeliveryID, store.DigestBytes(payload)); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledDeliveryLeavesProviderQueueWithoutClaiming(t *testing.T) {
	sender := &blockingRateLimitSender{
		firstStarted: make(chan struct{}), secondStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
	releaseFirst := sync.OnceFunc(func() { close(sender.releaseFirst) })
	server, database := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), sender)
	request := protocol.DeliveryRequest{
		ProtocolVersion: protocol.Version, DeliveryID: "blocking-delivery", IdempotencyID: "blocking-delivery",
		OriginatingEvent: "event", Kind: protocol.DeliveryKindFinal, AccountID: "42", ContextID: "100",
		ReplyTarget: "tg:v1:100:0:9", Text: "reply",
	}
	firstDone := make(chan protocol.DeliveryResponse, 1)
	firstFinished := make(chan struct{})
	t.Cleanup(func() {
		releaseFirst()
		<-firstFinished
	})
	go func() {
		defer close(firstFinished)
		firstDone <- postTestDelivery(t, server, context.Background(), request)
	}()
	select {
	case <-sender.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first delivery did not reach Telegram")
	}
	waitingRequest := request
	waitingRequest.DeliveryID = "canceled-delivery"
	waitingRequest.IdempotencyID = waitingRequest.DeliveryID
	waitingCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitingDone := make(chan protocol.DeliveryResponse, 1)
	waitingFinished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseFirst()
		<-waitingFinished
	})
	go func() {
		defer close(waitingFinished)
		waitingDone <- postTestDelivery(t, server, waitingCtx, waitingRequest)
	}()
	select {
	case response := <-waitingDone:
		t.Fatalf("waiting request completed before cancellation: %+v", response)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case response := <-waitingDone:
		if response.Status != protocol.DeliveryStatusRetryableError {
			t.Fatalf("canceled request = %+v, want retryable", response)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled delivery remained blocked behind Telegram")
	}
	if _, err := database.GetDelivery(context.Background(), waitingRequest.DeliveryID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("canceled delivery was claimed: %v", err)
	}
	releaseFirst()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first delivery did not finish")
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.calls != 1 {
		t.Fatalf("Telegram sends = %d, want 1", sender.calls)
	}
}

func postTestDelivery(t *testing.T, server *Server, ctx context.Context, request protocol.DeliveryRequest) protocol.DeliveryResponse {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/deliveries", bytes.NewReader(body))
	httpRequest.Header.Set("Authorization", "Bearer "+testOutboundToken())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httpRequest)
	if response.Code != http.StatusOK {
		t.Fatalf("delivery status = %d body=%s", response.Code, response.Body.String())
	}
	var decoded protocol.DeliveryResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
