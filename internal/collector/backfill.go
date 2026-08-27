package collector

import (
	"context"
	"fmt"
	"time"

	"steamlink/internal/task"
)

// BackfillSpread 是全库回填任务铺开的时间窗。
// 一个拥有 500 款游戏的用户，任务会被摊在这个窗口内逐步执行，
// 而不是瞬间涌入限流器。
const BackfillSpread = 12 * time.Hour

// EnqueueBackfill 为用户的全部游戏入队成就同步任务。
//
// 优先级固定为 PriorityBackfill（最低）：新用户绑定会一次性产生数百条任务，
// 若与实时会话结算同级排队，会拖垮所有用户的实时性（设计文档 §6.8）。
func EnqueueBackfill(ctx context.Context, q task.Queue, steamID uint64,
	appIDs []uint32, now time.Time) error {

	if len(appIDs) == 0 {
		return nil
	}

	step := BackfillSpread / time.Duration(len(appIDs))
	if step < time.Second {
		step = time.Second
	}

	for i, appID := range appIDs {
		if err := q.Enqueue(ctx, task.Task{
			Type:      task.TypeAchievementSync,
			SteamID:   steamID,
			AppID:     appID,
			Priority:  task.PriorityBackfill,
			NextRunAt: now.Add(time.Duration(i) * step),
		}); err != nil {
			return fmt.Errorf("collector: 入队回填任务失败 (appid=%d): %w", appID, err)
		}
	}
	return nil
}
