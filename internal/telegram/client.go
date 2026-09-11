package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"github.com/sozercan/orka-gateway-telegram/internal/protocol"
)

const (
	getMeMethod       = "getMe"
	setWebhookMethod  = "setWebhook"
	sendMessageMethod = "sendMessage"

	telegramSendMessageMaxCharacters = 4096
	telegramTruncationSuffix         = "\n\n[response truncated to fit Telegram]"
	telegramWebhookMaxConnections    = 1
)

// Client is a bounded Telegram Bot API client. The bot token is retained only
// for endpoint construction and is redacted from all returned descriptions.
type Client struct {
	baseURL              *url.URL
	botToken             string
	httpClient           *http.Client
	maxResponseBodyBytes int64
	disableLinkPreviews  bool
}

// ClientOption customizes a Client.
type ClientOption func(*Client)

// WithHTTPClient supplies the transport and timeout policy used by the client.
// The value is cloned and redirects are disabled so the token-bearing request
// path cannot be redirected to another host.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(client *Client) {
		if httpClient != nil {
			clone := *httpClient
			clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			}
			client.httpClient = &clone
		}
	}
}

// WithMaxResponseBodyBytes overrides the Telegram response body limit. It is
// primarily useful for tests; non-positive values are ignored.
func WithMaxResponseBodyBytes(limit int64) ClientOption {
	return func(client *Client) {
		if limit > 0 {
			client.maxResponseBodyBytes = limit
		}
	}
}

// WithMaxResponseBytes is a concise alias for WithMaxResponseBodyBytes.
func WithMaxResponseBytes(limit int64) ClientOption {
	return WithMaxResponseBodyBytes(limit)
}

// WithDisableLinkPreviews controls Telegram link preview boxes on sendMessage.
// When true, outgoing text messages include link_preview_options.is_disabled.
func WithDisableLinkPreviews(disabled bool) ClientOption {
	return func(client *Client) {
		client.disableLinkPreviews = disabled
	}
}

// NewClient creates a Telegram Bot API client using the configurable base URL.
// Errors intentionally do not echo the base URL or token.
func NewClient(baseURL, botToken string, options ...ClientOption) (*Client, error) {
	parsedBaseURL, err := parseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	botToken = strings.TrimSpace(botToken)
	if !validBotToken(botToken) {
		return nil, errors.New("telegram bot token is invalid")
	}
	client := &Client{
		baseURL:  parsedBaseURL,
		botToken: botToken,
		httpClient: &http.Client{
			Timeout: defaultHTTPClientTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxResponseBodyBytes: DefaultMaxResponseBodyBytes,
	}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	return client, nil
}

// WebhookConfig controls the optional setWebhook call during Initialize.
type WebhookConfig struct {
	URL                string
	SecretToken        string
	DropPendingUpdates bool
}

// Initialize confirms the configured token with getMe, then optionally sets a
// webhook. getMe always runs first so callers have a verified bot identity.
func (c *Client) Initialize(ctx context.Context, webhook *WebhookConfig) (User, error) {
	bot, err := c.GetMe(ctx)
	if err != nil {
		return User{}, err
	}
	if webhook != nil && strings.TrimSpace(webhook.URL) != "" {
		if err := c.SetWebhook(ctx, webhook.URL, webhook.SecretToken, webhook.DropPendingUpdates); err != nil {
			return User{}, err
		}
	}
	return bot, nil
}

// GetMe invokes Telegram getMe and returns the authenticated bot identity.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var bot User
	meta, err := c.do(ctx, http.MethodGet, getMeMethod, nil, nil, &bot)
	if err != nil {
		return User{}, err
	}
	if bot.ID <= 0 || !bot.IsBot {
		return User{}, c.invalidSuccessError(getMeMethod, meta.statusCode, "invalid bot identity")
	}
	return bot, nil
}

// ConfigureWebhook is a no-op when webhookURL is empty and otherwise delegates
// to SetWebhook.
func (c *Client) ConfigureWebhook(ctx context.Context, webhookURL, secretToken string, dropPendingUpdates bool) error {
	if strings.TrimSpace(webhookURL) == "" {
		return nil
	}
	return c.SetWebhook(ctx, webhookURL, secretToken, dropPendingUpdates)
}

