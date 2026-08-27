package store

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"steamlink/internal/domain"
)

type ProbeRepo struct{ db *gorm.DB }

func NewProbeRepo(db *gorm.DB) *ProbeRepo { return &ProbeRepo{db: db} }

// Ensure 在用户绑定时初始化探针状态。
// 使用 DO NOTHING 保证幂等 —— 重新绑定不得重置正在进行的会话。
func (r *ProbeRepo) Ensure(ctx context.Context, steamID uint64, now time.Time) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).
		Create(&ProbeState{
			SteamID:     steamID,
			Tier:        0, // 新用户按最高频率采集，后续由分层规则调整
			NextProbeAt: now,
			UpdatedAt:   now,
		}).Error
}

// Due 返回到期待探测的用户，按到期时刻升序。仅供测试与只读场景使用；
// 采集路径必须用 Claim，否则多 worker 会重复探测同一批用户。
func (r *ProbeRepo) Due(ctx context.Context, now time.Time, limit int) ([]ProbeState, error) {
	var out []ProbeState
	err := r.db.WithContext(ctx).
		Where("next_probe_at <= ?", now).
		Order("next_probe_at").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// Claim 领取到期待探测的用户，并立即把 next_probe_at 推到租约到期时刻。
//
// 与任务表同样的道理：worker 设计为可多实例水平扩展，裸 SELECT 会让
// 两个 worker 取到同一批用户 —— API 调用翻倍事小，两条 advanceOne
// 并发读改写同一行 probe_state 才是真问题：后写覆盖先写，去抖计数与
// LastSeenPlayingAt 都可能丢失，直接产出错误的会话记录。
//
// 租约到期后未被 Save 更新的行会被自动回收，因此 worker 崩溃不会
// 让用户永久停止探测。
func (r *ProbeRepo) Claim(ctx context.Context, now time.Time, limit int,
	lease time.Duration) ([]ProbeState, error) {

	var out []ProbeState

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []ProbeState
		if err := tx.Clauses(clause.Locking{
			Strength: "UPDATE",
			Options:  "SKIP LOCKED",
		}).
			Where("next_probe_at <= ?", now).
			Order("next_probe_at").
			Limit(limit).
			Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		ids := make([]uint64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.SteamID)
		}

		if err := tx.Model(&ProbeState{}).
			Where("steam_id64 IN ?", ids).
			Updates(map[string]any{
				"next_probe_at": now.Add(lease),
				"updated_at":    now,
			}).Error; err != nil {
			return err
		}

		out = rows
		return nil
	})

	return out, err
}

// Save 落库状态机推进后的新状态。
// Idle 时必须把 current_appid 等字段写回 NULL，不能残留旧值。
func (r *ProbeRepo) Save(ctx context.Context, steamID uint64, s domain.State,
	tier int8, nextProbeAt, now time.Time) error {

	appID, started, lastSeen, miss := FromDomain(s)

	return r.db.WithContext(ctx).Model(&ProbeState{}).
		Where("steam_id64 = ?", steamID).
		Updates(map[string]any{
			"current_appid":        appID,
			"session_started_at":   started,
			"last_seen_playing_at": lastSeen,
			"miss_count":           miss,
			"tier":                 tier,
			"last_probe_at":        now,
			"next_probe_at":        nextProbeAt,
			"updated_at":           now,
		}).Error
}

// Stale 返回 last_probe_at 早于 before 且仍标记为「在玩」的记录。
// worker 长时间宕机后，这些会话的时长已不可信，需要强制结算（设计文档 §9.4）。
func (r *ProbeRepo) Stale(ctx context.Context, before time.Time) ([]ProbeState, error) {
	var out []ProbeState
	err := r.db.WithContext(ctx).
		Where("current_appid IS NOT NULL AND last_probe_at < ?", before).
		Find(&out).Error
	return out, err
}

// ToDomain 把存储行转成状态机可用的纯值。
func ToDomain(p ProbeState) domain.State {
	var s domain.State
	if p.CurrentAppID != nil {
		s.AppID = *p.CurrentAppID
	}
	if p.SessionStartedAt != nil {
		s.StartedAt = *p.SessionStartedAt
	}
	if p.LastSeenPlayingAt != nil {
		s.LastSeenPlayingAt = *p.LastSeenPlayingAt
	}
	if p.MissCount > 0 {
		s.MissCount = uint8(p.MissCount)
	}
	return s
}

// FromDomain 把状态机的值拆成可写入的可空字段。
func FromDomain(s domain.State) (appID *uint32, started, lastSeen *time.Time, miss int8) {
	if s.AppID == 0 {
		return nil, nil, nil, 0
	}
	a, st, ls := s.AppID, s.StartedAt, s.LastSeenPlayingAt
	return &a, &st, &ls, int8(s.MissCount)
}
