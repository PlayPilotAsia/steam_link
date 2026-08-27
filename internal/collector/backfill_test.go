package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

// 回填任务必须用最低优先级 —— 一个新用户会一次性产生数百条任务，
// 若与实时会话结算同级排队，会把所有用户的实时性拖垮。
func TestEnqueueBackfill_UsesLowestPriority(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	q := task.NewMySQLQueue(db)
	require.NoError(t, EnqueueBackfill(ctx, q, 1, []uint32{440, 620, 730}, now))

	var rows []store.SyncTask
	require.NoError(t, db.Order("appid").Find(&rows).Error)
	require.Len(t, rows, 3)

	for _, r := range rows {
		require.Equal(t, task.PriorityBackfill, r.Priority)
		require.Equal(t, task.TypeAchievementSync, r.Type)
	}
}

// 回填任务在时间上错开，避免瞬间打满限流器。
func TestEnqueueBackfill_SpreadsOverTime(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	ids := make([]uint32, 100)
	for i := range ids {
		ids[i] = uint32(1000 + i)
	}

	q := task.NewMySQLQueue(db)
	require.NoError(t, EnqueueBackfill(ctx, q, 1, ids, now))

	var first, last store.SyncTask
	require.NoError(t, db.Order("next_run_at").Take(&first).Error)
	require.NoError(t, db.Order("next_run_at DESC").Take(&last).Error)

	require.True(t, last.NextRunAt.After(first.NextRunAt.Add(time.Minute)),
		"回填任务应在时间上铺开，而非全部堆在同一时刻")
}

// 实时任务必须能插队到回填任务前面。
func TestEnqueueBackfill_RealtimeTasksJumpQueue(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)

	q := task.NewMySQLQueue(db)
	require.NoError(t, EnqueueBackfill(ctx, q, 1, []uint32{440, 620}, past))

	require.NoError(t, q.Enqueue(ctx, task.Task{
		Type: task.TypeSessionSettle, SteamID: 1, AppID: 730,
		Priority: task.PriorityRealtime, NextRunAt: past,
	}))

	got, err := q.Claim(ctx, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, task.TypeSessionSettle, got[0].Type)
}
