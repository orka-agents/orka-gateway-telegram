package store

import (
	"context"
	"database/sql"
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

	for _, table := range []string{"telegram_updates", "deliveries", "delivery_idempotency", "delivery_aliases"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("table %q count = %d, want 1", table, count)
		}
	}
	var aliasIndexCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'index' AND name = 'delivery_aliases_delivery_id_idx'`).Scan(&aliasIndexCount); err != nil {
		t.Fatal(err)
	}
	if aliasIndexCount != 1 {
		t.Fatalf("delivery alias foreign-key index count = %d, want 1", aliasIndexCount)
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

func TestDeliveryIdempotencyAliasesReplaySingleRecord(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "adapter.db"))
	digest := DigestBytes([]byte("logical delivery"))

	first, shouldSend, err := store.BeginDeliveryWithIdempotency(ctx, "delivery-first", "stable-idempotency", digest, digest)
	if err != nil || !shouldSend {
		t.Fatalf("first BeginDeliveryWithIdempotency = (%+v, %v, %v)", first, shouldSend, err)
	}
	if first.DeliveryID != "delivery-first" || first.IdempotencyID != "stable-idempotency" {
		t.Fatalf("first record = %+v", first)
	}
	delivered, err := store.MarkDeliveryDelivered(ctx, first.DeliveryID, digest, "telegram:100:7")
	if err != nil {
		t.Fatal(err)
	}

	replayed, shouldSend, err := store.BeginDeliveryWithIdempotency(ctx, "delivery-retry", "stable-idempotency", digest, digest)
	if err != nil {
		t.Fatal(err)
	}
	if shouldSend || replayed.DeliveryID != first.DeliveryID || replayed.ProviderMessageID != delivered.ProviderMessageID {
		t.Fatalf("replayed record = (%+v, %v)", replayed, shouldSend)
	}
	byAlias, err := store.GetDelivery(ctx, "delivery-retry")
	if err != nil || byAlias.DeliveryID != first.DeliveryID {
		t.Fatalf("GetDelivery(alias) = (%+v, %v)", byAlias, err)
	}

	if _, _, err := store.BeginDeliveryWithIdempotency(ctx, "delivery-conflict", "stable-idempotency", DigestBytes([]byte("different")), DigestBytes([]byte("different"))); !errors.Is(err, ErrDigestConflict) {
		t.Fatalf("conflicting logical delivery error = %v, want ErrDigestConflict", err)
	}
	if _, _, err := store.BeginDeliveryWithIdempotency(ctx, "delivery-retry", "different-idempotency", digest, digest); !errors.Is(err, ErrDigestConflict) {
		t.Fatalf("reused delivery alias error = %v, want ErrDigestConflict", err)
	}
}

func TestOpenMigratesDeliveryIdempotencyAliases(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "adapter.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at_ms INTEGER NOT NULL
	) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range migrations[0].statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES (1, ?)`, nowMillis()); err != nil {
		t.Fatal(err)
	}
	legacyDigest := DigestBytes([]byte("legacy request with delivery A and idempotency B"))
	logicalDigest := DigestBytes([]byte("logical request with stable idempotency B"))
	if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(
		delivery_id, request_digest, state, provider_message_id, safe_message, created_at_ms, updated_at_ms
	) VALUES ('legacy-delivery', ?, 'delivered', 'telegram:100:9', '', ?, ?)`, legacyDigest, nowMillis(), nowMillis()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated := openTestStore(t, path)
	record, shouldSend, err := migrated.BeginDeliveryWithIdempotency(
		ctx, "legacy-delivery", "legacy-stable-idempotency", logicalDigest, legacyDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if shouldSend || record.IdempotencyID != "legacy-stable-idempotency" || record.DigestVersion != 2 ||
		record.RequestDigest != logicalDigest || record.LegacyRequestDigest != legacyDigest ||
		record.ProviderMessageID != "telegram:100:9" {
		t.Fatalf("migrated record = %+v", record)
	}
	byStableID, err := migrated.GetDelivery(ctx, "legacy-stable-idempotency")
	if err != nil || byStableID.DeliveryID != "legacy-delivery" {
		t.Fatalf("stable legacy alias = (%+v, %v)", byStableID, err)
	}
	var aliases int
	if err := migrated.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_aliases WHERE delivery_id = 'legacy-delivery'`).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if aliases != 2 {
		t.Fatalf("legacy alias count = %d, want 2", aliases)
	}
	var baseDigest, metadataDigest string
	if err := migrated.db.QueryRowContext(ctx, `SELECT d.request_digest, i.request_digest
		FROM deliveries AS d JOIN delivery_idempotency AS i ON i.delivery_id = d.delivery_id
		WHERE d.delivery_id = 'legacy-delivery'`).Scan(&baseDigest, &metadataDigest); err != nil {
		t.Fatal(err)
	}
	if baseDigest != legacyDigest || metadataDigest != logicalDigest {
		t.Fatalf("persisted digests = base:%s metadata:%s", baseDigest, metadataDigest)
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

func TestPruneTerminalBeforeRetainsActiveRecords(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "adapter.db"))
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	oldMillis := cutoff.Add(-time.Hour).UnixMilli()
	recentMillis := cutoff.Add(time.Hour).UnixMilli()

	oldUpdateDigest := DigestBytes([]byte("old update"))
	if _, _, err := store.CreateUpdate(ctx, 1001, oldUpdateDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveUpdateResponse(ctx, 1001, oldUpdateDigest, json.RawMessage(`{"status":"accepted"}`)); err != nil {
		t.Fatal(err)
	}
	recentUpdateDigest := DigestBytes([]byte("recent update"))
	if _, _, err := store.CreateUpdate(ctx, 1002, recentUpdateDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveUpdateResponse(ctx, 1002, recentUpdateDigest, json.RawMessage(`{"status":"accepted"}`)); err != nil {
		t.Fatal(err)
	}
	pendingUpdateDigest := DigestBytes([]byte("pending update"))
	if _, _, err := store.CreateUpdate(ctx, 1003, pendingUpdateDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE telegram_updates SET updated_at_ms = ? WHERE update_id IN (1001, 1003)`, oldMillis); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE telegram_updates SET updated_at_ms = ? WHERE update_id = 1002`, recentMillis); err != nil {
		t.Fatal(err)
	}

	states := []struct {
		id    string
		state DeliveryState
	}{
		{id: "old-delivered", state: DeliveryStateDelivered},
		{id: "old-permanent", state: DeliveryStatePermanent},
		{id: "old-unknown", state: DeliveryStateUnknown},
		{id: "old-retryable", state: DeliveryStateRetryable},
		{id: "old-sending", state: DeliveryStateSending},
		{id: "recent-delivered", state: DeliveryStateDelivered},
	}
	for _, item := range states {
		digest := DigestBytes([]byte(item.id))
		record, shouldSend, err := store.BeginDelivery(ctx, item.id, digest)
		if err != nil || !shouldSend {
			t.Fatalf("BeginDelivery(%q) = (%+v, %v, %v)", item.id, record, shouldSend, err)
		}
		switch item.state {
		case DeliveryStateDelivered:
			_, err = store.MarkDeliveryDelivered(ctx, record.DeliveryID, digest, "telegram:100:1")
		case DeliveryStatePermanent:
			_, err = store.MarkDeliveryPermanent(ctx, record.DeliveryID, digest, "permanent")
		case DeliveryStateUnknown:
			_, err = store.MarkDeliveryUnknown(ctx, record.DeliveryID, digest, "unknown")
		case DeliveryStateRetryable:
			_, err = store.MarkDeliveryRetryable(ctx, record.DeliveryID, digest, "retry")
		case DeliveryStateSending:
		}
		if err != nil {
			t.Fatal(err)
		}
		updatedAt := oldMillis
		if item.id == "recent-delivered" {
			updatedAt = recentMillis
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE deliveries SET updated_at_ms = ? WHERE delivery_id = ?`, updatedAt, item.id); err != nil {
			t.Fatal(err)
		}
	}

	pruned, err := store.PruneTerminalBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Updates != 1 || pruned.Deliveries != 3 {
		t.Fatalf("pruned = %+v, want one update and three deliveries", pruned)
	}
	if _, err := store.GetUpdate(ctx, 1001); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old acknowledged update error = %v, want ErrNotFound", err)
	}
	for _, updateID := range []int64{1002, 1003} {
		if _, err := store.GetUpdate(ctx, updateID); err != nil {
			t.Fatalf("retained update %d: %v", updateID, err)
		}
	}
	for _, deliveryID := range []string{"old-delivered", "old-permanent", "old-unknown"} {
		if _, err := store.GetDelivery(ctx, deliveryID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old terminal delivery %q error = %v, want ErrNotFound", deliveryID, err)
		}
	}
	for _, deliveryID := range []string{"old-retryable", "old-sending", "recent-delivered"} {
		if _, err := store.GetDelivery(ctx, deliveryID); err != nil {
			t.Fatalf("retained delivery %q: %v", deliveryID, err)
		}
	}
	var danglingAliases int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_aliases AS a
		LEFT JOIN deliveries AS d ON d.delivery_id = a.delivery_id WHERE d.delivery_id IS NULL`).Scan(&danglingAliases); err != nil {
		t.Fatal(err)
	}
	if danglingAliases != 0 {
		t.Fatalf("dangling delivery aliases = %d", danglingAliases)
	}
}

func TestMigratedStableIDAcceptsNewDeliveryAttempt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "adapter.db")
	logicalDigest := DigestBytes([]byte("v1 request with equal delivery and idempotency IDs"))
	seedV1Deliveries(t, path, map[string]struct {
		digest     string
		providerID string
	}{
		"stable-v1-id": {digest: logicalDigest, providerID: "telegram:100:12"},
	})
	store := openTestStore(t, path)
	newAttemptLegacyDigest := DigestBytes([]byte("v2 retry with a new delivery attempt ID"))
	record, shouldSend, err := store.BeginDeliveryWithIdempotency(
		ctx, "new-attempt-id", "stable-v1-id", logicalDigest, newAttemptLegacyDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if shouldSend || record.DeliveryID != "stable-v1-id" || record.IdempotencyID != "stable-v1-id" ||
		record.ProviderMessageID != "telegram:100:12" {
		t.Fatalf("stable migrated replay = (%+v, %v)", record, shouldSend)
	}
	byAttempt, err := store.GetDelivery(ctx, "new-attempt-id")
	if err != nil || byAttempt.DeliveryID != "stable-v1-id" {
		t.Fatalf("new attempt alias = (%+v, %v)", byAttempt, err)
	}
}

func TestMigratedLegacyIdentitiesRejectCrossDeliveryCollision(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "adapter.db")
	seedV1Deliveries(t, path, map[string]struct {
		digest     string
		providerID string
	}{
		"legacy-a": {digest: DigestBytes([]byte("legacy-a")), providerID: "telegram:100:11"},
		"legacy-b": {digest: DigestBytes([]byte("legacy-b")), providerID: "telegram:100:12"},
	})
	store := openTestStore(t, path)
	if _, _, err := store.BeginDeliveryWithIdempotency(
		ctx, "legacy-a", "legacy-b", DigestBytes([]byte("logical")), DigestBytes([]byte("legacy-a")),
	); !errors.Is(err, ErrDigestConflict) {
		t.Fatalf("cross-delivery legacy identity error = %v, want ErrDigestConflict", err)
	}
}

func seedV1Deliveries(t *testing.T, path string, records map[string]struct {
	digest     string
	providerID string
}) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at_ms INTEGER NOT NULL
	) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range migrations[0].statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES (1, ?)`, nowMillis()); err != nil {
		t.Fatal(err)
	}
	for deliveryID, record := range records {
		if _, err := db.ExecContext(ctx, `INSERT INTO deliveries(
			delivery_id, request_digest, state, provider_message_id, safe_message, created_at_ms, updated_at_ms
		) VALUES (?, ?, 'delivered', ?, '', ?, ?)`, deliveryID, record.digest, record.providerID, nowMillis(), nowMillis()); err != nil {
			t.Fatal(err)
		}
	}
}
