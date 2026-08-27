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

// UpsertAchievementSchema 写入某款游戏的成就定义。
// 这张表不带用户维度：1000 个用户共玩 5000 款游戏，成就定义只需拉 5000 次。
func (r *GameRepo) UpsertAchievementSchema(ctx context.Context, appID uint32,
	achs []steam.SchemaAchievement, now time.Time) error {

	if len(achs) == 0 {
		return nil
	}

	rows := make([]AppAchievement, 0, len(achs))
	for _, a := range achs {
		var hidden int8
		if a.Hidden {
			hidden = 1
		}
		rows = append(rows, AppAchievement{
			AppID:       appID,
			APIName:     a.APIName,
			DisplayName: a.DisplayName,
			Description: a.Description,
			Icon:        a.Icon,
			IconGray:    a.IconGray,
			Hidden:      hidden,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "appid"}, {Name: "api_name"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"display_name", "description", "icon", "icon_gray", "hidden", "updated_at",
		}),
	}).CreateInBatches(&rows, upsertBatchSize).Error
}

// MarkAppAchievements 标记某款游戏是否有成就系统及成就总数。
//
// 必须是 upsert：新购游戏可能在 L3 校准把它写进 apps 表之前，
// 就被 L1 结算触发了成就同步。此时纯 UPDATE 影响 0 行且不报错，
// has_achievements 永远停留在「无行 → -1」，于是每次游玩都重新
// 入队一次 SchemaSync，每次白烧一回 GetSchemaForGame —— 正是
// 设计 §6.5 警告的「陷入死循环并持续消耗配额」。
func (r *GameRepo) MarkAppAchievements(ctx context.Context, appID uint32,
	has int8, total uint16, now time.Time) error {

	row := App{
		AppID:           appID,
		HasAchievements: has,
		AchTotal:        total,
		SchemaSyncedAt:  &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "appid"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"has_achievements", "ach_total", "schema_synced_at", "updated_at",
		}),
	}).Create(&row).Error
}

func (r *GameRepo) SchemaAchievementCount(ctx context.Context, appID uint32) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&AppAchievement{}).
		Where("appid = ?", appID).Count(&n).Error
	return n, err
}

// UpdateGlobalPercentages 批量回填成就的全球解锁率。
// 只更新已存在的定义行，不新增 —— 定义由 UpsertAchievementSchema 负责。
func (r *GameRepo) UpdateGlobalPercentages(ctx context.Context, appID uint32,
	pcts map[string]float64, now time.Time) error {

	if len(pcts) == 0 {
		return nil
	}

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for name, pct := range pcts {
			if err := tx.Model(&AppAchievement{}).
				Where("appid = ? AND api_name = ?", appID, name).
				Updates(map[string]any{"global_pct": pct, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
