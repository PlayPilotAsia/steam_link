package steam

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	// ErrQuotaExhausted 表示当日 Steam 调用配额已耗尽。
	ErrQuotaExhausted = errors.New("steam: daily quota exhausted")
	// ErrCircuitOpen 表示因收到 429 而处于全局熔断期。
	ErrCircuitOpen = errors.New("steam: circuit breaker open")
)

// DailyQuotaLimit 是单个 API Key 的日调用上限，由 Steam API 使用条款规定。
const DailyQuotaLimit = 100_000

type Limiter interface {
	Acquire(ctx context.Context) error
}

// tokenBucketScript 是原子的令牌桶实现。
// KEYS[1]=桶 key  ARGV[1]=速率(个/秒)  ARGV[2]=容量  ARGV[3]=当前时间(毫秒)
// 返回 1 表示放行，0 表示限流。
var tokenBucketScript = redis.NewScript(`
local rate     = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now_ms   = tonumber(ARGV[3])

local state    = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens   = tonumber(state[1])
local last_ms  = tonumber(state[2])

if tokens == nil then
  tokens  = capacity
  last_ms = now_ms
end

-- 按经过的时间回填令牌，上限为容量
local delta = math.max(0, now_ms - last_ms) / 1000.0 * rate
tokens = math.min(capacity, tokens + delta)

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call('HMSET', KEYS[1], 'tokens', tokens, 'ts', now_ms)
-- 桶闲置足够久后自动过期，避免僵尸 key
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity / rate * 1000) + 10000)
return allowed
`)

type RedisLimiter struct {
	rdb     *redis.Client
	rate    int
	burst   int
	nowFunc func() time.Time
}

func NewRedisLimiter(rdb *redis.Client, ratePerSec, burst int) *RedisLimiter {
	return &RedisLimiter{
		rdb:     rdb,
		rate:    ratePerSec,
		burst:   burst,
		nowFunc: func() time.Time { return time.Now().UTC() },
	}
}

const (
	bucketKey    = "steam:bucket"
	breakerKey   = "steam:breaker"
	quotaKeyBase = "steam:quota:"
)

func (l *RedisLimiter) quotaKey() string {
	return quotaKeyBase + l.nowFunc().Format("20060102")
}

func (l *RedisLimiter) Acquire(ctx context.Context) error {
	// 1. 熔断优先：熔断期内一律拒绝，不消耗令牌
	n, err := l.rdb.Exists(ctx, breakerKey).Result()
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrCircuitOpen
	}

	// 2. 日配额守卫
	used, err := l.QuotaUsed(ctx)
	if err != nil {
		return err
	}
	if used >= DailyQuotaLimit {
		return ErrQuotaExhausted
	}

	// 3. 令牌桶
	now := l.nowFunc().UnixMilli()
	allowed, err := tokenBucketScript.Run(ctx, l.rdb,
		[]string{bucketKey}, l.rate, l.burst, now).Int()
	if err != nil {
		return err
	}
	if allowed == 0 {
		// 本地节流，请求还没发出去 —— 与 Steam 返回 429 是两回事
		return ErrThrottled
	}

	// 4. 计入配额。放行后才计数，保证与实际调用数一致。
	key := l.quotaKey()
	if err := l.rdb.Incr(ctx, key).Err(); err != nil {
		return err
	}
	return l.rdb.Expire(ctx, key, 48*time.Hour).Err()
}

func (l *RedisLimiter) QuotaUsed(ctx context.Context) (int64, error) {
	v, err := l.rdb.Get(ctx, l.quotaKey()).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return v, err
}

// TripBreaker 在收到 429 后调用，令所有 worker 实例统一退避。
func (l *RedisLimiter) TripBreaker(ctx context.Context, d time.Duration) error {
	return l.rdb.Set(ctx, breakerKey, "1", d).Err()
}

var _ Limiter = (*RedisLimiter)(nil)
