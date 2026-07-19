package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func validDelivery() DeliveryRequest {
	return DeliveryRequest{
		ProtocolVersion: Version,
		DeliveryID:      "delivery-1", IdempotencyID: "delivery-1", OriginatingEvent: "event-1",
		Kind: DeliveryKindFinal, AccountID: "bot-1", ContextID: "chat-1", ReplyTarget: "tg:v1:1:0:2",
		Text: "hello",
	}
}

func TestDecodeDeliveryRequest(t *testing.T) {
	request := validDelivery()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeDeliveryRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryID != request.DeliveryID {
		t.Fatalf("deliveryID = %q", got.DeliveryID)
	}
}

func TestDecodeDeliveryRequestRejectsUnknownAndOversized(t *testing.T) {
	if _, err := DecodeDeliveryRequest([]byte(`{"protocolVersion":"orka.gateway.v1","unknown":true}`)); err == nil {
		t.Fatal("expected unknown field rejection")
	}
	request := validDelivery()
	request.Text = strings.Repeat("x", MaxTextBytes+1)
	body, _ := json.Marshal(request)
	if _, err := DecodeDeliveryRequest(body); err == nil {
		t.Fatal("expected oversized text rejection")
	}
}

func TestConstantTimeBearerEqual(t *testing.T) {
	if !ConstantTimeBearerEqual("token", "token") {
		t.Fatal("matching tokens rejected")
	}
	if ConstantTimeBearerEqual("token", "other") || ConstantTimeBearerEqual("", "") {
		t.Fatal("invalid tokens accepted")
	}
}

func TestValidateIngressResponse(t *testing.T) {
	valid := &IngressResponse{Status: IngressStatusAccepted, EventID: "gev-1", State: "Queued"}
	if err := ValidateIngressResponse(valid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []*IngressResponse{
		{Status: "ok", EventID: "gev-1", State: "Queued"},
		{Status: IngressStatusAccepted, EventID: "", State: "Queued"},
		{Status: IngressStatusAccepted, EventID: "gev-1", State: "Unknown"},
	} {
		if err := ValidateIngressResponse(invalid); err == nil {
			t.Fatalf("expected validation error for %+v", invalid)
		}
	}
}

func TestValidateDeliveryRequestRequiresExactProtocolVersion(t *testing.T) {
	request := validDelivery()
	request.ProtocolVersion = " " + Version + " "
	if err := ValidateDeliveryRequest(&request); err == nil {
		t.Fatal("expected noncanonical protocol version rejection")
	}
}
