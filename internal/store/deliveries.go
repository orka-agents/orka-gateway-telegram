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
	DeliveryID          string
	IdempotencyID       string
	DigestVersion       int
	RequestDigest       string
	LegacyRequestDigest string
	State               DeliveryState
	ProviderMessageID   string
	SafeMessage         string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Terminal reports whether this record is safe to replay without contacting Telegram.
func (r DeliveryRecord) Terminal() bool {
	return r.State.Terminal()
}

// CreateDelivery atomically creates a sending record. The returned boolean is
// true only for the caller that inserted it. Existing retryable records are not
// claimed; callers that execute sends should use BeginDelivery.
func (s *Store) CreateDelivery(ctx context.Context, deliveryID, requestDigest string) (DeliveryRecord, bool, error) {
	return s.CreateDeliveryWithIdempotency(ctx, deliveryID, deliveryID, requestDigest)
}

// CreateDeliveryWithIdempotency atomically creates a sending record keyed by
// both the per-attempt delivery ID and the stable logical idempotency ID.
func (s *Store) CreateDeliveryWithIdempotency(ctx context.Context, deliveryID, idempotencyID, requestDigest string) (DeliveryRecord, bool, error) {
	return s.createOrBeginDelivery(ctx, deliveryID, idempotencyID, requestDigest, requestDigest, false)
}

// BeginDelivery atomically claims a new or retryable delivery for sending. It
// returns shouldSend=false for an in-flight or terminal record, allowing callers
// to replay the stored outcome without a duplicate Telegram request.
func (s *Store) BeginDelivery(ctx context.Context, deliveryID, requestDigest string) (record DeliveryRecord, shouldSend bool, err error) {
	return s.BeginDeliveryWithIdempotency(ctx, deliveryID, deliveryID, requestDigest, requestDigest)
}

// BeginDeliveryWithIdempotency atomically claims a new or retryable logical
// delivery. A new delivery ID may replay an existing stable idempotency ID, but
// it is durably aliased so either ID suppresses future provider sends.
func (s *Store) BeginDeliveryWithIdempotency(
	ctx context.Context,
	deliveryID string,
	idempotencyID string,
	requestDigest string,
	legacyRequestDigest string,
) (record DeliveryRecord, shouldSend bool, err error) {
	return s.createOrBeginDelivery(ctx, deliveryID, idempotencyID, requestDigest, legacyRequestDigest, true)
}

