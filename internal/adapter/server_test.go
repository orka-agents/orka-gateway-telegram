package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/orka-gateway-telegram/internal/protocol"
	"github.com/sozercan/orka-gateway-telegram/internal/store"
	"github.com/sozercan/orka-gateway-telegram/internal/telegram"
)

func testWebhookSecret() string { return strings.Repeat("w", 32) }
func testInboundToken() string  { return strings.Repeat("i", 32) }
func testOutboundToken() string { return strings.Repeat("o", 32) }

type fakeTelegram struct {
	mu      sync.Mutex
	count   int
	result  telegram.SendResult
	err     error
	targets []telegram.ReplyTarget
	texts   []string
}

func (f *fakeTelegram) SendMessage(_ context.Context, target telegram.ReplyTarget, text string) (telegram.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	f.targets = append(f.targets, target)
	f.texts = append(f.texts, text)
	return f.result, f.err
}

func testServer(t *testing.T, ingress http.Handler, sender TelegramSender) (*Server, *store.Store) {
	t.Helper()
	upstream := httptest.NewServer(ingress)
	t.Cleanup(upstream.Close)
	database, err := store.Open(filepath.Join(t.TempDir(), "adapter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	server, err := New(Config{
		BotID: 42, WebhookSecret: testWebhookSecret(), OutboundBearerToken: testOutboundToken(),
		IngressURL: upstream.URL + "/api/v1/gateways/test/telegram/events", IngressBearerToken: testInboundToken(), ConformanceChatID: 100,
		AdapterVersion: "test", HTTPClient: upstream.Client(),
	}, database, sender)
	if err != nil {
		t.Fatal(err)
	}
	return server, database
}

func TestWebhookAdmissionAndDuplicate(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	ingress := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer "+testInboundToken() {
			t.Errorf("Authorization = %q", got)
		}
		var event protocol.EventEnvelope
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.ExternalEventID != "telegram:42:99" || event.ContextID != "-100" || event.Sender.ID != "7" {
			t.Errorf("unexpected event: %+v", event)
		}
		writeJSON(w, http.StatusAccepted, protocol.IngressResponse{Status: "accepted", EventID: "gev-1", State: "Queued"})
	})
	server, _ := testServer(t, ingress, &fakeTelegram{})
	body := []byte(`{"update_id":99,"message":{"message_id":3,"from":{"id":7,"first_name":"Ada"},"date":1,"chat":{"id":-100,"type":"private"},"text":"hello"}}`)

	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPost, "/telegram/webhook", bytes.NewReader(body))
		request.Header.Set(telegramWebhookAuthHeader, testWebhookSecret())
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d body=%s", i, response.Code, response.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("ingress calls = %d, want 1", calls)
	}
}

func TestWebhookAuthenticationAndUnsupportedUpdate(t *testing.T) {
	calls := 0
	server, _ := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }), &fakeTelegram{})

	bad := httptest.NewRequest(http.MethodPost, "/telegram/webhook", bytes.NewReader([]byte(`{"update_id":1}`)))
	bad.Header.Set(telegramWebhookAuthHeader, "wrong")
	badResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusUnauthorized {
		t.Fatalf("bad secret status = %d", badResponse.Code)
	}

	unsupported := httptest.NewRequest(http.MethodPost, "/telegram/webhook", bytes.NewReader([]byte(`{"update_id":2,"channel_post":{"message_id":1,"date":1,"chat":{"id":1,"type":"channel"},"text":"ignored"}}`)))
	unsupported.Header.Set(telegramWebhookAuthHeader, testWebhookSecret())
	unsupportedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(unsupportedResponse, unsupported)
	if unsupportedResponse.Code != http.StatusOK || calls != 0 {
		t.Fatalf("unsupported status=%d calls=%d", unsupportedResponse.Code, calls)
	}

	invisible := httptest.NewRequest(http.MethodPost, "/telegram/webhook", bytes.NewReader([]byte(`{"update_id":3,"message":{"message_id":2,"from":{"id":2,"is_bot":false,"first_name":"Ada"},"date":1700000000,"chat":{"id":2,"type":"private"},"text":"\u200b\u200d"}}`)))
	invisible.Header.Set(telegramWebhookAuthHeader, testWebhookSecret())
	invisibleResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(invisibleResponse, invisible)
	if invisibleResponse.Code != http.StatusOK || calls != 0 {
		t.Fatalf("invisible status=%d calls=%d", invisibleResponse.Code, calls)
	}
}

