package e2e

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"steamlink/internal/collector"
	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

const testSteamID uint64 = 76561197960287930

func e2eDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:localdev-root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"
	}
	db, err := store.NewDB(dsn, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	for _, tbl := range []string{
		"sync_tasks", "probe_state", "achievement_unlocks",
		"play_sessions", "user_games", "app_achievements", "apps", "steam_links",
	} {
		require.NoError(t, db.Exec("DELETE FROM "+tbl).Error)
	}
	return db
}

// rig 把整条管线组装起来，时钟由测试完全掌控。
type rig struct {
	db     *gorm.DB
	fake   *fakeSteam
	prober *collector.Prober
	runner *task.Runner
	queue  task.Queue
	now    time.Time
}

func newRig(t *testing.T, start time.Time) *rig {
	db := e2eDB(t)
	fake := newFakeSteam(t)

	r := &rig{db: db, fake: fake, now: start}
	nowFn := func() time.Time { return r.now }

	sc := steam.New("testkey", steam.WithBaseURL(fake.URL()))

	// 队列必须用同一个假时钟。若它固定用 time.Now，Claim 会拿真实墙钟
	// 去比对假时钟写入的 next_run_at —— 「延迟 5 分钟结算」这类断言会变成
	// 时间炸弹：真实时间早于假时间时通过，晚于则任务被立刻领走，测试挂掉。
	queue := task.NewMySQLQueue(db, task.WithClock(nowFn))
	probes := store.NewProbeRepo(db)
	games := store.NewGameRepo(db)
	sessions := store.NewSessionRepo(db)
	links := store.NewLinkRepo(db)

	r.queue = queue
	r.prober = collector.NewProber(collector.ProberDeps{
		Steam: sc, Probes: probes, Tasks: queue, Now: nowFn,
	})

	runner := task.NewRunner(queue, task.RunnerOptions{
		Concurrency: 1,
		Now:         nowFn,
		// 不注入 Logger，NewRunner 会自动回退到 DiscardHandler
	})
	runner.Register(task.TypeSessionSettle, collector.NewSettler(collector.SettlerDeps{
		Steam: sc, Games: games, Sessions: sessions, Tasks: queue, Now: nowFn,
	}).Handle)
	runner.Register(task.TypeSchemaSync, collector.NewSchemaSyncer(collector.SchemaDeps{
		Steam: sc, Games: games, Tasks: queue, Now: nowFn,
	}).Handle)
	runner.Register(task.TypeAchievementSync, collector.NewAchievementSyncer(collector.AchievementDeps{
		Steam: sc, Games: games, Sessions: sessions, Links: links, Tasks: queue, Now: nowFn,
	}).Handle)
	runner.Register(task.TypeLibrarySync, collector.NewReconciler(collector.ReconcilerDeps{
		Steam: sc, Games: games, Sessions: sessions, Links: links, Tasks: queue, Now: nowFn,
	}).Handle)
	r.runner = runner

	require.NoError(t, links.Link(context.Background(), 1001, testSteamID))
	require.NoError(t, probes.Ensure(context.Background(), testSteamID, start))

	return r
}

func (r *rig) advance(d time.Duration) { r.now = r.now.Add(d) }

func (r *rig) probe(t *testing.T) {
	t.Helper()
	require.NoError(t, r.prober.RunOnce(context.Background()))
}

// drain 反复执行任务直到没有到期任务为止。
func (r *rig) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 20; i++ {
		n, err := r.runner.RunOnce(context.Background())
		require.NoError(t, err)
		if n == 0 {
			return
		}
	}
	t.Fatal("任务队列未能收敛，可能存在无限重新入队")
}

