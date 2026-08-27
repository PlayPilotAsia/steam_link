package task

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/PlayPilotAsia/steam_link/internal/store"
)

func TestRunner_DispatchesByType(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	var got atomic.Int64
	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	r.Register(TypeLibrarySync, func(_ context.Context, tk Task) error {
		got.Store(int64(tk.SteamID))
		return nil
	})

	n, err := r.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, int64(1), got.Load())

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusSucceeded, row.Status)
}

// handler 返回普通错误 → 退避重试。
func TestRunner_FailureSchedulesRetry(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	r.Register(TypeLibrarySync, func(context.Context, Task) error {
		return errors.New("transient failure")
	})

	_, err := r.RunOnce(ctx)
	require.NoError(t, err)

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusRetrying, row.Status)
	require.Equal(t, uint16(1), row.Attempts)
}

// 包装了 ErrPermanent 的错误 → 直接置为成功，不重试。
// 这是「该游戏没有成就系统」等场景的处理方式：
// 它不是失败，重试永远不会成功，只会白白消耗配额。
func TestRunner_PermanentErrorMarksSucceeded(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityNormal, NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	r.Register(TypeAchievementSync, func(context.Context, Task) error {
		return fmt.Errorf("app has no stats: %w", ErrPermanent)
	})

	_, err := r.RunOnce(ctx)
	require.NoError(t, err)

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusSucceeded, row.Status)
	require.Equal(t, uint16(0), row.Attempts, "永久错误不应累加重试次数")
}

// 未注册的任务类型不能让 worker 崩溃，也不能无限重试。
func TestRunner_UnregisteredTypeGoesToRetry(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeSchemaSync, AppID: 620,
		Priority: PriorityNormal, NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	_, err := r.RunOnce(ctx)
	require.NoError(t, err)

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusRetrying, row.Status)
	require.Contains(t, row.LastError, "no handler")
}

// handler panic 必须被捕获，不能带崩整个 worker。
func TestRunner_RecoversFromPanic(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	r.Register(TypeLibrarySync, func(context.Context, Task) error {
		panic("boom")
	})

	require.NotPanics(t, func() {
		_, _ = r.RunOnce(ctx)
	})

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusRetrying, row.Status)
	require.Contains(t, row.LastError, "panic")
}
