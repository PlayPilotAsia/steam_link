package domain

import "time"

// Tier 决定用户的探针频率。分层是让高频轮询在配额上可行的关键：
// 只有少数活跃用户需要分钟级探测，多数用户可以降到天级。
type Tier int8

const (
	TierActive  Tier = 0 // 24 小时内有游玩
	TierRecent  Tier = 1 // 7 天内
	TierDormant Tier = 2 // 30 天内
	TierAsleep  Tier = 3 // 超过 30 天，或从未游玩
)

// ClassifyTier 根据最后游玩时刻判定分层。
func ClassifyTier(lastPlayed, now time.Time) Tier {
	if lastPlayed.IsZero() {
		return TierAsleep
	}

	switch elapsed := now.Sub(lastPlayed); {
	case elapsed < 24*time.Hour:
		return TierActive
	case elapsed < 7*24*time.Hour:
		return TierRecent
	case elapsed < 30*24*time.Hour:
		return TierDormant
	default:
		return TierAsleep
	}
}

// ProbeInterval 返回该分层的探针间隔。
// 未知值退化到最保守的间隔 —— 返回零会导致对该用户忙轮询，瞬间打满配额。
func ProbeInterval(t Tier) time.Duration {
	switch t {
	case TierActive:
		return 2 * time.Minute
	case TierRecent:
		return 15 * time.Minute
	case TierDormant:
		return 2 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// NextProbeAt 计算下次探测时刻。
//
// 正在游玩的用户一律按最高频率探测，与其 tier 无关：
// 会话正在进行时降频会让结束时刻严重失真，甚至让沉睡用户的
// 会话等上一整天才被结算。
func NextProbeAt(t Tier, playing bool, now time.Time) time.Time {
	if playing {
		return now.Add(ProbeInterval(TierActive))
	}
	return now.Add(ProbeInterval(t))
}
