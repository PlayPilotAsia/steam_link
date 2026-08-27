package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sampleSession(started time.Time) PlaySession {
	return PlaySession{
		SteamID: 76561197960287930, AppID: 440,
		StartedAt: started, EndedAt: started.Add(30 * time.Minute),
		DurationMin: 30, Source: SourceProbe,
		CreatedAt: started,
	}
}

func TestSessionRepo_Insert(t *testing.T) {
	r := NewSessionRepo(testDB(t))
	started := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	ok, err := r.Insert(context.Background(), sampleSession(started))
	require.NoError(t, err)
	require.True(t, ok)
}

// 租约回收会导致任务重跑。重复写入必须被唯一键挡住，
// 且不能当作错误上报 —— 否则任务会被无谓地重试到死信。
func TestSessionRepo_InsertIsIdempotent(t *testing.T) {
	db := testDB(t)
	r := NewSessionRepo(db)
	ctx := context.Background()
	started := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	ok, err := r.Insert(ctx, sampleSession(started))
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = r.Insert(ctx, sampleSession(started))
	require.NoError(t, err, "重复写入不是错误")
	require.False(t, ok, "应报告未插入")

	var n int64
	require.NoError(t, db.Model(&PlaySession{}).Count(&n).Error)
	require.Equal(t, int64(1), n)
}

// 同一游戏的不同会话（起始时刻不同）互不冲突。
func TestSessionRepo_DifferentStartTimesCoexist(t *testing.T) {
	db := testDB(t)
	r := NewSessionRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	_, err := r.Insert(ctx, sampleSession(base))
	require.NoError(t, err)
	_, err = r.Insert(ctx, sampleSession(base.Add(2*time.Hour)))
	require.NoError(t, err)

	var n int64
	require.NoError(t, db.Model(&PlaySession{}).Count(&n).Error)
	require.Equal(t, int64(2), n)
}

// 供 L3 校准判断某天是否已有实测会话。
func TestSessionRepo_HasSessionOn(t *testing.T) {
	r := NewSessionRepo(testDB(t))
	ctx := context.Background()
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	_, err := r.Insert(ctx, sampleSession(day.Add(14*time.Hour)))
	require.NoError(t, err)

	has, err := r.HasSessionOn(ctx, 76561197960287930, 440, day)
	require.NoError(t, err)
	require.True(t, has)

	has, err = r.HasSessionOn(ctx, 76561197960287930, 440, day.AddDate(0, 0, -1))
	require.NoError(t, err)
	require.False(t, has)
}
