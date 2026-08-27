package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/store"
)

// fakeSteam 让我们精确构造三种隐私状态。
type fakeSteam struct {
	steam.Client
	summaries []steam.PlayerSummary
	sumErr    error
	games     []steam.OwnedGame
	gamesErr  error
}

func (f *fakeSteam) GetPlayerSummaries(context.Context, []uint64) ([]steam.PlayerSummary, error) {
	return f.summaries, f.sumErr
}

func (f *fakeSteam) GetOwnedGames(context.Context, uint64) ([]steam.OwnedGame, error) {
	return f.games, f.gamesErr
}

func TestProbeVisibility_OK(t *testing.T) {
	f := &fakeSteam{
		summaries: []steam.PlayerSummary{{SteamID: 1, CommunityVisibilityState: 3}},
		games:     []steam.OwnedGame{{AppID: 620, Name: "Portal 2"}},
	}

	state, games, err := ProbeVisibility(context.Background(), f, 1)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityOK, state)
	require.Len(t, games, 1)
}

// 资料整体不公开。
func TestProbeVisibility_ProfilePrivate(t *testing.T) {
	f := &fakeSteam{
		summaries: []steam.PlayerSummary{{SteamID: 1, CommunityVisibilityState: 1}},
	}

	state, games, err := ProbeVisibility(context.Background(), f, 1)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityProfilePrivate, state)
	require.Empty(t, games, "资料私密时不应继续拉游戏库，省一次调用")
}

// 关键场景：资料公开但「游戏详情」单独设为不公开。
// 这是最容易被误判为「用户没有游戏」的情况。
func TestProbeVisibility_GameDetailsPrivate(t *testing.T) {
	f := &fakeSteam{
		summaries: []steam.PlayerSummary{{SteamID: 1, CommunityVisibilityState: 3}},
		gamesErr:  steam.ErrProfilePrivate,
	}

	state, games, err := ProbeVisibility(context.Background(), f, 1)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityGameDetailsPrivate, state)
	require.Empty(t, games)
}

// 账号真的存在但一款游戏都没有 —— 必须判定为正常，不是私密。
func TestProbeVisibility_PublicButEmptyLibrary(t *testing.T) {
	f := &fakeSteam{
		summaries: []steam.PlayerSummary{{SteamID: 1, CommunityVisibilityState: 3}},
		games:     []steam.OwnedGame{},
	}

	state, _, err := ProbeVisibility(context.Background(), f, 1)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityOK, state)
}

// SteamID 不存在时 Steam 返回空 players 数组。
func TestProbeVisibility_UnknownSteamID(t *testing.T) {
	f := &fakeSteam{summaries: []steam.PlayerSummary{}}

	_, _, err := ProbeVisibility(context.Background(), f, 1)
	require.ErrorIs(t, err, ErrSteamAccountNotFound)
}
