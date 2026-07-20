package telegram

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultMaxResponseBodyBytes bounds every Telegram API response read by a
	// Client. Telegram responses used by this adapter are small; keeping the
	// bound conservative prevents a broken or hostile endpoint from exhausting
	// memory.
	DefaultMaxResponseBodyBytes int64 = 256 << 10

	ReplyTargetPrefix        = "tg:v1:"
	ConformanceReplyTarget   = "conformance"
	maxProviderDescription   = 1024
	defaultHTTPClientTimeout = 15 * time.Second
)

var (
	ErrUnsupportedUpdate = errors.New("telegram update is not a private text message")
	ErrInvalidUpdate     = errors.New("telegram update is invalid")
)

// User is the subset of a Telegram User used by the adapter.
type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name,omitempty"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

// Chat is the subset of a Telegram Chat used by the adapter.
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	Title     string `json:"title,omitempty"`
}

// Message is the subset of a Telegram Message used for ingress mapping and
// sendMessage results.
type Message struct {
	MessageID       int64      `json:"message_id"`
	MessageThreadID int64      `json:"message_thread_id,omitempty"`
	From            *User      `json:"from,omitempty"`
	Date            int64      `json:"date"`
	Chat            Chat       `json:"chat"`
	Text            string     `json:"text,omitempty"`
	ReplyToMessage  *Message   `json:"reply_to_message,omitempty"`
	Quote           *TextQuote `json:"quote,omitempty"`
}

// TextQuote is the bounded subset of Telegram quote metadata used to preserve
// reply intent for conversational agents.
type TextQuote struct {
	Text string `json:"text"`
}

// Update is the subset of a Telegram Update accepted by the webhook. Only
// Message is mapped to Orka; the other fields are retained so callers can
// decode Telegram payloads without treating unsupported update kinds as
// malformed JSON.
type Update struct {
	UpdateID      int64    `json:"update_id"`
	Message       *Message `json:"message,omitempty"`
	EditedMessage *Message `json:"edited_message,omitempty"`
	ChannelPost   *Message `json:"channel_post,omitempty"`
}

// ReplyTarget is the Telegram destination encoded in an Orka replyTarget.
type ReplyTarget struct {
	ChatID    int64
	ThreadID  int64
	MessageID int64
}

// String returns the canonical compact reply target.
func (t ReplyTarget) String() string {
	return fmt.Sprintf("%s%d:%d:%d", ReplyTargetPrefix, t.ChatID, t.ThreadID, t.MessageID)
}

// Validate checks whether the target can be used by sendMessage.
func (t ReplyTarget) Validate() error {
	if t.ChatID == 0 {
		return errors.New("telegram reply target chat id is required")
	}
	if t.ThreadID < 0 {
		return errors.New("telegram reply target thread id is invalid")
	}
	if t.MessageID < 0 {
		return errors.New("telegram reply target message id is invalid")
	}
	return nil
}

// ResultClass describes whether an attempted Telegram send is known to have
// succeeded, is safe to retry, is known to be permanent, or has an uncertain
// outcome. Ambiguous results must not be blindly retried because Telegram may
// already have accepted the message.
type ResultClass string

const (
	ResultDelivered    ResultClass = "delivered"
	ResultRetryable    ResultClass = "retryable"
	ResultNonRetryable ResultClass = "non-retryable"
	ResultAmbiguous    ResultClass = "ambiguous"
)

// Classification is an alias kept for callers that prefer the more explicit
// name.
type Classification = ResultClass

const (
	ClassificationDelivered    = ResultDelivered
	ClassificationRetryable    = ResultRetryable
	ClassificationNonRetryable = ResultNonRetryable
	ClassificationAmbiguous    = ResultAmbiguous
)

// ResponseParameters is Telegram's optional error metadata.
type ResponseParameters struct {
	MigrateToChatID int64 `json:"migrate_to_chat_id,omitempty"`
	RetryAfter      int   `json:"retry_after,omitempty"`
}

// SendResult is the classified outcome of sendMessage. When Classification is
// ResultDelivered, MessageID and ProviderMessageID are populated. Description
// is bounded, sanitized provider text and never contains the bot token.
type SendResult struct {
	Classification    ResultClass
	HTTPStatus        int
	ErrorCode         int
	Description       string
	RetryAfter        time.Duration
	MessageID         int64
	ProviderMessageID string
	Message           *Message
	Truncated         bool
}

// Delivered reports whether Telegram confirmed the send.
func (r SendResult) Delivered() bool {
	return r.Classification == ResultDelivered
}

// APIError is a safe, classified Telegram API error. Its Error output excludes
// request URLs and bot tokens so it can be logged by callers.
type APIError struct {
	Operation      string
	Classification ResultClass
	HTTPStatus     int
	ErrorCode      int
	Description    string
	RetryAfter     time.Duration
	cause          error
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	operation := strings.TrimSpace(e.Operation)
	if operation == "" {
		operation = "request"
	}
	message := "telegram " + operation + " failed"
	if e.HTTPStatus != 0 {
		message += " with HTTP " + strconv.Itoa(e.HTTPStatus)
	}
	if e.ErrorCode != 0 && e.ErrorCode != e.HTTPStatus {
		message += " (error " + strconv.Itoa(e.ErrorCode) + ")"
	}
	if e.Description != "" {
		message += ": " + e.Description
	}
	return message
}

func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
