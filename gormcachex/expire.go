package gormcachex

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DeleteExpired deletes the rows whose entries are rotten (their ExpiresAt
// passed by the wall clock), ChunkSize at a time, and returns how many it
// deleted: the table never shrinks on its own. Run it from time to time, on
// one instance at a time (a job queue's singleton job, say); several at once
// are safe, only wasteful. A row whose lease is still held is kept: a fetch of
// its key is running and will backfill it.
//
// Each batch is two statements, each committed on its own: a read of up to
// ChunkSize rotten keys, then their deletion, which checks again that each
// row is still rotten, so a row written in between is kept. The deletion
// locks rows in the order writes do (see inLockOrder).
func (b *Backend[T]) DeleteExpired(ctx context.Context) (int64, error) {
	return b.bytes.deleteExpired(ctx)
}

func (b *byteBackend) deleteExpired(ctx context.Context) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rotten := b.rotten(time.Now().UTC())
		var keys []string
		if err := b.db.WithContext(ctx).Table(b.table).Select("?", keyColumn).
			Where(rotten).Limit(b.chunkSize).Find(&keys).Error; err != nil {
			return total, fmt.Errorf("gormcachex: find rotten rows: %w", err)
		}
		if len(keys) == 0 {
			return total, nil
		}
		var deleted int64
		if err := b.write(ctx, func(db *gorm.DB) error {
			res := b.inLockOrder(db, clause.And(clause.IN{Column: keyColumn, Values: anys(keys)}, rotten)).Delete(nil)
			deleted = res.RowsAffected
			return res.Error
		}); err != nil {
			return total, fmt.Errorf("gormcachex: delete rotten rows: %w", err)
		}
		total += deleted
		if len(keys) < b.chunkSize {
			return total, nil
		}
	}
}

// rotten matches the rows rotten at now that no fetch holds a lease of.
func (b *byteBackend) rotten(now time.Time) clause.Expression {
	expired := clause.Lt{Column: clause.Column{Name: "expires_at"}, Value: now}
	return clause.And(expired, clause.Or(clause.Eq{Column: leaseUntil, Value: nil}, clause.Lt{Column: leaseUntil, Value: now}))
}
