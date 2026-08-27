package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLinkRepo_LinkAndFetch(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))

	got, err := r.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Equal(t, uint64(76561197960287930), got.SteamID)
	require.Equal(t, VisibilityUnknown, got.VisibilityState)
}

// 同一个 Steam 账号不能同时被两个本站账号绑定。
func TestLinkRepo_SteamIDTaken(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))
	err := r.Link(ctx, 1002, 76561197960287930)
	require.ErrorIs(t, err, ErrSteamIDTaken)
}

// 生成列的核心价值：解绑后该 Steam 账号可被他人重新绑定。
func TestLinkRepo_UnlinkFreesSteamIDForOthers(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))
	require.NoError(t, r.Unlink(ctx, 1001))

	require.NoError(t, r.Link(ctx, 1002, 76561197960287930),
		"解绑后 active_steam_id 变为 NULL，不再占用唯一键")
}

// 同一用户重新绑定同一账号应恢复原记录，历史数据自动可见。
func TestLinkRepo_RelinkSameAccountRestores(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))
	require.NoError(t, r.Unlink(ctx, 1001))
	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))

	got, err := r.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Nil(t, got.UnlinkedAt)
}

func TestLinkRepo_ByUserID_NotLinked(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	_, err := r.ByUserID(context.Background(), 9999)
	require.ErrorIs(t, err, ErrNotLinked)
}

// 连续私密探测累加，探测成功后清零。
func TestLinkRepo_PrivateStrikes(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()
	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))

	n, err := r.BumpPrivateStrikes(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Equal(t, int8(1), n)

	n, err = r.BumpPrivateStrikes(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Equal(t, int8(2), n)

	require.NoError(t, r.ResetPrivateStrikes(ctx, 76561197960287930))
	got, err := r.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Equal(t, int8(0), got.PrivateStrikes)
}

// 已解绑的账号不应出现在采集名单中。
func TestLinkRepo_ActiveSteamIDsExcludesUnlinked(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))
	require.NoError(t, r.Link(ctx, 1002, 76561197960287931))
	require.NoError(t, r.Unlink(ctx, 1002))

	ids, err := r.ActiveSteamIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []uint64{76561197960287930}, ids)
}
