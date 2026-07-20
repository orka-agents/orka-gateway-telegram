package faultproxy_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/orka-gateway-telegram/internal/faultproxy"
)

const testControlToken = "control-token-for-fault-proxy-tests"

func newProxy(t *testing.T, options ...func(*faultproxy.Config)) *faultproxy.Server {
	t.Helper()
	cfg := faultproxy.Config{
		ControlToken: testControlToken,
		Now: func() time.Time {
			return time.Date(2026, time.July, 20, 12, 34, 56, 789, time.UTC)
		},
		DefaultDelay: 5 * time.Millisecond,
	}
	for _, option := range options {
		option(&cfg)
	}
	proxy, err := faultproxy.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}

func serve(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func authorizedHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testControlToken}
}

func replacePlan(t *testing.T, proxy *faultproxy.Server, actions ...faultproxy.Action) {
	t.Helper()
	if err := proxy.ReplacePlan(actions); err != nil {
		t.Fatal(err)
	}
}

func sendBody(chatID int64, text string, threadID, replyMessageID int64) string {
	request := map[string]any{"chat_id": chatID, "text": text}
	if threadID != 0 {
		request["message_thread_id"] = threadID
	}
	if replyMessageID != 0 {
		request["reply_parameters"] = map[string]any{
			"message_id":                  replyMessageID,
			"allow_sending_without_reply": true,
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)

	for _, path := range []string{faultproxy.HealthPath, faultproxy.ReadyPath} {
		response := serve(t, proxy.Handler(), http.MethodGet, path, "", nil)
		if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
			t.Fatalf("%s response = (%d, %q), want (200, %q)", path, response.Code, response.Body.String(), "ok\n")
		}
	}
}

