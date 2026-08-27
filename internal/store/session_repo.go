package store

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SessionRepo struct{ db *gorm.DB }

func NewSessionRepo(db *gorm.DB) *SessionRepo { return &SessionRepo{db: db} }

// Insert 写入一条游戏会话。
//
// play_sessions 是自增主键的追加表，本身不幂等，而租约回收必然导致任务重跑。
// uk_session(steam_id64, appid, started_at) 唯一键挡住重复写入，
// 此处把冲突翻译成 (false, nil) 而非错误 —— 重复不是失败，
// 若当作失败上报，任务会被无谓重试直到进入死信。
func (r *SessionRepo) Insert(ctx context.Context, s PlaySession) (bool, error) {
	// DoNothing 在 MySQL 上生成 ON DUPLICATE KEY UPDATE <pk>=<pk>，
	// 冲突时 RowsAffected 为 0，据此区分「插入了」与「已存在」。
	res := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).
		Create(&s)

	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// SetPlaytime 写入用户某款游戏的累计时长与最后游玩时刻。
//
// 必须是 upsert 而非纯 UPDATE。用户当天新购的游戏在 user_games 里还没有行
// （要等次日 L3 校准才 upsert 进来），纯 UPDATE 会影响 0 行且不报错，
// 导致下一次结算读到的 prev 仍是 0：
//
//	玩 30 分钟 → delta = 30-0 = 30，写会话，SetPlaytime 静默失败
//	再玩 20 分钟 → Steam 报 50，prev 仍是 0 → delta = 50，再写一条会话
//	50 分钟的真实游玩被记成 80 分钟，且两条会话 started_at 不同，
//	uk_session 也拦不住。
func (r *SessionRepo) SetPlaytime(ctx context.Context, steamID uint64, appID uint32,
	minutes uint32, lastPlayed *time.Time, now time.Time) error {

	row := UserGame{
		SteamID:            steamID,
		AppID:              appID,
		PlaytimeForeverMin: minutes,
		RtimeLastPlayed:    lastPlayed,
		FirstSeenAt:        now,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	updates := []string{"playtime_forever_min", "updated_at"}
	if lastPlayed != nil {
		updates = append(updates, "rtime_last_played")
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "steam_id64"}, {Name: "appid"}},
		DoUpdates: clause.AssignmentColumns(updates),
	}).Create(&row).Error
}

// HasSessionOn 判断指定自然日（UTC）是否已有该游戏的会话记录。
// L3 校准据此避免为已被探针捕获的游玩重复补录推断会话。
func (r *SessionRepo) HasSessionOn(ctx context.Context, steamID uint64,
	appID uint32, day time.Time) (bool, error) {

	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)

	var n int64
	err := r.db.WithContext(ctx).Model(&PlaySession{}).
		Where("steam_id64 = ? AND appid = ? AND started_at >= ? AND started_at < ?",
			steamID, appID, start, start.AddDate(0, 0, 1)).
		Count(&n).Error
	return n > 0, err
}

// UpsertUnlocks 批量写入成就解锁记录。
//
// 主键 (steam_id64, appid, api_name) 天然幂等，解锁时刻取自 Steam 的
// unlocktime。成就与时长的本质差异在此：成就自带精确时间戳，
// 不需要 diff 逻辑，重复同步无害。
func (r *SessionRepo) UpsertUnlocks(ctx context.Context, steamID uint64,
	appID uint32, unlocks []AchievementUnlock, now time.Time) error {

	if len(unlocks) == 0 {
		return nil
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "steam_id64"}, {Name: "appid"}, {Name: "api_name"}},
		DoNothing: true,
	}).CreateInBatches(&unlocks, 200).Error
}

func (r *SessionRepo) CountUnlocks(ctx context.Context, steamID uint64, appID uint32) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&AchievementUnlock{}).
		Where("steam_id64 = ? AND appid = ?", steamID, appID).Count(&n).Error
	return n, err
}

func (r *SessionRepo) ListUnlocks(ctx context.Context, steamID uint64,
	appID uint32) ([]AchievementUnlock, error) {

	var out []AchievementUnlock
	err := r.db.WithContext(ctx).
		Where("steam_id64 = ? AND appid = ?", steamID, appID).
		Order("unlocked_at DESC").
		Find(&out).Error
	return out, err
}

// UnlockRow 是成就时间线的展示行。
type UnlockRow struct {
	AppID       uint32    `gorm:"column:appid"`
	AppName     string    `gorm:"column:app_name"`
	APIName     string    `gorm:"column:api_name"`
	DisplayName string    `gorm:"column:display_name"`
	Icon        string    `gorm:"column:icon"`
	UnlockedAt  time.Time `gorm:"column:unlocked_at"`
}

// RecentUnlocks 返回最近解锁的成就时间线。
// unlocked_at 取自 Steam 的 unlocktime，是精确值而非推断值。
func (r *SessionRepo) RecentUnlocks(ctx context.Context, steamID uint64,
	limit int) ([]UnlockRow, error) {

	var out []UnlockRow
	err := r.db.WithContext(ctx).
		Table("achievement_unlocks AS u").
		Select(`u.appid, a.name AS app_name, u.api_name,
		        d.display_name, d.icon, u.unlocked_at`).
		Joins("LEFT JOIN apps AS a ON a.appid = u.appid").
		Joins("LEFT JOIN app_achievements AS d ON d.appid = u.appid AND d.api_name = u.api_name").
		Where("u.steam_id64 = ?", steamID).
		Order("u.unlocked_at DESC").
		Limit(limit).
		Scan(&out).Error
	return out, err
}
