package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"steamlink/internal/domain"
	"steamlink/internal/logging"
	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

// SettleDelay 是会话结束到查询时长之间的等待时间。
// Steam 的 playtime_forever 在游戏退出后才最终结算，立刻查询会拿到旧值。
const SettleDelay = 5 * time.Minute

// maxDuePerRun 限制单轮取出的用户数，避免一次拉取过多。
const maxDuePerRun = 1000

type ProberDeps struct {
	Steam  steam.Client
	Probes *store.ProbeRepo
	Tasks  task.Queue
	Now    func() time.Time
	Logger *slog.Logger
}

type Prober struct {
	d  ProberDeps
	lg *slog.Logger
}

func NewProber(d ProberDeps) *Prober {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Prober{d: d, lg: d.Logger.With("component", "prober")}
}

// ProbeLease 是探针领取用户后的租约时长。
// 它必须长于一轮探测的耗时，又不能长到让 worker 崩溃后用户长时间失联。
const ProbeLease = 5 * time.Minute

// RunOnce 执行一轮探测：领取到期用户 → 分批调用 → 推进状态机 → 落库并入队。
//
// 用 Claim 而非 Due：worker 可多实例部署，裸查询会让多个实例
// 重复探测同一批用户并并发覆写 probe_state。
func (p *Prober) RunOnce(ctx context.Context) error {
	now := p.d.Now()

	due, err := p.d.Probes.Claim(ctx, now, maxDuePerRun, ProbeLease)
	if err != nil {
		return fmt.Errorf("collector: 领取到期用户失败: %w", err)
	}
	if len(due) == 0 {
		return nil
	}

	var firstErr error
	for _, batch := range chunk(due, steam.MaxSummariesBatch) {
		if err := p.runBatch(ctx, batch, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *Prober) runBatch(ctx context.Context, batch []store.ProbeState, now time.Time) error {
	ids := make([]uint64, 0, len(batch))
	for _, b := range batch {
		ids = append(ids, b.SteamID)
	}

	sums, err := p.d.Steam.GetPlayerSummaries(ctx, ids)
	if err != nil {
		// 关键：请求失败与「用户没在玩」是两回事。直接返回，
		// 保持所有状态不变，等下一轮重试。绝不能把空结果喂给状态机。
		p.lg.Warn("探针批次请求失败，本轮跳过且状态不变",
			slog.Int("batch_size", len(ids)), slog.String("err", err.Error()))
		return fmt.Errorf("collector: 探针请求失败: %w", err)
	}

	observed := make(map[uint64]uint32, len(sums))
	for _, s := range sums {
		observed[s.SteamID] = s.GameID
	}

	var missing int
	for _, row := range batch {
		gameID, present := observed[row.SteamID]
		if !present {
			// 响应中缺失该用户（账号被封、SteamID 失效等）。
			// 同样不能判定为「没在玩」，跳过本轮。
			missing++
			p.lg.Debug("响应中缺失该用户，跳过本轮", logging.SteamID(row.SteamID))
			continue
		}
		if err := p.advanceOne(ctx, row, gameID, now); err != nil {
			return err
		}
	}

	// 持续性的缺失说明有一批账号已失效，值得在运维层面关注
	if missing > 0 {
		p.lg.Info("探针批次存在缺失用户",
			slog.Int("missing", missing), slog.Int("batch_size", len(ids)))
	}
	return nil
}

func (p *Prober) advanceOne(ctx context.Context, row store.ProbeState,
	gameID uint32, now time.Time) error {

	prev := store.ToDomain(row)
	next, events := domain.Advance(prev, domain.Probe{GameID: gameID}, now)

	for _, e := range events {
		if e.Kind == domain.SessionStarted {
			p.lg.Info("会话开始",
				logging.SteamID(row.SteamID),
				slog.Uint64("appid", uint64(e.AppID)))
			continue
		}

		p.lg.Info("会话结束，入队结算",
			logging.SteamID(row.SteamID),
			slog.Uint64("appid", uint64(e.AppID)),
			slog.Time("started_at", e.StartedAt),
			slog.Time("ended_at", e.EndedAt))

		if err := p.enqueueSettle(ctx, row.SteamID, e, now); err != nil {
			return err
		}
	}

	// 用最后一次游玩时刻重新分层。正在游玩时以当前时刻计，
	// 保证其落入 TierActive。
	lastPlayed := lastPlayedOf(row, next, now)
	tier := domain.ClassifyTier(lastPlayed, now)
	nextAt := domain.NextProbeAt(tier, next.AppID != 0, now)

	return p.d.Probes.Save(ctx, row.SteamID, next, int8(tier), nextAt, now)
}

// lastPlayedOf 推断用于分层的「最后游玩时刻」。
func lastPlayedOf(row store.ProbeState, next domain.State, now time.Time) time.Time {
	if next.AppID != 0 {
		return now // 正在游玩
	}
	if row.LastSeenPlayingAt != nil {
		return *row.LastSeenPlayingAt
	}
	return time.Time{}
}

func (p *Prober) enqueueSettle(ctx context.Context, steamID uint64,
	e domain.Event, now time.Time) error {

	payload, err := json.Marshal(task.SessionPayload{
		StartedAt: e.StartedAt,
		EndedAt:   e.EndedAt,
		Source:    store.SourceProbe, // 探针实测，起止时刻可信
	})
	if err != nil {
		return err
	}

	return p.d.Tasks.Enqueue(ctx, task.Task{
		Type:      task.TypeSessionSettle,
		SteamID:   steamID,
		AppID:     e.AppID,
		Payload:   payload,
		Priority:  task.PriorityRealtime,
		NextRunAt: now.Add(SettleDelay),
	})
}

func chunk[T any](s []T, size int) [][]T {
	var out [][]T
	for size < len(s) {
		s, out = s[size:], append(out, s[0:size:size])
	}
	return append(out, s)
}
