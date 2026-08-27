package steam

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func serveFixture(t *testing.T, name string, status int) *HTTPClient {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return New("testkey", WithBaseURL(srv.URL))
}

// 游戏详情私密时 Steam 返回 HTTP 200 + 空对象，必须归一化为 ErrProfilePrivate，
// 而不是「成功但游戏数为 0」。
func TestGetOwnedGames_PrivateProfileIsError(t *testing.T) {
	c := serveFixture(t, "owned_games_private.json", 200)

	_, err := c.GetOwnedGames(context.Background(), 76561197960287930)
	require.ErrorIs(t, err, ErrProfilePrivate)
}

func TestGetPlayerAchievements_ProfilePrivate(t *testing.T) {
	c := serveFixture(t, "achievements_profile_private.json", 200)

	_, err := c.GetPlayerAchievements(context.Background(), 76561197960287930, 440)
	require.ErrorIs(t, err, ErrProfilePrivate)
}

// 「该游戏没有成就系统」必须是一个与隐私墙不同的错误 —— 上层据此永久跳过该游戏。
// 若二者混淆，无成就的游戏会陷入无限重试并持续消耗配额。
func TestGetPlayerAchievements_AppHasNoStats(t *testing.T) {
	c := serveFixture(t, "achievements_no_stats.json", 200)

	_, err := c.GetPlayerAchievements(context.Background(), 76561197960287930, 440)
	require.ErrorIs(t, err, ErrAppHasNoStats)
	require.False(t, errors.Is(err, ErrProfilePrivate), "两类错误必须可区分")
}

func TestGetPlayerSummaries_MissingGameIDMeansNotPlaying(t *testing.T) {
	c := serveFixture(t, "summaries_mixed.json", 200)

	got, err := c.GetPlayerSummaries(context.Background(),
		[]uint64{76561197960287930, 76561197960287931})
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, uint32(440), got[0].GameID)
	require.Equal(t, uint32(0), got[1].GameID, "缺失 gameid 字段应为 0，表示不在玩")
}

func TestGetOwnedGames_ParsesEmojiAndTimestamps(t *testing.T) {
	c := serveFixture(t, "owned_games_emoji.json", 200)

	got, err := c.GetOwnedGames(context.Background(), 76561197960287930)
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, "Portal 2 🧪", got[0].Name)
	require.Equal(t, uint32(1234), got[0].PlaytimeForeverMin)
	require.Equal(t, int64(1756180800), got[0].RtimeLastPlayed.Unix())
	require.True(t, got[1].RtimeLastPlayed.IsZero(), "rtime 为 0 应转为零值时间")
}

func TestRateLimitedMapsToSentinel(t *testing.T) {
	c := serveFixture(t, "owned_games_private.json", http.StatusTooManyRequests)

	_, err := c.GetOwnedGames(context.Background(), 76561197960287930)
	require.ErrorIs(t, err, ErrRateLimited)
}

func TestGetGlobalAchievementPercentages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 该接口不需要 key，且参数名是 gameid 而非 appid
		require.Equal(t, "440", r.URL.Query().Get("gameid"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"achievementpercentages":{"achievements":[
			{"name":"ACH_A","percent":42.5},
			{"name":"ACH_B","percent":3.125}
		]}}`)
	}))
	defer srv.Close()

	c := New("testkey", WithBaseURL(srv.URL))
	got, err := c.GetGlobalAchievementPercentages(context.Background(), 440)

	require.NoError(t, err)
	require.InDelta(t, 42.5, got["ACH_A"], 0.001)
	require.InDelta(t, 3.125, got["ACH_B"], 0.001)
}
