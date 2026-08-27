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

// PrivateStrikeLimit（连续探测到私密后停止重试的阈值）定义在 enqueue.go，
// 与 handlePrivateStrike 放在一起。

// DailyReconcileJitter 把每日校准任务打散到一个时间窗内，
// 避免所有用户在同一秒集中触发。
const DailyReconcileJitter = 6 * time.Hour

type ReconcilerDeps struct {
	Steam    steam.Client
	Games    *store.GameRepo
	Sessions *store.SessionRepo
	Links    *store.LinkRepo
	Tasks    task.Queue
	Now      func() time.Time
}

type Reconciler struct{ d ReconcilerDeps }

func NewReconciler(d ReconcilerDeps) *Reconciler {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{d: d}
}

// Handle 处理 TypeLibrarySync 任务：同步游戏库并补录漏采的会话。
func (r *Reconciler) Handle(ctx context.Context, t task.Task) error {
	now := r.d.Now()

	owned, err := r.d.Steam.GetOwnedGames(ctx, t.SteamID)
	if errors.Is(err, steam.ErrProfilePrivate) {
		return handlePrivateStrike(ctx, r.d.Links, r.d.Steam, t.SteamID)
	}
	if err != nil {
		return fmt.Errorf("collector: 拉取游戏库失败: %w", err)
	}

	_ = r.d.Links.ResetPrivateStrikes(ctx, t.SteamID)
	_ = r.d.Links.UpdateVisibility(ctx, t.SteamID, store.VisibilityOK)

	// 先取旧快照，再写新快照 —— 顺序不能反，否则差值恒为 0
	known, err := r.d.Games.PlaytimeMap(ctx, t.SteamID)
	if err != nil {
		return fmt.Errorf("collector: 读取已记录时长失败: %w", err)
	}

	if err := r.d.Games.UpsertApps(ctx, owned); err != nil {
		return fmt.Errorf("collector: 写入游戏元数据失败: %w", err)
	}
	if err := r.d.Games.UpsertUserGames(ctx, t.SteamID, owned, now); err != nil {
		return fmt.Errorf("collector: 写入游戏库失败: %w", err)
	}

	for _, g := range owned {
		if err := r.reconcileGame(ctx, t.SteamID, g, known[g.AppID], now); err != nil {
			return err
		}
	}
	return nil
}

// reconcileGame 对单款游戏做差分补录。
func (r *Reconciler) reconcileGame(ctx context.Context, steamID uint64,
	g steam.OwnedGame, prevMin uint32, now time.Time) error {

	if g.PlaytimeForeverMin <= prevMin {
		return nil
	}
	delta := g.PlaytimeForeverMin - prevMin

	// rtime_last_played 是方案 C 的价值所在：它给推断出的会话
	// 一个可信的时间锚点，而不是笼统地归属到「某一天」。
	anchor := g.RtimeLastPlayed
	if anchor.IsZero() {
		anchor = now
	}

	has, err := r.d.Sessions.HasSessionOn(ctx, steamID, g.AppID, anchor)
	if err != nil {
		return fmt.Errorf("collector: 查询当日会话失败: %w", err)
	}
	if !has {
		if _, err := r.d.Sessions.Insert(ctx, store.PlaySession{
			SteamID:     steamID,
			AppID:       g.AppID,
			StartedAt:   anchor.Add(-time.Duration(delta) * time.Minute),
			EndedAt:     anchor,
			DurationMin: delta,
			Source:      store.SourceReconcile, // 明确标记为推断值
			CreatedAt:   now,
		}); err != nil {
			return fmt.Errorf("collector: 补录会话失败: %w", err)
		}
	}

	return enqueueAchievementSync(ctx, r.d.Games, r.d.Tasks, steamID, g.AppID, now)
}

// ReconcileInterval 是两次校准之间的最小间隔。
// 取 20 小时而非 24：留出余量，避免因执行耗时导致某天被跳过。
const ReconcileInterval = 20 * time.Hour

// ScheduleDaily 为「距上次成功校准超过 ReconcileInterval」的用户入队，
// 执行时刻打散到一个时间窗内。
//
// 必须按 last_verified_at 过滤，不能无条件全量入队。原因是 Enqueue 的
// LEAST(next_run_at, ?) 语义：已成功任务的 next_run_at 是过去的时刻，
// LEAST 会保留那个过去值，于是任务复活为 Pending 且立即到期。
// worker 每重启一次就会触发一轮全量 GetOwnedGames —— 1000 用户就是
// 1000 次调用，而 L3 一整天的预算才 1000 次。
//
// 按 last_verified_at 过滤还顺带解决了另一个问题：worker 停机一整天时，
// 那天漏掉的校准会在重启后自动补上。
func (r *Reconciler) ScheduleDaily(ctx context.Context) error {
	now := r.d.Now()

	ids, err := r.d.Links.StaleSteamIDs(ctx, now.Add(-ReconcileInterval))
	if err != nil {
		return fmt.Errorf("collector: 查询待校准用户失败: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}

	step := DailyReconcileJitter / time.Duration(len(ids))
	for i, id := range ids {
		if err := r.d.Tasks.Enqueue(ctx, task.Task{
			Type:      task.TypeLibrarySync,
			SteamID:   id,
			Priority:  task.PriorityNormal,
			NextRunAt: now.Add(time.Duration(i) * step),
		}); err != nil {
			return fmt.Errorf("collector: 入队每日校准失败: %w", err)
		}
	}
	return nil
}
