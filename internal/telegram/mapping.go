package telegram

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sozercan/orka-gateway-telegram/internal/protocol"
)

// MapUpdate maps one Telegram private text message into the Orka gateway
// protocol. Unsupported update kinds return ErrUnsupportedUpdate so webhook
// handlers can acknowledge and ignore them without treating them as failures.
func MapUpdate(botID int64, update Update) (*protocol.EventEnvelope, error) {
	if !IsPrivateTextUpdate(update) {
		return nil, ErrUnsupportedUpdate
	}
	message := update.Message
	if botID <= 0 || update.UpdateID < 0 || message == nil || message.MessageID <= 0 ||
		message.Chat.ID == 0 || message.From == nil || message.From.ID == 0 || message.Date <= 0 {
		return nil, ErrInvalidUpdate
	}
	if !validMessageText(message.Text) {
		return nil, fmt.Errorf("%w: text is invalid", ErrInvalidUpdate)
	}

	occurredAt := unixTime(message.Date)
	event := &protocol.EventEnvelope{
		ProtocolVersion: protocol.Version,
		ExternalEventID: fmt.Sprintf("telegram:%d:%d", botID, update.UpdateID),
		EventType:       protocol.EventTypeText,
		AccountID:       strconv.FormatInt(botID, 10),
		ContextID:       strconv.FormatInt(message.Chat.ID, 10),
		Sender: protocol.Sender{
			ID:          strconv.FormatInt(message.From.ID, 10),
			DisplayName: DisplayName(*message.From),
		},
		Text:        message.Text,
		ReplyTarget: FormatReplyTarget(message.Chat.ID, message.MessageThreadID, message.MessageID),
		OccurredAt:  &occurredAt,
	}
	if message.MessageThreadID > 0 {
		event.ThreadID = strconv.FormatInt(message.MessageThreadID, 10)
	}
	return event, nil
}

// UpdateToEvent is a convenience alias with update-first argument ordering.
func UpdateToEvent(update Update, botID int64) (*protocol.EventEnvelope, error) {
	return MapUpdate(botID, update)
}

// IsPrivateTextUpdate reports whether an update is in the adapter's supported
// ingress subset. Edited messages, channel posts, non-private chats, and
// non-text messages are intentionally ignored.
func IsPrivateTextUpdate(update Update) bool {
	return update.Message != nil && update.Message.Chat.Type == "private" && update.Message.Text != ""
}

// DisplayName builds and sanitizes the display name exposed to Orka. Telegram
// first/last names take precedence; username is a fallback.
func DisplayName(user User) string {
	name := strings.TrimSpace(strings.Join([]string{user.FirstName, user.LastName}, " "))
	name = SanitizeDisplayName(name)
	if name == "" {
		name = SanitizeDisplayName(user.Username)
	}
	return name
}

// SanitizeDisplayName strips controls and Unicode formatting controls, folds
// whitespace, and bounds the result to the protocol identity limit without
// splitting UTF-8 sequences.
func SanitizeDisplayName(value string) string {
	value = strings.ToValidUTF8(value, "")
	var builder strings.Builder
	builder.Grow(min(len(value), protocol.MaxIdentityBytes))
	pendingSpace := false
	for _, r := range value {
		if unicode.IsSpace(r) {
			if builder.Len() > 0 {
				pendingSpace = true
			}
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if pendingSpace {
			if builder.Len()+1 > protocol.MaxIdentityBytes {
				break
			}
			builder.WriteByte(' ')
			pendingSpace = false
		}
		encodedLen := utf8.RuneLen(r)
		if encodedLen < 0 || builder.Len()+encodedLen > protocol.MaxIdentityBytes {
			break
		}
		builder.WriteRune(r)
	}
	return strings.TrimSpace(builder.String())
}

func validMessageText(text string) bool {
	if text == "" || len(text) > protocol.MaxTextBytes || !utf8.ValidString(text) {
		return false
	}
	return !strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
	})
}

func unixTime(seconds int64) time.Time {
	return time.Unix(seconds, 0).UTC()
}
