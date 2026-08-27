package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"steamlink/internal/steam"
)

// upsertBatchSize 控制单条 INSERT 的行数。一个用户的游戏库可达数千款，
// 必须批量写入 —— 逐条 INSERT 会产生数千次数据库往返。
const upsertBatchSize = 200

type GameRepo struct{ db *gorm.DB }

func NewGameRepo(db *gorm.DB) *GameRepo { return &GameRepo{db: db} }

// UpsertApps 写入全局游戏元数据。这些数据跨用户共享，不带用户维度。
// 注意不要覆盖 has_achievements 与 ach_total —— 它们由 Schema 同步任务维护。
func (r *GameRepo) UpsertApps(ctx context.Context, games []steam.OwnedGame) error {
	if len(games) == 0 {
		return nil
	}
	now := time.Now().UTC()

	rows := make([]App, 0, len(games))
	for _, g := range games {
		rows = append(rows, App{
			AppID:           g.AppID,
			Name:            g.Name,
			ImgIconURL:      g.ImgIconURL,
			HasAchievements: -1, // 仅新建时生效，见下方 DoUpdates 列表
			CreatedAt:       now,
			UpdatedAt:       now,
		})
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "appid"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "img_icon_url", "updated_at"}),
	}).CreateInBatches(&rows, upsertBatchSize).Error
}

// UpsertUserGames 写入用户的游戏库快照。
// first_seen_at 在冲突时不更新，用于识别新购入的游戏。
func (r *GameRepo) UpsertUserGames(ctx context.Context, steamID uint64,
	games []steam.OwnedGame, now time.Time) error {

	if len(games) == 0 {
		return nil
	}

	rows := make([]UserGame, 0, len(games))
	for _, g := range games {
		var last *time.Time
		if !g.RtimeLastPlayed.IsZero() {
			t := g.RtimeLastPlayed
			last = &t
		}
		rows = append(rows, UserGame{
			SteamID:            steamID,
			AppID:              g.AppID,
			PlaytimeForeverMin: g.PlaytimeForeverMin,
			Playtime2WeeksMin:  g.Playtime2WeeksMin,
			RtimeLastPlayed:    last,
			FirstSeenAt:        now,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "steam_id64"}, {Name: "appid"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"playtime_forever_min", "playtime_2weeks_min",
			"rtime_last_played", "updated_at",
		}),
	}).CreateInBatches(&rows, upsertBatchSize).Error
}

func (r *GameRepo) ListUserGames(ctx context.Context, steamID uint64) ([]UserGame, error) {
	var out []UserGame
	err := r.db.WithContext(ctx).
		Where("steam_id64 = ?", steamID).
		Order("appid").
		Find(&out).Error
	return out, err
}

// PlaytimeMap 返回 appid → 库中已记录的累计分钟数，供差分计算使用。
func (r *GameRepo) PlaytimeMap(ctx context.Context, steamID uint64) (map[uint32]uint32, error) {
	type row struct {
		AppID uint32
		Mins  uint32
	}
	var rows []row
	err := r.db.WithContext(ctx).Model(&UserGame{}).
		Select("appid AS app_id, playtime_forever_min AS mins").
		Where("steam_id64 = ?", steamID).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	m := make(map[uint32]uint32, len(rows))
	for _, r := range rows {
		m[r.AppID] = r.Mins
	}
	return m, nil
}

// LibraryRow 是 user_games 联 apps 后的展示行。
type LibraryRow struct {
	AppID              uint32     `gorm:"column:appid"`
	Name               string     `gorm:"column:name"`
	ImgIconURL         string     `gorm:"column:img_icon_url"`
	PlaytimeForeverMin uint32     `gorm:"column:playtime_forever_min"`
	Playtime2WeeksMin  uint32     `gorm:"column:playtime_2weeks_min"`
	RtimeLastPlayed    *time.Time `gorm:"column:rtime_last_played"`
	AchUnlocked        uint16     `gorm:"column:ach_unlocked"`
	AchTotal           uint16     `gorm:"column:ach_total"`
}

// ListLibrary 返回按累计时长倒序的游戏库。
// 名称与图标存在全局的 apps 表中，此处联表取出。
func (r *GameRepo) ListLibrary(ctx context.Context, steamID uint64) ([]LibraryRow, error) {
	var rows []LibraryRow
	err := r.db.WithContext(ctx).
		Table("user_games AS ug").
		Select(`ug.appid, a.name, a.img_icon_url,
		        ug.playtime_forever_min, ug.playtime_2weeks_min,
		        ug.rtime_last_played, ug.ach_unlocked, ug.ach_total`).
		Joins("LEFT JOIN apps AS a ON a.appid = ug.appid").
		Where("ug.steam_id64 = ?", steamID).
		Order("ug.playtime_forever_min DESC, ug.appid").
		Scan(&rows).Error
	return rows, err
}

// HasAchievements 返回 apps.has_achievements：-1 未知、0 无成就、1 有成就。
// 游戏不存在时返回 -1。
func (r *GameRepo) HasAchievements(ctx context.Context, appID uint32) (int8, error) {
	var v int8
	err := r.db.WithContext(ctx).Model(&App{}).
		Select("has_achievements").
		Where("appid = ?", appID).
		Take(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return -1, nil
	}
	return v, err
}
