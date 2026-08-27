package collector

import (
	"context"
	"errors"
	"fmt"

	"steamlink/internal/steam"
	"steamlink/internal/task"
)

// ErrDeferredByQuota 表示任务因配额压力被推迟。
//
// 它包装 task.ErrDeferred，因此 Runner 会走 Defer 路径 —— 推迟但不累加
// attempts。这一点是必需的：配额耗尽可能持续数小时到次日，若按普通失败
// 累加 attempts，这批任务会在配额恢复前就被推进死信。
var ErrDeferredByQuota = fmt.Errorf("collector: 配额压力下推迟: %w", task.ErrDeferred)

type DegradeLevel int

const (
	DegradeNone     DegradeLevel = 0 // 正常
	DegradeBackfill DegradeLevel = 1 // 停掉低优先级回填
	DegradeAll      DegradeLevel = 2 // 停掉全部队列任务，只保留探针
)

// 降级阈值，占日配额（100,000）的比例
const (
	backfillStopThreshold int64 = 80_000
	allStopThreshold      int64 = 95_000
)

func quotaLevel(used int64) DegradeLevel {
	switch {
	case used >= allStopThreshold:
		return DegradeAll
	case used >= backfillStopThreshold:
		return DegradeBackfill
	default:
		return DegradeNone
	}
}

type QuotaGuard interface {
	Level(ctx context.Context) (DegradeLevel, error)
}

type RedisQuotaGuard struct{ l *steam.RedisLimiter }

func NewRedisQuotaGuard(l *steam.RedisLimiter) *RedisQuotaGuard {
	return &RedisQuotaGuard{l: l}
}

func (g *RedisQuotaGuard) Level(ctx context.Context) (DegradeLevel, error) {
	used, err := g.l.QuotaUsed(ctx)
	if err != nil {
		// 读不到配额时保守放行：宁可多用一点配额，
		// 也不要因为 Redis 抖动就停掉全部采集。
		return DegradeNone, nil
	}
	return quotaLevel(used), nil
}

// WithQuotaGuard 包装 handler，在配额压力下按优先级丢弃任务。
//
// minPriority 是 DegradeBackfill 级别下仍允许执行的最低优先级数值
// （数值小者优先，因此传 task.PriorityNormal 意味着放行 Realtime 与 Normal）。
func WithQuotaGuard(g QuotaGuard, minPriority int8, h task.Handler) task.Handler {
	return func(ctx context.Context, t task.Task) error {
		level, err := g.Level(ctx)
		if err != nil {
			return err
		}

		switch level {
		case DegradeAll:
			return fmt.Errorf("配额已逼近上限，推迟全部队列任务: %w", ErrDeferredByQuota)
		case DegradeBackfill:
			if t.Priority > minPriority {
				return fmt.Errorf("配额紧张，推迟低优先级任务: %w", ErrDeferredByQuota)
			}
		}

		err = h(ctx, t)

		// 本地令牌桶拒绝、熔断、日配额耗尽都不是任务本身的失败，
		// 统一转成推迟。否则回填高峰期任务会因反复触发限流而被推入死信。
		switch {
		case errors.Is(err, steam.ErrThrottled):
			return fmt.Errorf("本地限流，稍后重试: %w", task.ErrDeferred)
		case errors.Is(err, steam.ErrCircuitOpen):
			return fmt.Errorf("熔断中，稍后重试: %w", task.ErrDeferred)
		case errors.Is(err, steam.ErrQuotaExhausted):
			return fmt.Errorf("日配额已耗尽: %w", task.ErrDeferred)
		}
		return err
	}
}
