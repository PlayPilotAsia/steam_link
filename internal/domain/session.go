// Package domain 存放不依赖任何 IO 的业务规则。
// 本包禁止导入 gorm、net/http，也禁止调用 time.Now —— 时钟一律由调用方传入。
// 这使得所有边界情况都能用表驱动测试穷举验证。
package domain

import "time"

// MissThreshold 是判定会话结束前允许的连续「观测不到」次数。
//
// 去抖不是优化，是必需的：Steam 的 GetPlayerSummaries 偶发返回不完整数据，
// 没有去抖时一次网络抖动就会把一局连续游戏切割成多段碎片会话。
const MissThreshold uint8 = 1

// MaxSessionDuration 是单条会话的时长上限，超过后强制结算并开启新会话。
const MaxSessionDuration = 24 * time.Hour

type EventKind int

const (
	SessionStarted EventKind = iota + 1
	SessionEnded
)

// State 是单个用户的会话状态。AppID 为 0 表示 Idle（当前不在玩）。
type State struct {
	AppID             uint32
	StartedAt         time.Time
	LastSeenPlayingAt time.Time
	MissCount         uint8
}

func (s State) isIdle() bool { return s.AppID == 0 }

// Probe 是一次探针观测结果。GameID 为 0 表示当前不在玩游戏。
//
// 重要：调用方绝不能在「探针请求失败」时构造 Probe{GameID: 0}。
// 请求失败与「用户没在玩」是两回事，前者应当直接跳过本轮、保持状态不变。
type Probe struct {
	GameID uint32
}

type Event struct {
	Kind      EventKind
	AppID     uint32
	StartedAt time.Time
	EndedAt   time.Time
}

// Advance 推进状态机一步，返回新状态与产出的事件。
// 纯函数：不修改入参，相同输入永远产生相同输出。
func Advance(prev State, p Probe, now time.Time) (State, []Event) {
	switch {
	case prev.isIdle() && p.GameID == 0:
		return prev, nil

	case prev.isIdle():
		return start(p.GameID, now), []Event{{
			Kind: SessionStarted, AppID: p.GameID, StartedAt: now,
		}}

	case p.GameID == prev.AppID:
		// 持续游玩。先检查是否超过时长上限，需要强制翻篇。
		if now.Sub(prev.StartedAt) > MaxSessionDuration {
			return start(prev.AppID, now), []Event{
				endEvent(prev),
				{Kind: SessionStarted, AppID: prev.AppID, StartedAt: now},
			}
		}
		next := prev
		next.LastSeenPlayingAt = now
		next.MissCount = 0
		return next, nil

	case p.GameID == 0 && prev.MissCount < MissThreshold:
		// 去抖：仅累加计数，不改动任何时刻，不产出事件
		next := prev
		next.MissCount++
		return next, nil

	case p.GameID == 0:
		return State{}, []Event{endEvent(prev)}

	default:
		// 切换到了另一款游戏
		return start(p.GameID, now), []Event{
			endEvent(prev),
			{Kind: SessionStarted, AppID: p.GameID, StartedAt: now},
		}
	}
}

func start(appID uint32, now time.Time) State {
	return State{AppID: appID, StartedAt: now, LastSeenPlayingAt: now, MissCount: 0}
}

// endEvent 用 LastSeenPlayingAt 而非当前时刻作为结束时刻 ——
// 当前时刻已经晚了一到两个探针周期，用它会把空档算进游玩时长。
func endEvent(s State) Event {
	return Event{
		Kind:      SessionEnded,
		AppID:     s.AppID,
		StartedAt: s.StartedAt,
		EndedAt:   s.LastSeenPlayingAt,
	}
}
