package store

import (
	"context"
	"testing"
	"time"

	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/stretchr/testify/require"
)

func sampleGames() []steam.OwnedGame {
	return []steam.OwnedGame{
		{AppID: 620, Name: "Portal 2 🧪", ImgIconURL: "abc",
			PlaytimeForeverMin: 100, Playtime2WeeksMin: 30,
			RtimeLastPlayed: time.Unix(1756180800, 0).UTC()},
		{AppID: 730, Name: "反恐精英 ⚡", ImgIconURL: "def",
			PlaytimeForeverMin: 5000},
	}
}

// emoji 必须能完整往返 —— 这验证了 utf8mb4 链路端到端可用。
func TestGameRepo_UpsertPreservesEmoji(t *testing.T) {
	r := NewGameRepo(testDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, r.UpsertApps(ctx, sampleGames()))
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, sampleGames(), now))

	got, err := r.ListUserGames(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Len(t, got, 2)

	var apps []App
	require.NoError(t, testDBHandle(t).Order("appid").Find(&apps).Error)
	require.Equal(t, "Portal 2 🧪", apps[0].Name)
	require.Equal(t, "反恐精英 ⚡", apps[1].Name)
}

// 重复 upsert 只更新不新增，且 first_seen_at 保持首次值。
func TestGameRepo_UpsertIsIdempotent(t *testing.T) {
	db := testDB(t)
	r := NewGameRepo(db)
	ctx := context.Background()

	first := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.UpsertApps(ctx, sampleGames()))
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, sampleGames(), first))

	updated := sampleGames()
	updated[0].PlaytimeForeverMin = 999
	later := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, updated, later))

	got, err := r.ListUserGames(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Len(t, got, 2, "重复 upsert 不应新增行")

	require.Equal(t, uint32(999), got[0].PlaytimeForeverMin, "时长应被更新")
	require.Equal(t, first.Unix(), got[0].FirstSeenAt.Unix(),
		"first_seen_at 必须保持首次入库时间，否则无法识别新购入的游戏")
}

func TestGameRepo_PlaytimeMap(t *testing.T) {
	r := NewGameRepo(testDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, r.UpsertApps(ctx, sampleGames()))
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, sampleGames(), now))

	m, err := r.PlaytimeMap(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Equal(t, map[uint32]uint32{620: 100, 730: 5000}, m)
}

// 游戏库列表需要联 apps 表拿名称与图标 —— user_games 本身不存这些。
func TestGameRepo_ListLibraryJoinsAppMetadata(t *testing.T) {
	r := NewGameRepo(testDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, r.UpsertApps(ctx, sampleGames()))
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, sampleGames(), now))

	rows, err := r.ListLibrary(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// 按累计时长倒序：730 有 5000 分钟，620 有 100 分钟
	require.Equal(t, uint32(730), rows[0].AppID)
	require.Equal(t, "反恐精英 ⚡", rows[0].Name)
	require.Equal(t, uint32(5000), rows[0].PlaytimeForeverMin)

	require.Equal(t, uint32(620), rows[1].AppID)
	require.Equal(t, "abc", rows[1].ImgIconURL)
	require.NotNil(t, rows[1].RtimeLastPlayed)
}
