package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/PlayPilotAsia/steam_link/internal/domain"
	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

type stubSteam struct {
	steam.Client
	calls   [][]uint64
	results map[uint64]uint32 // steamID → gameID
	err     error
}

func (s *stubSteam) GetPlayerSummaries(_ context.Context, ids []uint64) ([]steam.PlayerSummary, error) {
	s.calls = append(s.calls, append([]uint64(nil), ids...))
	if s.err != nil {
		return nil, s.err
	}
	out := make([]steam.PlayerSummary, 0, len(ids))
	for _, id := range ids {
		out = append(out, steam.PlayerSummary{
			SteamID:                  id,
			CommunityVisibilityState: 3,
			GameID:                   s.results[id],
		})
	}
	return out, nil
}

func newProbeFixture(t *testing.T, now time.Time, ids ...uint64) (*store.ProbeRepo, task.Queue, *gorm.DB) {
	t.Helper()
	db := storeTestDB(t)
	pr := store.NewProbeRepo(db)
	for _, id := range ids {
		require.NoError(t, pr.Ensure(context.Background(), id, now))
	}
	return pr, task.NewMySQLQueue(db), db
}

// 100 个以内的用户应合并为一次请求 —— 这是整个方案的成本支点。
func TestProber_BatchesUpToLimit(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	ids := make([]uint64, 0, 150)
	for i := 0; i < 150; i++ {
		ids = append(ids, uint64(76561197960287930+i))
	}
	pr, q, _ := newProbeFixture(t, now, ids...)

	st := &stubSteam{results: map[uint64]uint32{}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})

	require.NoError(t, p.RunOnce(context.Background()))

	require.Len(t, st.calls, 2, "150 个用户应拆成 2 批")
	require.Len(t, st.calls[0], steam.MaxSummariesBatch)
	require.Len(t, st.calls[1], 50)
}

// 观测到用户在玩游戏 → 状态转为 Playing，但此时不产生任何任务。
func TestProber_StartsSession(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, db := newProbeFixture(t, now, 1)

	st := &stubSteam{results: map[uint64]uint32{1: 440}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})
	require.NoError(t, p.RunOnce(context.Background()))

	due, err := pr.Due(context.Background(), now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, uint32(440), store.ToDomain(due[0]).AppID)

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n, "会话开始不应产生任务，只有结束才需要结算")
}

// 会话结束 → 入队结算任务，且延迟 5 分钟执行。
func TestProber_EnqueuesSettleTaskOnSessionEnd(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, db := newProbeFixture(t, now, 1)
	ctx := context.Background()

	// 预置一个已累计 1 次 miss 的进行中会话，下一轮观测不到即结束
	require.NoError(t, pr.Save(ctx, 1, domain.State{
		AppID: 440, StartedAt: now.Add(-30 * time.Minute),
		LastSeenPlayingAt: now.Add(-2 * time.Minute), MissCount: 1,
	}, 0, now, now))

	st := &stubSteam{results: map[uint64]uint32{1: 0}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})
	require.NoError(t, p.RunOnce(ctx))

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, task.TypeSessionSettle, row.Type)
	require.Equal(t, uint32(440), row.AppID)
	require.Equal(t, task.PriorityRealtime, row.Priority)
	require.Equal(t, now.Add(SettleDelay).Unix(), row.NextRunAt.Unix(),
		"Steam 的 playtime 在退出后才结算，必须延迟查询")
}

// 关键安全测试：请求失败绝不能被当成「所有人都没在玩」。
// 否则一次网络抖动会让整批用户的会话被同时误判结束。
func TestProber_RequestFailureDoesNotEndSessions(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, db := newProbeFixture(t, now, 1)
	ctx := context.Background()

	require.NoError(t, pr.Save(ctx, 1, domain.State{
		AppID: 440, StartedAt: now.Add(-30 * time.Minute),
		LastSeenPlayingAt: now.Add(-2 * time.Minute), MissCount: 1,
	}, 0, now, now))

	st := &stubSteam{err: errors.New("connection reset")}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})

	require.Error(t, p.RunOnce(ctx))

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n, "请求失败不得产生任何结算任务")

	due, err := pr.Due(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, uint32(440), store.ToDomain(due[0]).AppID, "会话状态必须保持不变")
}

// Steam 返回的 players 数组可能缺少某些 SteamID（账号被封等），
// 这些用户同样不能被当作「没在玩」。
func TestProber_MissingPlayerInResponseIsSkipped(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, db := newProbeFixture(t, now, 1, 2)
	ctx := context.Background()

	require.NoError(t, pr.Save(ctx, 2, domain.State{
		AppID: 440, StartedAt: now.Add(-time.Hour),
		LastSeenPlayingAt: now.Add(-2 * time.Minute), MissCount: 1,
	}, 0, now, now))

	// stub 只返回 id=1，id=2 缺失
	st := &partialSteam{present: map[uint64]uint32{1: 0}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})
	require.NoError(t, p.RunOnce(ctx))

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n, "响应中缺失的用户应跳过，不得判定会话结束")
}

type partialSteam struct {
	steam.Client
	present map[uint64]uint32
}

func (s *partialSteam) GetPlayerSummaries(_ context.Context, ids []uint64) ([]steam.PlayerSummary, error) {
	var out []steam.PlayerSummary
	for _, id := range ids {
		if g, ok := s.present[id]; ok {
			out = append(out, steam.PlayerSummary{
				SteamID: id, CommunityVisibilityState: 3, GameID: g,
			})
		}
	}
	return out, nil
}

// 沉睡用户一旦开始游玩，必须立刻升到最高探测频率。
func TestProber_PlayingUserUpgradesToActiveTier(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, _ := newProbeFixture(t, now, 1)
	ctx := context.Background()

	// 预置一个沉睡用户
	require.NoError(t, pr.Save(ctx, 1, domain.State{}, int8(domain.TierAsleep),
		now, now))

	st := &stubSteam{results: map[uint64]uint32{1: 440}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})
	require.NoError(t, p.RunOnce(ctx))

	due, err := pr.Due(ctx, now.Add(3*time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, due, 1, "开始游玩后应在 2 分钟内再次到期")
	require.Equal(t, int8(domain.TierActive), due[0].Tier)
}
