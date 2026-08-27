package steam

import (
	"context"
	"time"
)

type PlayerSummary struct {
	SteamID                  uint64
	PersonaName              string
	Avatar                   string
	CommunityVisibilityState int    // 1=非公开 3=公开
	PersonaState             int    // 0=离线 1=在线 ...
	GameID                   uint32 // 0 表示当前不在玩游戏
	GameExtraInfo            string
}

type OwnedGame struct {
	AppID              uint32
	Name               string
	ImgIconURL         string
	PlaytimeForeverMin uint32
	Playtime2WeeksMin  uint32
	RtimeLastPlayed    time.Time // 零值表示从未游玩
}

type PlayerAchievement struct {
	APIName    string
	Achieved   bool
	UnlockTime time.Time // 仅 Achieved 为 true 时有意义
}

type SchemaAchievement struct {
	APIName     string
	DisplayName string
	Description string
	Icon        string
	IconGray    string
	Hidden      bool
}

type GameSchema struct {
	AppID        uint32
	Name         string
	Achievements []SchemaAchievement
}

// Client 是访问 Steam 的唯一抽象。其他包不得自行构造 Steam 请求。
type Client interface {
	GetPlayerSummaries(ctx context.Context, ids []uint64) ([]PlayerSummary, error)
	GetOwnedGames(ctx context.Context, id uint64) ([]OwnedGame, error)
	GetRecentlyPlayedGames(ctx context.Context, id uint64) ([]OwnedGame, error)
	GetPlayerAchievements(ctx context.Context, id uint64, appID uint32) ([]PlayerAchievement, error)
	GetSchemaForGame(ctx context.Context, appID uint32) (GameSchema, error)
	GetGlobalAchievementPercentages(ctx context.Context, appID uint32) (map[string]float64, error)
}
