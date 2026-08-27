package collector

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/PlayPilotAsia/steam_link/internal/domain"
	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

// worker 停机数小时后重启，probe_state 里会残留卡在 Playing 的僵尸会话。
// 这些会话的时长已不可信，必须强制结算而非继续累积。
func TestHealer_ForceSettlesZombieSessions(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	old := time.Date(2026, 8, 26, 2, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	probes := store.NewProbeRepo(db)
	require.NoError(t, probes.Ensure(ctx, 1, old))
	require.NoError(t, probes.Save(ctx, 1, domain.State{
		AppID: 440, StartedAt: old, LastSeenPlayingAt: old.Add(10 * time.Minute),
	}, 0, old.Add(2*time.Minute), old))

	h := NewHealer(probes, task.NewMySQLQueue(db), func() time.Time { return now })
	require.NoError(t, h.Run(ctx))

	// 状态应被重置为 Idle
	due, err := probes.Due(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, uint32(0), store.ToDomain(due[0]).AppID)

	// 并入队一条结算任务
	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, task.TypeSessionSettle, row.Type)
	require.Equal(t, uint32(440), row.AppID)

	// 关键：这条会话跨越了宕机窗口，必须标记为推断而非实测。
	// 设计 §3.2 立下的原则是「推断数据不得冒充实测数据」。
	var payload task.SessionPayload
	require.NoError(t, json.Unmarshal(row.Payload, &payload))
	require.Equal(t, store.SourceReconcile, payload.Source,
		"自愈补结的会话必须标记为推断来源")
}

// 近期仍在正常探测的会话不得被自愈打断。
func TestHealer_LeavesFreshSessionsAlone(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	probes := store.NewProbeRepo(db)
	require.NoError(t, probes.Ensure(ctx, 1, now))
	require.NoError(t, probes.Save(ctx, 1, domain.State{
		AppID: 440, StartedAt: now.Add(-20 * time.Minute),
		LastSeenPlayingAt: now.Add(-2 * time.Minute),
	}, 0, now.Add(2*time.Minute), now.Add(-2*time.Minute)))

	h := NewHealer(probes, task.NewMySQLQueue(db), func() time.Time { return now })
	require.NoError(t, h.Run(ctx))

	due, err := probes.Due(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, uint32(440), store.ToDomain(due[0]).AppID, "进行中的会话不应被打断")

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n)
}
