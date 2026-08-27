package store

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// NewDB 构造 GORM 连接，并把 GORM 自身的日志桥接到 slog。
func NewDB(dsn string, lg *slog.Logger) (*gorm.DB, error) {
	return gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: newGormSlogLogger(lg),
		// 全部时间统一按 UTC 处理，避免 worker 与 DB 时区不一致导致会话时刻错乱
		NowFunc: nowUTC,
	})
}

func nowUTC() time.Time { return time.Now().UTC() }

// gormSlogLogger 把 GORM 的日志接口适配到 slog，
// 避免 GORM 绕过项目的日志规范直接写 stdout。
type gormSlogLogger struct {
	lg            *slog.Logger
	slowThreshold time.Duration
}

func newGormSlogLogger(lg *slog.Logger) gormlogger.Interface {
	return &gormSlogLogger{
		lg:            lg.With("component", "gorm"),
		slowThreshold: 200 * time.Millisecond,
	}
}

func (l *gormSlogLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }

func (l *gormSlogLogger) Info(ctx context.Context, msg string, args ...any) {
	l.lg.InfoContext(ctx, msg, slog.Any("args", args))
}

func (l *gormSlogLogger) Warn(ctx context.Context, msg string, args ...any) {
	l.lg.WarnContext(ctx, msg, slog.Any("args", args))
}

func (l *gormSlogLogger) Error(ctx context.Context, msg string, args ...any) {
	l.lg.ErrorContext(ctx, msg, slog.Any("args", args))
}

func (l *gormSlogLogger) Trace(ctx context.Context, begin time.Time,
	fc func() (string, int64), err error) {

	elapsed := time.Since(begin)
	sql, rows := fc()

	switch {
	case err != nil && err != gorm.ErrRecordNotFound:
		// ErrRecordNotFound 是正常的业务分支，不该记为错误
		l.lg.ErrorContext(ctx, "SQL 执行失败",
			slog.String("sql", sql), slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed), slog.String("err", err.Error()))
	case elapsed > l.slowThreshold:
		l.lg.WarnContext(ctx, "慢查询",
			slog.String("sql", sql), slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed))
	default:
		l.lg.DebugContext(ctx, "SQL",
			slog.String("sql", sql), slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed))
	}
}
