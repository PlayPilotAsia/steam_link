package task

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"steamlink/internal/store"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:localdev-root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"
	}
	db, err := store.NewDB(dsn, testLogger())
	require.NoError(t, err, "需要本地 MySQL 并已初始化：./scripts/dev/up.sh")
	require.NoError(t, db.Exec("DELETE FROM sync_tasks").Error)
	return db
}
