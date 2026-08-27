package logging

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNew_RespectsLevelAndFormat(t *testing.T) {
	lg := New("debug", "text")
	require.True(t, lg.Enabled(nil, slog.LevelDebug))

	lg = New("warn", "json")
	require.False(t, lg.Enabled(nil, slog.LevelInfo))
	require.True(t, lg.Enabled(nil, slog.LevelWarn))
}

// 未知级别退化到 Info，而不是 panic 或静默丢弃全部日志。
func TestNew_UnknownLevelFallsBackToInfo(t *testing.T) {
	lg := New("verbose", "json")
	require.True(t, lg.Enabled(nil, slog.LevelInfo))
	require.False(t, lg.Enabled(nil, slog.LevelDebug))
}

// SteamID 必须以字符串输出：日志采集链路多经 JSON，
// 7.6×10^16 的数字会丢精度，变成一个不存在的账号。
func TestSteamID_IsString(t *testing.T) {
	attr := SteamID(76561197960287930)
	require.Equal(t, "steam_id", attr.Key)
	require.Equal(t, slog.KindString, attr.Value.Kind())
	require.Equal(t, "76561197960287930", attr.Value.String())
}
