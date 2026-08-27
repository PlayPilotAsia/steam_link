package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyTier(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		lastPlayed time.Time
		want       Tier
	}{
		{"1 小时前游玩 → 活跃", now.Add(-time.Hour), TierActive},
		{"23 小时前 → 活跃", now.Add(-23 * time.Hour), TierActive},
		{"3 天前 → 近期", now.AddDate(0, 0, -3), TierRecent},
		{"6 天前 → 近期", now.AddDate(0, 0, -6), TierRecent},
		{"20 天前 → 休眠", now.AddDate(0, 0, -20), TierDormant},
		{"60 天前 → 沉睡", now.AddDate(0, 0, -60), TierAsleep},
		{"从未游玩 → 沉睡", time.Time{}, TierAsleep},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ClassifyTier(tc.lastPlayed, now))
		})
	}
}

func TestProbeInterval(t *testing.T) {
	require.Equal(t, 2*time.Minute, ProbeInterval(TierActive))
	require.Equal(t, 15*time.Minute, ProbeInterval(TierRecent))
	require.Equal(t, 2*time.Hour, ProbeInterval(TierDormant))
	require.Equal(t, 24*time.Hour, ProbeInterval(TierAsleep))
}

// 正在游玩的用户必须按最高频率探测，无论其 tier 是什么 ——
// 否则一个沉睡用户突然开始游玩，会话要等一整天才会被结算。
func TestNextProbeAt_PlayingUserAlwaysHighFrequency(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.Equal(t, now.Add(2*time.Minute), NextProbeAt(TierAsleep, true, now))
	require.Equal(t, now.Add(2*time.Minute), NextProbeAt(TierDormant, true, now))
}

func TestNextProbeAt_IdleUserFollowsTier(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.Equal(t, now.Add(24*time.Hour), NextProbeAt(TierAsleep, false, now))
	require.Equal(t, now.Add(2*time.Minute), NextProbeAt(TierActive, false, now))
}

// 未知的 tier 值必须退化到最保守的频率，而不是 panic 或零间隔。
// 零间隔会造成对该用户的忙轮询，瞬间打满配额。
func TestProbeInterval_UnknownTierIsConservative(t *testing.T) {
	require.Equal(t, 24*time.Hour, ProbeInterval(Tier(99)))
	require.Equal(t, 24*time.Hour, ProbeInterval(Tier(-1)))
}
