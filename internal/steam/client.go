package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.steampowered.com"

// MaxSummariesBatch 是 GetPlayerSummaries 单次请求的 SteamID 上限，由 Steam 规定。
const MaxSummariesBatch = 100

type HTTPClient struct {
	apiKey  string
	baseURL string
	hc      *http.Client
	limiter Limiter // 可为 nil，表示不限流（仅测试使用）
}

type Option func(*HTTPClient)

func WithLimiter(l Limiter) Option { return func(c *HTTPClient) { c.limiter = l } }

func WithBaseURL(u string) Option          { return func(c *HTTPClient) { c.baseURL = u } }
func WithHTTPClient(h *http.Client) Option { return func(c *HTTPClient) { c.hc = h } }

func New(apiKey string, opts ...Option) *HTTPClient {
	c := &HTTPClient{
		apiKey:  apiKey,
		baseURL: DefaultBaseURL,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// getJSON 发起请求并解码。HTTP 层的失败在此统一归一化。
func (c *HTTPClient) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	if c.limiter != nil {
		if err := c.limiter.Acquire(ctx); err != nil {
			return err
		}
	}

	q.Set("key", c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("steam: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// 收到 429 说明本地限流阈值仍然过高，立即全局熔断 60 秒
		if b, ok := c.limiter.(interface {
			TripBreaker(context.Context, time.Duration) error
		}); ok {
			_ = b.TripBreaker(ctx, 60*time.Second)
		}
		return ErrRateLimited
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		// Steam 对私密资料的部分接口直接返回 401/403
		return ErrProfilePrivate
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("steam: unexpected status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// ---------- GetPlayerSummaries ----------

type rawSummaries struct {
	Response struct {
		Players []struct {
			SteamID                  string `json:"steamid"`
			PersonaName              string `json:"personaname"`
			Avatar                   string `json:"avatar"`
			CommunityVisibilityState int    `json:"communityvisibilitystate"`
			PersonaState             int    `json:"personastate"`
			// gameid 在不玩游戏时字段缺失，且类型为字符串
			GameID        string `json:"gameid"`
			GameExtraInfo string `json:"gameextrainfo"`
		} `json:"players"`
	} `json:"response"`
}

func (c *HTTPClient) GetPlayerSummaries(ctx context.Context, ids []uint64) ([]PlayerSummary, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxSummariesBatch {
		return nil, fmt.Errorf("steam: batch size %d exceeds limit %d", len(ids), MaxSummariesBatch)
	}

	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(id, 10)
	}
	q := url.Values{"steamids": {strings.Join(parts, ",")}}

	var raw rawSummaries
	if err := c.getJSON(ctx, "/ISteamUser/GetPlayerSummaries/v0002/", q, &raw); err != nil {
		return nil, err
	}

	out := make([]PlayerSummary, 0, len(raw.Response.Players))
	for _, p := range raw.Response.Players {
		sid, _ := strconv.ParseUint(p.SteamID, 10, 64)
		gid, _ := strconv.ParseUint(p.GameID, 10, 32) // 缺失时为空串，解析得 0
		out = append(out, PlayerSummary{
			SteamID:                  sid,
			PersonaName:              p.PersonaName,
			Avatar:                   p.Avatar,
			CommunityVisibilityState: p.CommunityVisibilityState,
			PersonaState:             p.PersonaState,
			GameID:                   uint32(gid),
			GameExtraInfo:            p.GameExtraInfo,
		})
	}
	return out, nil
}

// ---------- GetOwnedGames / GetRecentlyPlayedGames ----------

type rawGames struct {
	Response struct {
		GameCount *int `json:"game_count"` // 指针：用于区分「字段缺失」与「值为 0」
		Games     []struct {
			AppID           uint32 `json:"appid"`
			Name            string `json:"name"`
			ImgIconURL      string `json:"img_icon_url"`
			PlaytimeForever uint32 `json:"playtime_forever"`
			Playtime2Weeks  uint32 `json:"playtime_2weeks"`
			RtimeLastPlayed int64  `json:"rtime_last_played"`
		} `json:"games"`
	} `json:"response"`
}

func (r rawGames) toDomain() []OwnedGame {
	out := make([]OwnedGame, 0, len(r.Response.Games))
	for _, g := range r.Response.Games {
		var last time.Time
		if g.RtimeLastPlayed > 0 {
			last = time.Unix(g.RtimeLastPlayed, 0).UTC()
		}
		out = append(out, OwnedGame{
			AppID:              g.AppID,
			Name:               g.Name,
			ImgIconURL:         g.ImgIconURL,
			PlaytimeForeverMin: g.PlaytimeForever,
			Playtime2WeeksMin:  g.Playtime2Weeks,
			RtimeLastPlayed:    last,
		})
	}
	return out
}

func (c *HTTPClient) GetOwnedGames(ctx context.Context, id uint64) ([]OwnedGame, error) {
	q := url.Values{
		"steamid":                   {strconv.FormatUint(id, 10)},
		"include_appinfo":           {"1"},
		"include_played_free_games": {"1"},
	}

	var raw rawGames
	if err := c.getJSON(ctx, "/IPlayerService/GetOwnedGames/v0001/", q, &raw); err != nil {
		return nil, err
	}

	// 关键：游戏详情非公开时 Steam 返回 {"response":{}}，game_count 字段整个缺失。
	// 一个真正拥有 0 款游戏的公开账号会返回 "game_count":0。用指针区分二者。
	if raw.Response.GameCount == nil {
		return nil, ErrProfilePrivate
	}
	return raw.toDomain(), nil
}

func (c *HTTPClient) GetRecentlyPlayedGames(ctx context.Context, id uint64) ([]OwnedGame, error) {
	q := url.Values{"steamid": {strconv.FormatUint(id, 10)}}

	var raw rawGames
	if err := c.getJSON(ctx, "/IPlayerService/GetRecentlyPlayedGames/v0001/", q, &raw); err != nil {
		return nil, err
	}
	if raw.Response.GameCount == nil {
		return nil, ErrProfilePrivate
	}
	return raw.toDomain(), nil
}

// ---------- GetPlayerAchievements ----------

type rawPlayerAch struct {
	PlayerStats struct {
		Success      bool   `json:"success"`
		Error        string `json:"error"`
		Achievements []struct {
			APIName    string `json:"apiname"`
			Achieved   int    `json:"achieved"`
			UnlockTime int64  `json:"unlocktime"`
		} `json:"achievements"`
	} `json:"playerstats"`
}

func (c *HTTPClient) GetPlayerAchievements(ctx context.Context, id uint64, appID uint32) ([]PlayerAchievement, error) {
	q := url.Values{
		"steamid": {strconv.FormatUint(id, 10)},
		"appid":   {strconv.FormatUint(uint64(appID), 10)},
		"l":       {"schinese"},
	}

	var raw rawPlayerAch
	if err := c.getJSON(ctx, "/ISteamUserStats/GetPlayerAchievements/v0001/", q, &raw); err != nil {
		return nil, err
	}
	if !raw.PlayerStats.Success {
		return nil, classifyPlayerStatsError(raw.PlayerStats.Error)
	}

	out := make([]PlayerAchievement, 0, len(raw.PlayerStats.Achievements))
	for _, a := range raw.PlayerStats.Achievements {
		var ut time.Time
		if a.UnlockTime > 0 {
			ut = time.Unix(a.UnlockTime, 0).UTC()
		}
		out = append(out, PlayerAchievement{
			APIName:    a.APIName,
			Achieved:   a.Achieved == 1,
			UnlockTime: ut,
		})
	}
	return out, nil
}

// ---------- GetSchemaForGame ----------

type rawSchema struct {
	Game struct {
		GameName           string `json:"gameName"`
		AvailableGameStats struct {
			Achievements []struct {
				Name        string `json:"name"` // 即 apiname
				DisplayName string `json:"displayName"`
				Description string `json:"description"`
				Icon        string `json:"icon"`
				IconGray    string `json:"icongray"`
				Hidden      int    `json:"hidden"`
			} `json:"achievements"`
		} `json:"availableGameStats"`
	} `json:"game"`
}

func (c *HTTPClient) GetSchemaForGame(ctx context.Context, appID uint32) (GameSchema, error) {
	q := url.Values{
		"appid": {strconv.FormatUint(uint64(appID), 10)},
		"l":     {"schinese"},
	}

	var raw rawSchema
	if err := c.getJSON(ctx, "/ISteamUserStats/GetSchemaForGame/v2/", q, &raw); err != nil {
		return GameSchema{}, err
	}

	s := GameSchema{AppID: appID, Name: raw.Game.GameName}
	for _, a := range raw.Game.AvailableGameStats.Achievements {
		s.Achievements = append(s.Achievements, SchemaAchievement{
			APIName:     a.Name,
			DisplayName: a.DisplayName,
			Description: a.Description,
			Icon:        a.Icon,
			IconGray:    a.IconGray,
			Hidden:      a.Hidden == 1,
		})
	}
	return s, nil
}

// 编译期断言：HTTPClient 必须满足 Client 接口
var _ Client = (*HTTPClient)(nil)
