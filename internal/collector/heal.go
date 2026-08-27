package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"steamlink/internal/domain"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

// StaleThreshold 是判定探针状态为「僵尸」的阈值。
// 超过它仍未被探测却标记为在玩，说明 worker 曾长时间宕机。
const StaleThreshold = time.Hour

type Healer struct {
	probes *store.ProbeRepo
	tasks  task.Queue
	now    func() time.Time
}

func NewHealer(probes *store.ProbeRepo, tasks task.Queue, now func() time.Time) *Healer {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Healer{probes: probes, tasks: tasks, now: now}
}

// Run 在 worker 启动时执行一次，强制结算僵尸会话。
//
// 这些会话跨越了 worker 的宕机窗口，实际结束时刻无从得知，
// 因此结算出的时长只能是推断值 —— 后续 L1 会用 Steam 的真实增量修正时长，
// 但起止时刻仍带有不确定性。
func (h *Healer) Run(ctx context.Context) error {
	now := h.now()

	stale, err := h.probes.Stale(ctx, now.Add(-StaleThreshold))
	if err != nil {
		return fmt.Errorf("collector: 查询僵尸会话失败: %w", err)
	}

	for _, row := range stale {
		state := store.ToDomain(row)

		payload, err := json.Marshal(task.SessionPayload{
			StartedAt: state.StartedAt,
			EndedAt:   state.LastSeenPlayingAt,
			// 这条会话跨越了 worker 的宕机窗口，真实结束时刻无从得知。
			// 标记为推断，不得以实测数据的身份进入永久事件流。
			Source: store.SourceReconcile,
		})
		if err != nil {
			return err
		}

		if err := h.tasks.Enqueue(ctx, task.Task{
			Type:      task.TypeSessionSettle,
			SteamID:   row.SteamID,
			AppID:     state.AppID,
			Payload:   payload,
			Priority:  task.PriorityNormal,
			NextRunAt: now,
		}); err != nil {
			return fmt.Errorf("collector: 入队僵尸会话结算失败: %w", err)
		}

		// 重置为 Idle，让下一轮探针从干净状态重新开始
		if err := h.probes.Save(ctx, row.SteamID, domain.State{},
			row.Tier, now, now); err != nil {
			return fmt.Errorf("collector: 重置探针状态失败: %w", err)
		}
	}
	return nil
}