// SetWebhook configures Telegram to deliver message updates to webhookURL.
func (c *Client) SetWebhook(ctx context.Context, webhookURL, secretToken string, dropPendingUpdates bool) error {
	webhookURL = strings.TrimSpace(webhookURL)
	if !validWebhookURL(webhookURL) {
		return validationAPIError(setWebhookMethod, "webhook URL is invalid")
	}
	secretToken = strings.TrimSpace(secretToken)
	if secretToken != "" && !validWebhookSecret(secretToken) {
		return validationAPIError(setWebhookMethod, "webhook secret is invalid")
	}
	request := setWebhookRequest{
		URL:                webhookURL,
		SecretToken:        secretToken,
		DropPendingUpdates: dropPendingUpdates,
		AllowedUpdates:     []string{"message"},
	}
	var applied bool
	meta, err := c.do(ctx, http.MethodPost, setWebhookMethod, request, []string{secretToken}, &applied)
	if err != nil {
		return err
	}
	if !applied {
		return c.invalidSuccessError(setWebhookMethod, meta.statusCode, "webhook was not applied")
	}
	return nil
}

// SendMessage sends text to a parsed Telegram reply target and classifies every
// outcome. A non-nil error is always an *APIError with the same classification
// as the returned result.
func (c *Client) SendMessage(ctx context.Context, target ReplyTarget, text string) (SendResult, error) {
	if err := target.Validate(); err != nil {
		apiErr := validationAPIError(sendMessageMethod, err.Error())
		return sendResultFromError(apiErr), apiErr
	}
	if !validOutboundText(text) {
		apiErr := validationAPIError(sendMessageMethod, "message text is invalid")
		return sendResultFromError(apiErr), apiErr
	}
	text, truncated := truncateTelegramText(text)
	request := sendMessageRequest{ChatID: target.ChatID, MessageThreadID: target.ThreadID, Text: text}
	if target.MessageID > 0 {
		request.ReplyParameters = &replyParameters{MessageID: target.MessageID, AllowSendingWithoutReply: true}
	}
	if c.disableLinkPreviews {
		request.LinkPreviewOptions = &linkPreviewOptions{IsDisabled: true}
	}
	var message Message
	meta, err := c.do(ctx, http.MethodPost, sendMessageMethod, request, nil, &message)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return sendResultFromError(apiErr), err
		}
		fallback := &APIError{Operation: sendMessageMethod, Classification: ResultAmbiguous, Description: "request outcome is unknown"}
		return sendResultFromError(fallback), fallback
	}
	if message.MessageID <= 0 || message.Chat.ID != target.ChatID ||
		(target.ThreadID > 0 && message.MessageThreadID != target.ThreadID) {
		apiErr := c.invalidSuccessError(sendMessageMethod, meta.statusCode, "invalid message result")
		return sendResultFromError(apiErr), apiErr
	}
	messageCopy := message
	return SendResult{
		Classification:    ResultDelivered,
		HTTPStatus:        meta.statusCode,
		MessageID:         message.MessageID,
		ProviderMessageID: "telegram:" + strconv.FormatInt(target.ChatID, 10) + ":" + strconv.FormatInt(message.MessageID, 10),
		Message:           &messageCopy,
		Truncated:         truncated,
	}, nil
}

// ClassifyTelegramFailure classifies a definitive Telegram or HTTP failure.
// A zero/unknown status is ambiguous because the provider outcome is unknown.
func ClassifyTelegramFailure(httpStatus, errorCode int) ResultClass {
	code := errorCode
	if code == 0 {
		code = httpStatus
	}
	switch {
	case code == http.StatusRequestTimeout,
		code == http.StatusConflict,
		code == http.StatusTooEarly,
		code == 420,
		code == http.StatusTooManyRequests,
		code >= 500 && code <= 599:
		return ResultRetryable
	case code >= 400 && code <= 499:
		return ResultNonRetryable
	case httpStatus >= 300 && httpStatus <= 499:
		return ResultNonRetryable
	case httpStatus >= 500 && httpStatus <= 599:
		return ResultRetryable
	default:
		return ResultAmbiguous
	}
}

