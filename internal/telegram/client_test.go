package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func testBotToken() string { return "123456:" + strings.Repeat("t", 24) }

func TestInitializeGetsMeThenOptionallySetsWebhook(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		requests []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.Method+" "+request.URL.Path)
		mu.Unlock()

		switch request.URL.Path {
		case "/bot" + testBotToken() + "/getMe":
			if request.Method != http.MethodGet {
				t.Errorf("getMe method = %s, want GET", request.Method)
			}
			writeTestJSON(t, writer, http.StatusOK, map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 123456, "is_bot": true, "first_name": "Orka"},
			})
		case "/bot" + testBotToken() + "/setWebhook":
			if request.Method != http.MethodPost {
				t.Errorf("setWebhook method = %s, want POST", request.Method)
			}
			var body struct {
				URL                string   `json:"url"`
				SecretToken        string   `json:"secret_token"`
				DropPendingUpdates bool     `json:"drop_pending_updates"`
				AllowedUpdates     []string `json:"allowed_updates"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode setWebhook request: %v", err)
			}
			if body.URL != "https://adapter.example/v1/telegram/webhook" || body.SecretToken != strings.Repeat("s", 24) {
				t.Errorf("unexpected setWebhook body: %+v", body)
			}
			if !body.DropPendingUpdates || len(body.AllowedUpdates) != 1 || body.AllowedUpdates[0] != "message" {
				t.Errorf("unexpected setWebhook options: %+v", body)
			}
			writeTestJSON(t, writer, http.StatusOK, map[string]any{"ok": true, "result": true})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, testBotToken())
	if err != nil {
		t.Fatal(err)
	}
	bot, err := client.Initialize(context.Background(), &WebhookConfig{
		URL:                "https://adapter.example/v1/telegram/webhook",
		SecretToken:        strings.Repeat("s", 24),
		DropPendingUpdates: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bot.ID != 123456 || !bot.IsBot || bot.FirstName != "Orka" {
		t.Fatalf("unexpected bot: %+v", bot)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		http.MethodGet + " /bot" + testBotToken() + "/getMe",
		http.MethodPost + " /bot" + testBotToken() + "/setWebhook",
	}
	if len(requests) != len(want) {
		t.Fatalf("request count = %d, want %d: %v", len(requests), len(want), requests)
	}
	for index := range want {
		if requests[index] != want[index] {
			t.Fatalf("request[%d] = %q, want %q", index, requests[index], want[index])
		}
	}
}

func TestConfigureWebhookEmptyIsNoop(t *testing.T) {
	t.Parallel()

	client, err := NewClient("https://api.telegram.invalid", testBotToken(), WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP request")
			return nil, errors.New("unreachable")
		}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ConfigureWebhook(context.Background(), "  ", "", false); err != nil {
		t.Fatal(err)
	}
}

func TestSendMessageDelivered(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/bot"+testBotToken()+"/sendMessage" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", request.Header.Get("Content-Type"))
		}
		var body struct {
			ChatID          int64  `json:"chat_id"`
			MessageThreadID int64  `json:"message_thread_id"`
			Text            string `json:"text"`
			ReplyParameters struct {
				MessageID                int64 `json:"message_id"`
				AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
			} `json:"reply_parameters"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode sendMessage request: %v", err)
		}
		if body.ChatID != 42 || body.MessageThreadID != 7 || body.Text != "hello" || body.ReplyParameters.MessageID != 9 || !body.ReplyParameters.AllowSendingWithoutReply {
			t.Errorf("unexpected sendMessage request: %+v", body)
		}
		writeTestJSON(t, writer, http.StatusOK, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 101,
				"date":       1_700_000_000,
				"chat":       map[string]any{"id": 42, "type": "private"},
				"text":       "hello",
			},
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, testBotToken())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.SendMessage(context.Background(), ReplyTarget{ChatID: 42, ThreadID: 7, MessageID: 9}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Delivered() || result.MessageID != 101 || result.ProviderMessageID != "telegram:42:101" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Message == nil || result.Message.MessageID != 101 {
		t.Fatalf("message result = %+v", result.Message)
	}
}

func TestSendMessageTruncatesToTelegramTextLimit(t *testing.T) {
	t.Parallel()

	var sentText string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode sendMessage request: %v", err)
		}
		sentText = body.Text
		writeTestJSON(t, writer, http.StatusOK, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 102,
				"date":       1_700_000_000,
				"chat":       map[string]any{"id": 42, "type": "private"},
				"text":       body.Text,
			},
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, testBotToken())
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Repeat("🙂", telegramSendMessageMaxCharacters+1)
	result, err := client.SendMessage(context.Background(), ReplyTarget{ChatID: 42}, input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Delivered() || !result.Truncated {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !strings.HasSuffix(sentText, telegramTruncationSuffix) {
		t.Fatalf("sent text does not contain truncation suffix: %q", sentText)
	}
	if characters := utf8.RuneCountInString(sentText); characters > telegramSendMessageMaxCharacters {
		t.Fatalf("sent text characters = %d, want <= %d", characters, telegramSendMessageMaxCharacters)
	}
	if sentText == input {
		t.Fatal("oversized text was not truncated")
	}
}

func TestTruncateTelegramTextHonorsCharacterBoundary(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		strings.Repeat("a", telegramSendMessageMaxCharacters),
		strings.Repeat("🙂", telegramSendMessageMaxCharacters),
		strings.Repeat("e\u0301", telegramSendMessageMaxCharacters/2),
	} {
		got, truncated := truncateTelegramText(value)
		if truncated || got != value {
			t.Fatalf("exact-limit text was changed: characters=%d truncated=%v", utf8.RuneCountInString(value), truncated)
		}
	}
}

func TestSendMessageClassifiesTelegramFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		status         int
		body           string
		maxBody        int64
		want           ResultClass
		wantRetryAfter time.Duration
	}{
		{
			name: "rate limited", status: http.StatusTooManyRequests,
			body: `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}`,
			want: ResultRetryable, wantRetryAfter: 3 * time.Second,
		},
		{
			name: "provider failure", status: http.StatusInternalServerError,
			body: `{"ok":false,"error_code":500,"description":"Internal Server Error"}`,
			want: ResultRetryable,
		},
		{
			name: "bad target", status: http.StatusBadRequest,
			body: `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
			want: ResultNonRetryable,
		},
		{
			name: "malformed success", status: http.StatusOK,
			body: `not-json`,
			want: ResultAmbiguous,
		},
		{
			name: "malformed provider failure", status: http.StatusBadGateway,
			body: `<html>bad gateway</html>`,
			want: ResultAmbiguous,
		},
		{
			name: "oversized success", status: http.StatusOK,
			body: strings.Repeat("x", 65), maxBody: 64,
			want: ResultAmbiguous,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()

			options := []ClientOption(nil)
			if test.maxBody > 0 {
				options = append(options, WithMaxResponseBodyBytes(test.maxBody))
			}
			client, err := NewClient(server.URL, testBotToken(), options...)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.SendMessage(context.Background(), ReplyTarget{ChatID: 42}, "hello")
			if err == nil {
				t.Fatal("expected classified error")
			}
			if result.Classification != test.want {
				t.Fatalf("classification = %q, want %q (error: %v)", result.Classification, test.want, err)
			}
			if result.RetryAfter != test.wantRetryAfter {
				t.Fatalf("retryAfter = %v, want %v", result.RetryAfter, test.wantRetryAfter)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Classification != test.want {
				t.Fatalf("API error = %#v, want classification %q", apiErr, test.want)
			}
		})
	}
}

func TestSendMessagePreWriteTransportFailureIsRetryableAndTokenSafe(t *testing.T) {
	t.Parallel()

	client, err := NewClient("https://api.telegram.invalid", testBotToken(), WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("request to " + request.URL.String() + " failed")
		}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.SendMessage(context.Background(), ReplyTarget{ChatID: 42}, "hello")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if result.Classification != ResultRetryable {
		t.Fatalf("classification = %q, want retryable", result.Classification)
	}
	if strings.Contains(err.Error(), testBotToken()) || strings.Contains(result.Description, testBotToken()) {
		t.Fatalf("error leaked bot token: %v / %q", err, result.Description)
	}
}

func TestSendMessagePostWriteTransportFailureIsAmbiguous(t *testing.T) {
	t.Parallel()
	client, err := NewClient("https://api.telegram.invalid", testBotToken(), WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
				trace.WroteRequest(httptrace.WroteRequestInfo{})
			}
			return nil, errors.New("connection lost after write")
		}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.SendMessage(context.Background(), ReplyTarget{ChatID: 42}, "hello")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if result.Classification != ResultAmbiguous {
		t.Fatalf("classification = %q, want ambiguous", result.Classification)
	}
}

func TestProviderDescriptionsRedactBotAndWebhookSecrets(t *testing.T) {
	t.Parallel()

	webhookSecret := strings.Repeat("s", 24)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		description := "rejected token=" + testBotToken()
		if strings.HasSuffix(request.URL.Path, "/setWebhook") {
			description += " secret=" + webhookSecret
		}
		writeTestJSON(t, writer, http.StatusBadRequest, map[string]any{
			"ok": false, "error_code": 400, "description": description,
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, testBotToken())
	if err != nil {
		t.Fatal(err)
	}
	result, sendErr := client.SendMessage(context.Background(), ReplyTarget{ChatID: 42}, "hello")
	if sendErr == nil {
		t.Fatal("expected send error")
	}
	if strings.Contains(sendErr.Error(), testBotToken()) || strings.Contains(result.Description, testBotToken()) {
		t.Fatal("send result leaked bot token")
	}
	webhookErr := client.SetWebhook(context.Background(), "https://adapter.example/hook", webhookSecret, false)
	if webhookErr == nil {
		t.Fatal("expected webhook error")
	}
	if strings.Contains(webhookErr.Error(), testBotToken()) || strings.Contains(webhookErr.Error(), webhookSecret) {
		t.Fatal("webhook error leaked a secret")
	}
}

func TestClassifyTelegramFailure(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		status, code int
		want         ResultClass
	}{
		{0, 0, ResultAmbiguous},
		{http.StatusOK, 429, ResultRetryable},
		{http.StatusTooManyRequests, 0, ResultRetryable},
		{http.StatusServiceUnavailable, 0, ResultRetryable},
		{http.StatusUnauthorized, 0, ResultNonRetryable},
		{http.StatusOK, 400, ResultNonRetryable},
	} {
		if got := ClassifyTelegramFailure(test.status, test.code); got != test.want {
			t.Errorf("ClassifyTelegramFailure(%d, %d) = %q, want %q", test.status, test.code, got, test.want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func writeTestJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
