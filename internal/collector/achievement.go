package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type AchievementDeps struct {
	Steam    steam.Client
	Games    *store.GameRepo
	Sessions *store.SessionRepo
	Links    *store.LinkRepo
	Tasks    task.Queue
	Now      func() time.Time
}

type AchievementSyncer struct{ d AchievementDeps }

func NewAchievementSyncer(d AchievementDeps) *AchievementSyncer {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &AchievementSyncer{d: d}
}

// Handle 处理 TypeAchievementSync 任务。
//
// 本函数的核心是对三类错误的严格区分（设计文档 §6.5）：
//
//	ErrAppHasNoStats  → 游戏级问题，永久标记 apps.has_achievements=0，任务算成功
//	ErrProfilePrivate → 用户级问题，累加 strike，绝不能污染全局的 app 标记
//	其他错误          → 真故障，退避重试
//
// 混淆前两者的后果最严重：把用户的隐私问题标记到 app 上，
// 会让所有用户都不再同步这款游戏的成就。
func (a *AchievementSyncer) Handle(ctx context.Context, t task.Task) error {
	now := a.d.Now()

	// Schema 是成就展示的前提，缺失时先补齐
	total, err := a.d.Games.SchemaAchievementCount(ctx, t.AppID)
	if err != nil {
		return fmt.Errorf("collector: 查询成就定义数失败: %w", err)
	}
	if total == 0 {
		return a.d.Tasks.Enqueue(ctx, task.Task{
			Type: task.TypeSchemaSync, SteamID: t.SteamID, AppID: t.AppID,
			Priority: t.Priority, NextRunAt: now,
		})
	}

	achs, err := a.d.Steam.GetPlayerAchievements(ctx, t.SteamID, t.AppID)

	switch {
	case errors.Is(err, steam.ErrAppHasNoStats):
		// 游戏级：永久标记，所有用户都不必再试
		if e := a.d.Games.MarkAppAchievements(ctx, t.AppID, 0, 0, now); e != nil {
			return fmt.Errorf("collector: 标记无成就失败: %w", e)
		}
		return fmt.Errorf("collector: 游戏 %d 无成就系统: %w", t.AppID, task.ErrPermanent)

	case errors.Is(err, steam.ErrProfilePrivate):
		// 用户级：只影响这一个用户，不得触碰 apps 表
		return handlePrivateStrike(ctx, a.d.Links, a.d.Steam, t.SteamID)

	case err != nil:
		return fmt.Errorf("collector: 拉取玩家成就失败: %w", err)
	}

	_ = a.d.Links.ResetPrivateStrikes(ctx, t.SteamID)

	rows := make([]store.AchievementUnlock, 0, len(achs))
	for _, ach := range achs {
		if !ach.Achieved {
			continue
		}
		unlocked := ach.UnlockTime
		if unlocked.IsZero() {
			// 极少数老游戏解锁时刻为 0，退化为当前时刻
			unlocked = now
		}
		rows = append(rows, store.AchievementUnlock{
			SteamID:    t.SteamID,
			AppID:      t.AppID,
			APIName:    ach.APIName,
			UnlockedAt: unlocked,
			CreatedAt:  now,
		})
	}

	if err := a.d.Sessions.UpsertUnlocks(ctx, t.SteamID, t.AppID, rows, now); err != nil {
		return fmt.Errorf("collector: 写入成就解锁失败: %w", err)
	}

	return a.d.Games.SetAchievementProgress(ctx, t.SteamID, t.AppID,
		uint16(len(rows)), uint16(total), now)
}
