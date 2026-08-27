package collector

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"steamlink/internal/store"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func storeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:localdev-root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"
	}
	db, err := store.NewDB(dsn, testLogger())
	require.NoError(t, err, "需要本地 MySQL 并已初始化：./scripts/dev/up.sh")

	for _, tbl := range []string{
		"sync_tasks", "probe_state", "achievement_unlocks",
		"play_sessions", "user_games", "app_achievements", "apps", "steam_links",
	} {
		require.NoError(t, db.Exec("DELETE FROM "+tbl).Error)
	}
	return db
}
