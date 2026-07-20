package store

import (
	"context"
	"fmt"
	"time"
)

// PruneResult reports terminal records removed by one retention pass.
type PruneResult struct {
	Updates    int64
	Deliveries int64
}

// PruneTerminalBefore removes acknowledged Telegram updates and terminal
// delivery outcomes older than cutoff. Pending updates and sending/retryable
// deliveries are retained so cleanup cannot create duplicate work.
func (s *Store) PruneTerminalBefore(ctx context.Context, cutoff time.Time) (PruneResult, error) {
	if cutoff.IsZero() {
		return PruneResult{}, fmt.Errorf("%w: retention cutoff is required", ErrInvalidArgument)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return PruneResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	cutoffMillis := cutoff.UTC().UnixMilli()
	updates, err := tx.ExecContext(ctx, `DELETE FROM telegram_updates
		WHERE orka_response IS NOT NULL AND updated_at_ms < ?`, cutoffMillis)
	if err != nil {
		return PruneResult{}, fmt.Errorf("prune Telegram updates: %w", err)
	}
	deliveries, err := tx.ExecContext(ctx, `DELETE FROM deliveries
		WHERE state IN ('delivered', 'permanent', 'unknown') AND updated_at_ms < ?`, cutoffMillis)
	if err != nil {
		return PruneResult{}, fmt.Errorf("prune delivery outcomes: %w", err)
	}
	result := PruneResult{}
	if result.Updates, err = updates.RowsAffected(); err != nil {
		return PruneResult{}, fmt.Errorf("read pruned Telegram update count: %w", err)
	}
	if result.Deliveries, err = deliveries.RowsAffected(); err != nil {
		return PruneResult{}, fmt.Errorf("read pruned delivery count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PruneResult{}, fmt.Errorf("commit retention pruning: %w", err)
	}
	return result, nil
}