func TestGetMeAndSetWebhookReturnTokenSafeSuccess(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	botToken := "123456789:telegram-api-token-must-not-leak"
	webhookSecret := "webhook-secret-must-not-leak"

	getMe := serve(t, proxy.Handler(), http.MethodGet, "/bot"+botToken+"/getMe", "", nil)
	if getMe.Code != http.StatusOK {
		t.Fatalf("getMe status = %d, want 200: %s", getMe.Code, getMe.Body.String())
	}
	var getMeEnvelope struct {
		OK     bool `json:"ok"`
		Result struct {
			ID       int64  `json:"id"`
			IsBot    bool   `json:"is_bot"`
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.Unmarshal(getMe.Body.Bytes(), &getMeEnvelope); err != nil {
		t.Fatal(err)
	}
	if !getMeEnvelope.OK || !getMeEnvelope.Result.IsBot || getMeEnvelope.Result.ID <= 0 || getMeEnvelope.Result.Username == "" {
		t.Fatalf("unexpected getMe response: %+v", getMeEnvelope)
	}

	setWebhook := serve(t, proxy.Handler(), http.MethodPost, "/bot"+botToken+"/setWebhook",
		`{"url":"https://example.test/webhook","secret_token":"`+webhookSecret+`"}`,
		map[string]string{"Content-Type": "application/json"})
	if setWebhook.Code != http.StatusOK {
		t.Fatalf("setWebhook status = %d, want 200: %s", setWebhook.Code, setWebhook.Body.String())
	}
	var setWebhookEnvelope struct {
		OK     bool `json:"ok"`
		Result bool `json:"result"`
	}
	if err := json.Unmarshal(setWebhook.Body.Bytes(), &setWebhookEnvelope); err != nil {
		t.Fatal(err)
	}
	if !setWebhookEnvelope.OK || !setWebhookEnvelope.Result {
		t.Fatalf("unexpected setWebhook response: %+v", setWebhookEnvelope)
	}

	combined := getMe.Body.String() + setWebhook.Body.String()
	for _, secret := range []string{botToken, webhookSecret, testControlToken} {
		if strings.Contains(combined, secret) {
			t.Fatalf("response disclosed secret %q", secret)
		}
	}
}

func TestDefaultSendMessageMatchesRequestAndRecordsSafeAudit(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	text := "sensitive full message text"

	response := serve(t, proxy.Handler(), http.MethodPost, "/botopaque-token/sendMessage", sendBody(42, text, 7, 9),
		map[string]string{"Content-Type": "application/json"})
	if response.Code != http.StatusOK {
		t.Fatalf("sendMessage status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID       int64  `json:"message_id"`
			MessageThreadID int64  `json:"message_thread_id"`
			Text            string `json:"text"`
			Chat            struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Result.MessageID != 1 || envelope.Result.Chat.ID != 42 || envelope.Result.Text != text || envelope.Result.MessageThreadID != 7 {
		t.Fatalf("unexpected sendMessage response: %+v", envelope)
	}

	state := proxy.State()
	if len(state.PendingActions) != 0 || len(state.Audit) != 1 {
		t.Fatalf("state = %+v, want one audit entry and no pending actions", state)
	}
	entry := state.Audit[0]
	wantHash := sha256.Sum256([]byte(text))
	if entry.Sequence != 1 || entry.Action != faultproxy.ActionSuccess || entry.ChatID != 42 || entry.TextLength != len(text) ||
		entry.TextSHA256 != hex.EncodeToString(wantHash[:]) || entry.ReplyTarget.MessageThreadID != 7 || entry.ReplyTarget.MessageID != 9 ||
		!entry.Timestamp.Equal(time.Date(2026, time.July, 20, 12, 34, 56, 789, time.UTC)) {
		t.Fatalf("unexpected audit entry: %+v", entry)
	}

	stateResponse := serve(t, proxy.Handler(), http.MethodGet, faultproxy.ControlStatePath, "", authorizedHeaders())
	if stateResponse.Code != http.StatusOK {
		t.Fatalf("state status = %d, want 200: %s", stateResponse.Code, stateResponse.Body.String())
	}
	if strings.Contains(stateResponse.Body.String(), text) {
		t.Fatalf("audit state disclosed full text: %s", stateResponse.Body.String())
	}
}

func TestControlAPIReplacesAndResetsPlan(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	planBody := `{"actions":[{"action":"rate_limit","retry_after":7},{"action":"delayed_success","delay_ms":12}]}`

	unauthorized := serve(t, proxy.Handler(), http.MethodPut, faultproxy.ControlPlanPath, planBody, nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}
	wrongToken := "wrong-control-token-that-must-not-leak"
	wrong := serve(t, proxy.Handler(), http.MethodPut, faultproxy.ControlPlanPath, planBody,
		map[string]string{"Authorization": "Bearer " + wrongToken})
	if wrong.Code != http.StatusUnauthorized || strings.Contains(wrong.Body.String(), wrongToken) {
		t.Fatalf("wrong-token response = (%d, %q)", wrong.Code, wrong.Body.String())
	}

	replaced := serve(t, proxy.Handler(), http.MethodPut, faultproxy.ControlPlanPath, planBody, authorizedHeaders())
	if replaced.Code != http.StatusOK {
		t.Fatalf("replace status = %d, want 200: %s", replaced.Code, replaced.Body.String())
	}
	state := proxy.State()
	if len(state.PendingActions) != 2 || state.PendingActions[0].Kind != faultproxy.ActionRateLimit || state.PendingActions[1].Kind != faultproxy.ActionDelayedSuccess {
		t.Fatalf("pending plan = %+v", state.PendingActions)
	}

	reset := serve(t, proxy.Handler(), http.MethodDelete, faultproxy.ControlPlanPath, "", authorizedHeaders())
	if reset.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204: %s", reset.Code, reset.Body.String())
	}
	if pending := proxy.State().PendingActions; len(pending) != 0 {
		t.Fatalf("pending plan after reset = %+v, want empty", pending)
	}
}

func TestSendMessageOneShotActions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		action        faultproxy.Action
		wantStatus    int
		wantSubstring string
		check         func(*testing.T, []byte)
	}{
		{name: "success", action: faultproxy.Action{Kind: faultproxy.ActionSuccess}, wantStatus: http.StatusOK, wantSubstring: `"ok":true`},
		{name: "rate limit", action: faultproxy.Action{Kind: faultproxy.ActionRateLimit, RetryAfterSeconds: 7}, wantStatus: http.StatusTooManyRequests, wantSubstring: `"retry_after":7`},
		{name: "server error", action: faultproxy.Action{Kind: faultproxy.ActionServerError}, wantStatus: http.StatusInternalServerError, wantSubstring: `"error_code":500`},
		{name: "bad request", action: faultproxy.Action{Kind: faultproxy.ActionBadRequest}, wantStatus: http.StatusBadRequest, wantSubstring: "chat not found"},
		{name: "forbidden", action: faultproxy.Action{Kind: faultproxy.ActionForbidden}, wantStatus: http.StatusForbidden, wantSubstring: `"error_code":403`},
		{name: "malformed success", action: faultproxy.Action{Kind: faultproxy.ActionMalformedSuccess}, wantStatus: http.StatusOK, wantSubstring: "not-json"},
		{
			name: "mismatched success", action: faultproxy.Action{Kind: faultproxy.ActionMismatchedSuccess}, wantStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				t.Helper()
				var envelope struct {
					Result struct {
						Chat struct {
							ID int64 `json:"id"`
						} `json:"chat"`
					} `json:"result"`
				}
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Result.Chat.ID == 42 {
					t.Fatalf("mismatched_success returned requested chat ID: %s", body)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			proxy := newProxy(t)
			replacePlan(t, proxy, test.action)
			response := serve(t, proxy.Handler(), http.MethodPost, "/botsecret/sendMessage", sendBody(42, "hello", 0, 0), nil)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantSubstring != "" && !strings.Contains(response.Body.String(), test.wantSubstring) {
				t.Fatalf("body = %q, want substring %q", response.Body.String(), test.wantSubstring)
			}
			if test.check != nil {
				test.check(t, response.Body.Bytes())
			}
			state := proxy.State()
			if len(state.PendingActions) != 0 || len(state.Audit) != 1 || state.Audit[0].Action != test.action.Kind {
				t.Fatalf("state after one-shot action = %+v", state)
			}

			defaultResponse := serve(t, proxy.Handler(), http.MethodPost, "/botsecret/sendMessage", sendBody(42, "second", 0, 0), nil)
			if defaultResponse.Code != http.StatusOK || !strings.Contains(defaultResponse.Body.String(), `"ok":true`) {
				t.Fatalf("default response after action consumption = (%d, %q)", defaultResponse.Code, defaultResponse.Body.String())
			}
		})
	}
}

func TestDelayedSuccessWaitsThenReturnsMatchingMessage(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	replacePlan(t, proxy, faultproxy.Action{Kind: faultproxy.ActionDelayedSuccess, DelayMilliseconds: 40})

	started := time.Now()
	response := serve(t, proxy.Handler(), http.MethodPost, "/bottoken/sendMessage", sendBody(-10042, "delayed", 3, 4), nil)
	elapsed := time.Since(started)
	if elapsed < 30*time.Millisecond {
		t.Fatalf("delayed_success returned after %v, want at least 30ms", elapsed)
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":-10042`) || !strings.Contains(response.Body.String(), `"text":"delayed"`) {
		t.Fatalf("delayed response = (%d, %q)", response.Code, response.Body.String())
	}
}

func TestDropAfterReadClosesConnectionAndAuditsRequest(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	replacePlan(t, proxy, faultproxy.Action{Kind: faultproxy.ActionDropAfterRead})
	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/botnever-log-this-token/sendMessage", strings.NewReader(sendBody(77, "read before drop", 0, 8)))
	if err != nil {
		t.Fatal(err)
	}
	request.Close = true
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("drop_after_read returned a response, want connection error")
	}

	state := proxy.State()
	if len(state.Audit) != 1 || state.Audit[0].Action != faultproxy.ActionDropAfterRead || state.Audit[0].ChatID != 77 || state.Audit[0].ReplyTarget.MessageID != 8 {
		t.Fatalf("audit after drop = %+v", state.Audit)
	}
}

