package steam

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	require.NoError(t, c.Ping(context.Background()).Err(),
		"需要本地 Redis：docker compose up -d redis")
	require.NoError(t, c.FlushDB(context.Background()).Err())
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// 桶容量耗尽后必须拒绝，而不是无限放行。
func TestRedisLimiter_BurstThenReject(t *testing.T) {
	l := NewRedisLimiter(testRedis(t), 1, 3) // 1 req/s，桶容量 3
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, l.Acquire(ctx), "前 3 次应放行（burst）")
	}

	err := l.Acquire(ctx)
	require.ErrorIs(t, err, ErrThrottled, "第 4 次应被本地令牌桶拦下")
	require.NotErrorIs(t, err, ErrRateLimited,
		"本地节流不得与 Steam 返回 429 混为一谈：前者应推迟任务，后者要触发熔断")
}

// 令牌按速率回填。
func TestRedisLimiter_Refills(t *testing.T) {
	l := NewRedisLimiter(testRedis(t), 10, 1) // 10 req/s
	ctx := context.Background()

	require.NoError(t, l.Acquire(ctx))
	require.ErrorIs(t, l.Acquire(ctx), ErrThrottled)

	time.Sleep(150 * time.Millisecond) // 10 req/s 下 100ms 回填 1 个
	require.NoError(t, l.Acquire(ctx), "回填后应放行")
}

// 熔断期间一律拒绝，与令牌数无关。
func TestRedisLimiter_CircuitBreaker(t *testing.T) {
	l := NewRedisLimiter(testRedis(t), 100, 100)
	ctx := context.Background()

	require.NoError(t, l.Acquire(ctx))
	require.NoError(t, l.TripBreaker(ctx, 2*time.Second))
	require.ErrorIs(t, l.Acquire(ctx), ErrCircuitOpen)
}

// 每次放行都要计入当日配额。
func TestRedisLimiter_CountsQuota(t *testing.T) {
	l := NewRedisLimiter(testRedis(t), 100, 100)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, l.Acquire(ctx))
	}

	used, err := l.QuotaUsed(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(5), used)
}
