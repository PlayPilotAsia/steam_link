package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type achStub struct {
	steam.Client
	achs []steam.PlayerAchievement
	err  error
}

func (s *achStub) GetPlayerAchievements(context.Context, uint64, uint32) ([]steam.PlayerAchievement, error) {
	return s.achs, s.err
}

// 达到 strike 阈值时 handlePrivateStrike 会做一次精确探测，
// 让它失败以回退到「仅游戏详情私密」。
func (s *achStub) GetPlayerSummaries(context.Context, []uint64) ([]steam.PlayerSummary, error) {
	return nil, errors.New("stub: 不提供 summaries")
}

func newAchSyncer(t *testing.T, st steam.Client, db *gorm.DB, now time.Time) *AchievementSyncer {
	t.Helper()
	return NewAchievementSyncer(AchievementDeps{
		Steam: st, Games: store.NewGameRepo(db), Sessions: store.NewSessionRepo(db),
		Links: store.NewLinkRepo(db), Tasks: task.NewMySQLQueue(db),
		Now: func() time.Time { return now },
	})
}

func seedAppWithAchievements(t *testing.T, db *gorm.DB, now time.Time) {
	t.Helper()
	ctx := context.Background()
	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))
	require.NoError(t, games.UpsertAchievementSchema(ctx, 440, []steam.SchemaAchievement{
		{APIName: "A", DisplayName: "甲"},
		{APIName: "B", DisplayName: "乙"},
		{APIName: "C", DisplayName: "丙"},
	}, now))
	require.NoError(t, games.MarkAppAchievements(ctx, 440, 1, 3, now))
}

// 解锁时刻直接取 Steam 的 unlocktime —— 成就自带精确时间戳，
// 不需要像时长那样靠采样差分推断。
func TestAchievementSyncer_StoresUnlocksWithSteamTimestamp(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	unlockA := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	st := &achStub{achs: []steam.PlayerAchievement{
		{APIName: "A", Achieved: true, UnlockTime: unlockA},
		{APIName: "B", Achieved: false},
		{APIName: "C", Achieved: true, UnlockTime: now.Add(-time.Hour)},
	}}

	s := newAchSyncer(t, st, db, now)
	require.NoError(t, s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440}))

	var rows []store.AchievementUnlock
	require.NoError(t, db.Where("steam_id64 = ?", uint64(1)).
		Order("api_name").Find(&rows).Error)
	require.Len(t, rows, 2, "只写入已解锁的成就")
	require.Equal(t, "A", rows[0].APIName)
	require.Equal(t, unlockA.Unix(), rows[0].UnlockedAt.Unix())

	// 进度必须回写到 user_games，供列表页展示
	games, err := store.NewGameRepo(db).ListUserGames(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, uint16(2), games[0].AchUnlocked)
	require.Equal(t, uint16(3), games[0].AchTotal)
	require.NotNil(t, games[0].AchSyncedAt)
}

// 重复同步幂等：主键冲突静默跳过，不产生重复记录。
func TestAchievementSyncer_RerunIsIdempotent(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	st := &achStub{achs: []steam.PlayerAchievement{
		{APIName: "A", Achieved: true, UnlockTime: now.Add(-time.Hour)},
	}}

	s := newAchSyncer(t, st, db, now)
	tk := task.Task{Type: task.TypeAchievementSync, SteamID: 1, AppID: 440}
	require.NoError(t, s.Handle(ctx, tk))
	require.NoError(t, s.Handle(ctx, tk))

	n, err := store.NewSessionRepo(db).CountUnlocks(ctx, 1, 440)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

// 三类错误必须区分处理之一：该游戏没有成就 → 永久标记，任务算成功。
func TestAchievementSyncer_NoStatsMarksAppPermanently(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	st := &achStub{err: steam.ErrAppHasNoStats}
	s := newAchSyncer(t, st, db, now)

	err := s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440})
	require.ErrorIs(t, err, task.ErrPermanent)

	has, err := store.NewGameRepo(db).HasAchievements(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int8(0), has, "无成就的游戏必须永久标记，否则每次游玩都会重复调用")
}

// 三类错误之二：隐私墙 → 累加 strike，达阈值后停止重试。
// 注意它绝不能被误当成「该游戏没有成就」而永久标记 app —— 那会影响所有用户。
func TestAchievementSyncer_ProfilePrivateDoesNotMarkApp(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))

	st := &achStub{err: steam.ErrProfilePrivate}
	s := newAchSyncer(t, st, db, now)

	err := s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440})
	require.Error(t, err)

	has, err := store.NewGameRepo(db).HasAchievements(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int8(1), has, "用户隐私问题不得污染全局的游戏成就标记")
}

// 三类错误之三：网络故障 → 普通错误，走退避重试。
func TestAchievementSyncer_TransientErrorIsRetryable(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	st := &achStub{err: errors.New("connection reset by peer")}
	s := newAchSyncer(t, st, db, now)

	err := s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440})
	require.Error(t, err)
	require.NotErrorIs(t, err, task.ErrPermanent, "网络故障必须可重试")
}

// Schema 尚未同步时，应先入队 Schema 任务再处理成就。
func TestAchievementSyncer_RequiresSchemaFirst(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &achStub{}
	s := newAchSyncer(t, st, db, now)
	require.NoError(t, s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440}))

	var row store.SyncTask
	require.NoError(t, db.Where("task_type = ?", task.TypeSchemaSync).Take(&row).Error)
	require.Equal(t, uint32(440), row.AppID)
}