type setWebhookRequest struct {
	URL                string   `json:"url"`
	SecretToken        string   `json:"secret_token,omitempty"`
	DropPendingUpdates bool     `json:"drop_pending_updates,omitempty"`
	AllowedUpdates     []string `json:"allowed_updates"`
}

type setWebhookWire struct {
	URL                string   `json:"url"`
	SecretToken        string   `json:"secret_token,omitempty"`
	DropPendingUpdates bool     `json:"drop_pending_updates,omitempty"`
	MaxConnections     int      `json:"max_connections"`
	AllowedUpdates     []string `json:"allowed_updates"`
}

func (request setWebhookRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(setWebhookWire{
		request.URL,
		request.SecretToken,
		request.DropPendingUpdates,
		telegramWebhookMaxConnections,
		request.AllowedUpdates,
	})
}

type replyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply,omitempty"`
}

type linkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled"`
}

type sendMessageRequest struct {
	ChatID             int64               `json:"chat_id"`
	MessageThreadID    int64               `json:"message_thread_id,omitempty"`
	Text               string              `json:"text"`
	ReplyParameters    *replyParameters    `json:"reply_parameters,omitempty"`
	LinkPreviewOptions *linkPreviewOptions `json:"link_preview_options,omitempty"`
}

type apiEnvelope struct {
	OK          *bool               `json:"ok"`
	Result      json.RawMessage     `json:"result"`
	ErrorCode   int                 `json:"error_code,omitempty"`
	Description string              `json:"description,omitempty"`
	Parameters  *ResponseParameters `json:"parameters,omitempty"`
}

type responseMeta struct {
	statusCode int
}

func (c *Client) do(
	ctx context.Context,
	httpMethod string,
	operation string,
	payload any,
	extraSecrets []string,
	result any,
) (responseMeta, error) {
	var requestBody io.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return responseMeta{}, &APIError{
				Operation: operation, Classification: ResultNonRetryable, Description: "request could not be encoded",
			}
		}
		requestBody = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, httpMethod, c.endpoint(operation), requestBody)
	if err != nil {
		return responseMeta{}, &APIError{
			Operation: operation, Classification: ResultNonRetryable, Description: "request could not be created",
		}
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	var wroteRequest atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) }}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := c.httpClient.Do(request)
	if err != nil {
		return responseMeta{}, c.transportError(ctx, operation, err, wroteRequest.Load())
	}
	defer response.Body.Close() //nolint:errcheck
	meta := responseMeta{statusCode: response.StatusCode}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBodyBytes+1))
	if readErr != nil {
		return meta, c.responseError(operation, response.StatusCode, "response body could not be read")
	}
	if int64(len(body)) > c.maxResponseBodyBytes {
		return meta, c.responseError(operation, response.StatusCode, "response body exceeded limit")
	}

	var envelope apiEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		return meta, c.responseError(operation, response.StatusCode, "response was not valid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return meta, c.responseError(operation, response.StatusCode, "response contained trailing data")
	}
	if envelope.OK == nil {
		return meta, c.responseError(operation, response.StatusCode, "response omitted result status")
	}
	if !*envelope.OK {
		retryAfter := retryAfterDuration(envelope.Parameters)
		description := c.safeDescription(envelope.Description, extraSecrets...)
		if description == "" {
			description = "Telegram rejected the request"
		}
		classification := ClassifyTelegramFailure(response.StatusCode, envelope.ErrorCode)
		return meta, &APIError{
			Operation: operation, Classification: classification, HTTPStatus: response.StatusCode,
			ErrorCode: envelope.ErrorCode, Description: description, RetryAfter: retryAfter,
		}
	}
	if len(envelope.Result) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Result), []byte("null")) {
		return meta, c.invalidSuccessError(operation, response.StatusCode, "response omitted result")
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return meta, c.invalidSuccessError(operation, response.StatusCode, "response result was invalid")
	}
	return meta, nil
}

func (c *Client) endpoint(operation string) string {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/bot" + c.botToken + "/" + operation
	endpoint.RawPath = ""
	return endpoint.String()
}

