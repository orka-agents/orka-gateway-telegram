// Package faultproxy implements a deterministic, in-memory Telegram Bot API
// fault simulator. It retains only bounded, token-free request metadata.
package faultproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

const (
	maxRetryAfterSeconds = int64((1<<63 - 1) / int64(time.Second))

	// HealthPath is the unauthenticated liveness endpoint.
	HealthPath = "/healthz"
	// ReadyPath is the unauthenticated readiness endpoint.
	ReadyPath = "/readyz"
	// ControlPlanPath replaces or resets the one-shot FIFO action plan.
	ControlPlanPath = "/control/plan"
	// ControlStatePath returns pending actions and safe audit metadata.
	ControlStatePath = "/control/state"
)

// ActionKind identifies a one-shot sendMessage outcome.
type ActionKind string

const (
	ActionSuccess           ActionKind = "success"
	ActionRateLimit         ActionKind = "rate_limit"
	ActionServerError       ActionKind = "server_error"
	ActionBadRequest        ActionKind = "bad_request"
	ActionForbidden         ActionKind = "forbidden"
	ActionMalformedSuccess  ActionKind = "malformed_success"
	ActionMismatchedSuccess ActionKind = "mismatched_success"
	ActionDropAfterRead     ActionKind = "drop_after_read"
	ActionDelayedSuccess    ActionKind = "delayed_success"
)

// Action configures one FIFO sendMessage outcome. retry_after is expressed in
// whole seconds and delay_ms is expressed in milliseconds. Zero uses the
// server default for the corresponding action.
type Action struct {
	Kind              ActionKind `json:"action"`
	RetryAfterSeconds int        `json:"retry_after,omitempty"`
	DelayMilliseconds int64      `json:"delay_ms,omitempty"`
}

// UnmarshalJSON accepts both the canonical object form and a compact string
// form such as "server_error" for actions that need no parameters.
func (a *Action) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("action is empty")
	}
	if data[0] == '"' {
		var kind ActionKind
		if err := json.Unmarshal(data, &kind); err != nil {
			return errors.New("action is invalid")
		}
		*a = Action{Kind: kind}
		return nil
	}
	var wire struct {
		Kind              ActionKind `json:"action"`
		Type              ActionKind `json:"type"`
		RetryAfterSeconds int        `json:"retry_after,omitempty"`
		DelayMilliseconds int64      `json:"delay_ms,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return errors.New("action is invalid")
	}
	if wire.Kind != "" && wire.Type != "" && wire.Kind != wire.Type {
		return errors.New("action kind is ambiguous")
	}
	kind := wire.Kind
	if kind == "" {
		kind = wire.Type
	}
	*a = Action{
		Kind:              kind,
		RetryAfterSeconds: wire.RetryAfterSeconds,
		DelayMilliseconds: wire.DelayMilliseconds,
	}
	return nil
}

// Validate checks that an action is supported and only carries parameters
// relevant to that action.
func (a Action) Validate() error {
	switch a.Kind {
	case ActionSuccess, ActionServerError, ActionBadRequest, ActionForbidden,
		ActionMalformedSuccess, ActionMismatchedSuccess, ActionDropAfterRead:
	case ActionRateLimit:
		if a.RetryAfterSeconds < 0 {
			return errors.New("retry_after must not be negative")
		}
		if int64(a.RetryAfterSeconds) > maxRetryAfterSeconds {
			return errors.New("retry_after exceeds the supported duration")
		}
	case ActionDelayedSuccess:
		if a.DelayMilliseconds < 0 {
			return errors.New("delay_ms must not be negative")
		}
		const maxDelayMilliseconds = int64(^uint64(0)>>1) / int64(time.Millisecond)
		if a.DelayMilliseconds > maxDelayMilliseconds {
			return errors.New("delay_ms is too large")
		}
	default:
		return errors.New("unsupported action")
	}
	if a.Kind != ActionRateLimit && a.RetryAfterSeconds != 0 {
		return errors.New("retry_after is only valid for rate_limit")
	}
	if a.Kind != ActionDelayedSuccess && a.DelayMilliseconds != 0 {
		return errors.New("delay_ms is only valid for delayed_success")
	}
	return nil
}

// ReplyTarget is the safe subset of Telegram reply routing metadata retained
// in the audit log.
type ReplyTarget struct {
	MessageThreadID int64 `json:"message_thread_id,omitempty"`
	MessageID       int64 `json:"message_id,omitempty"`
}

// AuditEntry contains only safe metadata. TextSHA256 is lowercase hexadecimal,
// and TextLength is the UTF-8 byte length of the request text.
type AuditEntry struct {
	Sequence    uint64      `json:"sequence"`
	Action      ActionKind  `json:"action"`
	ChatID      int64       `json:"chat_id"`
	TextLength  int         `json:"text_length"`
	TextSHA256  string      `json:"text_sha256"`
	ReplyTarget ReplyTarget `json:"reply_target"`
	Timestamp   time.Time   `json:"timestamp"`
}

// State is a defensive snapshot of the pending plan and retained audit log.
type State struct {
	PendingActions []Action     `json:"pending_actions"`
	Audit          []AuditEntry `json:"audit"`
}

// Config controls the in-memory server. ControlToken is required and is hashed
// during construction rather than retained as plaintext.
type Config struct {
	ControlToken             string
	BotID                    int64
	DefaultRetryAfterSeconds int
	DefaultDelay             time.Duration
	MaxRequestBodyBytes      int64
	AuditCapacity            int
	Now                      func() time.Time
}
