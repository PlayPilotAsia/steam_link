package store

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/PlayPilotAsia/steam_link/internal/testsupport"
)

// testLogger 静默日志输出，避免测试被 SQL trace 淹没。
func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := testDBHandle(t)

	// 每个用例前清空，保证互不干扰。跨包的隔离由 testsupport 的独立库负责。
	for _, tbl := range []string{
		"sync_tasks", "probe_state", "achievement_unlocks",
		"play_sessions", "user_games", "app_achievements", "apps", "steam_links",
	} {
		require.NoError(t, db.Exec("DELETE FROM "+tbl).Error)
	}
	return db
}

func testDBHandle(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := NewDB(testsupport.DSN(t), testLogger())
	require.NoError(t, err)
	return db
}
