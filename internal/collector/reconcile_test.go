package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

type ownedStub struct {
	steam.Client
	games []steam.OwnedGame
	err   error
}

func (s *ownedStub) GetOwnedGames(context.Context, uint64) ([]steam.OwnedGame, error) {
	return s.games, s.err
}

// 达到 strike 阈值时 handlePrivateStrike 会做一次精确探测。
// 这里让它失败，从而回退到「仅游戏详情私密」——
// 与 GetOwnedGames 返回 ErrProfilePrivate 的语义一致。
func (s *ownedStub) GetPlayerSummaries(context.Context, []uint64) ([]steam.PlayerSummary, error) {
	return nil, errors.New("stub: 不提供 summaries")
}

func newReconciler(t *testing.T, st steam.Client, db *gorm.DB, now time.Time) *Reconciler {
	t.Helper()
	return NewReconciler(ReconcilerDeps{
		Steam: st, Games: store.NewGameRepo(db), Sessions: store.NewSessionRepo(db),
		Links: store.NewLinkRepo(db), Tasks: task.NewMySQLQueue(db),
		Now: func() time.Time { return now },
	})
}

// 新购入的游戏应被写入游戏库。
func TestReconciler_AddsNewlyPurchasedGames(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	st := &ownedStub{games: []steam.OwnedGame{
		{AppID: 440, Name: "TF2", PlaytimeForeverMin: 100},
		{AppID: 730, Name: "CS", PlaytimeForeverMin: 0},
	}}

	r := newReconciler(t, st, db, now)
	require.NoError(t, r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1}))

	rows, err := store.NewGameRepo(db).ListUserGames(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

// 核心兜底行为：时长增长了但当天没有实测会话（短会话、隐身游玩、
// 探针宕机窗口）→ 补一条推断会话，并明确标记来源。
func TestReconciler_BackfillsMissedSession(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)
	lastPlayed := time.Date(2026, 8, 25, 22, 30, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))

	st := &ownedStub{games: []steam.OwnedGame{{
		AppID: 440, Name: "TF2", PlaytimeForeverMin: 118,
		RtimeLastPlayed: lastPlayed,
	}}}

	r := newReconciler(t, st, db, now)
	require.NoError(t, r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1}))

	var sess store.PlaySession
	require.NoError(t, db.Take(&sess).Error)
	require.Equal(t, store.SourceReconcile, sess.Source,
		"推断出的会话必须标记来源，不能伪装成实测数据")
	require.Equal(t, uint32(18), sess.DurationMin)
	require.Equal(t, lastPlayed.Unix(), sess.EndedAt.Unix(),
		"应以 rtime_last_played 作为时间锚点")
	require.Equal(t, lastPlayed.Add(-18*time.Minute).Unix(), sess.StartedAt.Unix())
}

// 已被探针实测捕获的游玩不得重复补录。
func TestReconciler_SkipsWhenProbeSessionExists(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)
	lastPlayed := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	sessions := store.NewSessionRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))

	// 同一天已有实测会话
	_, err := sessions.Insert(ctx, store.PlaySession{
		SteamID: 1, AppID: 440,
		StartedAt: lastPlayed.Add(-30 * time.Minute), EndedAt: lastPlayed,
		DurationMin: 30, Source: store.SourceProbe, CreatedAt: now,
	})
	require.NoError(t, err)

	st := &ownedStub{games: []steam.OwnedGame{{
		AppID: 440, PlaytimeForeverMin: 130, RtimeLastPlayed: lastPlayed,
	}}}

	r := newReconciler(t, st, db, now)
	require.NoError(t, r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1}))

	var n int64
	require.NoError(t, db.Model(&store.PlaySession{}).
		Where("source = ?", store.SourceReconcile).Count(&n).Error)
	require.Zero(t, n, "当天已有实测会话时不应补录推断会话")
}

// 连续 3 次探测到私密 → 降级并停止重试（设计文档 §8.3）。
func TestReconciler_PrivateStrikesDegradeUser(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))

	st := &ownedStub{err: steam.ErrProfilePrivate}
	r := newReconciler(t, st, db, now)

	for i := 0; i < 2; i++ {
		err := r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1})
		require.Error(t, err)
		require.NotErrorIs(t, err, task.ErrPermanent, "前两次应可重试")
	}

	err := r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1})
	require.ErrorIs(t, err, task.ErrPermanent, "第三次应停止重试")

	link, err := links.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityGameDetailsPrivate, link.VisibilityState)
}

// 探测成功后 strike 计数清零。
func TestReconciler_SuccessResetsStrikes(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))
	_, err := links.BumpPrivateStrikes(ctx, 1)
	require.NoError(t, err)

	st := &ownedStub{games: []steam.OwnedGame{{AppID: 440, Name: "TF2"}}}
	r := newReconciler(t, st, db, now)
	require.NoError(t, r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1}))

	link, err := links.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Equal(t, int8(0), link.PrivateStrikes)
	require.Equal(t, store.VisibilityOK, link.VisibilityState)
}

// ScheduleDaily 为所有活跃用户入队，已解绑的排除在外。
func TestReconciler_ScheduleDailyCoversActiveUsers(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))
	require.NoError(t, links.Link(ctx, 1002, 2))
	require.NoError(t, links.Unlink(ctx, 1002))

	r := newReconciler(t, &ownedStub{}, db, now)
	require.NoError(t, r.ScheduleDaily(ctx))

	var rows []store.SyncTask
	require.NoError(t, db.Where("task_type = ?", task.TypeLibrarySync).Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, uint64(1), rows[0].SteamID)
}

// worker 重启会再次调用 ScheduleDaily。刚校准过的用户不得被重新排队 ——
// 否则每次重启都触发一轮全量 GetOwnedGames，1000 用户就烧掉 L3 一整天的预算。
func TestReconciler_ScheduleDailySkipsRecentlyVerified(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))
	// 模拟刚刚成功校准过（UpdateVisibility 会写入 last_verified_at）
	require.NoError(t, links.UpdateVisibility(ctx, 1, store.VisibilityOK))

	r := newReconciler(t, &ownedStub{}, db, now)
	require.NoError(t, r.ScheduleDaily(ctx))

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).
		Where("task_type = ?", task.TypeLibrarySync).Count(&n).Error)
	require.Zero(t, n, "刚校准过的用户不应被重新排队")
}

// 反过来，超过间隔未校准的用户必须被捞回来 ——
// 这同时覆盖了「worker 停机一整天，那天的校准要能补上」的场景。
func TestReconciler_ScheduleDailyPicksUpOverdueUsers(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))
	require.NoError(t, links.UpdateVisibility(ctx, 1, store.VisibilityOK))
	require.NoError(t, db.Model(&store.SteamLink{}).Where("steam_id64 = ?", 1).
		Update("last_verified_at", now.Add(-30*time.Hour)).Error)

	r := newReconciler(t, &ownedStub{}, db, now)
	require.NoError(t, r.ScheduleDaily(ctx))

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).
		Where("task_type = ?", task.TypeLibrarySync).Count(&n).Error)
	require.Equal(t, int64(1), n)
}
