package protocol

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

func DecodeDeliveryRequest(body []byte) (*DeliveryRequest, error) {
	if len(body) == 0 || len(body) > MaxHTTPBodyBytes {
		return nil, errors.New("invalid delivery request size")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request DeliveryRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("invalid delivery request: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := ValidateDeliveryRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

func ValidateDeliveryRequest(request *DeliveryRequest) error {
	if request == nil {
		return errors.New("delivery request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
	}
	for name, value := range map[string]string{
		"deliveryId": request.DeliveryID, "idempotencyId": request.IdempotencyID,
		"originatingEventId": request.OriginatingEvent, "accountId": request.AccountID,
		"contextId": request.ContextID, "replyTarget": request.ReplyTarget,
	} {
		if err := validateRequiredIdentity(name, value); err != nil {
			return err
		}
	}
	if request.Kind != DeliveryKindFinal && request.Kind != DeliveryKindError {
		return fmt.Errorf("unsupported delivery kind %q", request.Kind)
	}
	if request.Text == "" || len(request.Text) > MaxTextBytes || !utf8.ValidString(request.Text) || containsUnsafeControl(request.Text, true) {
		return errors.New("delivery text is invalid")
	}
	return nil
}

func ValidateIngressResponse(response *IngressResponse) error {
	if response == nil {
		return errors.New("ingress response is required")
	}
	switch response.Status {
	case IngressStatusAccepted, IngressStatusDuplicate, IngressStatusRejected, IngressStatusDeadLettered:
	default:
		return fmt.Errorf("unsupported ingress status %q", response.Status)
	}
	if err := validateRequiredIdentity("eventId", response.EventID); err != nil {
		return err
	}
	switch response.State {
	case "Accepted", "Queued", "Dispatching", "TaskCreated", "Completed", "Rejected", "DeadLettered", "Expired":
	default:
		return fmt.Errorf("unsupported ingress state %q", response.State)
	}
	if len(response.Message) > 1024 || !utf8.ValidString(response.Message) || containsUnsafeControl(response.Message, true) {
		return errors.New("ingress message is invalid")
	}
	return nil
}

func ConstantTimeBearerEqual(got, want string) bool {
	got = strings.TrimSpace(got)
	want = strings.TrimSpace(want)
	if got == "" || want == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func BearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func SanitizeMessage(message string, limit int) string {
	message = strings.TrimSpace(message)
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, message)
	if limit > 0 && len(message) > limit {
		message = message[:limit]
		for !utf8.ValidString(message) && len(message) > 0 {
			message = message[:len(message)-1]
		}
	}
	return message
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return errors.New("request must contain exactly one JSON value")
}

func validateRequiredIdentity(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if value != strings.TrimSpace(value) || len(value) > MaxIdentityBytes || !utf8.ValidString(value) || containsUnsafeControl(value, false) {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func containsUnsafeControl(value string, allowNewlines bool) bool {
	return strings.ContainsFunc(value, func(r rune) bool {
		if !unicode.IsControl(r) {
			return false
		}
		return !(allowNewlines && (r == '\n' || r == '\r' || r == '\t'))
	})
}
