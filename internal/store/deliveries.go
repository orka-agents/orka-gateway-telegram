package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DeliveryState is the durable state of one outbound Telegram send.
type DeliveryState string

const (
	deliverySendingLease = 2 * time.Minute

	DeliveryStateSending   DeliveryState = "sending"
	DeliveryStateRetryable DeliveryState = "retryable"
	DeliveryStateDelivered DeliveryState = "delivered"
	DeliveryStatePermanent DeliveryState = "permanent"
	DeliveryStateUnknown   DeliveryState = "unknown"
)

// Valid reports whether state is persisted by this store.
func (state DeliveryState) Valid() bool {
	switch state {
	case DeliveryStateSending, DeliveryStateRetryable, DeliveryStateDelivered, DeliveryStatePermanent, DeliveryStateUnknown:
		return true
	default:
		return false
	}
}

// Terminal reports whether a delivery must be replayed rather than sent again.
// Unknown is terminal because retrying an ambiguous Telegram send can duplicate it.
func (state DeliveryState) Terminal() bool {
	switch state {
	case DeliveryStateDelivered, DeliveryStatePermanent, DeliveryStateUnknown:
		return true
	default:
		return false
	}
}

// DeliveryRecord is the durable idempotency record for one Orka delivery.
type DeliveryRecord struct {
	DeliveryID        string
	RequestDigest     string
	State             DeliveryState
	ProviderMessageID string
	SafeMessage       string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Terminal reports whether this record is safe to replay without contacting Telegram.
func (r DeliveryRecord) Terminal() bool {
	return r.State.Terminal()
}

// CreateDelivery atomically creates a sending record. The returned boolean is
// true only for the caller that inserted it. Existing retryable records are not
// claimed; callers that execute sends should use BeginDelivery.
func (s *Store) CreateDelivery(ctx context.Context, deliveryID, requestDigest string) (DeliveryRecord, bool, error) {
	return s.createOrBeginDelivery(ctx, deliveryID, requestDigest, false)
}

// BeginDelivery atomically claims a new or retryable delivery for sending. It
// returns shouldSend=false for an in-flight or terminal record, allowing callers
// to replay the stored outcome without a duplicate Telegram request.
func (s *Store) BeginDelivery(ctx context.Context, deliveryID, requestDigest string) (record DeliveryRecord, shouldSend bool, err error) {
	return s.createOrBeginDelivery(ctx, deliveryID, requestDigest, true)
}

func (s *Store) createOrBeginDelivery(ctx context.Context, deliveryID, requestDigest string, claimRetryable bool) (DeliveryRecord, bool, error) {
	deliveryID, err := normalizeIdentity(deliveryID)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	requestDigest, err = normalizeDigest(requestDigest)
	if err != nil {
		return DeliveryRecord{}, false, err
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	record, err := getDelivery(ctx, tx, deliveryID)
	switch {
	case err == nil:
		if record.RequestDigest != requestDigest {
			return DeliveryRecord{}, false, ErrDigestConflict
		}
		if record.State == DeliveryStateSending && time.Since(record.UpdatedAt) >= deliverySendingLease {
			now := nowMillis()
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries
				SET state = 'unknown', provider_message_id = '', safe_message = ?, updated_at_ms = ?
				WHERE delivery_id = ? AND request_digest = ? AND state = 'sending'`,
				DeliveryOutcomeUnknownMessage, now, deliveryID, requestDigest,
			); err != nil {
				return DeliveryRecord{}, false, fmt.Errorf("terminalize stale delivery claim: %w", err)
			}
			record.State = DeliveryStateUnknown
			record.ProviderMessageID = ""
			record.SafeMessage = DeliveryOutcomeUnknownMessage
			record.UpdatedAt = millisTime(now)
			if err := tx.Commit(); err != nil {
				return DeliveryRecord{}, false, fmt.Errorf("commit stale delivery recovery: %w", err)
			}
			return record, false, nil
		}
		if claimRetryable && record.State == DeliveryStateRetryable {
			now := nowMillis()
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries
				SET state = 'sending', provider_message_id = '', safe_message = '', updated_at_ms = ?
				WHERE delivery_id = ? AND request_digest = ? AND state = 'retryable'`,
				now, deliveryID, requestDigest,
			); err != nil {
				return DeliveryRecord{}, false, fmt.Errorf("claim retryable delivery: %w", err)
			}
			record.State = DeliveryStateSending
			record.ProviderMessageID = ""
			record.SafeMessage = ""
			record.UpdatedAt = millisTime(now)
			if err := tx.Commit(); err != nil {
				return DeliveryRecord{}, false, fmt.Errorf("commit retryable delivery claim: %w", err)
			}
			return record, true, nil
		}
		if err := tx.Commit(); err != nil {
			return DeliveryRecord{}, false, fmt.Errorf("commit existing delivery lookup: %w", err)
		}
		return record, false, nil
	case !errors.Is(err, ErrNotFound):
		return DeliveryRecord{}, false, err
	}

	now := nowMillis()
	if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(
		delivery_id, request_digest, state, provider_message_id, safe_message, created_at_ms, updated_at_ms
	) VALUES (?, ?, 'sending', '', '', ?, ?)`, deliveryID, requestDigest, now, now); err != nil {
		return DeliveryRecord{}, false, fmt.Errorf("create delivery record: %w", err)
	}
	record = DeliveryRecord{
		DeliveryID:    deliveryID,
		RequestDigest: requestDigest,
		State:         DeliveryStateSending,
		CreatedAt:     millisTime(now),
		UpdatedAt:     millisTime(now),
	}
	if err := tx.Commit(); err != nil {
		return DeliveryRecord{}, false, fmt.Errorf("commit delivery creation: %w", err)
	}
	return record, true, nil
}

// GetDelivery returns one outbound delivery record.
func (s *Store) GetDelivery(ctx context.Context, deliveryID string) (DeliveryRecord, error) {
	deliveryID, err := normalizeIdentity(deliveryID)
	if err != nil {
		return DeliveryRecord{}, err
	}
	if err := s.ready(); err != nil {
		return DeliveryRecord{}, err
	}
	return getDelivery(ctx, s.db, deliveryID)
}

// UpdateDelivery atomically completes the currently sending attempt. Allowed
// target states are retryable, delivered, permanent, and unknown. Repeating the
// exact same result is idempotent; overwriting a different or terminal result
// returns ErrInvalidTransition.
func (s *Store) UpdateDelivery(
	ctx context.Context,
	deliveryID string,
	requestDigest string,
	state DeliveryState,
	providerMessageID string,
	safeMessage string,
) (DeliveryRecord, error) {
	deliveryID, err := normalizeIdentity(deliveryID)
	if err != nil {
		return DeliveryRecord{}, err
	}
	requestDigest, err = normalizeDigest(requestDigest)
	if err != nil {
		return DeliveryRecord{}, err
	}
	if !state.Valid() || state == DeliveryStateSending {
		return DeliveryRecord{}, fmt.Errorf("%w: delivery target state is invalid", ErrInvalidArgument)
	}
	providerMessageID, err = normalizeProviderMessageID(providerMessageID)
	if err != nil {
		return DeliveryRecord{}, err
	}
	if state != DeliveryStateDelivered {
		providerMessageID = ""
	}
	safeMessage = normalizeSafeMessage(safeMessage)
	if state == DeliveryStateUnknown && safeMessage == "" {
		safeMessage = DeliveryOutcomeUnknownMessage
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return DeliveryRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()

	record, err := getDelivery(ctx, tx, deliveryID)
	if err != nil {
		return DeliveryRecord{}, err
	}
	if record.RequestDigest != requestDigest {
		return DeliveryRecord{}, ErrDigestConflict
	}
	if record.State != DeliveryStateSending {
		if record.State == state && record.ProviderMessageID == providerMessageID && record.SafeMessage == safeMessage {
			if err := tx.Commit(); err != nil {
				return DeliveryRecord{}, fmt.Errorf("commit existing delivery result: %w", err)
			}
			return record, nil
		}
		return DeliveryRecord{}, ErrInvalidTransition
	}

	now := nowMillis()
	result, err := tx.ExecContext(ctx, `UPDATE deliveries
		SET state = ?, provider_message_id = ?, safe_message = ?, updated_at_ms = ?
		WHERE delivery_id = ? AND request_digest = ? AND state = 'sending'`,
		string(state), providerMessageID, safeMessage, now, deliveryID, requestDigest,
	)
	if err != nil {
		return DeliveryRecord{}, fmt.Errorf("update delivery result: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return DeliveryRecord{}, fmt.Errorf("read delivery update result: %w", err)
	}
	if rows != 1 {
		return DeliveryRecord{}, ErrInvalidTransition
	}
	record.State = state
	record.ProviderMessageID = providerMessageID
	record.SafeMessage = safeMessage
	record.UpdatedAt = millisTime(now)
	if err := tx.Commit(); err != nil {
		return DeliveryRecord{}, fmt.Errorf("commit delivery result: %w", err)
	}
	return record, nil
}

// MarkDeliveryDelivered records a successful provider send.
func (s *Store) MarkDeliveryDelivered(ctx context.Context, deliveryID, requestDigest, providerMessageID string) (DeliveryRecord, error) {
	return s.UpdateDelivery(ctx, deliveryID, requestDigest, DeliveryStateDelivered, providerMessageID, "")
}

// MarkDeliveryRetryable records a failure that may be claimed by BeginDelivery.
func (s *Store) MarkDeliveryRetryable(ctx context.Context, deliveryID, requestDigest, safeMessage string) (DeliveryRecord, error) {
	return s.UpdateDelivery(ctx, deliveryID, requestDigest, DeliveryStateRetryable, "", safeMessage)
}

// MarkDeliveryPermanent records a terminal provider rejection.
func (s *Store) MarkDeliveryPermanent(ctx context.Context, deliveryID, requestDigest, safeMessage string) (DeliveryRecord, error) {
	return s.UpdateDelivery(ctx, deliveryID, requestDigest, DeliveryStatePermanent, "", safeMessage)
}

// MarkDeliveryUnknown records an ambiguous terminal outcome that must not be resent.
func (s *Store) MarkDeliveryUnknown(ctx context.Context, deliveryID, requestDigest, safeMessage string) (DeliveryRecord, error) {
	return s.UpdateDelivery(ctx, deliveryID, requestDigest, DeliveryStateUnknown, "", safeMessage)
}

func getDelivery(ctx context.Context, queryer queryRower, deliveryID string) (DeliveryRecord, error) {
	var (
		record          DeliveryRecord
		state           string
		createdAtMillis int64
		updatedAtMillis int64
	)
	err := queryer.QueryRowContext(ctx, `SELECT
		delivery_id, request_digest, state, provider_message_id, safe_message, created_at_ms, updated_at_ms
		FROM deliveries WHERE delivery_id = ?`, deliveryID).Scan(
		&record.DeliveryID,
		&record.RequestDigest,
		&state,
		&record.ProviderMessageID,
		&record.SafeMessage,
		&createdAtMillis,
		&updatedAtMillis,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryRecord{}, wrapNotFound("delivery")
	}
	if err != nil {
		return DeliveryRecord{}, fmt.Errorf("get delivery record: %w", err)
	}
	record.State = DeliveryState(state)
	if !record.State.Valid() {
		return DeliveryRecord{}, errors.New("get delivery record: invalid persisted state")
	}
	record.CreatedAt = millisTime(createdAtMillis)
	record.UpdatedAt = millisTime(updatedAtMillis)
	return record, nil
}
