// Package store provides durable idempotency records for Telegram updates and
// outbound Orka deliveries.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	busyTimeoutMilliseconds = 5_000
	maxDigestBytes          = 64
	maxIdentityBytes        = 256
	maxOrkaResponseBytes    = 64 << 10
	maxSafeMessageBytes     = 1_024

	// DeliveryOutcomeUnknownMessage is persisted when a process restart makes
	// it impossible to know whether Telegram accepted an in-flight send.
	DeliveryOutcomeUnknownMessage = "delivery outcome is unknown after adapter restart"
)

var (
	// ErrNotFound means the requested update or delivery does not exist.
	ErrNotFound = errors.New("store record not found")
	// ErrDigestConflict means a stable external ID was reused for different content.
	ErrDigestConflict = errors.New("store digest conflict")
	// ErrInvalidArgument means a record key, digest, response, or state is invalid.
	ErrInvalidArgument = errors.New("invalid store argument")
	// ErrInvalidTransition means an immutable result or delivery state would be overwritten.
	ErrInvalidTransition = errors.New("invalid store state transition")
)

// Store is safe for concurrent use by goroutines. A database file must be
// owned by one running adapter process; opening the same file represents a
// restart and converts any lingering sending deliveries to unknown.
type Store struct {
	db *sql.DB

	closeOnce sync.Once
	closeErr  error
}

// Open opens or creates a durable SQLite database at path.
func Open(path string) (*Store, error) {
	return OpenContext(context.Background(), path)
}

// OpenContext opens or creates a durable SQLite database at path, applies all
// migrations, and recovers interrupted outbound sends conservatively.
func OpenContext(ctx context.Context, path string) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidArgument)
	}

	path = strings.TrimSpace(path)
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("%w: a filesystem database path is required", ErrInvalidArgument)
	}
	path = filepath.Clean(path)
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	path = absolutePath
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite store: %w", err)
	}
	// WAL permits readers during writes. SQLite still serializes writers, while
	// the busy timeout below lets short competing transactions wait safely.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	store := &Store{db: db}
	ok := false
	defer func() {
		if !ok {
			_ = db.Close()
		}
	}()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite store: %w", err)
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		return nil, fmt.Errorf("enable sqlite WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return nil, errors.New("enable sqlite WAL: database did not enter WAL mode")
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	if err := recoverInterruptedDeliveries(ctx, db); err != nil {
		return nil, err
	}

	ok = true
	return store, nil
}

// Close releases the database. It is safe to call more than once.

// Ping verifies that the SQLite connection can serve reads.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.ready(); err != nil {
		return err
	}
	var value int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&value); err != nil {
		return fmt.Errorf("ping store: %w", err)
	}
	if value != 1 {
		return errors.New("store readiness query returned an invalid result")
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

func sqliteDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("mode", "rwc")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMilliseconds))
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Set("_txlock", "immediate")
	q.Set("_dqs", "false")
	u.RawQuery = q.Encode()
	return u.String()
}

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE telegram_updates (
				update_id INTEGER PRIMARY KEY,
				digest TEXT NOT NULL CHECK(length(digest) = 64),
				orka_response BLOB,
				created_at_ms INTEGER NOT NULL,
				updated_at_ms INTEGER NOT NULL
			) STRICT`,
			`CREATE TABLE deliveries (
				delivery_id TEXT PRIMARY KEY,
				request_digest TEXT NOT NULL CHECK(length(request_digest) = 64),
				state TEXT NOT NULL CHECK(state IN ('sending', 'retryable', 'delivered', 'permanent', 'unknown')),
				provider_message_id TEXT NOT NULL DEFAULT '',
				safe_message TEXT NOT NULL DEFAULT '',
				created_at_ms INTEGER NOT NULL,
				updated_at_ms INTEGER NOT NULL
			) STRICT`,
		},
	},
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin store migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at_ms INTEGER NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	var current int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read store migration version: %w", err)
	}
	if current > len(migrations) {
		return errors.New("store schema is newer than this adapter")
	}

	for _, migration := range migrations {
		if migration.version <= current {
			continue
		}
		if migration.version != current+1 {
			return errors.New("store migration sequence is invalid")
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply store migration %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at_ms) VALUES (?, ?)`,
			migration.version, nowMillis(),
		); err != nil {
			return fmt.Errorf("record store migration %d: %w", migration.version, err)
		}
		current = migration.version
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit store migrations: %w", err)
	}
	return nil
}

func recoverInterruptedDeliveries(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `UPDATE deliveries
		SET state = 'unknown',
			provider_message_id = '',
			safe_message = ?,
			updated_at_ms = ?
		WHERE state = 'sending'`, DeliveryOutcomeUnknownMessage, nowMillis()); err != nil {
		return fmt.Errorf("recover interrupted deliveries: %w", err)
	}
	return nil
}

func normalizeDigest(digest string) (string, error) {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if len(digest) != maxDigestBytes {
		return "", fmt.Errorf("%w: digest must be a SHA-256 hex value", ErrInvalidArgument)
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("%w: digest must be a SHA-256 hex value", ErrInvalidArgument)
	}
	return digest, nil
}

// DigestBytes returns the normalized SHA-256 digest used by store records.
func DigestBytes(data []byte) string {
	// Kept here instead of accepting payloads in record methods so the store
	// never needs to retain or include raw request bodies in errors.
	return sha256Hex(data)
}

func normalizeIdentity(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxIdentityBytes || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
		return "", fmt.Errorf("%w: identity is invalid", ErrInvalidArgument)
	}
	return value, nil
}

func normalizeProviderMessageID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maxIdentityBytes || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
		return "", fmt.Errorf("%w: provider message ID is invalid", ErrInvalidArgument)
	}
	return value, nil
}

func normalizeOrkaResponse(response json.RawMessage) (json.RawMessage, error) {
	if len(response) == 0 || len(response) > maxOrkaResponseBytes || !json.Valid(response) {
		return nil, fmt.Errorf("%w: Orka response is invalid", ErrInvalidArgument)
	}
	var compact bytes.Buffer
	compact.Grow(len(response))
	if err := json.Compact(&compact, response); err != nil {
		return nil, fmt.Errorf("%w: Orka response is invalid", ErrInvalidArgument)
	}
	return append(json.RawMessage(nil), compact.Bytes()...), nil
}

func normalizeSafeMessage(message string) string {
	message = strings.TrimSpace(message)
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, message)
	if len(message) <= maxSafeMessageBytes {
		return message
	}
	message = message[:maxSafeMessageBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Store) ready() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: store is not open", ErrInvalidArgument)
	}
	return nil
}

func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidArgument)
	}
	if err := s.ready(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin store transaction: %w", err)
	}
	return tx, nil
}

func nowMillis() int64 {
	return time.Now().UTC().UnixMilli()
}

func millisTime(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}

func wrapNotFound(kind string) error {
	return fmt.Errorf("%w: %s", ErrNotFound, kind)
}
