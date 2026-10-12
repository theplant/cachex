package gormcachex

import (
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/theplant/cachex/v2"
)

// maxLeaseChunk keeps a lease statement under SQLite's 32766 bound
// parameters: up to 6 a key.
const maxLeaseChunk = 4000

var (
	leaseToken = clause.Column{Name: "lease_token"}
	leaseUntil = clause.Column{Name: "lease_until"}
)

// Leaser returns the backend's leaser, nil unless Config.Leases is set.
func (b *Backend[T]) Leaser() cachex.Leaser[T] {
	if l, ok := b.Backend.(cachex.Leasing[T]); ok {
		return l.Leaser()
	}
	return nil
}

var _ cachex.Leasing[any] = (*Backend[any])(nil)

func (b *byteBackend) Leaser() cachex.ByteLeaser {
	if !b.leases {
		return nil
	}
	return leaser{b}
}

type leaser struct{ b *byteBackend }

var (
	_ cachex.ByteLeasing = (*byteBackend)(nil)
	_ cachex.ByteLeaser  = leaser{}
)

func newToken() string {
	var buf [16]byte
	for i := range 2 {
		u := rand.Uint64()
		for j := range 8 {
			buf[8*i+j] = byte(u >> (8 * j))
		}
	}
	return hex.EncodeToString(buf[:])
}

// chunks returns the storable keys once each, in byte order (the order rows
// are locked in, see setMany), with their prefixed forms, in chunks. A key
// twice in one statement would get two tokens, the row only one of them.
func (l leaser) chunks(keys []string) [][2][]string {
	keys = slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(keys))), func(key string) bool { return !storable(l.b.prefix + key) })
	var out [][2][]string
	for chunk := range slices.Chunk(keys, min(l.b.chunkSize, maxLeaseChunk)) {
		prefixed := make([]string, len(chunk))
		for i, key := range chunk {
			prefixed[i] = l.b.prefix + key
		}
		out = append(out, [2][]string{chunk, prefixed})
	}
	return out
}

// Lease takes the leases of a chunk in three statements, each committed on
// its own: insert a row holding only the lease for each missing key; take
// the lease of each row nobody leases (or whose lease expired); read back
// which rows hold this call's tokens.
func (l leaser) Lease(ctx context.Context, keys []string, ttl time.Duration) (map[string]string, error) {
	out := map[string]string{}
	errs := map[string]error{}
	for _, key := range keys { // left out by chunks: not another instance's
		if !storable(l.b.prefix + key) {
			errs[key] = fmt.Errorf("gormcachex: lease %q: the key column cannot hold it: %w", key, cachex.ErrUnsupportedKey)
		}
	}
	db := l.b.db.WithContext(ctx)
	for _, c := range l.chunks(keys) {
		chunk, prefixed := c[0], c[1]
		now := time.Now()
		until := now.Add(ttl)
		tokens := make([]string, len(chunk))
		rows := make([]leasedRow, len(chunk))
		cases := make([]any, 0, 2*len(chunk))
		for i := range chunk {
			tokens[i] = newToken()
			rows[i] = leasedRow{Key: prefixed[i], Value: []byte{}, ExpiresAt: until, UpdatedAt: now, LeaseToken: &tokens[i], LeaseUntil: &until}
			cases = append(cases, prefixed[i], tokens[i])
		}
		err := l.b.write(ctx, func(db *gorm.DB) error {
			return db.Table(l.b.table).Clauses(clause.OnConflict{Columns: []clause.Column{keyColumn}, DoNothing: true}).Create(&rows).Error
		})
		if err == nil {
			err = l.b.write(ctx, func(db *gorm.DB) error {
				free := clause.Or(clause.Eq{Column: leaseToken, Value: nil}, clause.Lt{Column: leaseUntil, Value: now})
				return l.b.inLockOrder(db, clause.And(clause.IN{Column: keyColumn, Values: anys(prefixed)}, free)).
					Updates(map[string]any{"lease_token": caseByKey(db, "", cases), "lease_until": until}).Error
			})
		}
		var held []leasedRow
		if err == nil {
			err = db.Table(l.b.table).Select("?, ?", keyColumn, leaseToken).
				Where(clause.IN{Column: keyColumn, Values: anys(prefixed)}).Find(&held).Error
		}
		if err != nil {
			for _, key := range chunk {
				errs[key] = fmt.Errorf("gormcachex: lease %q: %w", key, err)
			}
			continue
		}
		mine := make(map[string]string, len(chunk))
		for i := range chunk {
			mine[prefixed[i]] = tokens[i]
		}
		for _, r := range held {
			if r.LeaseToken != nil && *r.LeaseToken == mine[r.Key] {
				out[strings.TrimPrefix(r.Key, l.b.prefix)] = *r.LeaseToken
			}
		}
	}
	return out, cachex.NewBatchError(errs)
}