func (c *Client) transportError(ctx context.Context, operation string, transportErr error, wroteRequest bool) *APIError {
	description := "request failed before a complete Telegram response was received"
	var cause error
	if ctxErr := ctx.Err(); ctxErr != nil {
		cause = ctxErr
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			description = "request deadline exceeded; outcome is unknown"
		case errors.Is(ctxErr, context.Canceled):
			description = "request was canceled; outcome is unknown"
		}
	} else {
		var netErr net.Error
		if errors.As(transportErr, &netErr) && netErr.Timeout() {
			description = "request timed out; outcome is unknown"
		}
	}
	classification := ResultAmbiguous
	if !wroteRequest {
		classification = ResultRetryable
		description = "request failed before it was sent"
	}
	return &APIError{
		Operation: operation, Classification: classification,
		Description: description, cause: cause,
	}
}

func (c *Client) responseError(operation string, statusCode int, description string) *APIError {
	// Without a valid Telegram envelope, an intermediary may have replaced the
	// provider response after Telegram accepted the message. Retrying would risk
	// a duplicate, so malformed responses are always ambiguous.
	return &APIError{
		Operation: operation, Classification: ResultAmbiguous, HTTPStatus: statusCode, Description: description,
	}
}

func (c *Client) invalidSuccessError(operation string, statusCode int, description string) *APIError {
	return &APIError{
		Operation: operation, Classification: ResultAmbiguous, HTTPStatus: statusCode, Description: description,
	}
}

func (c *Client) safeDescription(value string, extraSecrets ...string) string {
	secrets := make([]string, 0, len(extraSecrets)+1)
	secrets = append(secrets, c.botToken)
	secrets = append(secrets, extraSecrets...)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
		value = strings.ReplaceAll(value, url.PathEscape(secret), "[REDACTED]")
		value = strings.ReplaceAll(value, url.QueryEscape(secret), "[REDACTED]")
	}
	return protocol.SanitizeMessage(value, maxProviderDescription)
}

func sendResultFromError(apiErr *APIError) SendResult {
	if apiErr == nil {
		return SendResult{Classification: ResultAmbiguous, Description: "request outcome is unknown"}
	}
	return SendResult{
		Classification: apiErr.Classification,
		HTTPStatus:     apiErr.HTTPStatus,
		ErrorCode:      apiErr.ErrorCode,
		Description:    apiErr.Description,
		RetryAfter:     apiErr.RetryAfter,
	}
}

func validationAPIError(operation, description string) *APIError {
	return &APIError{Operation: operation, Classification: ResultNonRetryable, Description: description}
}

func retryAfterDuration(parameters *ResponseParameters) time.Duration {
	if parameters == nil || parameters.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(parameters.RetryAfter) * time.Second
}

func parseBaseURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("telegram API base URL is invalid")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, errors.New("telegram API base URL must use http or https")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed, nil
}

func validBotToken(token string) bool {
	if token == "" || len(token) > 512 || strings.ContainsAny(token, "/\\?#") {
		return false
	}
	return !strings.ContainsFunc(token, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func validWebhookURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.User == nil
}

func validWebhookSecret(secret string) bool {
	if len(secret) > 256 {
		return false
	}
	for _, r := range secret {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return secret != ""
}

func validOutboundText(text string) bool {
	if text == "" || len(text) > protocol.MaxTextBytes || !utf8.ValidString(text) {
		return false
	}
	return !strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
	})
}

func truncateTelegramText(text string) (string, bool) {
	// sendMessage limits text by Unicode characters. Telegram entity offsets use
	// UTF-16 units, but that is a separate contract and no parse mode is used here.
	if utf8.RuneCountInString(text) <= telegramSendMessageMaxCharacters {
		return text, false
	}
	limit := telegramSendMessageMaxCharacters - utf8.RuneCountInString(telegramTruncationSuffix)
	var builder strings.Builder
	used := 0
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		cluster := graphemes.Str()
		clusterRunes := utf8.RuneCountInString(cluster)
		if used+clusterRunes > limit {
			break
		}
		builder.WriteString(cluster)
		used += clusterRunes
	}
	builder.WriteString(telegramTruncationSuffix)
	return builder.String(), true
}
