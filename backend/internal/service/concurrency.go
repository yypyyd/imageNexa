package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrConcurrencyBackendUnavailable = errors.New("concurrency backend unavailable")

// ConcurrencyService is a Redis-backed, self-healing concurrency limiter shared
// by the per-user gate (画图台 + API key) and the per-account upstream gate.
//
// Each slot is a member of a sorted set keyed by the subject (user/account),
// scored with its expiry time. Acquire prunes expired members first, so a slot
// whose Release was lost (crash / missed defer) auto-frees after the TTL — the
// count can never leak forever. Bounded gates fail closed when Redis is
// unavailable; observational capacity reads are best-effort.
type ConcurrencyService struct {
	redis *redis.Client
	// ttl covers the image/video execution deadlines plus bookkeeping headroom.
	// A crashed worker cannot retain a slot indefinitely.
	ttl int
}

func NewConcurrencyService(rdb *redis.Client) *ConcurrencyService {
	return &ConcurrencyService{redis: rdb, ttl: 900} // 15 min
}

// acquireScript: KEYS[1]=set, ARGV[1]=max (0=unlimited), ARGV[2]=ttl secs,
// ARGV[3]=token. Prunes expired members, then admits the token iff under max.
// Returns 1 on success, 0 when full.
var acquireScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local n = redis.call('ZCARD', KEYS[1])
local max = tonumber(ARGV[1])
if max > 0 and n >= max then return 0 end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[2]), ARGV[3])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
return 1
`)

// Acquire takes one slot under `key` (capped at max; 0 = unlimited), tagged with
// `token`. An unlimited gate does not need Redis. A bounded generation gate is
// fail-closed when Redis is unavailable: silently bypassing it can submit the
// same upstream account concurrently and multiply provider spend.
func (c *ConcurrencyService) Acquire(ctx context.Context, key string, max int, token string) (bool, error) {
	if max <= 0 {
		return true, nil
	}
	if c == nil || c.redis == nil {
		return false, ErrConcurrencyBackendUnavailable
	}
	res, err := acquireScript.Run(ctx, c.redis, []string{key}, max, c.ttl, token).Int()
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrConcurrencyBackendUnavailable, err)
	}
	return res == 1, nil
}

// Release frees the slot held by `token` under `key`. Safe to call even if the
// slot already expired.
func (c *ConcurrencyService) Release(ctx context.Context, key, token string) {
	if c == nil || c.redis == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	recordBookkeepingError("release concurrency slot", c.redis.ZRem(cleanupCtx, key, token).Err())
}

// ActiveCount reports live slots after pruning crash-expired members. It is
// observational only; Redis failure returns zero and never affects dispatch.
func (c *ConcurrencyService) ActiveCount(ctx context.Context, key string) int64 {
	counts, ok := c.ActiveCounts(ctx, []string{key})
	if !ok {
		return 0
	}
	return counts[key]
}

// ActiveCounts observes several gates in one Redis pipeline. ok=false means
// the observation is unavailable; callers must treat that as unknown and rely
// on Acquire's atomic decision rather than incorrectly declaring accounts full.
func (c *ConcurrencyService) ActiveCounts(ctx context.Context, keys []string) (map[string]int64, bool) {
	if c == nil || c.redis == nil {
		return nil, false
	}
	unique := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	if len(unique) == 0 {
		return map[string]int64{}, true
	}
	// Use Redis time, as Acquire does. Observation must never prune a live
	// lease using a worker's potentially skewed clock. Bound each read-only Lua
	// batch so a large account pool does not monopolize the Redis event loop.
	const batchSize = 128
	pipe := c.redis.Pipeline()
	commands := make([]*redis.Cmd, 0, (len(unique)+batchSize-1)/batchSize)
	for start := 0; start < len(unique); start += batchSize {
		end := min(start+batchSize, len(unique))
		commands = append(commands, pipe.Eval(ctx, activeCountsScript, unique[start:end]))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, false
	}
	counts := make(map[string]int64, len(unique))
	index := 0
	for _, command := range commands {
		values, err := command.Int64Slice()
		if err != nil {
			return nil, false
		}
		for _, count := range values {
			counts[unique[index]] = count
			index++
		}
	}
	return counts, true
}

// Counts only live leases; expiration cleanup belongs to atomic acquisition.
const activeCountsScript = `
local now = redis.call('TIME')[1]
local counts = {}
for i, key in ipairs(KEYS) do
    counts[i] = redis.call('ZCOUNT', key, '(' .. now, '+inf')
end
return counts
`

// NextCursor returns the next value of a shared round-robin counter (starting at
// 0) and advances it. It is Redis-backed on purpose: a per-process counter
// restarts at 0 on every deploy, which makes the scheduler re-pick the head of
// the account list over and over and leaves the tail of a large pool idle.
// ok=false when Redis is unavailable, so the caller can fall back.
func (c *ConcurrencyService) NextCursor(ctx context.Context, key string) (uint64, bool) {
	if c == nil || c.redis == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	n, err := c.redis.Incr(ctx, "rr:"+key).Result()
	if err != nil {
		return 0, false
	}
	return uint64(n - 1), true
}