// caseByKey is `CASE key WHEN k1 THEN v1 ... END` for pairs k1, v1, ...,
// its values cast to pgType on PostgreSQL, which otherwise takes them for
// text. The cast is `?::type`: GORM expands a slice (a []byte value) bound
// right after a "(", as in CAST(?, into a list.
func caseByKey(db *gorm.DB, pgType string, pairs []any) clause.Expr {
	then := " WHEN ? THEN ?"
	if pgType != "" && db.Name() == "postgres" {
		then = " WHEN ? THEN ?::" + pgType
	}
	var sql strings.Builder
	sql.WriteString("CASE ?")
	for range len(pairs) / 2 {
		sql.WriteString(then)
	}
	sql.WriteString(" END")
	return clause.Expr{SQL: sql.String(), Vars: append([]any{keyColumn}, pairs...)}
}

// holding matches the rows of prefixed whose lease holds tokens.
func holding(prefixed, tokens []string) clause.Expression {
	ors := make([]clause.Expression, len(prefixed))
	for i := range prefixed {
		ors[i] = clause.And(clause.Eq{Column: keyColumn, Value: prefixed[i]}, clause.Eq{Column: leaseToken, Value: tokens[i]})
	}
	if len(ors) == 1 { // a lone clause.Or joins the conditions around it with OR
		return ors[0]
	}
	return clause.Or(ors...)
}

// PutLeased updates the rows whose lease holds its token in one statement a
// chunk, clearing the lease, and deletes in another the rows of the keys
// without an entry. A row a write deleted is not there to update: an old
// value never brings it back.
func (l leaser) PutLeased(ctx context.Context, tokens map[string]string, entries map[string]cachex.ByteEntry) error {
	errs := map[string]error{}
	for _, c := range l.chunks(slices.Collect(maps.Keys(tokens))) {
		chunk, prefixed := c[0], c[1]
		var setP, setT, delP, delT []string
		var setKeys, delKeys []string
		var values, expires []any
		for i, key := range chunk {
			if e, ok := entries[key]; ok {
				setKeys, setP, setT = append(setKeys, key), append(setP, prefixed[i]), append(setT, tokens[key])
				values = append(values, prefixed[i], e.Data)
				expires = append(expires, prefixed[i], e.ExpiresAt)
			} else {
				delKeys, delP, delT = append(delKeys, key), append(delP, prefixed[i]), append(delT, tokens[key])
			}
		}
		now := time.Now()
		if len(setKeys) > 0 {
			if err := l.b.write(ctx, func(db *gorm.DB) error {
				return l.b.inLockOrder(db, holding(setP, setT)).Updates(map[string]any{
					"value": caseByKey(db, "bytea", values), "expires_at": caseByKey(db, "timestamptz", expires), "updated_at": now,
					"lease_token": nil, "lease_until": nil,
				}).Error
			}); err != nil {
				for _, key := range setKeys {
					errs[key] = fmt.Errorf("gormcachex: put leased %q: %w", key, err)
				}
			}
		}
		if len(delKeys) > 0 {
			if err := l.b.write(ctx, func(db *gorm.DB) error {
				return l.b.inLockOrder(db, holding(delP, delT)).Delete(nil).Error
			}); err != nil {
				for _, key := range delKeys {
					errs[key] = fmt.Errorf("gormcachex: put leased %q: %w", key, err)
				}
			}
		}
	}
	return cachex.NewBatchError(errs)
}

// Release deletes the rows holding only a lease of tokens, and clears the
// lease of the others.
func (l leaser) Release(ctx context.Context, tokens map[string]string) error {
	errs := map[string]error{}
	for _, c := range l.chunks(slices.Collect(maps.Keys(tokens))) {
		chunk, prefixed := c[0], c[1]
		ts := make([]string, len(chunk))
		for i, key := range chunk {
			ts[i] = tokens[key]
		}
		err := l.b.write(ctx, func(db *gorm.DB) error {
			empty := clause.Expr{SQL: "LENGTH(?) = 0", Vars: []any{clause.Column{Name: "value"}}}
			return l.b.inLockOrder(db, clause.And(holding(prefixed, ts), empty)).Delete(nil).Error
		})
		if err == nil {
			err = l.b.write(ctx, func(db *gorm.DB) error {
				return l.b.inLockOrder(db, holding(prefixed, ts)).Updates(map[string]any{"lease_token": nil, "lease_until": nil}).Error
			})
		}
		if err != nil {
			for _, key := range chunk {
				errs[key] = fmt.Errorf("gormcachex: release lease of %q: %w", key, err)
			}
		}
	}
	return cachex.NewBatchError(errs)
}