func TestDeliveryIsDurablyIdempotent(t *testing.T) {
	sender := &fakeTelegram{result: telegram.SendResult{
		Classification: telegram.ResultDelivered, ProviderMessageID: "123", MessageID: 123,
	}}
	server, _ := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), sender)
	requestBody, _ := json.Marshal(protocol.DeliveryRequest{
		ProtocolVersion: protocol.Version, DeliveryID: "delivery-1", IdempotencyID: "delivery-1",
		OriginatingEvent: "event-1", Kind: protocol.DeliveryKindFinal, AccountID: "42", ContextID: "100",
		ReplyTarget: "tg:v1:100:0:9", Text: "reply",
	})

	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/deliveries", bytes.NewReader(requestBody))
		request.Header.Set("Authorization", "Bearer "+testOutboundToken())
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d", response.Code)
		}
		var result protocol.DeliveryResponse
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.Status != protocol.DeliveryStatusDelivered || result.ProviderMessageID != "123" {
			t.Fatalf("response = %+v", result)
		}
	}
	if sender.count != 1 {
		t.Fatalf("Telegram sends = %d, want 1", sender.count)
	}
}

func TestDeliveryRouteMismatchIsPersistedAndReplayed(t *testing.T) {
	tests := []struct {
		name    string
		request protocol.DeliveryRequest
	}{
		{
			name: "account",
			request: protocol.DeliveryRequest{
				ProtocolVersion:  protocol.Version,
				DeliveryID:       "delivery-route-account",
				IdempotencyID:    "delivery-route-account",
				OriginatingEvent: "event-1",
				Kind:             protocol.DeliveryKindFinal,
				AccountID:        "99",
				ContextID:        "100",
				ReplyTarget:      "tg:v1:100:0:9",
				Text:             "reply",
			},
		},
		{
			name: "context",
			request: protocol.DeliveryRequest{
				ProtocolVersion:  protocol.Version,
				DeliveryID:       "delivery-route-context",
				IdempotencyID:    "delivery-route-context",
				OriginatingEvent: "event-1",
				Kind:             protocol.DeliveryKindFinal,
				AccountID:        "42",
				ContextID:        "200",
				ReplyTarget:      "tg:v1:100:0:9",
				Text:             "reply",
			},
		},
		{
			name: "thread",
			request: protocol.DeliveryRequest{
				ProtocolVersion:  protocol.Version,
				DeliveryID:       "delivery-route-thread",
				IdempotencyID:    "delivery-route-thread",
				OriginatingEvent: "event-1",
				Kind:             protocol.DeliveryKindFinal,
				AccountID:        "42",
				ContextID:        "100",
				ThreadID:         "8",
				ReplyTarget:      "tg:v1:100:7:9",
				Text:             "reply",
			},
		},
		{
			name: "conformance context",
			request: protocol.DeliveryRequest{
				ProtocolVersion:  protocol.Version,
				DeliveryID:       "delivery-route-conformance",
				IdempotencyID:    "delivery-route-conformance",
				OriginatingEvent: "event-1",
				Kind:             protocol.DeliveryKindFinal,
				AccountID:        "conformance",
				ContextID:        "100",
				ReplyTarget:      "conformance",
				Text:             "reply",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sender := &fakeTelegram{result: telegram.SendResult{
				Classification:    telegram.ResultDelivered,
				ProviderMessageID: "123",
				MessageID:         123,
			}}
			server, _ := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), sender)
			requestBody, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}

			for range 2 {
				request := httptest.NewRequest(http.MethodPost, "/v1/deliveries", bytes.NewReader(requestBody))
				request.Header.Set("Authorization", "Bearer "+testOutboundToken())
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d", response.Code)
				}
				var result protocol.DeliveryResponse
				if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
					t.Fatal(err)
				}
				if result.Status != protocol.DeliveryStatusNonRetryableError || result.Message != "delivery routing mismatch" {
					t.Fatalf("response = %+v", result)
				}
			}
			if sender.count != 0 {
				t.Fatalf("Telegram sends = %d, want 0", sender.count)
			}
		})
	}
}

