package api

import (
	"context"
	"errors"

	"steamlink/internal/steam"
	"steamlink/internal/store"
)

// ErrSteamAccountNotFound 表示该 SteamID 在 Steam 侧不存在。
var ErrSteamAccountNotFound = errors.New("api: steam account not found")

// ProbeVisibility 探测两个互相独立的隐私开关（见设计文档 §2.3），
// 返回可见性状态与已拉取到的游戏库（状态为 OK 时非空）。
//
// 顺利时只消耗 2 次 Steam 调用；资料私密时提前返回，只消耗 1 次。
func ProbeVisibility(ctx context.Context, c steam.Client, steamID uint64) (int8, []steam.OwnedGame, error) {
	sums, err := c.GetPlayerSummaries(ctx, []uint64{steamID})
	if err != nil {
		return store.VisibilityUnknown, nil, err
	}
	if len(sums) == 0 {
		return store.VisibilityUnknown, nil, ErrSteamAccountNotFound
	}

	// 开关一：个人资料公开性
	if sums[0].CommunityVisibilityState != 3 {
		return store.VisibilityProfilePrivate, nil, nil
	}

	// 开关二：游戏详情公开性。注意这里的 ErrProfilePrivate 语义是
	// 「游戏详情不公开」而非「整个资料不公开」—— 上面已经确认资料是公开的。
	games, err := c.GetOwnedGames(ctx, steamID)
	if errors.Is(err, steam.ErrProfilePrivate) {
		return store.VisibilityGameDetailsPrivate, nil, nil
	}
	if err != nil {
		return store.VisibilityUnknown, nil, err
	}

	return store.VisibilityOK, games, nil
}
