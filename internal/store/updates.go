package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// UpdateRecord is the durable admission record for one Telegram update. A nil
// OrkaResponse means the update was admitted but no response has been saved.
type UpdateRecord struct {
	UpdateID     int64
	Digest       string
	OrkaResponse json.RawMessage
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// HasOrkaResponse reports whether the update has a terminal response available
// for replay.
func (r UpdateRecord) HasOrkaResponse() bool {
	return len(r.OrkaResponse) != 0
}

// CreateUpdate atomically admits a Telegram update. The returned boolean is
// true only for the caller that inserted the record. Reusing an update ID with
// a different digest returns ErrDigestConflict.
func (s *Store) CreateUpdate(ctx context.Context, updateID int64, digest string) (UpdateRecord, bool, error) {
	if updateID < 0 {
		return UpdateRecord{}, false, fmt.Errorf("%w: Telegram update ID is invalid", ErrInvalidArgument)
	}
	digest, err := normalizeDigest(digest)
	if err != nil {
		return UpdateRecord{}, false, err
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return UpdateRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	record, err := getUpdate(ctx, tx, updateID)
	switch {
	case err == nil:
		if record.Digest != digest {
			return UpdateRecord{}, false, ErrDigestConflict
		}
		if err := tx.Commit(); err != nil {
			return UpdateRecord{}, false, fmt.Errorf("commit existing update admission: %w", err)
		}
		return record, false, nil
	case !errors.Is(err, ErrNotFound):
		return UpdateRecord{}, false, err
	}

	now := nowMillis()
	if _, err := tx.ExecContext(ctx, `INSERT INTO telegram_updates(
		update_id, digest, orka_response, created_at_ms, updated_at_ms
	) VALUES (?, ?, NULL, ?, ?)`, updateID, digest, now, now); err != nil {
		return UpdateRecord{}, false, fmt.Errorf("create Telegram update record: %w", err)
	}
	record = UpdateRecord{
		UpdateID:  updateID,
		Digest:    digest,
		CreatedAt: millisTime(now),
		UpdatedAt: millisTime(now),
	}
	if err := tx.Commit(); err != nil {
		return UpdateRecord{}, false, fmt.Errorf("commit Telegram update admission: %w", err)
	}
	return record, true, nil
}

// GetUpdate returns one Telegram update admission record.
func (s *Store) GetUpdate(ctx context.Context, updateID int64) (UpdateRecord, error) {
	if updateID < 0 {
		return UpdateRecord{}, fmt.Errorf("%w: Telegram update ID is invalid", ErrInvalidArgument)
	}
	if err := s.ready(); err != nil {
		return UpdateRecord{}, err
	}
	return getUpdate(ctx, s.db, updateID)
}

// SaveUpdateResponse atomically stores the terminal Orka response for an
// admitted update. The response is immutable: writing the same normalized JSON
// is idempotent, while writing a different response returns ErrInvalidTransition.
func (s *Store) SaveUpdateResponse(ctx context.Context, updateID int64, digest string, response json.RawMessage) (UpdateRecord, error) {
	if updateID < 0 {
		return UpdateRecord{}, fmt.Errorf("%w: Telegram update ID is invalid", ErrInvalidArgument)
	}
	digest, err := normalizeDigest(digest)
	if err != nil {
		return UpdateRecord{}, err
	}
	response, err = normalizeOrkaResponse(response)
	if err != nil {
		return UpdateRecord{}, err
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return UpdateRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()

	record, err := getUpdate(ctx, tx, updateID)
	if err != nil {
		return UpdateRecord{}, err
	}
	if record.Digest != digest {
		return UpdateRecord{}, ErrDigestConflict
	}
	if record.HasOrkaResponse() {
		if !bytes.Equal(record.OrkaResponse, response) {
			return UpdateRecord{}, ErrInvalidTransition
		}
		if err := tx.Commit(); err != nil {
			return UpdateRecord{}, fmt.Errorf("commit existing Orka response: %w", err)
		}
		return record, nil
	}

	now := nowMillis()
	if _, err := tx.ExecContext(ctx, `UPDATE telegram_updates
		SET orka_response = ?, updated_at_ms = ?
		WHERE update_id = ? AND digest = ? AND orka_response IS NULL`,
		[]byte(response), now, updateID, digest,
	); err != nil {
		return UpdateRecord{}, fmt.Errorf("save Orka response: %w", err)
	}
	record.OrkaResponse = append(json.RawMessage(nil), response...)
	record.UpdatedAt = millisTime(now)
	if err := tx.Commit(); err != nil {
		return UpdateRecord{}, fmt.Errorf("commit Orka response: %w", err)
	}
	return record, nil
}

// UpdateResponse is an alias for SaveUpdateResponse.
func (s *Store) UpdateResponse(ctx context.Context, updateID int64, digest string, response json.RawMessage) (UpdateRecord, error) {
	return s.SaveUpdateResponse(ctx, updateID, digest, response)
}

type rowScanner interface {
	Scan(dest ...any) error
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getUpdate(ctx context.Context, queryer queryRower, updateID int64) (UpdateRecord, error) {
	var (
		record          UpdateRecord
		response        []byte
		createdAtMillis int64
		updatedAtMillis int64
	)
	err := scanUpdate(queryer.QueryRowContext(ctx, `SELECT
		update_id, digest, orka_response, created_at_ms, updated_at_ms
		FROM telegram_updates WHERE update_id = ?`, updateID),
		&record, &response, &createdAtMillis, &updatedAtMillis,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateRecord{}, wrapNotFound("Telegram update")
	}
	if err != nil {
		return UpdateRecord{}, fmt.Errorf("get Telegram update record: %w", err)
	}
	if response != nil {
		record.OrkaResponse = append(json.RawMessage(nil), response...)
	}
	record.CreatedAt = millisTime(createdAtMillis)
	record.UpdatedAt = millisTime(updatedAtMillis)
	return record, nil
}

func scanUpdate(scanner rowScanner, record *UpdateRecord, response *[]byte, createdAtMillis, updatedAtMillis *int64) error {
	return scanner.Scan(
		&record.UpdateID,
		&record.Digest,
		response,
		createdAtMillis,
		updatedAtMillis,
	)
}
