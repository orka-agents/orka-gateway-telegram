package faultproxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka-gateway-telegram/internal/faultproxy"
	"github.com/sozercan/orka-gateway-telegram/internal/telegram"
)

func TestProxyInteroperatesWithTelegramClient(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	client, err := telegram.NewClient(server.URL, "123456:"+strings.Repeat("t", 24), telegram.WithHTTPClient(&http.Client{Timeout: time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	bot, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bot.ID <= 0 || !bot.IsBot {
		t.Fatalf("getMe bot = %+v", bot)
	}
	if err := client.SetWebhook(context.Background(), "https://example.test/webhook", strings.Repeat("s", 32), true); err != nil {
		t.Fatal(err)
	}

	result, err := client.SendMessage(context.Background(), telegram.ReplyTarget{ChatID: 42, ThreadID: 7, MessageID: 9}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Delivered() || result.Message == nil || result.Message.Chat.ID != 42 || result.Message.Text != "hello" || result.Message.MessageThreadID != 7 ||
		result.Message.ReplyToMessage == nil || result.Message.ReplyToMessage.MessageID != 9 {
		t.Fatalf("default result = %+v", result)
	}

	tests := []struct {
		name           string
		action         faultproxy.Action
		classification telegram.ResultClass
		retryAfter     time.Duration
	}{
		{name: "rate limit", action: faultproxy.Action{Kind: faultproxy.ActionRateLimit, RetryAfterSeconds: 3}, classification: telegram.ResultRetryable, retryAfter: 3 * time.Second},
		{name: "server error", action: faultproxy.Action{Kind: faultproxy.ActionServerError}, classification: telegram.ResultRetryable},
		{name: "bad request", action: faultproxy.Action{Kind: faultproxy.ActionBadRequest}, classification: telegram.ResultNonRetryable},
		{name: "forbidden", action: faultproxy.Action{Kind: faultproxy.ActionForbidden}, classification: telegram.ResultNonRetryable},
		{name: "malformed success", action: faultproxy.Action{Kind: faultproxy.ActionMalformedSuccess}, classification: telegram.ResultAmbiguous},
		{name: "mismatched success", action: faultproxy.Action{Kind: faultproxy.ActionMismatchedSuccess}, classification: telegram.ResultAmbiguous},
		{name: "drop after read", action: faultproxy.Action{Kind: faultproxy.ActionDropAfterRead}, classification: telegram.ResultAmbiguous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			replacePlan(t, proxy, test.action)
			result, err := client.SendMessage(context.Background(), telegram.ReplyTarget{ChatID: 42}, "fault")
			if err == nil {
				t.Fatal("expected classified send error")
			}
			if result.Classification != test.classification || result.RetryAfter != test.retryAfter {
				t.Fatalf("result = %+v, want classification %q retryAfter %v (error: %v)", result, test.classification, test.retryAfter, err)
			}
			var apiError *telegram.APIError
			if !errors.As(err, &apiError) || apiError.Classification != test.classification {
				t.Fatalf("error = %#v, want Telegram APIError classification %q", err, test.classification)
			}
		})
	}

	replacePlan(t, proxy, faultproxy.Action{Kind: faultproxy.ActionDelayedSuccess, DelayMilliseconds: 5})
	result, err = client.SendMessage(context.Background(), telegram.ReplyTarget{ChatID: 42}, "delayed")
	if err != nil || !result.Delivered() {
		t.Fatalf("delayed result = %+v, error = %v", result, err)
	}
}