func TestPlanConsumptionIsFIFOAndConcurrentSafe(t *testing.T) {
	t.Parallel()
	const requests = 128
	proxy := newProxy(t, func(cfg *faultproxy.Config) { cfg.AuditCapacity = requests })
	cycle := []faultproxy.Action{
		{Kind: faultproxy.ActionSuccess},
		{Kind: faultproxy.ActionRateLimit, RetryAfterSeconds: 1},
		{Kind: faultproxy.ActionServerError},
		{Kind: faultproxy.ActionBadRequest},
		{Kind: faultproxy.ActionForbidden},
		{Kind: faultproxy.ActionMalformedSuccess},
		{Kind: faultproxy.ActionMismatchedSuccess},
		{Kind: faultproxy.ActionDelayedSuccess, DelayMilliseconds: 1},
	}
	plan := make([]faultproxy.Action, requests)
	for i := range plan {
		plan[i] = cycle[i%len(cycle)]
	}
	replacePlan(t, proxy, plan...)

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(requests)
	for i := 0; i < requests; i++ {
		go func(chatID int64) {
			defer wait.Done()
			<-start
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/botconcurrent/sendMessage", strings.NewReader(sendBody(chatID, "message", 0, 0)))
			proxy.Handler().ServeHTTP(response, request)
		}(int64(i + 1))
	}
	close(start)
	wait.Wait()

	state := proxy.State()
	if len(state.PendingActions) != 0 || len(state.Audit) != requests {
		t.Fatalf("state sizes = pending %d audit %d, want 0 and %d", len(state.PendingActions), len(state.Audit), requests)
	}
	chatIDs := make([]int, 0, requests)
	for i, entry := range state.Audit {
		if entry.Sequence != uint64(i+1) {
			t.Fatalf("audit[%d].sequence = %d, want %d", i, entry.Sequence, i+1)
		}
		if entry.Action != plan[i].Kind {
			t.Fatalf("audit[%d].action = %q, want FIFO action %q", i, entry.Action, plan[i].Kind)
		}
		chatIDs = append(chatIDs, int(entry.ChatID))
	}
	sort.Ints(chatIDs)
	for i, chatID := range chatIDs {
		if chatID != i+1 {
			t.Fatalf("concurrent audit chat IDs = %v", chatIDs)
		}
	}
}