func (s *Store) createOrBeginDelivery(
	ctx context.Context,
	deliveryID string,
	idempotencyID string,
	requestDigest string,
	legacyRequestDigest string,
	claimRetryable bool,
) (DeliveryRecord, bool, error) {
	deliveryID, err := normalizeIdentity(deliveryID)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	idempotencyID, err = normalizeIdentity(idempotencyID)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	requestDigest, err = normalizeDigest(requestDigest)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	legacyRequestDigest, err = normalizeDigest(legacyRequestDigest)
	if err != nil {
		return DeliveryRecord{}, false, err
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return DeliveryRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	record, found, err := resolveDelivery(ctx, tx, deliveryID, idempotencyID)
	switch {
	case err != nil:
		return DeliveryRecord{}, false, err
	case found:
		switch record.DigestVersion {
		case 1:
			exactAttempt := record.DeliveryID == deliveryID && record.LegacyRequestDigest == legacyRequestDigest
			stableReplay := record.DeliveryID == idempotencyID && record.LegacyRequestDigest == requestDigest
			if !exactAttempt && !stableReplay {
				return DeliveryRecord{}, false, ErrDigestConflict
			}
			if err := upgradeLegacyDelivery(ctx, tx, &record, idempotencyID, requestDigest); err != nil {
				return DeliveryRecord{}, false, err
			}
		case 2:
			if record.IdempotencyID != idempotencyID || record.RequestDigest != requestDigest {
				return DeliveryRecord{}, false, ErrDigestConflict
			}
		default:
			return DeliveryRecord{}, false, errors.New("delivery record has an unsupported digest version")
		}
		if err := ensureDeliveryAlias(ctx, tx, deliveryID, record.DeliveryID); err != nil {
			return DeliveryRecord{}, false, err
		}
		if err := ensureDeliveryAlias(ctx, tx, idempotencyID, record.DeliveryID); err != nil {
			return DeliveryRecord{}, false, err
		}
		if record.State == DeliveryStateSending && time.Since(record.UpdatedAt) >= deliverySendingLease {
			now := nowMillis()
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries
				SET state = 'unknown', provider_message_id = '', safe_message = ?, updated_at_ms = ?
				WHERE delivery_id = ? AND request_digest = ? AND state = 'sending'`,
				DeliveryOutcomeUnknownMessage, now, record.DeliveryID, record.LegacyRequestDigest,
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
				now, record.DeliveryID, record.LegacyRequestDigest,
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
	}

	now := nowMillis()
	if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(
		delivery_id, request_digest, state, provider_message_id, safe_message, created_at_ms, updated_at_ms
	) VALUES (?, ?, 'sending', '', '', ?, ?)`, deliveryID, legacyRequestDigest, now, now); err != nil {
		return DeliveryRecord{}, false, fmt.Errorf("create delivery record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_idempotency(
		delivery_id, idempotency_id, digest_version, request_digest
	) VALUES (?, ?, 2, ?)`, deliveryID, idempotencyID, requestDigest); err != nil {
		return DeliveryRecord{}, false, fmt.Errorf("create delivery idempotency record: %w", err)
	}
	if err := ensureDeliveryAlias(ctx, tx, deliveryID, deliveryID); err != nil {
		return DeliveryRecord{}, false, err
	}
	if err := ensureDeliveryAlias(ctx, tx, idempotencyID, deliveryID); err != nil {
		return DeliveryRecord{}, false, err
	}
	record = DeliveryRecord{
		DeliveryID:          deliveryID,
		IdempotencyID:       idempotencyID,
		DigestVersion:       2,
		RequestDigest:       requestDigest,
		LegacyRequestDigest: legacyRequestDigest,
		State:               DeliveryStateSending,
		CreatedAt:           millisTime(now),
		UpdatedAt:           millisTime(now),
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
	record, found, err := getDeliveryByIdentity(ctx, s.db, deliveryID)
	if err != nil {
		return DeliveryRecord{}, err
	}
	if !found {
		return DeliveryRecord{}, wrapNotFound("delivery")
	}
	return record, nil
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
		string(state), providerMessageID, safeMessage, now, deliveryID, record.LegacyRequestDigest,
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
	return scanDelivery(queryer.QueryRowContext(ctx, `SELECT
		d.delivery_id, COALESCE(i.idempotency_id, d.delivery_id), COALESCE(i.digest_version, 1),
		COALESCE(i.request_digest, d.request_digest), d.request_digest,
		d.state, d.provider_message_id, d.safe_message, d.created_at_ms, d.updated_at_ms
		FROM deliveries AS d
		LEFT JOIN delivery_idempotency AS i ON i.delivery_id = d.delivery_id
		WHERE d.delivery_id = ?`, deliveryID))
}

func getDeliveryByAlias(ctx context.Context, queryer queryRower, aliasID string) (DeliveryRecord, error) {
	return scanDelivery(queryer.QueryRowContext(ctx, `SELECT
		d.delivery_id, COALESCE(i.idempotency_id, d.delivery_id), COALESCE(i.digest_version, 1),
		COALESCE(i.request_digest, d.request_digest), d.request_digest, d.state,
		d.provider_message_id, d.safe_message, d.created_at_ms, d.updated_at_ms
		FROM delivery_aliases AS a
		JOIN deliveries AS d ON d.delivery_id = a.delivery_id
		LEFT JOIN delivery_idempotency AS i ON i.delivery_id = d.delivery_id
		WHERE a.alias_id = ?`, aliasID))
}

func resolveDelivery(ctx context.Context, queryer queryRower, deliveryID, idempotencyID string) (DeliveryRecord, bool, error) {
	byDelivery, deliveryFound, deliveryErr := getDeliveryByIdentity(ctx, queryer, deliveryID)
	byIdempotency, idempotencyFound, idempotencyErr := getDeliveryByIdentity(ctx, queryer, idempotencyID)
	if deliveryErr != nil {
		return DeliveryRecord{}, false, deliveryErr
	}
	if idempotencyErr != nil {
		return DeliveryRecord{}, false, idempotencyErr
	}
	if deliveryFound && idempotencyFound && byDelivery.DeliveryID != byIdempotency.DeliveryID {
		return DeliveryRecord{}, false, ErrDigestConflict
	}
	if deliveryFound {
		return byDelivery, true, nil
	}
	if idempotencyFound {
		return byIdempotency, true, nil
	}
	return DeliveryRecord{}, false, nil
}

func getDeliveryByIdentity(ctx context.Context, queryer queryRower, identity string) (DeliveryRecord, bool, error) {
	byAlias, aliasErr := getDeliveryByAlias(ctx, queryer, identity)
	byPrimary, primaryErr := getDelivery(ctx, queryer, identity)
	aliasFound := aliasErr == nil
	primaryFound := primaryErr == nil
	if aliasErr != nil && !errors.Is(aliasErr, ErrNotFound) {
		return DeliveryRecord{}, false, aliasErr
	}
	if primaryErr != nil && !errors.Is(primaryErr, ErrNotFound) {
		return DeliveryRecord{}, false, primaryErr
	}
	if aliasFound && primaryFound && byAlias.DeliveryID != byPrimary.DeliveryID {
		return DeliveryRecord{}, false, ErrDigestConflict
	}
	if aliasFound {
		return byAlias, true, nil
	}
	if primaryFound {
		return byPrimary, true, nil
	}
	return DeliveryRecord{}, false, nil
}

func ensureDeliveryAlias(ctx context.Context, tx *sql.Tx, aliasID, deliveryID string) error {
	var primaryDeliveryID string
	err := tx.QueryRowContext(ctx, `SELECT delivery_id FROM deliveries WHERE delivery_id = ?`, aliasID).Scan(&primaryDeliveryID)
	if err == nil && primaryDeliveryID != deliveryID {
		return ErrDigestConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check delivery alias collision: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO delivery_aliases(alias_id, delivery_id)
		VALUES (?, ?) ON CONFLICT(alias_id) DO NOTHING`, aliasID, deliveryID)
	if err != nil {
		return fmt.Errorf("create delivery alias: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read delivery alias result: %w", err)
	}
	if rows == 1 {
		return nil
	}
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT delivery_id FROM delivery_aliases WHERE alias_id = ?`, aliasID).Scan(&existing); err != nil {
		return fmt.Errorf("read delivery alias: %w", err)
	}
	if existing != deliveryID {
		return ErrDigestConflict
	}
	return nil
}

func upgradeLegacyDelivery(ctx context.Context, tx *sql.Tx, record *DeliveryRecord, idempotencyID, requestDigest string) error {
	if record == nil || record.DigestVersion != 1 {
		return fmt.Errorf("%w: legacy delivery record is invalid", ErrInvalidArgument)
	}
	if err := ensureDeliveryAlias(ctx, tx, idempotencyID, record.DeliveryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_idempotency(
		delivery_id, idempotency_id, digest_version, request_digest
	) VALUES (?, ?, 2, ?)`, record.DeliveryID, idempotencyID, requestDigest); err != nil {
		return fmt.Errorf("create legacy delivery idempotency record: %w", err)
	}
	record.IdempotencyID = idempotencyID
	record.DigestVersion = 2
	record.RequestDigest = requestDigest
	return nil
}

func scanDelivery(row *sql.Row) (DeliveryRecord, error) {
	var (
		record          DeliveryRecord
		state           string
		createdAtMillis int64
		updatedAtMillis int64
	)
	err := row.Scan(
		&record.DeliveryID,
		&record.IdempotencyID,
		&record.DigestVersion,
		&record.RequestDigest,
		&record.LegacyRequestDigest,
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
