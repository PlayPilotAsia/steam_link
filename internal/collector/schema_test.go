package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

type schemaStub struct {
	steam.Client
	schema steam.GameSchema
	err    error
	calls  int
}

func (s *schemaStub) GetSchemaForGame(context.Context, uint32) (steam.GameSchema, error) {
	s.calls++
	return s.schema, s.err
}

func (s *schemaStub) GetGlobalAchievementPercentages(context.Context, uint32) (map[string]float64, error) {
	return map[string]float64{"A": 55.5}, nil
}

func TestSchemaSyncer_StoresDefinitionsAndMarksApp(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{
		AppID: 440, Name: "Team Fortress 2",
		Achievements: []steam.SchemaAchievement{
			{APIName: "TF_SCOUT_LONG_DISTANCE_RUNNER", DisplayName: "马拉松选手",
				Description: "累计跑动 25 公里", Icon: "a.jpg", IconGray: "a_gray.jpg"},
			{APIName: "TF_HIDDEN", DisplayName: "隐藏成就", Hidden: true},
		},
	}}

	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440}))

	var achs []store.AppAchievement
	require.NoError(t, db.Where("appid = ?", 440).Order("api_name").Find(&achs).Error)
	require.Len(t, achs, 2)
	require.Equal(t, "隐藏成就", achs[0].DisplayName)
	require.Equal(t, int8(1), achs[0].Hidden)
	require.Equal(t, "累计跑动 25 公里", achs[1].Description)

	var app store.App
	require.NoError(t, db.Where("appid = ?", 440).Take(&app).Error)
	require.Equal(t, int8(1), app.HasAchievements)
	require.Equal(t, uint16(2), app.AchTotal)
	require.NotNil(t, app.SchemaSyncedAt)
}

// 没有成就系统的游戏必须被永久标记，且任务算成功而非失败。
// 把它当失败重试会让这类游戏陷入死循环并持续消耗配额。
func TestSchemaSyncer_NoStatsMarksAppAndSucceeds(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{err: steam.ErrAppHasNoStats}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	err := s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440})
	require.ErrorIs(t, err, task.ErrPermanent, "应作为永久错误，由 runner 置为成功")

	has, err := games.HasAchievements(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int8(0), has, "必须永久标记为无成就")
}

// 返回空成就列表 = 该游戏确实没有成就，同样永久标记。
func TestSchemaSyncer_EmptyAchievementListMarksNoStats(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{AppID: 440, Name: "TF2"}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440}))

	has, err := games.HasAchievements(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int8(0), has)
}

// Schema 同步完成后，若任务携带了 SteamID，应接着入队该用户的成就下钻。
func TestSchemaSyncer_ChainsToAchievementSync(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{
		AppID:        440,
		Achievements: []steam.SchemaAchievement{{APIName: "A", DisplayName: "甲"}},
	}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{
		Type: task.TypeSchemaSync, SteamID: 1, AppID: 440}))

	var row store.SyncTask
	require.NoError(t, db.Where("task_type = ?", task.TypeAchievementSync).Take(&row).Error)
	require.Equal(t, uint64(1), row.SteamID)
	require.Equal(t, uint32(440), row.AppID)
}

// 重复同步幂等：定义被覆盖更新，不产生重复行。
func TestSchemaSyncer_RerunIsIdempotent(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{
		AppID:        440,
		Achievements: []steam.SchemaAchievement{{APIName: "A", DisplayName: "甲"}},
	}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	tk := task.Task{Type: task.TypeSchemaSync, AppID: 440}
	require.NoError(t, s.Handle(ctx, tk))
	require.NoError(t, s.Handle(ctx, tk))

	n, err := games.SchemaAchievementCount(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

// 全球解锁率应与成就定义一并落库。
func TestSchemaSyncer_StoresGlobalPercentages(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{
		AppID:        440,
		Achievements: []steam.SchemaAchievement{{APIName: "A", DisplayName: "甲"}},
	}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440}))

	var ach store.AppAchievement
	require.NoError(t, db.Where("appid = ? AND api_name = ?", 440, "A").Take(&ach).Error)
	require.InDelta(t, 55.5, ach.GlobalPct, 0.001)
}

// 解锁率拉取失败不得让整个 Schema 同步失败 ——
// 成就定义是主数据，稀有度只是锦上添花。
func TestSchemaSyncer_GlobalPercentFailureIsNonFatal(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &pctFailStub{schemaStub: schemaStub{schema: steam.GameSchema{
		AppID:        440,
		Achievements: []steam.SchemaAchievement{{APIName: "A", DisplayName: "甲"}},
	}}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440}),
		"解锁率失败不应影响成就定义同步")

	n, err := games.SchemaAchievementCount(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

type pctFailStub struct{ schemaStub }

func (s *pctFailStub) GetGlobalAchievementPercentages(context.Context, uint32) (map[string]float64, error) {
	return nil, errors.New("service unavailable")
}
