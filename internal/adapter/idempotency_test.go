package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sozercan/orka-gateway-telegram/internal/protocol"
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
