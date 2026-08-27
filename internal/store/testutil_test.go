package store

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// testDSN 指向本地开发用的 MySQL（默认复用常驻容器 dev-mysql）。
// 必须带 parseTime=true 与 loc=UTC，否则 DATETIME 扫描进 time.Time 会失败或带错时区。
func testDSN() string {
	if v := os.Getenv("TEST_MYSQL_DSN"); v != "" {
		return v
	}
	return "root:localdev-root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"
}

// testLogger 静默日志输出，避免测试被 SQL trace 淹没。
func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := NewDB(testDSN(), testLogger())
	require.NoError(t, err, "需要本地 MySQL 且已初始化：./scripts/dev/up.sh")

	// 每个用例前清空，保证互不干扰
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
	db, err := NewDB(testDSN(), testLogger())
	require.NoError(t, err)
	return db
}
