package telegram

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FormatReplyTarget encodes a Telegram destination for an Orka event.
func FormatReplyTarget(chatID, threadID, messageID int64) string {
	return ReplyTarget{ChatID: chatID, ThreadID: threadID, MessageID: messageID}.String()
}

// ParseReplyTarget parses the adapter's compact reply target. The special
// "conformance" target resolves to the caller-supplied chat ID and deliberately
// carries no thread or source-message ID.
func ParseReplyTarget(value string, conformanceChatID int64) (ReplyTarget, error) {
	value = strings.TrimSpace(value)
	if value == ConformanceReplyTarget {
		target := ReplyTarget{ChatID: conformanceChatID}
		if err := target.Validate(); err != nil {
			return ReplyTarget{}, errors.New("telegram conformance chat id is required")
		}
		return target, nil
	}

	parts := strings.Split(value, ":")
	if len(parts) != 5 || parts[0] != "tg" || parts[1] != "v1" {
		return ReplyTarget{}, errors.New("invalid telegram reply target")
	}
	chatID, err := parseCanonicalInt64(parts[2])
	if err != nil {
		return ReplyTarget{}, fmt.Errorf("invalid telegram reply target chat id: %w", err)
	}
	threadID, err := parseCanonicalInt64(parts[3])
	if err != nil {
		return ReplyTarget{}, fmt.Errorf("invalid telegram reply target thread id: %w", err)
	}
	messageID, err := parseCanonicalInt64(parts[4])
	if err != nil {
		return ReplyTarget{}, fmt.Errorf("invalid telegram reply target message id: %w", err)
	}
	target := ReplyTarget{ChatID: chatID, ThreadID: threadID, MessageID: messageID}
	if err := target.Validate(); err != nil {
		return ReplyTarget{}, err
	}
	return target, nil
}

func parseCanonicalInt64(value string) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "+") {
		return 0, errors.New("invalid integer")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != value {
		return 0, errors.New("invalid integer")
	}
	return parsed, nil
}