func TestInvalidSendDoesNotConsumePlan(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	replacePlan(t, proxy, faultproxy.Action{Kind: faultproxy.ActionServerError})

	response := serve(t, proxy.Handler(), http.MethodPost, "/bottoken/sendMessage", `{"chat_id":42,"text":`, nil)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"error_code":400`) {
		t.Fatalf("invalid request response = (%d, %q)", response.Code, response.Body.String())
	}
	state := proxy.State()
	if len(state.PendingActions) != 1 || len(state.Audit) != 0 {
		t.Fatalf("invalid request changed state: %+v", state)
	}
}

func TestInvalidPlanIsRejectedWithoutReplacingExistingPlan(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	replacePlan(t, proxy, faultproxy.Action{Kind: faultproxy.ActionSuccess})

	for _, body := range []string{
		`{"actions":[{"action":"unknown"}]}`,
		`{"actions":[{"action":"rate_limit","retry_after":-1}]}`,
		`{"actions":[{"action":"delayed_success","delay_ms":-1}]}`,
		`{"actions":[{"action":"success","retry_after":1}]}`,
		`{"actions":[{"action":"success"}]} trailing`,
	} {
		response := serve(t, proxy.Handler(), http.MethodPut, faultproxy.ControlPlanPath, body, authorizedHeaders())
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid plan %q status = %d, want 400: %s", body, response.Code, response.Body.String())
		}
		state := proxy.State()
		if len(state.PendingActions) != 1 || state.PendingActions[0].Kind != faultproxy.ActionSuccess {
			t.Fatalf("invalid plan replaced state: %+v", state.PendingActions)
		}
	}
}

func TestTelegramAndControlTokensAreNeverEchoed(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	telegramToken := "123456:telegram-path-token-secret"
	wrongControlToken := "wrong-control-secret"

	responses := []*httptest.ResponseRecorder{
		serve(t, proxy.Handler(), http.MethodGet, "/bot"+telegramToken+"/unknown", "", nil),
		serve(t, proxy.Handler(), http.MethodPost, "/bot"+telegramToken+"/sendMessage", `not-json`, nil),
		serve(t, proxy.Handler(), http.MethodGet, faultproxy.ControlStatePath, "", map[string]string{"Authorization": "Bearer " + wrongControlToken}),
		serve(t, proxy.Handler(), http.MethodGet, faultproxy.ControlStatePath, "", authorizedHeaders()),
	}
	var combined bytes.Buffer
	for _, response := range responses {
		combined.Write(response.Body.Bytes())
		for name, values := range response.Header() {
			combined.WriteString(name)
			combined.WriteString(strings.Join(values, ","))
		}
	}
	for _, secret := range []string{telegramToken, testControlToken, wrongControlToken} {
		if strings.Contains(combined.String(), secret) {
			t.Fatalf("response data disclosed secret %q: %s", secret, combined.String())
		}
	}
}

func TestActionPlanAcceptsCompactStringForm(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	response := serve(t, proxy.Handler(), http.MethodPut, faultproxy.ControlPlanPath,
		`{"actions":["server_error","success"]}`, authorizedHeaders())
	if response.Code != http.StatusOK {
		t.Fatalf("compact plan status = %d, want 200: %s", response.Code, response.Body.String())
	}
	state := proxy.State()
	if len(state.PendingActions) != 2 || state.PendingActions[0].Kind != faultproxy.ActionServerError || state.PendingActions[1].Kind != faultproxy.ActionSuccess {
		t.Fatalf("compact plan = %+v", state.PendingActions)
	}
}

func TestRequestBodyLimitIsEnforcedWithoutAuditingText(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t, func(cfg *faultproxy.Config) { cfg.MaxRequestBodyBytes = 64 })
	body := sendBody(42, strings.Repeat("secret-text-", 20), 0, 0)
	response := serve(t, proxy.Handler(), http.MethodPost, "/bottoken/sendMessage", body, nil)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status = %d, want 413: %s", response.Code, response.Body.String())
	}
	if state := proxy.State(); len(state.Audit) != 0 {
		t.Fatalf("oversized request was audited: %+v", state.Audit)
	}
}

func TestStateReturnsDefensiveCopies(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	replacePlan(t, proxy, faultproxy.Action{Kind: faultproxy.ActionSuccess})
	first := proxy.State()
	first.PendingActions[0].Kind = faultproxy.ActionServerError
	second := proxy.State()
	if second.PendingActions[0].Kind != faultproxy.ActionSuccess {
		t.Fatalf("State exposed mutable plan storage: %+v", second.PendingActions)
	}

	serve(t, proxy.Handler(), http.MethodPost, "/bottoken/sendMessage", sendBody(1, "x", 0, 0), nil)
	withAudit := proxy.State()
	withAudit.Audit[0].Action = faultproxy.ActionForbidden
	if proxy.State().Audit[0].Action != faultproxy.ActionSuccess {
		t.Fatal("State exposed mutable audit storage")
	}
}

func TestMalformedSuccessIsNotJSON(t *testing.T) {
	t.Parallel()
	proxy := newProxy(t)
	replacePlan(t, proxy, faultproxy.Action{Kind: faultproxy.ActionMalformedSuccess})
	response := serve(t, proxy.Handler(), http.MethodPost, "/bottoken/sendMessage", sendBody(42, "hello", 0, 0), nil)
	var value any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err == nil {
		t.Fatalf("malformed_success returned valid JSON: %s", response.Body.String())
	}
}

func TestRetryAfterRejectsDurationOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("duration-overflow control value is not representable as int on this architecture")
	}
	tooLarge := int64((1<<63-1)/int64(time.Second)) + 1
	if err := (faultproxy.Action{Kind: faultproxy.ActionRateLimit, RetryAfterSeconds: int(tooLarge)}).Validate(); err == nil {
		t.Fatal("expected overflowing action retry_after to be rejected")
	}
	if _, err := faultproxy.New(faultproxy.Config{
		ControlToken:             testControlToken,
		DefaultRetryAfterSeconds: int(tooLarge),
	}); err == nil {
		t.Fatal("expected overflowing default retry_after to be rejected")
	}
}