func TestDeliveryRoutingAcceptsMatchingStandardAndConformanceTargets(t *testing.T) {
	tests := []struct {
		name       string
		request    protocol.DeliveryRequest
		wantTarget telegram.ReplyTarget
	}{
		{
			name: "standard thread",
			request: protocol.DeliveryRequest{
				ProtocolVersion:  protocol.Version,
				DeliveryID:       "delivery-route-standard-valid",
				IdempotencyID:    "delivery-route-standard-valid",
				OriginatingEvent: "event-1",
				Kind:             protocol.DeliveryKindFinal,
				AccountID:        "42",
				ContextID:        "100",
				ThreadID:         "7",
				ReplyTarget:      "tg:v1:100:7:9",
				Text:             "reply",
			},
			wantTarget: telegram.ReplyTarget{ChatID: 100, ThreadID: 7, MessageID: 9},
		},
		{
			name: "conformance",
			request: protocol.DeliveryRequest{
				ProtocolVersion:  protocol.Version,
				DeliveryID:       "delivery-route-conformance-valid",
				IdempotencyID:    "delivery-route-conformance-valid",
				OriginatingEvent: "conformance-event",
				Kind:             protocol.DeliveryKindFinal,
				AccountID:        "conformance",
				ContextID:        "conformance",
				ReplyTarget:      "conformance",
				Text:             "conformance authentication probe",
			},
			wantTarget: telegram.ReplyTarget{ChatID: 100},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sender := &fakeTelegram{result: telegram.SendResult{
				Classification:    telegram.ResultDelivered,
				ProviderMessageID: "123",
				MessageID:         123,
			}}
			server, _ := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), sender)
			requestBody, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/deliveries", bytes.NewReader(requestBody))
			request.Header.Set("Authorization", "Bearer "+testOutboundToken())
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			var result protocol.DeliveryResponse
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result.Status != protocol.DeliveryStatusDelivered {
				t.Fatalf("response = %+v", result)
			}
			sender.mu.Lock()
			defer sender.mu.Unlock()
			if sender.count != 1 || len(sender.targets) != 1 || sender.targets[0] != test.wantTarget {
				t.Fatalf("Telegram sends=%d targets=%+v, want one send to %+v", sender.count, sender.targets, test.wantTarget)
			}
		})
	}
}

func TestAmbiguousDeliveryBecomesTerminal(t *testing.T) {
	sender := &fakeTelegram{result: telegram.SendResult{Classification: telegram.ResultAmbiguous, Description: "outcome unknown"}}
	server, _ := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), sender)
	requestBody, _ := json.Marshal(protocol.DeliveryRequest{
		ProtocolVersion: protocol.Version, DeliveryID: "delivery-ambiguous", IdempotencyID: "delivery-ambiguous",
		OriginatingEvent: "event-1", Kind: protocol.DeliveryKindFinal, AccountID: "42", ContextID: "100",
		ReplyTarget: "tg:v1:100:0:9", Text: "reply",
	})
	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/deliveries", bytes.NewReader(requestBody))
		request.Header.Set("Authorization", "Bearer "+testOutboundToken())
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		var result protocol.DeliveryResponse
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.Status != protocol.DeliveryStatusNonRetryableError {
			t.Fatalf("response = %+v", result)
		}
	}
	if sender.count != 1 {
		t.Fatalf("Telegram sends = %d, want 1", sender.count)
	}
}

func TestProtocolHealthRequiresOutboundBearer(t *testing.T) {
	server, _ := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &fakeTelegram{})
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	authorizedRequest := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer "+testOutboundToken())
	authorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d", authorized.Code)
	}
}

func TestValidateIngressURL(t *testing.T) {
	for _, value := range []string{
		"http://127.0.0.1:8080/api/v1/gateways/ns/gw/events",
		"http://orka-api.orka-system.svc.cluster.local:8080/api/v1/gateways/ns/gw/events",
		"https://orka.example.test/api/v1/gateways/ns/gw/events",
	} {
		if _, err := validateIngressURL(value); err != nil {
			t.Errorf("validateIngressURL(%q): %v", value, err)
		}
	}
	for _, value := range []string{
		"http://10.0.0.1/api/v1/gateways/ns/gw/events",
		"https://user@example.test/api/v1/gateways/ns/gw/events",
		"https://orka.example.test/wrong",
		"https://orka.example.test/api/v1/gateways/ns/gw/events?token=x",
	} {
		if _, err := validateIngressURL(value); err == nil {
			t.Errorf("validateIngressURL(%q) succeeded", value)
		}
	}
}

