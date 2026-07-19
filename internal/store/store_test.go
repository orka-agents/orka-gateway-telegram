package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAppliesMigrationsAndPragmas(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "nested", "adapter.db"))

	var journalMode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != busyTimeoutMilliseconds {
		t.Fatalf("busy_timeout = %d, want %d", busyTimeout, busyTimeoutMilliseconds)
	}

	var synchronous int
	if err := store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 {
		t.Fatalf("synchronous = %d, want FULL (2)", synchronous)
	}

	var foreignKeys int
	if err := store.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}

	var migrationVersion int
	if err := store.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&migrationVersion); err != nil {
		t.Fatal(err)
	}
	if migrationVersion != len(migrations) {
		t.Fatalf("migration version = %d, want %d", migrationVersion, len(migrations))
	}

	for _, table := range []string{"telegram_updates", "deliveries"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("table %q count = %d, want 1", table, count)
		}
	}
}

func TestUpdateAdmissionPersistsAndRejectsDigestConflict(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "adapter.db")
	digest := DigestBytes([]byte("normalized Telegram update"))
	conflictingDigest := DigestBytes([]byte("different Telegram update"))

	first := openTestStore(t, path)
	record, created, err := first.CreateUpdate(ctx, 42, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !created || record.UpdateID != 42 || record.Digest != digest || record.HasOrkaResponse() {
		t.Fatalf("created update = %#v, created = %v", record, created)
	}

	response := json.RawMessage(`{ "status": "accepted", "eventId": "event-42", "state": "queued" }`)
	record, err = first.SaveUpdateResponse(ctx, 42, digest, response)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(record.OrkaResponse), `{"status":"accepted","eventId":"event-42","state":"queued"}`; got != want {
		t.Fatalf("OrkaResponse = %s, want %s", got, want)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openTestStore(t, path)
	replayed, created, err := second.CreateUpdate(ctx, 42, digest)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("reopened duplicate update was created again")
	}
	if !replayed.HasOrkaResponse() || string(replayed.OrkaResponse) != string(record.OrkaResponse) {
		t.Fatalf("replayed update = %#v, want saved response", replayed)
	}

	if _, _, err := second.CreateUpdate(ctx, 42, conflictingDigest); !errors.Is(err, ErrDigestConflict) {
		t.Fatalf("CreateUpdate conflict error = %v, want ErrDigestConflict", err)
	}
	if _, err := second.SaveUpdateResponse(ctx, 42, conflictingDigest, response); !errors.Is(err, ErrDigestConflict) {
		t.Fatalf("SaveUpdateResponse conflict error = %v, want ErrDigestConflict", err)
	}
	if _, err := second.SaveUpdateResponse(ctx, 42, digest, json.RawMessage(`{"status":"rejected"}`)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("SaveUpdateResponse overwrite error = %v, want ErrInvalidTransition", err)
	}
}

func TestRestartConvertsSendingDeliveryToUnknown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "adapter.db")
	digest := DigestBytes([]byte("delivery request"))

	first := openTestStore(t, path)
	record, shouldSend, err := first.BeginDelivery(ctx, "delivery-1", digest)
	if err != nil {
		t.Fatal(err)
	}
	if !shouldSend || record.State != DeliveryStateSending {
		t.Fatalf("BeginDelivery = %#v, shouldSend = %v", record, shouldSend)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openTestStore(t, path)
	record, err = second.GetDelivery(ctx, "delivery-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != DeliveryStateUnknown || !record.Terminal() {
		t.Fatalf("recovered state = %q, want terminal unknown", record.State)
	}
	if record.SafeMessage != DeliveryOutcomeUnknownMessage {
		t.Fatalf("safe message = %q, want %q", record.SafeMessage, DeliveryOutcomeUnknownMessage)
	}

	replayed, shouldSend, err := second.BeginDelivery(ctx, "delivery-1", digest)
	if err != nil {
		t.Fatal(err)
	}
	if shouldSend || replayed.State != DeliveryStateUnknown {
		t.Fatalf("replayed delivery = %#v, shouldSend = %v", replayed, shouldSend)
	}
}

