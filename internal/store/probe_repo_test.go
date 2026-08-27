package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/PlayPilotAsia/steam_link/internal/domain"
)

func TestProbeRepo_EnsureAndDue(t *testing.T) {
	r := NewProbeRepo(testDB(t))
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 76561197960287930, now))

	due, err := r.Due(ctx, now, 100)
	require.NoError(t, err)
	require.Len(t, due, 1, "新建的探针状态应立即到期，让新用户马上被采集")
}

// Ensure 必须幂等 —— 重新绑定时不能重置正在进行的会话。
func TestProbeRepo_EnsureIsIdempotent(t *testing.T) {
	db := testDB(t)
	r := NewProbeRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, now))
	require.NoError(t, r.Save(ctx, 1,
		domain.State{AppID: 440, StartedAt: now, LastSeenPlayingAt: now},
		0, now.Add(2*time.Minute), now))

	require.NoError(t, r.Ensure(ctx, 1, now.Add(time.Hour)))

	var row ProbeState
	require.NoError(t, db.Where("steam_id64 = ?", uint64(1)).Take(&row).Error)
	require.NotNil(t, row.CurrentAppID, "已有的会话状态不得被 Ensure 覆盖")
	require.Equal(t, uint32(440), *row.CurrentAppID)
}

// 状态在 domain 与存储之间往返必须无损。
func TestProbeRepo_SaveLoadRoundTrip(t *testing.T) {
	r := NewProbeRepo(testDB(t))
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, now))

	want := domain.State{
		AppID:             730,
		StartedAt:         now,
		LastSeenPlayingAt: now.Add(4 * time.Minute),
		MissCount:         1,
	}
	require.NoError(t, r.Save(ctx, 1, want, 0, now.Add(6*time.Minute), now))

	due, err := r.Due(ctx, now.Add(10*time.Minute), 100)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, want, ToDomain(due[0]))
}

// Idle 状态必须把 current_appid 写回 NULL，不能残留旧 appid。
func TestProbeRepo_SaveIdleClearsAppID(t *testing.T) {
	db := testDB(t)
	r := NewProbeRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, now))
	require.NoError(t, r.Save(ctx, 1,
		domain.State{AppID: 440, StartedAt: now, LastSeenPlayingAt: now},
		0, now, now))
	require.NoError(t, r.Save(ctx, 1, domain.State{}, 0, now.Add(time.Minute), now))

	var row ProbeState
	require.NoError(t, db.Where("steam_id64 = ?", uint64(1)).Take(&row).Error)
	require.Nil(t, row.CurrentAppID)
	require.Nil(t, row.SessionStartedAt)
}

// worker 可多实例部署，探针领取必须与任务表一样受 SKIP LOCKED 保护，
// 否则两个实例会重复探测同一批用户并并发覆写 probe_state ——
// 去抖计数与 LastSeenPlayingAt 被后写覆盖，直接产出错误会话。
func TestProbeRepo_ClaimIsExclusive(t *testing.T) {
	db := testDB(t)
	r := NewProbeRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	for i := uint64(1); i <= 5; i++ {
		require.NoError(t, r.Ensure(ctx, i, now))
	}

	first, err := r.Claim(ctx, now, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 5)

	second, err := r.Claim(ctx, now, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Empty(t, second, "已被领取的用户在租约内不得被再次领取")
}

// 租约到期后自动回收，worker 崩溃不会让用户永久停止探测。
func TestProbeRepo_ClaimReclaimsAfterLease(t *testing.T) {
	r := NewProbeRepo(testDB(t))
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, now))

	_, err := r.Claim(ctx, now, 10, 5*time.Minute)
	require.NoError(t, err)

	again, err := r.Claim(ctx, now.Add(6*time.Minute), 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, again, 1, "租约过期后必须能被重新领取")
}

// 供启动自愈使用：找出长时间未被探测但仍标记为「在玩」的僵尸会话。
func TestProbeRepo_StaleFindsZombieSessions(t *testing.T) {
	r := NewProbeRepo(testDB(t))
	ctx := context.Background()
	old := time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, old))
	require.NoError(t, r.Save(ctx, 1,
		domain.State{AppID: 440, StartedAt: old, LastSeenPlayingAt: old},
		0, old.Add(2*time.Minute), old))

	require.NoError(t, r.Ensure(ctx, 2, old))
	require.NoError(t, r.Save(ctx, 2, domain.State{}, 0, old, old))

	stale, err := r.Stale(ctx, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, stale, 1, "只有仍在 Playing 的僵尸会话需要自愈")
	require.Equal(t, uint64(1), stale[0].SteamID)
}
