package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/store"
)

// 领取后任务转为执行中，且 next_run_at 被推到租约到期时刻 ——
// 这使得扫描条件无需 OR，租约过期即自动可领。
func TestClaim_SetsRunningAndExtendsLease(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: now.Add(-time.Second),
	}))

	got, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)

	row := takeOnly(t, db)
	require.Equal(t, StatusRunning, row.Status)
	require.Equal(t, now.Add(5*time.Minute).Unix(), row.NextRunAt.Unix(),
		"next_run_at 应被推到租约到期时刻")
}

// 租约未过期时不可被重复领取。
func TestClaim_DoesNotStealLiveLease(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(testDB(t), fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: now.Add(-time.Second),
	}))

	first, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 1)

	second, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Empty(t, second, "租约有效期内不得被其他 worker 领走")
}

// 这是整个补偿方案的关键测试：worker 崩溃后任务必须能被回收，
// 否则会永久卡在「执行中」。
func TestClaim_ReclaimsExpiredLease(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: now.Add(-time.Second),
	}))

	claimed, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	// 模拟 worker 崩溃：租约到期但状态仍是「执行中」
	require.NoError(t, db.Model(&store.SyncTask{}).
		Where("id = ?", claimed[0].ID).
		Update("next_run_at", now.Add(-time.Minute)).Error)

	reclaimed, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1, "租约过期的任务必须能被回收")
	require.Equal(t, claimed[0].ID, reclaimed[0].ID)
}

// 未到期的任务不应被领取。
func TestClaim_SkipsFutureTasks(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(testDB(t), fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: now.Add(time.Hour),
	}))

	got, err := q.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Empty(t, got)
}

// 优先级必须先于时间生效：回填任务不能插队到实时任务前面。
func TestClaim_PriorityBeatsTime(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(testDB(t), fixedClock(now))
	ctx := context.Background()
	past := now.Add(-time.Hour)

	// 回填任务更早入队
	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityBackfill, NextRunAt: past.Add(-time.Hour),
	}))
	// 实时任务更晚，但优先级更高
	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeSessionSettle, SteamID: 1, AppID: 730,
		Priority: PriorityRealtime, NextRunAt: past,
	}))

	got, err := q.Claim(ctx, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, TypeSessionSettle, got[0].Type, "实时任务必须优先")
}

// 失败后按退避重排，且到期后可被重新领取。
func TestClaim_PicksUpRetriedTask(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: now.Add(-time.Second),
	}))

	claimed, err := q.Claim(ctx, 1, 5*time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.Fail(ctx, claimed[0].ID, errors.New("boom")))

	// 退避 30 秒内不可领取
	none, err := q.Claim(ctx, 1, 5*time.Minute)
	require.NoError(t, err)
	require.Empty(t, none)

	// 时钟推进过退避窗口后可以领取
	later := NewMySQLQueue(db, fixedClock(now.Add(time.Minute)))
	again, err := later.Claim(ctx, 1, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, again, 1)
	require.Equal(t, uint16(1), again[0].Attempts)
}

// Defer 过的任务在到期前不可领取，到期后可以 —— 且 attempts 仍为 0。
func TestClaim_RespectsDefer(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityBackfill, NextRunAt: now.Add(-time.Second),
	}))
	claimed, err := q.Claim(ctx, 1, 5*time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.Defer(ctx, claimed[0].ID, now.Add(time.Hour), "配额紧张"))

	none, err := q.Claim(ctx, 1, 5*time.Minute)
	require.NoError(t, err)
	require.Empty(t, none)

	later := NewMySQLQueue(db, fixedClock(now.Add(2*time.Hour)))
	again, err := later.Claim(ctx, 1, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, again, 1)
	require.Equal(t, uint16(0), again[0].Attempts)
}

// SKIP LOCKED 保证多 worker 并发扫描不会取到同一条任务。
// 这个行为在 SQLite 或 mock 上无法验证，必须打真实 MySQL。
func TestClaim_ConcurrentWorkersDoNotOverlap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	const total = 30
	q := NewMySQLQueue(db, fixedClock(now))
	for i := 0; i < total; i++ {
		require.NoError(t, q.Enqueue(ctx, Task{
			Type: TypeAchievementSync, SteamID: 1, AppID: uint32(1000 + i),
			Priority: PriorityNormal, NextRunAt: now.Add(-time.Second),
		}))
	}

	const workers = 4
	var (
		mu   sync.Mutex
		seen = map[uint64]int{}
		wg   sync.WaitGroup
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wq := NewMySQLQueue(db, fixedClock(now))
			for {
				got, err := wq.Claim(ctx, 5, 5*time.Minute)
				if err != nil || len(got) == 0 {
					return
				}
				mu.Lock()
				for _, tk := range got {
					seen[tk.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	require.Len(t, seen, total, "所有任务都应被领取")
	for id, n := range seen {
		require.Equal(t, 1, n, "任务 %d 被领取了 %d 次，SKIP LOCKED 未生效", id, n)
	}
}
