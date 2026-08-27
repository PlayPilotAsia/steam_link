// Package logging 构造标准库 slog 的 Logger。
// 全项目禁止 fmt.Printf / log.Printf，也不依赖 slog.Default()：
// Logger 一律通过依赖注入传递，便于测试静默与附加组件标识。
package logging

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// New 按级别与格式构造 Logger。format 为 "text" 时用 TextHandler，
// 其余情况一律 JSONHandler（生产默认）。
func New(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// parseLevel 把配置字符串映射到 slog 级别，未知值退化到 Info。
func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// SteamID 返回统一的 SteamID 日志属性。
//
// 必须以字符串输出：SteamID64 约 7.6×10^16，超过 JavaScript 与多数
// JSON 日志处理链路的安全整数范围，以数字记录会静默丢精度。
func SteamID(id uint64) slog.Attr {
	return slog.String("steam_id", strconv.FormatUint(id, 10))
}
