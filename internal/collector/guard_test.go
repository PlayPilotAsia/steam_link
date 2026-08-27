package collector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

type fixedGuard struct{ level DegradeLevel }

func (g fixedGuard) Level(context.Context) (DegradeLevel, error) { return g.level, nil }

func TestQuotaLevelFromUsage(t *testing.T) {
	require.Equal(t, DegradeNone, quotaLevel(0))
	require.Equal(t, DegradeNone, quotaLevel(79_999))
	require.Equal(t, DegradeBackfill, quotaLevel(80_000))
	require.Equal(t, DegradeBackfill, quotaLevel(94_999))
	require.Equal(t, DegradeAll, quotaLevel(95_000))
	require.Equal(t, DegradeAll, quotaLevel(200_000))
}

// 未降级时所有任务正常执行。
func TestWithQuotaGuard_PassesThroughWhenHealthy(t *testing.T) {
	called := false
	h := WithQuotaGuard(fixedGuard{DegradeNone}, task.PriorityNormal,
		func(context.Context, task.Task) error { called = true; return nil })

	require.NoError(t, h(context.Background(), task.Task{Priority: task.PriorityBackfill}))
	require.True(t, called)
}

// 配额超过 80% 时，回填任务被推迟而非执行。
func TestWithQuotaGuard_DefersBackfillUnderPressure(t *testing.T) {
	called := false
	h := WithQuotaGuard(fixedGuard{DegradeBackfill}, task.PriorityNormal,
		func(context.Context, task.Task) error { called = true; return nil })

	err := h(context.Background(), task.Task{Priority: task.PriorityBackfill})
	require.ErrorIs(t, err, ErrDeferredByQuota)
	require.False(t, called, "回填任务不应被执行")
}

// 同样的压力下，实时任务必须照常执行。
func TestWithQuotaGuard_KeepsRealtimeUnderPressure(t *testing.T) {
	called := false
	h := WithQuotaGuard(fixedGuard{DegradeBackfill}, task.PriorityNormal,
		func(context.Context, task.Task) error { called = true; return nil })

	require.NoError(t, h(context.Background(), task.Task{Priority: task.PriorityRealtime}))
	require.True(t, called)
}

// 配额几乎耗尽时，连普通任务也让位，只保留探针（探针不走任务队列）。
func TestWithQuotaGuard_DefersEverythingWhenCritical(t *testing.T) {
	h := WithQuotaGuard(fixedGuard{DegradeAll}, task.PriorityNormal,
		func(context.Context, task.Task) error { return nil })

	require.ErrorIs(t,
		h(context.Background(), task.Task{Priority: task.PriorityRealtime}),
		ErrDeferredByQuota)
	require.ErrorIs(t,
		h(context.Background(), task.Task{Priority: task.PriorityNormal}),
		ErrDeferredByQuota)
}

// 被推迟的任务是可重试的普通错误，绝不能是永久错误 ——
// 否则次日配额重置后它永远不会再执行。
func TestWithQuotaGuard_DeferIsRetryable(t *testing.T) {
	h := WithQuotaGuard(fixedGuard{DegradeAll}, task.PriorityNormal,
		func(context.Context, task.Task) error { return nil })

	err := h(context.Background(), task.Task{Priority: task.PriorityNormal})
	require.NotErrorIs(t, err, task.ErrPermanent)
	require.ErrorIs(t, err, task.ErrDeferred,
		"必须被 Runner 识别为推迟，否则会累加 attempts 并最终进入死信")
}

// 本地限流与熔断同样应转成推迟，而非计为任务失败。
func TestWithQuotaGuard_ConvertsThrottleToDefer(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"本地令牌桶", steam.ErrThrottled},
		{"熔断", steam.ErrCircuitOpen},
		{"日配额耗尽", steam.ErrQuotaExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := WithQuotaGuard(fixedGuard{DegradeNone}, task.PriorityNormal,
				func(context.Context, task.Task) error { return tc.err })

			err := h(context.Background(), task.Task{Priority: task.PriorityBackfill})
			require.ErrorIs(t, err, task.ErrDeferred)
		})
	}
}
