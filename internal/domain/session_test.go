package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func playing(appID uint32, started, lastSeen time.Time, miss uint8) State {
	return State{AppID: appID, StartedAt: started, LastSeenPlayingAt: lastSeen, MissCount: miss}
}

func TestAdvance_TransitionTable(t *testing.T) {
	cases := []struct {
		name       string
		prev       State
		probe      Probe
		now        time.Time
		wantState  State
		wantEvents []EventKind
	}{
		{
			name:      "Idle 观测不到游戏，保持 Idle",
			prev:      State{},
			probe:     Probe{GameID: 0},
			now:       t0,
			wantState: State{},
		},
		{
			name:       "Idle 观测到游戏，开始会话",
			prev:       State{},
			probe:      Probe{GameID: 440},
			now:        t0,
			wantState:  playing(440, t0, t0, 0),
			wantEvents: []EventKind{SessionStarted},
		},
		{
			name:      "持续游玩同一游戏，起始时刻不变",
			prev:      playing(440, t0, t0, 0),
			probe:     Probe{GameID: 440},
			now:       t0.Add(2 * time.Minute),
			wantState: playing(440, t0, t0.Add(2*time.Minute), 0),
		},
		{
			name:  "首次观测不到游戏，仅累加 miss 不结束会话",
			prev:  playing(440, t0, t0, 0),
			probe: Probe{GameID: 0},
			now:   t0.Add(2 * time.Minute),
			// 关键：起始与最后在玩时刻都保持不变，只有 MissCount 变化
			wantState: playing(440, t0, t0, 1),
		},
		{
			name:       "连续两次观测不到，结束会话",
			prev:       playing(440, t0, t0, 1),
			probe:      Probe{GameID: 0},
			now:        t0.Add(4 * time.Minute),
			wantState:  State{},
			wantEvents: []EventKind{SessionEnded},
		},
		{
			name:       "切换游戏，同时产出结束与开始",
			prev:       playing(440, t0, t0, 0),
			probe:      Probe{GameID: 730},
			now:        t0.Add(2 * time.Minute),
			wantState:  playing(730, t0.Add(2*time.Minute), t0.Add(2*time.Minute), 0),
			wantEvents: []EventKind{SessionEnded, SessionStarted},
		},
		{
			name:      "miss 后恢复游玩，计数归零且不产出事件",
			prev:      playing(440, t0, t0, 1),
			probe:     Probe{GameID: 440},
			now:       t0.Add(4 * time.Minute),
			wantState: playing(440, t0, t0.Add(4*time.Minute), 0),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, events := Advance(tc.prev, tc.probe, tc.now)
			require.Equal(t, tc.wantState, got)

			kinds := make([]EventKind, 0, len(events))
			for _, e := range events {
				kinds = append(kinds, e.Kind)
			}
			if tc.wantEvents == nil {
				require.Empty(t, kinds)
			} else {
				require.Equal(t, tc.wantEvents, kinds)
			}
		})
	}
}

// 会话结束时刻必须回填为最后一次观测到在玩的时刻，
// 而不是当前时刻 —— 否则会把两个探针周期的空档算进游玩时长。
func TestAdvance_EndedAtUsesLastSeenPlaying(t *testing.T) {
	lastSeen := t0.Add(10 * time.Minute)
	prev := playing(440, t0, lastSeen, 1)

	_, events := Advance(prev, Probe{GameID: 0}, t0.Add(14*time.Minute))
	require.Len(t, events, 1)

	require.Equal(t, SessionEnded, events[0].Kind)
	require.Equal(t, t0, events[0].StartedAt)
	require.Equal(t, lastSeen, events[0].EndedAt, "结束时刻应为最后在玩时刻，非当前时刻")
}

// 切换游戏时，被结束的那局同样用 LastSeenPlayingAt 作为结束时刻。
func TestAdvance_SwitchGameEndsPreviousAtLastSeen(t *testing.T) {
	lastSeen := t0.Add(6 * time.Minute)
	prev := playing(440, t0, lastSeen, 0)

	_, events := Advance(prev, Probe{GameID: 730}, t0.Add(8*time.Minute))
	require.Len(t, events, 2)

	require.Equal(t, uint32(440), events[0].AppID)
	require.Equal(t, lastSeen, events[0].EndedAt)
	require.Equal(t, uint32(730), events[1].AppID)
	require.Equal(t, t0.Add(8*time.Minute), events[1].StartedAt)
}

// 超长挂机强制结算并开启新会话，避免单条异常记录污染统计。
func TestAdvance_ForcesRolloverAfterMaxDuration(t *testing.T) {
	prev := playing(440, t0, t0.Add(23*time.Hour), 0)
	now := t0.Add(25 * time.Hour)

	got, events := Advance(prev, Probe{GameID: 440}, now)

	require.Len(t, events, 2)
	require.Equal(t, SessionEnded, events[0].Kind)
	require.Equal(t, SessionStarted, events[1].Kind)
	require.Equal(t, now, got.StartedAt, "应以当前时刻开启新会话")
	require.Equal(t, uint32(440), got.AppID)
}

// 状态机必须是纯函数：同样的输入永远得到同样的输出，且不修改入参。
func TestAdvance_IsPure(t *testing.T) {
	prev := playing(440, t0, t0, 0)
	snapshot := prev

	for i := 0; i < 3; i++ {
		got, events := Advance(prev, Probe{GameID: 0}, t0.Add(2*time.Minute))
		require.Equal(t, playing(440, t0, t0, 1), got)
		require.Empty(t, events)
	}
	require.Equal(t, snapshot, prev, "入参不得被修改")
}