func TestConcurrentWebhookReplayCallsOrkaOnce(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	ingress := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()
		if current == 1 {
			started <- struct{}{}
			<-release
		}
		writeJSON(w, http.StatusAccepted, protocol.IngressResponse{Status: protocol.IngressStatusAccepted, EventID: "gev-concurrent", State: "Queued"})
	})
	server, _ := testServer(t, ingress, &fakeTelegram{})
	body := []byte(`{"update_id":101,"message":{"message_id":3,"from":{"id":7,"first_name":"Ada"},"date":1,"chat":{"id":100,"type":"private"},"text":"hello"}}`)

	responses := make(chan int, 2)
	call := func() {
		request := httptest.NewRequest(http.MethodPost, "/telegram/webhook", bytes.NewReader(body))
		request.Header.Set(telegramWebhookAuthHeader, testWebhookSecret())
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		responses <- response.Code
	}
	go call()
	<-started
	go call()
	close(release)
	for range 2 {
		if status := <-responses; status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("ingress calls = %d, want 1", calls)
	}
}

func TestConcurrentWebhookAdmissionPreservesSameChatOrder(t *testing.T) {
	admitted := make(chan string, 2)
	releaseFirst := make(chan struct{})
	ingress := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event protocol.EventEnvelope
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid event"})
			return
		}
		admitted <- event.Text
		if event.Text == "first" {
			<-releaseFirst
		}
		writeJSON(w, http.StatusAccepted, protocol.IngressResponse{
			Status:  protocol.IngressStatusAccepted,
			EventID: "gev-" + event.Text,
			State:   "Queued",
		})
	})
	server, _ := testServer(t, ingress, &fakeTelegram{})

	post := func(updateID, messageID int64, text string) <-chan int {
		t.Helper()
		body, err := json.Marshal(telegram.Update{
			UpdateID: updateID,
			Message: &telegram.Message{
				MessageID: messageID,
				From:      &telegram.User{ID: 7, FirstName: "Ada"},
				Date:      1,
				Chat:      telegram.Chat{ID: 100, Type: "private"},
				Text:      text,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan int, 1)
		go func() {
			request := httptest.NewRequest(http.MethodPost, "/telegram/webhook", bytes.NewReader(body))
			request.Header.Set(telegramWebhookAuthHeader, testWebhookSecret())
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			done <- response.Code
		}()
		return done
	}

	firstDone := post(201, 1, "first")
	if got := <-admitted; got != "first" {
		t.Fatalf("first admitted event = %q", got)
	}
	secondDone := post(202, 2, "second")

	select {
	case got := <-admitted:
		close(releaseFirst)
		<-firstDone
		<-secondDone
		t.Fatalf("later same-chat event reached Orka while the first was in flight: %q", got)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	if got := <-admitted; got != "second" {
		t.Fatalf("second admitted event = %q", got)
	}
	if status := <-secondDone; status != http.StatusOK {
		t.Fatalf("second status = %d", status)
	}
}

func TestDeliveryResultPersistsAfterRequestCancellation(t *testing.T) {
	server, database := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &fakeTelegram{})
	digest := store.DigestBytes([]byte("delivery"))
	if _, shouldSend, err := database.BeginDelivery(context.Background(), "delivery-cancelled", digest); err != nil || !shouldSend {
		t.Fatalf("BeginDelivery() = (%v, %v)", shouldSend, err)
	}
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	persistCtx, cancel := deliveryPersistenceContext(parent)
	defer cancel()
	record, err := server.persistSendResult(persistCtx, protocol.DeliveryRequest{DeliveryID: "delivery-cancelled"}, digest, telegram.SendResult{
		Classification: telegram.ResultDelivered, ProviderMessageID: "telegram:1:2",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != store.DeliveryStateDelivered {
		t.Fatalf("state = %s", record.State)
	}
}

func TestReadyFailsWhenStoreIsClosed(t *testing.T) {
	server, database := testServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &fakeTelegram{})
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestInvalidIngressAcknowledgementIsRetried(t *testing.T) {
	ingress := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ok", "eventId": "gev-1", "state": "Queued"})
	})
	server, _ := testServer(t, ingress, &fakeTelegram{})
	body := []byte(`{"update_id":102,"message":{"message_id":3,"from":{"id":7,"first_name":"Ada"},"date":1,"chat":{"id":100,"type":"private"},"text":"hello"}}`)
	request := httptest.NewRequest(http.MethodPost, "/telegram/webhook", bytes.NewReader(body))
	request.Header.Set(telegramWebhookAuthHeader, testWebhookSecret())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}
