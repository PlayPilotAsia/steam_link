package task

import (
	"context"
	"time"
)

// 任务类型，对应 sync_tasks.task_type
const (
	TypeLibrarySync     int8 = 1 // L3 每日校准：全量拉游戏库
	TypeAchievementSync int8 = 2 // L2 成就下钻：单用户单游戏
	TypeSchemaSync      int8 = 3 // 全局成就定义同步：单游戏，与用户无关
	TypeSessionSettle   int8 = 4 // L1 会话结算
)

// 任务状态，对应 sync_tasks.status
const (
	StatusPending   int8 = 0
	StatusRunning   int8 = 1
	StatusSucceeded int8 = 2
	StatusRetrying  int8 = 3
	StatusDead      int8 = 4
)

// 优先级，数值小者优先。
// 新用户绑定会一次性产生数百条回填任务，必须用最低优先级，
// 否则会把所有用户的实时会话结算拖垮（设计文档 §6.8）。
const (
	PriorityRealtime int8 = 1
	PriorityNormal   int8 = 5
	PriorityBackfill int8 = 9
)

type Task struct {
	ID        uint64
	Type      int8
	SteamID   uint64
	AppID     uint32
	Payload   []byte
	Priority  int8
	Status    int8
	Attempts  uint16
	NextRunAt time.Time
	LastError string
}

// SessionPayload 是 TypeSessionSettle 任务携带的数据。
//
// Source 决定落库会话的可信度标记：探针捕获的传 store.SourceProbe，
// 启动自愈补结的传 store.SourceReconcile —— 后者跨越了 worker 宕机窗口，
// 起止时刻不可信，不得冒充实测数据（设计文档 §3.2、§9.4）。
type SessionPayload struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Source    int8      `json:"source"`
}

type Queue interface {
	// Enqueue 入队。同一 (Type, SteamID, AppID) 只保留一行，重复入队会合并。
	Enqueue(ctx context.Context, t Task) error
	// Claim 领取到期任务并续租。
	Claim(ctx context.Context, limit int, lease time.Duration) ([]Task, error)
	// Succeed 标记成功。
	Succeed(ctx context.Context, id uint64) error
	// Fail 记录失败并按指数退避重排，超过上限后转入死信。
	Fail(ctx context.Context, id uint64, cause error) error
	// Defer 推迟任务但**不累加 attempts**。
	//
	// 用于限流、熔断、配额降级这类「现在不能做，但不是失败」的场景。
	// 若这些场景走 Fail，配额耗尽期间的任务会因反复累加 attempts 被推入死信 ——
	// 而次日配额重置后它们本该正常执行。
	Defer(ctx context.Context, id uint64, until time.Time, reason string) error
}
