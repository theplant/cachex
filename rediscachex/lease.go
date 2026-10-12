package rediscachex

import (
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/theplant/cachex/v2"
)

// leasePrefix starts every lease key (see leaseKey).
const leasePrefix = "cachex-lease:"

// leaseKey returns the key of the lease of the data key prefixed, in its
// Redis Cluster slot, so that one script can touch both: with a hash tag in
// prefixed it is leasePrefix+"t:"+prefixed (the tag still comes first),
// without one prefixed is wrapped in braces; the "t:" keeps "X" and "{X}"
// apart. A key with a "}" but no hash tag has no such key; cachex.Cache
// rejects it (see cachex.ErrUnsupportedKey). Neither has the empty key (an
// empty KeyPrefix and key): "{}" is not a hash tag.
func leaseKey(prefixed string) (string, bool) {
	if hasHashTag(prefixed) {
		return leasePrefix + "t:" + prefixed, true
	}
	if prefixed == "" || strings.IndexByte(prefixed, '}') >= 0 { // "{}" is no tag
		return "", false
	}
	return leasePrefix + "{" + prefixed + "}", true
}

// hasHashTag reports whether s has a Redis hash tag: a "{" followed later by
// a "}" with something in between.
func hasHashTag(s string) bool {
	open := strings.IndexByte(s, '{')
	return open >= 0 && strings.IndexByte(s[open+1:], '}') > 0
}

func errNoLeaseKey(key string) error {
	return fmt.Errorf("rediscachex: key %q: %w", key, cachex.ErrUnsupportedKey)
}

// Leaser returns the backend's leaser, nil unless Config.Leases is set.
func (b *byteBackend) Leaser() cachex.ByteLeaser {
	if !b.leases {
		return nil
	}
	return leaser{b}
}

type leaser struct{ b *byteBackend }

var _ cachex.ByteLeaser = leaser{}

var newToken = func() string {
	var buf [16]byte
	for i := range 2 {
		u := rand.Uint64()
		for j := range 8 {
			buf[8*i+j] = byte(u >> (8 * j))
		}
	}
	return hex.EncodeToString(buf[:])
}

// Lease sets each lease key with SET NX PX, in pipelines of ChunkSize keys,
// and reads it back in the same pipeline: go-redis retries a pipeline whose
// replies were lost, and a SET NX that landed the first time fails the
// second, but the key then holds this call's token.
func (l leaser) Lease(ctx context.Context, keys []string, ttl time.Duration) (map[string]string, error) {
	out := map[string]string{}
	errs := map[string]error{}
	ttl = max(ttl, time.Millisecond)
	for chunk := range slices.Chunk(keys, l.b.chunkSize) {
		pipe := l.b.client.Pipeline()
		cmds := make([]*redis.BoolCmd, len(chunk))
		gets := make([]*redis.StringCmd, len(chunk))
		tokens := make([]string, len(chunk))
		for i, key := range chunk {
			if l.b.leaseSpace(key) {
				continue // never stored: nothing to lease
			}
			lk, ok := leaseKey(l.b.prefix + key)
			if !ok {
				errs[key] = errNoLeaseKey(key)
				continue
			}
			tokens[i] = newToken()
			cmds[i] = pipe.SetNX(ctx, lk, tokens[i], ttl)
			gets[i] = pipe.Get(ctx, lk)
		}
		if pipe.Len() == 0 {
			continue
		}
		execPipe(ctx, pipe)
		for i, key := range chunk {
			if cmds[i] == nil {
				continue
			}
			switch taken, err := cmds[i].Result(); {
			case err != nil:
				errs[key] = fmt.Errorf("rediscachex: lease %q: %w", key, err)
			case taken || gets[i].Val() == tokens[i]:
				out[key] = tokens[i]
			}
		}
	}
	return out, cachex.NewBatchError(errs)
}

// putLeased writes (ARGV[2] set: ARGV[2] for ARGV[3] milliseconds) or deletes
// KEYS[1] if KEYS[2], its lease, holds ARGV[1], and ends the lease.
var putLeased = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[2])
if ARGV[3] then
	redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
else
	redis.call('DEL', KEYS[1])
end
return 1
`)

// release deletes KEYS[1], a lease, if it holds ARGV[1].
var release = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then redis.call('DEL', KEYS[1]) end
return 0
`)

func (l leaser) PutLeased(ctx context.Context, tokens map[string]string, entries map[string]cachex.ByteEntry) error {
	return l.runScripts(ctx, "put", putLeased, tokens, func(key, prefixed, lk, token string) ([]string, []any) {
		if e, ok := entries[key]; ok {
			ms := max(time.Until(e.ExpiresAt).Milliseconds(), 1)
			return []string{prefixed, lk}, []any{token, e.Data, strconv.FormatInt(ms, 10)}
		}
		return []string{prefixed, lk}, []any{token}
	})
}

func (l leaser) Release(ctx context.Context, tokens map[string]string) error {
	return l.runScripts(ctx, "release", release, tokens, func(_, _, lk, token string) ([]string, []any) {
		return []string{lk}, []any{token}
	})
}

// runScripts runs script once per key of tokens, with the keys and args that
// call returns, in pipelines of ChunkSize keys. A pipeline runs EVALSHA; the
// keys whose node lacks the script (NOSCRIPT, after a restart or SCRIPT
// FLUSH) are run again with EVAL, which also loads it.
func (l leaser) runScripts(ctx context.Context, op string, script *redis.Script, tokens map[string]string,
	call func(key, prefixed, lk, token string) ([]string, []any)) error {
	errs := map[string]error{}
	for chunk := range slices.Chunk(slices.Sorted(maps.Keys(tokens)), l.b.chunkSize) {
		for _, eval := range []bool{false, true} {
			pipe := l.b.client.Pipeline()
			cmds := make([]*redis.Cmd, len(chunk))
			for i, key := range chunk {
				prefixed := l.b.prefix + key
				lk, ok := leaseKey(prefixed)
				if !ok {
					errs[key] = errNoLeaseKey(key)
					continue
				}
				keys, args := call(key, prefixed, lk, tokens[key])
				if eval {
					cmds[i] = script.Eval(ctx, pipe, keys, args...)
				} else {
					cmds[i] = script.EvalSha(ctx, pipe, keys, args...)
				}
			}
			if pipe.Len() == 0 {
				break
			}
			execPipe(ctx, pipe)
			var again []string
			for i, key := range chunk {
				if cmds[i] == nil {
					continue
				}
				switch err := cmds[i].Err(); {
				case err == nil:
				case !eval && redis.HasErrorPrefix(err, "NOSCRIPT"):
					again = append(again, key)
				default:
					errs[key] = fmt.Errorf("rediscachex: %s lease of %q: %w", op, key, err)
				}
			}
			if len(again) == 0 {
				break
			}
			chunk = again
		}
	}
	return cachex.NewBatchError(errs)
}

// endLease queues, for a write of key with leases on, the deletion of its
// lease, before the write itself: deleted after, a backfill holding the
// token could land between the two.
func (b *byteBackend) endLease(ctx context.Context, pipe redis.Pipeliner, key string) (*redis.IntCmd, error) {
	if !b.leases {
		return nil, nil
	}
	lk, ok := leaseKey(b.prefix + key)
	if !ok {
		return nil, errNoLeaseKey(key)
	}
	return pipe.Del(ctx, lk), nil
}
