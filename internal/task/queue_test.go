package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/PlayPilotAsia/steam_link/internal/store"
)

// fixedClock 让任务表的时间行为完全可控。
func fixedClock(t time.Time) QueueOption {
	return WithClock(func() time.Time { return t })
}

// takeOnly 取出表中唯一一行，断言确实只有一行。
func takeOnly(t *testing.T, db *gorm.DB) store.SyncTask {
	t.Helper()
	var rows []store.SyncTask
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	return rows[0]
}

func TestEnqueue_Insert(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 76561197960287930, AppID: 620,
		Priority: PriorityNormal, NextRunAt: now,
	}))

	row := takeOnly(t, db)
	require.Equal(t, TypeAchievementSync, row.Type)
	require.Equal(t, uint32(620), row.AppID)
	require.Equal(t, StatusPending, row.Status)
	require.Equal(t, now.Unix(), row.NextRunAt.Unix())
}

// 唯一键保证同一任务标识只有一行，重复入队不会堆积。
func TestEnqueue_IdempotentOnUniqueKey(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, q.Enqueue(ctx, Task{
			Type: TypeAchievementSync, SteamID: 76561197960287930, AppID: 620,
			Priority: PriorityNormal, NextRunAt: now,
		}))
	}

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Equal(t, int64(1), n, "重复入队必须合并为一行")
}

// 重复入队时取更早的执行时刻 —— 新的紧急需求不应被旧的远期排期压住。
func TestEnqueue_TakesEarlierNextRunAt(t *testing.T) {
	db := testDB(t)
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(base))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: base.Add(6 * time.Hour),
	}))
	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: base.Add(time.Minute),
	}))

	require.Equal(t, base.Add(time.Minute).Unix(), takeOnly(t, db).NextRunAt.Unix())
}

// 反向：已排期在前的任务，不会被更晚的入队推后。
func TestEnqueue_DoesNotPushBackEarlierSchedule(t *testing.T) {
	db := testDB(t)
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(base))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: base.Add(time.Minute),
	}))
	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: base.Add(6 * time.Hour),
	}))

	require.Equal(t, base.Add(time.Minute).Unix(), takeOnly(t, db).NextRunAt.Unix())
}

// 已成功的任务再次入队应复活为待执行 —— 这是「状态表而非日志表」的关键行为。
func TestEnqueue_RevivesSucceededTask(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityNormal, NextRunAt: now,
	}))

	// 直接改库模拟「已执行成功且累计过失败次数」，不依赖 Claim
	id := takeOnly(t, db).ID
	require.NoError(t, db.Model(&store.SyncTask{}).Where("id = ?", id).
		Updates(map[string]any{"status": StatusSucceeded, "attempts": 3}).Error)

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityNormal, NextRunAt: now.Add(time.Hour),
	}))

	row := takeOnly(t, db)
	require.Equal(t, StatusPending, row.Status, "已成功的任务应复活为待执行")
	require.Equal(t, uint16(0), row.Attempts, "重试次数应清零")
}

func TestFail_SchedulesBackoff(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: now,
	}))
	id := takeOnly(t, db).ID

	require.NoError(t, q.Fail(ctx, id, errors.New("boom")))

	row := takeOnly(t, db)
	require.Equal(t, StatusRetrying, row.Status)
	require.Equal(t, uint16(1), row.Attempts)
	require.Contains(t, row.LastError, "boom")
	require.Equal(t, now.Add(30*time.Second).Unix(), row.NextRunAt.Unix(),
		"首次失败退避 30 秒")
}

// 达到上限后转入死信并告警。
func TestFail_TransitionsToDead(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: now,
	}))
	id := takeOnly(t, db).ID

	require.NoError(t, db.Model(&store.SyncTask{}).Where("id = ?", id).
		Update("attempts", MaxAttempts-1).Error)
	require.NoError(t, q.Fail(ctx, id, errors.New("boom again")))

	require.Equal(t, StatusDead, takeOnly(t, db).Status)
}

// 退避序列必须在 6 小时封顶，且该分支真实可达 ——
// 否则「上限 6h」只是一句写在文档里的死代码。
func TestBackoff_CapsAtSixHours(t *testing.T) {
	require.Equal(t, 30*time.Second, backoff(1))
	require.Equal(t, time.Minute, backoff(2))
	require.Equal(t, 32*time.Minute, backoff(7))
	require.Equal(t, 4*time.Hour+16*time.Minute, backoff(10))
	require.Equal(t, 6*time.Hour, backoff(11), "超过 6 小时必须封顶")

	require.Less(t, backoff(MaxAttempts-1), 7*time.Hour)
	require.Equal(t, 6*time.Hour, backoff(MaxAttempts-1),
		"MaxAttempts 必须大到让 6 小时上限真实生效")
}

// Defer 推迟任务但不累加 attempts —— 限流与配额降级不是失败。
func TestDefer_DoesNotCountAsAttempt(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	q := NewMySQLQueue(db, fixedClock(now))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityBackfill, NextRunAt: now,
	}))
	id := takeOnly(t, db).ID

	for i := 0; i < 20; i++ {
		require.NoError(t, q.Defer(ctx, id, now.Add(time.Hour), "配额紧张"))
	}

	row := takeOnly(t, db)
	require.Equal(t, uint16(0), row.Attempts,
		"反复推迟不得累加重试次数，否则配额耗尽期间任务会被推入死信")
	require.NotEqual(t, StatusDead, row.Status)
	require.Equal(t, StatusPending, row.Status)
	require.Equal(t, now.Add(time.Hour).Unix(), row.NextRunAt.Unix())
	require.Contains(t, row.LastError, "配额紧张")
}
