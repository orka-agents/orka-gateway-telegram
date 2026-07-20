package telegram

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sozercan/orka-gateway-telegram/internal/protocol"
)

func TestMapUpdatePrivateText(t *testing.T) {
	t.Parallel()

	update := Update{
		UpdateID: 99,
		Message: &Message{
			MessageID:       34,
			MessageThreadID: 12,
			From: &User{
				ID:        789,
				FirstName: "  Ada\u202e\n",
				LastName:  " Lovelace\x00 ",
				Username:  "ignored",
			},
			Date: 1_700_000_000,
			Chat: Chat{ID: 456, Type: "private"},
			Text: "hello\nworld",
			ReplyToMessage: &Message{
				MessageID: 21,
				Text:      "Which deployment?\nThe production one.",
			},
			Quote: &TextQuote{Text: "The production one."},
		},
	}
	event, err := MapUpdate(123, update)
	if err != nil {
		t.Fatal(err)
	}
	if event.ProtocolVersion != protocol.Version || event.EventType != protocol.EventTypeText {
		t.Fatalf("unexpected protocol identity: %+v", event)
	}
	if event.ExternalEventID != "telegram:123:99" || event.AccountID != "123" || event.ContextID != "456" {
		t.Fatalf("unexpected event identity: %+v", event)
	}
	if event.ThreadID != "12" || event.ReplyTarget != "tg:v1:456:12:34" {
		t.Fatalf("unexpected reply routing: %+v", event)
	}
	if event.Sender.ID != "789" || event.Sender.DisplayName != "Ada Lovelace" {
		t.Fatalf("unexpected sender: %+v", event.Sender)
	}
	if event.Text != "hello\nworld" {
		t.Fatalf("text = %q", event.Text)
	}
	if event.Metadata[protocol.MetadataReplyToMessageID] != "21" ||
		event.Metadata[protocol.MetadataReplyToText] != "Which deployment? The production one." ||
		event.Metadata[protocol.MetadataQuoteText] != "The production one." {
		t.Fatalf("reply metadata = %#v", event.Metadata)
	}
	wantTime := time.Unix(1_700_000_000, 0).UTC()
	if event.OccurredAt == nil || !event.OccurredAt.Equal(wantTime) {
		t.Fatalf("occurredAt = %v, want %v", event.OccurredAt, wantTime)
	}
}

func TestMapUpdateRejectsUnsupportedKinds(t *testing.T) {
	t.Parallel()

	privateText := Message{
		MessageID: 1,
		From:      &User{ID: 2, FirstName: "Ada"},
		Date:      1_700_000_000,
		Chat:      Chat{ID: 2, Type: "private"},
		Text:      "hello",
	}
	tests := []struct {
		name   string
		update Update
	}{
		{name: "empty", update: Update{UpdateID: 1}},
		{name: "edited", update: Update{UpdateID: 1, EditedMessage: &privateText}},
		{name: "group", update: Update{UpdateID: 1, Message: &Message{
			MessageID: 1, From: &User{ID: 2}, Date: 1_700_000_000,
			Chat: Chat{ID: -100, Type: "group"}, Text: "hello",
		}}},
		{name: "non text", update: Update{UpdateID: 1, Message: &Message{
			MessageID: 1, From: &User{ID: 2}, Date: 1_700_000_000,
			Chat: Chat{ID: 2, Type: "private"},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := MapUpdate(123, test.update); !errors.Is(err, ErrUnsupportedUpdate) {
				t.Fatalf("error = %v, want ErrUnsupportedUpdate", err)
			}
		})
	}
}

func TestDisplayNameSanitization(t *testing.T) {
	t.Parallel()

	if got := DisplayName(User{FirstName: "\x00\u202e", Username: "  ada\nlovelace  "}); got != "ada lovelace" {
		t.Fatalf("fallback display name = %q", got)
	}
	invalidUTF8 := string([]byte{'a', 0xff, 'b'})
	if got := SanitizeDisplayName(invalidUTF8); got != "ab" {
		t.Fatalf("invalid UTF-8 display name = %q", got)
	}
	got := SanitizeDisplayName(strings.Repeat("é", protocol.MaxIdentityBytes))
	if len(got) > protocol.MaxIdentityBytes || !utf8.ValidString(got) {
		t.Fatalf("sanitized display name length/encoding invalid: %d bytes", len(got))
	}
}

func TestReplyTargetRoundTripAndConformance(t *testing.T) {
	t.Parallel()

	encoded := FormatReplyTarget(-100, 7, 9)
	if encoded != "tg:v1:-100:7:9" {
		t.Fatalf("encoded = %q", encoded)
	}
	target, err := ParseReplyTarget(encoded, 0)
	if err != nil {
		t.Fatal(err)
	}
	if target != (ReplyTarget{ChatID: -100, ThreadID: 7, MessageID: 9}) {
		t.Fatalf("target = %+v", target)
	}

	conformance, err := ParseReplyTarget(ConformanceReplyTarget, 12345)
	if err != nil {
		t.Fatal(err)
	}
	if conformance != (ReplyTarget{ChatID: 12345}) {
		t.Fatalf("conformance target = %+v", conformance)
	}
}

func TestParseReplyTargetRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"", "telegram:v1:1:0:2", "tg:v2:1:0:2", "tg:v1:+1:0:2",
		"tg:v1:01:0:2", "tg:v1:0:0:2", "tg:v1:1:-1:2", "tg:v1:1:0:-2",
	} {
		if _, err := ParseReplyTarget(value, 0); err == nil {
			t.Errorf("ParseReplyTarget(%q) succeeded", value)
		}
	}
	if _, err := ParseReplyTarget(ConformanceReplyTarget, 0); err == nil {
		t.Fatal("conformance target without chat ID succeeded")
	}
}
