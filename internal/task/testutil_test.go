package task

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/testsupport"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.NewDB(testsupport.DSN(t), testLogger())
	require.NoError(t, err)
	require.NoError(t, db.Exec("DELETE FROM sync_tasks").Error)
	return db
}
