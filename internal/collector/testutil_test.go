package collector

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/testsupport"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func storeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.NewDB(testsupport.DSN(t), testLogger())
	require.NoError(t, err)

	for _, tbl := range []string{
		"sync_tasks", "probe_state", "achievement_unlocks",
		"play_sessions", "user_games", "app_achievements", "apps", "steam_links",
	} {
		require.NoError(t, db.Exec("DELETE FROM "+tbl).Error)
	}
	return db
}