func TestDeliveryTerminalReplayAndDigestConflict(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "adapter.db"))
	digest := DigestBytes([]byte("delivery request"))
	conflictingDigest := DigestBytes([]byte("different delivery request"))

	_, shouldSend, err := store.BeginDelivery(ctx, "delivery-1", digest)
	if err != nil || !shouldSend {
		t.Fatalf("BeginDelivery shouldSend = %v, error = %v", shouldSend, err)
	}
	delivered, err := store.MarkDeliveryDelivered(ctx, "delivery-1", digest, "telegram-message-99")
	if err != nil {
		t.Fatal(err)
	}
	if delivered.State != DeliveryStateDelivered || delivered.ProviderMessageID != "telegram-message-99" || !delivered.Terminal() {
		t.Fatalf("delivered record = %#v", delivered)
	}

	replayed, shouldSend, err := store.BeginDelivery(ctx, "delivery-1", digest)
	if err != nil {
		t.Fatal(err)
	}
	if shouldSend || replayed != delivered {
		t.Fatalf("terminal replay = %#v, shouldSend = %v, want %#v", replayed, shouldSend, delivered)
	}
	if _, err := store.MarkDeliveryDelivered(ctx, "delivery-1", digest, "telegram-message-99"); err != nil {
		t.Fatalf("idempotent terminal update error = %v", err)
	}
	if _, err := store.MarkDeliveryPermanent(ctx, "delivery-1", digest, "do not retry"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal overwrite error = %v, want ErrInvalidTransition", err)
	}
	if _, _, err := store.BeginDelivery(ctx, "delivery-1", conflictingDigest); !errors.Is(err, ErrDigestConflict) {
		t.Fatalf("delivery digest conflict error = %v, want ErrDigestConflict", err)
	}
}

func TestRetryableDeliveryCanBeClaimedOnce(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "adapter.db"))
	digest := DigestBytes([]byte("delivery request"))

	if _, shouldSend, err := store.BeginDelivery(ctx, "delivery-1", digest); err != nil || !shouldSend {
		t.Fatalf("first BeginDelivery shouldSend = %v, error = %v", shouldSend, err)
	}
	retryable, err := store.MarkDeliveryRetryable(ctx, "delivery-1", digest, "  temporary\x00 failure  ")
	if err != nil {
		t.Fatal(err)
	}
	if retryable.State != DeliveryStateRetryable || retryable.SafeMessage != "temporary failure" {
		t.Fatalf("retryable record = %#v", retryable)
	}

	retried, shouldSend, err := store.BeginDelivery(ctx, "delivery-1", digest)
	if err != nil {
		t.Fatal(err)
	}
	if !shouldSend || retried.State != DeliveryStateSending || retried.SafeMessage != "" {
		t.Fatalf("retry claim = %#v, shouldSend = %v", retried, shouldSend)
	}
	permanent, err := store.MarkDeliveryPermanent(ctx, "delivery-1", digest, "provider rejected message")
	if err != nil {
		t.Fatal(err)
	}
	if permanent.State != DeliveryStatePermanent || !permanent.Terminal() {
		t.Fatalf("permanent record = %#v", permanent)
	}
	if _, shouldSend, err := store.BeginDelivery(ctx, "delivery-1", digest); err != nil || shouldSend {
		t.Fatalf("terminal BeginDelivery shouldSend = %v, error = %v", shouldSend, err)
	}
}

func TestConcurrentDeliveryAdmissionHasSingleSender(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "adapter.db"))
	digest := DigestBytes([]byte("delivery request"))

	const goroutines = 32
	var senders atomic.Int32
	errorsCh := make(chan error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start
			record, shouldSend, err := store.BeginDelivery(ctx, "delivery-1", digest)
			if err != nil {
				errorsCh <- err
				return
			}
			if record.State != DeliveryStateSending {
				errorsCh <- errors.New("unexpected delivery state")
				return
			}
			if shouldSend {
				senders.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if got := senders.Load(); got != 1 {
		t.Fatalf("senders = %d, want 1", got)
	}
}

func TestConcurrentUpdateAdmissionHasSingleCreator(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "adapter.db"))
	digest := DigestBytes([]byte("normalized Telegram update"))

	const goroutines = 32
	var creators atomic.Int32
	errorsCh := make(chan error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start
			_, created, err := store.CreateUpdate(ctx, 42, digest)
			if err != nil {
				errorsCh <- err
				return
			}
			if created {
				creators.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if got := creators.Load(); got != 1 {
		t.Fatalf("creators = %d, want 1", got)
	}
}

func TestGetMissingRecords(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "adapter.db"))

	if _, err := store.GetUpdate(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUpdate error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetDelivery(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDelivery error = %v, want ErrNotFound", err)
	}
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

func TestPingAndStaleSendingRecovery(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "adapter.db"))
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	digest := DigestBytes([]byte("request"))
	record, shouldSend, err := s.BeginDelivery(ctx, "delivery-stale", digest)
	if err != nil || !shouldSend {
		t.Fatalf("BeginDelivery() = (%+v, %v, %v)", record, shouldSend, err)
	}
	staleAt := time.Now().Add(-deliverySendingLease - time.Second).UnixMilli()
	if _, err := s.db.ExecContext(ctx, `UPDATE deliveries SET updated_at_ms = ? WHERE delivery_id = ?`, staleAt, record.DeliveryID); err != nil {
		t.Fatal(err)
	}
	recovered, shouldSend, err := s.BeginDelivery(ctx, record.DeliveryID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if shouldSend || recovered.State != DeliveryStateUnknown {
		t.Fatalf("stale delivery = (%+v, %v), want terminal unknown", recovered, shouldSend)
	}
}
