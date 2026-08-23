package service

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConcurrencyService is a Redis-backed, self-healing concurrency limiter shared
// by the per-user gate (画图台 + API key) and the per-account upstream gate.
//
// Each slot is a member of a sorted set keyed by the subject (user/account),
// scored with its expiry time. Acquire prunes expired members first, so a slot
// whose Release was lost (crash / missed defer) auto-frees after the TTL — the
// count can never leak forever. It's intentionally lossy-tolerant: if Redis is
// unavailable it FAILS OPEN (allows the work) rather than blocking generation.
type ConcurrencyService struct {
	redis *redis.Client
	// ttl is the max lifetime of a slot — the longest a generation can run
	// (video ~3min) plus head-room, after which a stuck slot self-heals.
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
// `token`. Returns true if admitted. Fail-open when Redis is down/unset.
func (c *ConcurrencyService) Acquire(ctx context.Context, key string, max int, token string) bool {
	if c == nil || c.redis == nil {
		return true
	}
	res, err := acquireScript.Run(ctx, c.redis, []string{key}, max, c.ttl, token).Int()
	if err != nil {
		return true // fail open — never block a generation on Redis trouble
	}
	return res == 1
}

// Release frees the slot held by `token` under `key`. Safe to call even if the
// slot already expired.
func (c *ConcurrencyService) Release(ctx context.Context, key, token string) {
	if c == nil || c.redis == nil {
		return
	}
	_ = c.redis.ZRem(ctx, key, token).Err()
}

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
