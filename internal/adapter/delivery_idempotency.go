package adapter

import (
	"encoding/json"

	"github.com/sozercan/orka-gateway-telegram/internal/protocol"
	"github.com/sozercan/orka-gateway-telegram/internal/store"
)

// deliveryRequestDigests returns both the current logical-delivery digest and
// the legacy v1 digest. V1 hashed the transport deliveryId; keeping that digest
// lets an exact pre-upgrade retry adopt its original idempotencyId without a
// second provider send.
func deliveryRequestDigests(request *protocol.DeliveryRequest) (logical string, legacy string, err error) {
	legacyPayload, err := json.Marshal(request)
	if err != nil {
		return "", "", err
	}
	canonical := *request
	canonical.DeliveryID = canonical.IdempotencyID
	logicalPayload, err := json.Marshal(&canonical)
	if err != nil {
		return "", "", err
	}
	return store.DigestBytes(logicalPayload), store.DigestBytes(legacyPayload), nil
}
