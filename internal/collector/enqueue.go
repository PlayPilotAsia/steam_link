package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

// PrivateStrikeLimit 是连续探测到非公开多少次后放弃该用户（设计文档 §8.3）。
const PrivateStrikeLimit int8 = 3

// enqueueAchievementSync 为某用户的某款游戏入队成就同步。
//
// 只在确认无成就时短路。其余情况一律入队 TypeAchievementSync 而非
// TypeSchemaSync —— 后者若不带 SteamID，SchemaSyncer 拉完定义就到此为止
// （它靠 t.SteamID == 0 判断是否要链回成就同步），用户的解锁状态永远拉不到。
//
// AchievementSyncer 自身已内建「schema 缺失 → 带 SteamID 入队 SchemaSync →
// SchemaSync 链回 AchievementSync」的完整路径，把入口收敛到它这里最可靠。
func enqueueAchievementSync(ctx context.Context, games *store.GameRepo,
	tasks task.Queue, steamID uint64, appID uint32, now time.Time) error {

	has, err := games.HasAchievements(ctx, appID)
	if err != nil {
		return fmt.Errorf("collector: 查询成就标记失败: %w", err)
	}
	if has == 0 {
		return nil // 已确认该游戏没有成就系统
	}

	return tasks.Enqueue(ctx, task.Task{
		Type:      task.TypeAchievementSync,
		SteamID:   steamID,
		AppID:     appID,
		Priority:  task.PriorityNormal,
		NextRunAt: now,
	})
}

// handlePrivateStrike 处理「探测到用户资料非公开」。
//
// Reconciler 与 AchievementSyncer 都会遇到这个分支，逻辑必须一致，
// 因此收敛到此处。达到阈值前返回可重试错误，达到阈值后返回永久错误
// 并落库精确的可见性状态。
func handlePrivateStrike(ctx context.Context, links *store.LinkRepo,
	sc steam.Client, steamID uint64) error {

	n, err := links.BumpPrivateStrikes(ctx, steamID)
	if err != nil {
		return fmt.Errorf("collector: 累加私密计数失败: %w", err)
	}

	if n < PrivateStrikeLimit {
		return fmt.Errorf("collector: 用户 %d 资料非公开（第 %d 次）", steamID, n)
	}

	// 达到阈值才做一次精确探测：GetOwnedGames 对「整个资料私密」和
	// 「仅游戏详情私密」返回完全相同的空对象，无法区分。若一律写成
	// GameDetailsPrivate，前端会给出错误的引导文案 —— 用户照着改也修不好。
	// 这次额外调用只在用户确实持续私密时发生，成本可控。
	state := store.VisibilityGameDetailsPrivate
	if sums, e := sc.GetPlayerSummaries(ctx, []uint64{steamID}); e == nil &&
		len(sums) > 0 && sums[0].CommunityVisibilityState != 3 {
		state = store.VisibilityProfilePrivate
	}

	if err := links.UpdateVisibility(ctx, steamID, state); err != nil {
		return err
	}
	return fmt.Errorf("collector: 用户 %d 连续 %d 次探测到非公开: %w",
		steamID, n, task.ErrPermanent)
}
