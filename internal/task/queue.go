package task

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/PlayPilotAsia/steam_link/internal/store"
)

// MaxAttempts 是转入死信前的最大重试次数。
//
// 取 12 而非更小的值，是为了让退避真正走到 6 小时上限：
// 序列为 30s、1m、2m、4m、8m、16m、32m、1h4m、2h8m、4h16m、6h，
// 累计重试窗口约 14 小时。Steam 侧的故障可能持续数小时，
// 一小时就放弃会让大量任务无谓地进入死信。
const MaxAttempts = 12

// MaxBackoff 是单次退避的上限。
const MaxBackoff = 6 * time.Hour

// LeaseDuration 是默认租约时长。worker 崩溃后，任务在此时长后被自动回收。
const LeaseDuration = 5 * time.Minute

type MySQLQueue struct {
	db      *gorm.DB
	nowFunc func() time.Time
}

type QueueOption func(*MySQLQueue)

// WithClock 注入时钟。端到端测试需要用假时钟回放时间线，
// 若队列固定使用 time.Now，Claim 会用真实墙钟比对假时钟写入的
// next_run_at，导致延迟执行类的断言变成时间炸弹式的 flaky test。
func WithClock(fn func() time.Time) QueueOption {
	return func(q *MySQLQueue) { q.nowFunc = fn }
}

func NewMySQLQueue(db *gorm.DB, opts ...QueueOption) *MySQLQueue {
	q := &MySQLQueue{db: db, nowFunc: func() time.Time { return time.Now().UTC() }}
	for _, o := range opts {
		o(q)
	}
	return q
}

// Enqueue 依赖 uk_task(task_type, steam_id64, appid) 唯一键实现幂等。
// sync_tasks 是状态表而非日志表：每个任务标识永远只有一行，反复复用。
func (q *MySQLQueue) Enqueue(ctx context.Context, t Task) error {
	now := q.nowFunc()

	row := store.SyncTask{
		Type:      t.Type,
		SteamID:   t.SteamID,
		AppID:     t.AppID,
		Payload:   t.Payload,
		Priority:  t.Priority,
		Status:    StatusPending,
		Attempts:  0,
		NextRunAt: t.NextRunAt,
		CreatedAt: now,
		UpdatedAt: now,
	}

	return q.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "task_type"}, {Name: "steam_id64"}, {Name: "appid"},
		},
		DoUpdates: clause.Assignments(map[string]any{
			"status":   StatusPending,
			"attempts": 0,
			// 取更早的执行时刻：新的紧急需求不应被旧的远期排期压住。
			//
			// 用参数占位而非 VALUES(next_run_at)：后者自 MySQL 8.0.20 起已废弃，
			// 在 8.4 上会持续打 deprecation warning。传参写法语义相同且不依赖该语法。
			"next_run_at": gorm.Expr("LEAST(next_run_at, ?)", t.NextRunAt),
			"payload":     row.Payload,
			"priority":    row.Priority,
			"last_error":  "",
			"updated_at":  now,
		}),
	}).Create(&row).Error
}

func (q *MySQLQueue) Succeed(ctx context.Context, id uint64) error {
	now := q.nowFunc()
	return q.db.WithContext(ctx).Model(&store.SyncTask{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     StatusSucceeded,
			"last_error": "",
			"updated_at": now,
		}).Error
}

// Fail 按指数退避重排，超过 MaxAttempts 后转入死信。
func (q *MySQLQueue) Fail(ctx context.Context, id uint64, cause error) error {
	now := q.nowFunc()

	return q.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row store.SyncTask
		if err := tx.Where("id = ?", id).Take(&row).Error; err != nil {
			return err
		}

		attempts := row.Attempts + 1
		msg := truncate(cause.Error(), 512)

		if attempts >= MaxAttempts {
			return tx.Model(&store.SyncTask{}).Where("id = ?", id).
				Updates(map[string]any{
					"status":     StatusDead,
					"attempts":   attempts,
					"last_error": msg,
					"updated_at": now,
				}).Error
		}

		return tx.Model(&store.SyncTask{}).Where("id = ?", id).
			Updates(map[string]any{
				"status":      StatusRetrying,
				"attempts":    attempts,
				"last_error":  msg,
				"next_run_at": now.Add(backoff(attempts)),
				"updated_at":  now,
			}).Error
	})
}

// Defer 推迟任务的执行时刻，但不累加 attempts。
//
// 与 Fail 的区别是语义上的：限流、熔断、配额降级都属于「现在不能做」，
// 而非「做了但失败了」。若走 Fail，配额耗尽的那几个小时里任务会被
// 反复累加 attempts 直至进入死信 —— 而它们在次日配额重置后本该正常执行。
func (q *MySQLQueue) Defer(ctx context.Context, id uint64, until time.Time, reason string) error {
	now := q.nowFunc()

	return q.db.WithContext(ctx).Model(&store.SyncTask{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":      StatusPending,
			"next_run_at": until,
			"last_error":  truncate(reason, 512),
			"updated_at":  now,
			// 刻意不动 attempts
		}).Error
}

// backoff 计算指数退避间隔：30s、1m、2m…… 上限 MaxBackoff。
//
// attempts 从 1 起算。移位前先判断，避免 uint16 大值导致移位溢出成 0 或负数 ——
// 那会让退避退化成「立即重试」，在故障期间形成忙循环。
func backoff(attempts uint16) time.Duration {
	const base = 30 * time.Second

	// 30s << 10 = 8h32m 已超过上限，无需继续移位
	if attempts > 10 {
		return MaxBackoff
	}

	d := base << (attempts - 1)
	if d > MaxBackoff {
		return MaxBackoff
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Claim 领取到期任务。
//
// 设计要点：统一使用 next_run_at 作为唯一的调度时间轴。领取时把 status 置为
// StatusRunning 并把 next_run_at 推到租约到期时刻，因此扫描条件不需要 OR：
//
//	status=0/3 且到期 → 正常待执行
//	status=1   且到期 → 租约已过期，持有它的 worker 已崩溃，自动回收
//
// FOR UPDATE SKIP LOCKED 保证多 worker 并发扫描互不阻塞、互不重复。
func (q *MySQLQueue) Claim(ctx context.Context, limit int, lease time.Duration) ([]Task, error) {
	now := q.nowFunc()
	var out []Task

	err := q.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []store.SyncTask
		if err := tx.Clauses(clause.Locking{
			Strength: "UPDATE",
			Options:  "SKIP LOCKED",
		}).
			Where("status IN ? AND next_run_at <= ?",
				[]int8{StatusPending, StatusRunning, StatusRetrying}, now).
			Order("priority, next_run_at").
			Limit(limit).
			Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		ids := make([]uint64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}

		// 续租：状态置为执行中，next_run_at 推到租约到期时刻
		if err := tx.Model(&store.SyncTask{}).
			Where("id IN ?", ids).
			Updates(map[string]any{
				"status":      StatusRunning,
				"next_run_at": now.Add(lease),
				"updated_at":  now,
			}).Error; err != nil {
			return err
		}

		for _, r := range rows {
			out = append(out, Task{
				ID:        r.ID,
				Type:      r.Type,
				SteamID:   r.SteamID,
				AppID:     r.AppID,
				Payload:   r.Payload,
				Priority:  r.Priority,
				Status:    StatusRunning,
				Attempts:  r.Attempts,
				NextRunAt: now.Add(lease),
				LastError: r.LastError,
			})
		}
		return nil
	})

	return out, err
}

var _ Queue = (*MySQLQueue)(nil)
