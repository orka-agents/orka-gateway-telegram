package protocol

import "time"

const (
	Version = "orka.gateway.v1"

	EventTypeText = "text"

	DeliveryKindFinal = "final"
	DeliveryKindError = "error"

	DeliveryStatusDelivered         = "delivered"
	DeliveryStatusRetryableError    = "retryableError"
	DeliveryStatusNonRetryableError = "nonRetryableError"

	IngressStatusAccepted     = "accepted"
	IngressStatusDuplicate    = "duplicate"
	IngressStatusRejected     = "rejected"
	IngressStatusDeadLettered = "deadLettered"

	MaxHTTPBodyBytes        = 256 << 10
	MaxTextBytes            = 64 << 10
	MaxIdentityBytes        = 256
	MaxMetadataValueBytes   = 256
	MaxAdapterResponseBytes = 64 << 10

	MetadataReplyToMessageID = "replyToMessageId"
	MetadataReplyToText      = "replyToText"
	MetadataQuoteText        = "quoteText"
)

type Sender struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
}

type EventEnvelope struct {
	ProtocolVersion string            `json:"protocolVersion"`
	ExternalEventID string            `json:"externalEventId"`
	EventType       string            `json:"eventType"`
	AccountID       string            `json:"accountId"`
	ContextID       string            `json:"contextId"`
	ThreadID        string            `json:"threadId,omitempty"`
	Sender          Sender            `json:"sender"`
	Text            string            `json:"text"`
	ReplyTarget     string            `json:"replyTarget,omitempty"`
	OccurredAt      *time.Time        `json:"occurredAt,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type IngressResponse struct {
	Status  string `json:"status"`
	EventID string `json:"eventId"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type Capabilities struct {
	InboundText        bool `json:"inboundText"`
	OutboundText       bool `json:"outboundText"`
	Threads            bool `json:"threads,omitempty"`
	SenderIdentity     bool `json:"senderIdentity,omitempty"`
	ExplicitSessions   bool `json:"explicitSessions,omitempty"`
	IdempotentDelivery bool `json:"idempotentDelivery"`
}

type CapabilitiesResponse struct {
	ProtocolVersion string       `json:"protocolVersion"`
	AdapterName     string       `json:"adapterName"`
	AdapterVersion  string       `json:"adapterVersion"`
	Capabilities    Capabilities `json:"capabilities"`
}

type HealthResponse struct {
	Status string `json:"status"`
}

type ResourceReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type DeliveryRequest struct {
	ProtocolVersion  string             `json:"protocolVersion"`
	DeliveryID       string             `json:"deliveryId"`
	IdempotencyID    string             `json:"idempotencyId"`
	OriginatingEvent string             `json:"originatingEventId"`
	TaskRef          *ResourceReference `json:"taskRef,omitempty"`
	SessionRef       *ResourceReference `json:"sessionRef,omitempty"`
	Kind             string             `json:"kind"`
	AccountID        string             `json:"accountId"`
	ContextID        string             `json:"contextId"`
	ThreadID         string             `json:"threadId,omitempty"`
	ReplyTarget      string             `json:"replyTarget"`
	Text             string             `json:"text"`
	Metadata         map[string]string  `json:"metadata,omitempty"`
}

type DeliveryResponse struct {
	Status            string `json:"status"`
	ProviderMessageID string `json:"providerMessageId,omitempty"`
	Message           string `json:"message,omitempty"`
}
