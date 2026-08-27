package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

type SchemaDeps struct {
	Steam steam.Client
	Games *store.GameRepo
	Tasks task.Queue
	Now   func() time.Time
}

type SchemaSyncer struct{ d SchemaDeps }

func NewSchemaSyncer(d SchemaDeps) *SchemaSyncer {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &SchemaSyncer{d: d}
}

// Handle 处理 TypeSchemaSync 任务：拉取并存储某款游戏的成就定义。
//
// 任务可携带 SteamID：若携带，同步完成后会接着入队该用户的成就下钻，
// 形成「发现新游戏 → 拉定义 → 拉用户解锁状态」的链条。
func (s *SchemaSyncer) Handle(ctx context.Context, t task.Task) error {
	now := s.d.Now()

	schema, err := s.d.Steam.GetSchemaForGame(ctx, t.AppID)

	// 该游戏没有成就系统。这不是失败 —— 重试永远不会成功，
	// 必须永久标记并让任务算作完成，否则会持续消耗配额。
	if errors.Is(err, steam.ErrAppHasNoStats) {
		if e := s.d.Games.MarkAppAchievements(ctx, t.AppID, 0, 0, now); e != nil {
			return fmt.Errorf("collector: 标记无成就失败: %w", e)
		}
		return fmt.Errorf("collector: 游戏 %d 无成就系统: %w", t.AppID, task.ErrPermanent)
	}
	if err != nil {
		return fmt.Errorf("collector: 拉取成就定义失败: %w", err)
	}

	// 返回空列表同样意味着该游戏没有成就
	if len(schema.Achievements) == 0 {
		return s.d.Games.MarkAppAchievements(ctx, t.AppID, 0, 0, now)
	}

	if err := s.d.Games.UpsertAchievementSchema(ctx, t.AppID, schema.Achievements, now); err != nil {
		return fmt.Errorf("collector: 写入成就定义失败: %w", err)
	}
	if err := s.d.Games.MarkAppAchievements(ctx, t.AppID, 1,
		uint16(len(schema.Achievements)), now); err != nil {
		return fmt.Errorf("collector: 标记有成就失败: %w", err)
	}

	// 全球解锁率是展示用的附加数据，拉取失败不影响主流程：
	// 成就定义才是主数据，不能因为稀有度拿不到就让整个任务重试。
	if pcts, err := s.d.Steam.GetGlobalAchievementPercentages(ctx, t.AppID); err == nil {
		if e := s.d.Games.UpdateGlobalPercentages(ctx, t.AppID, pcts, now); e != nil {
			return fmt.Errorf("collector: 写入全球解锁率失败: %w", e)
		}
	}

	if t.SteamID == 0 {
		return nil
	}
	return s.d.Tasks.Enqueue(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: t.SteamID, AppID: t.AppID,
		Priority: t.Priority, NextRunAt: now,
	})
}
