package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/PlayPilotAsia/steam_link/internal/logging"
)

// ErrPermanent 标记不可恢复的失败。handler 用 %w 包装它返回时，
// 任务会被直接置为成功而非重试 —— 例如「该游戏没有成就系统」，
// 重试永远不会成功，只会持续消耗配额。
var ErrPermanent = errors.New("task: permanent failure, do not retry")

// ErrDeferred 标记「现在不能做，但不是失败」。handler 用 %w 包装它返回时，
// 任务被推迟且 attempts 不累加。
//
// 限流、熔断、配额降级都属于这一类。它们若走普通失败路径，
// 配额耗尽的几小时内任务会被反复累加 attempts 直至进入死信 ——
// 而这些任务在次日配额重置后本该正常执行。
var ErrDeferred = errors.New("task: deferred, retry later without counting attempt")

// DeferDelay 是被推迟任务的重排间隔。
const DeferDelay = 15 * time.Minute

type Handler func(ctx context.Context, t Task) error

type RunnerOptions struct {
	Concurrency  int
	PollInterval time.Duration
	Lease        time.Duration
	Logger       *slog.Logger
	Now          func() time.Time
}

type Runner struct {
	q        Queue
	handlers map[int8]Handler
	opts     RunnerOptions
	lg       *slog.Logger
}

func NewRunner(q Queue, opts RunnerOptions) *Runner {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.Lease <= 0 {
		opts.Lease = LeaseDuration
	}
	// 未注入 Logger 时静默，而不是回退到 slog.Default() ——
	// 全局默认 Logger 会绕过项目的日志配置，且让测试输出变脏。
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Runner{
		q:        q,
		handlers: map[int8]Handler{},
		opts:     opts,
		lg:       opts.Logger.With("component", "task-runner"),
	}
}

func (r *Runner) Register(typ int8, h Handler) { r.handlers[typ] = h }

// RunOnce 领取一批任务并并发执行，返回处理的任务数。
func (r *Runner) RunOnce(ctx context.Context) (int, error) {
	tasks, err := r.q.Claim(ctx, r.opts.Concurrency*4, r.opts.Lease)
	if err != nil {
		return 0, err
	}
	if len(tasks) == 0 {
		return 0, nil
	}

	sem := make(chan struct{}, r.opts.Concurrency)
	var wg sync.WaitGroup

	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t Task) {
			defer wg.Done()
			defer func() { <-sem }()
			r.execute(ctx, t)
		}(t)
	}
	wg.Wait()

	return len(tasks), nil
}

func (r *Runner) execute(ctx context.Context, t Task) {
	lg := r.lg.With(
		slog.Uint64("task_id", t.ID),
		slog.Int("task_type", int(t.Type)),
		logging.SteamID(t.SteamID),
		slog.Uint64("appid", uint64(t.AppID)),
	)

	err := r.invoke(ctx, t)

	switch {
	case err == nil:
		lg.Debug("任务执行成功")
		if e := r.q.Succeed(ctx, t.ID); e != nil {
			lg.Error("标记任务成功失败", slog.String("err", e.Error()))
		}

	case errors.Is(err, ErrDeferred):
		// 限流、熔断、配额降级：不是失败，不计入 attempts。
		// 若走 Fail，配额耗尽的那几小时会把大量任务推进死信，
		// 而它们在次日配额重置后本该正常执行。
		until := r.opts.Now().Add(DeferDelay)
		lg.Info("任务推迟执行",
			slog.Time("until", until), slog.String("reason", err.Error()))
		if e := r.q.Defer(ctx, t.ID, until, err.Error()); e != nil {
			lg.Error("推迟任务失败", slog.String("err", e.Error()))
		}

	case errors.Is(err, ErrPermanent):
		// 永久失败也算「处理完毕」：重试没有意义
		lg.Info("任务永久失败，不再重试", slog.String("err", err.Error()))
		if e := r.q.Succeed(ctx, t.ID); e != nil {
			lg.Error("标记任务成功失败", slog.String("err", e.Error()))
		}

	default:
		lg.Warn("任务执行失败，将退避重试",
			slog.Int("attempts", int(t.Attempts)),
			slog.String("err", err.Error()))
		if e := r.q.Fail(ctx, t.ID, err); e != nil {
			lg.Error("标记任务失败失败", slog.String("err", e.Error()))
		}
	}
}

// invoke 调用 handler 并捕获 panic —— 单个任务的 bug 不应带崩整个 worker。
func (r *Runner) invoke(ctx context.Context, t Task) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("task: handler panic: %v", p)
		}
	}()

	h, ok := r.handlers[t.Type]
	if !ok {
		return fmt.Errorf("task: no handler registered for type %d", t.Type)
	}
	return h(ctx, t)
}

// Start 持续轮询直到 ctx 取消。无任务时按 PollInterval 休眠，
// 有任务时立即继续下一轮，保证积压能被快速消化。
func (r *Runner) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := r.RunOnce(ctx)
		if err != nil {
			r.lg.Error("任务轮询失败", slog.String("err", err.Error()))
		}
		if n > 0 {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(r.opts.PollInterval):
		}
	}
}