// 完整时间线：开始游玩 → 持续 30 分钟 → 退出 → Steam 延迟结算 → 落库。
func TestTimeline_FullSessionProducesAccurateRecord(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	r := newRig(t, start)

	// 20:00 用户启动游戏 440
	r.fake.StartPlaying(testSteamID, 440)
	r.probe(t)

	// 20:02 ~ 20:30 持续游玩，探针每 2 分钟一次。
	// 15 次而非 14：最后一次探测必须落在 20:30，因为 SessionEnded 的
	// EndedAt 取的是 LastSeenPlayingAt（最后一次观测到在玩的时刻）。
	// 少一次就是 20:28，后面所有时刻断言都会偏移 2 分钟。
	for i := 0; i < 15; i++ {
		r.advance(2 * time.Minute)
		r.probe(t)
	}
	require.Equal(t, start.Add(30*time.Minute), r.now, "此刻应为 20:30")

	// 20:30 用户退出。Steam 侧结算 30 分钟。
	r.fake.StopPlaying(testSteamID, 440, 30)
	r.fake.Unlock(testSteamID, 440, "ACH_A", r.now.Unix())

	// 20:32 探针首次观测不到 → 去抖，尚不结束会话
	r.advance(2 * time.Minute)
	r.probe(t)

	var n int64
	require.NoError(t, r.db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n, "首次未观测到时不应结束会话（去抖）")

	// 20:34 连续第二次观测不到 → 会话结束，入队结算
	r.advance(2 * time.Minute)
	r.probe(t)

	var settle store.SyncTask
	require.NoError(t, r.db.Where("task_type = ?", task.TypeSessionSettle).
		Take(&settle).Error)

	// 结算任务被延迟 5 分钟，此刻还不能执行
	r.drain(t)
	var sessions int64
	require.NoError(t, r.db.Model(&store.PlaySession{}).Count(&sessions).Error)
	require.Zero(t, sessions, "延迟窗口内不应结算")

	// 20:40 延迟窗口过去，执行结算
	r.advance(6 * time.Minute)
	r.drain(t)

	var sess store.PlaySession
	require.NoError(t, r.db.Take(&sess).Error)
	require.Equal(t, uint32(440), sess.AppID)
	require.Equal(t, uint32(30), sess.DurationMin, "时长应等于 Steam 的真实增量")
	require.Equal(t, store.SourceProbe, sess.Source)

	// 结束时刻应是最后一次观测到在玩的时刻（20:30），而非判定结束的时刻
	require.Equal(t, start.Add(30*time.Minute).Unix(), sess.EndedAt.Unix())
	require.Equal(t, start.Unix(), sess.StartedAt.Unix())

	// 成就应被连带同步
	var unlocks []store.AchievementUnlock
	require.NoError(t, r.db.Find(&unlocks).Error)
	require.Len(t, unlocks, 1)
	require.Equal(t, "ACH_A", unlocks[0].APIName)

	// 成就定义（全局共享）也应落库
	var defs int64
	require.NoError(t, r.db.Model(&store.AppAchievement{}).Count(&defs).Error)
	require.Equal(t, int64(2), defs, "含未解锁的成就定义")
}

// 短于探针间隔的会话被 L0 漏采，必须由 L3 校准捞回并标记为推断值。
func TestTimeline_ShortSessionRecoveredByReconcile(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	r := newRig(t, start)

	// 用户玩了 3 分钟，完整地卡在两次探针之间
	r.probe(t) // 20:00 未在玩
	r.fake.StartPlaying(testSteamID, 440)
	r.advance(time.Minute)
	r.fake.StopPlaying(testSteamID, 440, 3)
	r.advance(time.Minute)
	r.probe(t) // 20:02 已经退出了

	r.drain(t)
	var n int64
	require.NoError(t, r.db.Model(&store.PlaySession{}).Count(&n).Error)
	require.Zero(t, n, "L0 无法捕获短于探针间隔的会话")

	// 次日校准兜底
	r.advance(24 * time.Hour)
	require.NoError(t, r.queue.Enqueue(context.Background(), task.Task{
		Type: task.TypeLibrarySync, SteamID: testSteamID,
		Priority: task.PriorityNormal, NextRunAt: r.now,
	}))
	r.drain(t)

	var sess store.PlaySession
	require.NoError(t, r.db.Take(&sess).Error)
	require.Equal(t, uint32(3), sess.DurationMin)
	require.Equal(t, store.SourceReconcile, sess.Source,
		"校准补录的会话必须标记为推断值，不能伪装成实测数据")
}

// 切换游戏时，前一局结束、后一局开始，两条会话都要正确落库。
func TestTimeline_GameSwitchProducesTwoSessions(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	r := newRig(t, start)

	r.fake.StartPlaying(testSteamID, 440)
	r.probe(t)

	r.advance(20 * time.Minute)
	// 切到另一款游戏，前一局的时长结算到 Steam
	r.fake.StopPlaying(testSteamID, 440, 20)
	r.fake.StartPlaying(testSteamID, 730)
	r.probe(t)

	r.advance(15 * time.Minute)
	r.fake.StopPlaying(testSteamID, 730, 15)
	r.advance(2 * time.Minute)
	r.probe(t) // 去抖
	r.advance(2 * time.Minute)
	r.probe(t) // 结束

	r.advance(6 * time.Minute)
	r.drain(t)

	var sessions []store.PlaySession
	require.NoError(t, r.db.Order("appid").Find(&sessions).Error)
	require.Len(t, sessions, 2)

	require.Equal(t, uint32(440), sessions[0].AppID)
	require.Equal(t, uint32(20), sessions[0].DurationMin)
	require.Equal(t, uint32(730), sessions[1].AppID)
	require.Equal(t, uint32(15), sessions[1].DurationMin)
}
