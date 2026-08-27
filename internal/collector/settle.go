package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

type SettlerDeps struct {
	Steam    steam.Client
	Games    *store.GameRepo
	Sessions *store.SessionRepo
	Tasks    task.Queue
	Now      func() time.Time
}

type Settler struct{ d SettlerDeps }

func NewSettler(d SettlerDeps) *Settler {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Settler{d: d}
}

// Handle 处理 TypeSessionSettle 任务。
//
// 探针给出的起止时刻最多有一个轮询周期的误差，因此这里只信任
// Steam 的 playtime_forever 增量作为时长，起始时刻由结束时刻反推。
func (s *Settler) Handle(ctx context.Context, t task.Task) error {
	now := s.d.Now()

	var payload task.SessionPayload
	if err := json.Unmarshal(t.Payload, &payload); err != nil {
		// payload 损坏，重试也不会好转
		return fmt.Errorf("collector: 解析会话载荷失败: %v: %w", err, task.ErrPermanent)
	}

	recent, err := s.d.Steam.GetRecentlyPlayedGames(ctx, t.SteamID)
	if errors.Is(err, steam.ErrProfilePrivate) {
		return fmt.Errorf("collector: 用户 %d 资料非公开: %w", t.SteamID, task.ErrPermanent)
	}
	if err != nil {
		return fmt.Errorf("collector: 拉取近期游玩失败: %w", err)
	}

	var current *steam.OwnedGame
	for i := range recent {
		if recent[i].AppID == t.AppID {
			current = &recent[i]
			break
		}
	}
	if current == nil {
		// 游戏不在近期列表中（时长过短未被 Steam 记录，或已超出两周窗口）。
		// 无法结算，但也不是失败。
		return nil
	}

	known, err := s.d.Games.PlaytimeMap(ctx, t.SteamID)
	if err != nil {
		return fmt.Errorf("collector: 读取已记录时长失败: %w", err)
	}

	prev := known[t.AppID]
	if current.PlaytimeForeverMin <= prev {
		// 时长没有增长：探针误判，或 Steam 尚未完成结算。不写会话。
		return nil
	}
	delta := current.PlaytimeForeverMin - prev

	// 时长取 Steam 的真实增量，起始时刻反推
	started := payload.EndedAt.Add(-time.Duration(delta) * time.Minute)

	// 来源由入队方决定，不在此硬编码：探针捕获的是实测数据，
	// 启动自愈补结的跨越了宕机窗口，起止时刻不可信，必须标记为推断
	//（设计文档 §3.2 —— 推断数据不得冒充实测数据）。
	source := payload.Source
	if source == 0 {
		source = store.SourceProbe
	}

	if _, err := s.d.Sessions.Insert(ctx, store.PlaySession{
		SteamID:     t.SteamID,
		AppID:       t.AppID,
		StartedAt:   started,
		EndedAt:     payload.EndedAt,
		DurationMin: delta,
		Source:      source,
		CreatedAt:   now,
	}); err != nil {
		return fmt.Errorf("collector: 写入会话失败: %w", err)
	}

	var lastPlayed *time.Time
	if !current.RtimeLastPlayed.IsZero() {
		lp := current.RtimeLastPlayed
		lastPlayed = &lp
	}
	if err := s.d.Sessions.SetPlaytime(ctx, t.SteamID, t.AppID,
		current.PlaytimeForeverMin, lastPlayed, now); err != nil {
		return fmt.Errorf("collector: 更新累计时长失败: %w", err)
	}

	return enqueueAchievementSync(ctx, s.d.Games, s.d.Tasks, t.SteamID, t.AppID, now)
}
