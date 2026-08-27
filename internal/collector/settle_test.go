package collector

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type recentStub struct {
	steam.Client
	games []steam.OwnedGame
	err   error
}

func (s *recentStub) GetRecentlyPlayedGames(context.Context, uint64) ([]steam.OwnedGame, error) {
	return s.games, s.err
}

func settleTask(t *testing.T, started, ended time.Time) task.Task {
	t.Helper()
	p, err := json.Marshal(task.SessionPayload{StartedAt: started, EndedAt: ended})
	require.NoError(t, err)
	return task.Task{
		Type: task.TypeSessionSettle, SteamID: 1, AppID: 440, Payload: p,
	}
}

// 核心行为：时长取 Steam 的真实增量，起始时刻由结束时刻反推 ——
// 探针推算的起始时刻最多有一个轮询周期的误差，不可信。
func TestSettler_UsesSteamDeltaAndBackdatesStart(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, Name: "TF2", PlaytimeForeverMin: 100}}, now))

	// Steam 侧现在是 147 分钟，比库中记录多 47 分钟
	st := &recentStub{games: []steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 147}}}

	s := NewSettler(SettlerDeps{
		Steam: st, Games: games, Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	// 探针认为玩了 12:00–12:50（50 分钟），但真实增量是 47 分钟
	require.NoError(t, s.Handle(ctx, settleTask(t,
		now.Add(-time.Hour), now.Add(-10*time.Minute))))

	var sess store.PlaySession
	require.NoError(t, db.Take(&sess).Error)
	require.Equal(t, uint32(47), sess.DurationMin, "时长应取 Steam 增量而非探针推算")
	require.Equal(t, now.Add(-10*time.Minute).Unix(), sess.EndedAt.Unix())
	require.Equal(t, now.Add(-10*time.Minute).Add(-47*time.Minute).Unix(),
		sess.StartedAt.Unix(), "起始时刻应由结束时刻减去真实时长反推")
	require.Equal(t, store.SourceProbe, sess.Source)

	// 累计时长必须同步更新，否则下次差分会重复计算
	m, err := games.PlaytimeMap(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, uint32(147), m[440])
}

// 时长没有增长（探针误判、Steam 尚未结算）→ 不写会话，不算失败。
func TestSettler_NoDeltaWritesNothing(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))

	st := &recentStub{games: []steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}}
	s := NewSettler(SettlerDeps{
		Steam: st, Games: games, Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	require.NoError(t, s.Handle(ctx, settleTask(t, now.Add(-time.Hour), now)))

	var n int64
	require.NoError(t, db.Model(&store.PlaySession{}).Count(&n).Error)
	require.Zero(t, n)
}

// 该游戏有成就时应入队 L2 下钻。
func TestSettler_EnqueuesAchievementSync(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))
	// 标记该游戏有成就
	require.NoError(t, db.Model(&store.App{}).Where("appid = ?", 440).
		Update("has_achievements", 1).Error)

	st := &recentStub{games: []steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 130}}}
	s := NewSettler(SettlerDeps{
		Steam: st, Games: games, Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	require.NoError(t, s.Handle(ctx, settleTask(t, now.Add(-time.Hour), now)))

	var row store.SyncTask
	require.NoError(t, db.Where("task_type = ?", task.TypeAchievementSync).
		Take(&row).Error)
	require.Equal(t, uint32(440), row.AppID)
}

// 隐私墙 → 永久错误，不重试。持续重试只会白烧配额。
func TestSettler_PrivateProfileIsPermanent(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	st := &recentStub{err: steam.ErrProfilePrivate}
	s := NewSettler(SettlerDeps{
		Steam: st, Games: store.NewGameRepo(db), Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	err := s.Handle(ctx, settleTask(t, now.Add(-time.Hour), now))
	require.ErrorIs(t, err, task.ErrPermanent)
}

// 重复结算（租约回收后重跑）必须幂等。
func TestSettler_RerunIsIdempotent(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))

	st := &recentStub{games: []steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 140}}}
	s := NewSettler(SettlerDeps{
		Steam: st, Games: games, Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	tk := settleTask(t, now.Add(-time.Hour), now)
	require.NoError(t, s.Handle(ctx, tk))
	require.NoError(t, s.Handle(ctx, tk), "重跑不应报错")

	var n int64
	require.NoError(t, db.Model(&store.PlaySession{}).Count(&n).Error)
	require.Equal(t, int64(1), n, "重跑不应产生重复会话")
}
