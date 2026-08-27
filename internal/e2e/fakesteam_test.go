package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSteam 是一个可编程的 Steam API 假实现。
// 它让我们精确控制「用户正在玩什么」和「Steam 侧记录的时长」，
// 从而回放真实世界中难以复现的时序。
type fakeSteam struct {
	mu sync.Mutex

	// 当前正在玩的游戏，0 表示不在玩
	playing map[uint64]uint32
	// Steam 侧记录的累计时长（分钟）
	playtime map[uint64]map[uint32]uint32
	// 已解锁成就：steamID → appid → apiName → unlockUnix
	unlocked map[uint64]map[uint32]map[string]int64

	srv *httptest.Server
}

func newFakeSteam(t *testing.T) *fakeSteam {
	f := &fakeSteam{
		playing:  map[uint64]uint32{},
		playtime: map[uint64]map[uint32]uint32{},
		unlocked: map[uint64]map[uint32]map[string]int64{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ISteamUser/GetPlayerSummaries/v0002/", f.handleSummaries)
	mux.HandleFunc("/IPlayerService/GetOwnedGames/v0001/", f.handleGames)
	mux.HandleFunc("/IPlayerService/GetRecentlyPlayedGames/v0001/", f.handleGames)
	mux.HandleFunc("/ISteamUserStats/GetPlayerAchievements/v0001/", f.handleAchievements)
	mux.HandleFunc("/ISteamUserStats/GetSchemaForGame/v2/", f.handleSchema)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSteam) URL() string { return f.srv.URL }

// StartPlaying 让某用户开始游玩某款游戏。
func (f *fakeSteam) StartPlaying(steamID uint64, appID uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.playing[steamID] = appID
}

// StopPlaying 让用户退出游戏，并把本次游玩时长结算到累计时长中。
// 这模拟了 Steam 只在退出后才更新 playtime_forever 的真实行为。
func (f *fakeSteam) StopPlaying(steamID uint64, appID uint32, minutes uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.playing[steamID] = 0
	if f.playtime[steamID] == nil {
		f.playtime[steamID] = map[uint32]uint32{}
	}
	f.playtime[steamID][appID] += minutes
}

func (f *fakeSteam) Unlock(steamID uint64, appID uint32, apiName string, at int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unlocked[steamID] == nil {
		f.unlocked[steamID] = map[uint32]map[string]int64{}
	}
	if f.unlocked[steamID][appID] == nil {
		f.unlocked[steamID][appID] = map[string]int64{}
	}
	f.unlocked[steamID][appID][apiName] = at
}

func (f *fakeSteam) handleSummaries(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	type player struct {
		SteamID                  string `json:"steamid"`
		CommunityVisibilityState int    `json:"communityvisibilitystate"`
		PersonaName              string `json:"personaname"`
		GameID                   string `json:"gameid,omitempty"`
	}
	var players []player

	for _, raw := range strings.Split(r.URL.Query().Get("steamids"), ",") {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			continue
		}
		p := player{
			SteamID: raw, CommunityVisibilityState: 3,
			PersonaName: "Tester",
		}
		// 关键：不在玩时 gameid 字段整个缺失，而非为 "0"
		if g := f.playing[id]; g != 0 {
			p.GameID = strconv.FormatUint(uint64(g), 10)
		}
		players = append(players, p)
	}

	writeJSON(w, map[string]any{"response": map[string]any{"players": players}})
}

func (f *fakeSteam) handleGames(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, _ := strconv.ParseUint(r.URL.Query().Get("steamid"), 10, 64)

	type game struct {
		AppID           uint32 `json:"appid"`
		Name            string `json:"name"`
		PlaytimeForever uint32 `json:"playtime_forever"`
		ImgIconURL      string `json:"img_icon_url"`
	}
	games := []game{}
	for appID, mins := range f.playtime[id] {
		games = append(games, game{
			AppID: appID, Name: "Game " + strconv.FormatUint(uint64(appID), 10),
			PlaytimeForever: mins, ImgIconURL: "icon",
		})
	}

	count := len(games)
	writeJSON(w, map[string]any{"response": map[string]any{
		"game_count": count, "games": games,
	}})
}

func (f *fakeSteam) handleAchievements(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, _ := strconv.ParseUint(r.URL.Query().Get("steamid"), 10, 64)
	appID64, _ := strconv.ParseUint(r.URL.Query().Get("appid"), 10, 32)
	appID := uint32(appID64)

	type ach struct {
		APIName    string `json:"apiname"`
		Achieved   int    `json:"achieved"`
		UnlockTime int64  `json:"unlocktime"`
	}
	out := []ach{
		{APIName: "ACH_A"}, {APIName: "ACH_B"},
	}
	for i := range out {
		if at, ok := f.unlocked[id][appID][out[i].APIName]; ok {
			out[i].Achieved = 1
			out[i].UnlockTime = at
		}
	}

	writeJSON(w, map[string]any{"playerstats": map[string]any{
		"success": true, "achievements": out,
	}})
}

func (f *fakeSteam) handleSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"game": map[string]any{
		"gameName": "Fake Game",
		"availableGameStats": map[string]any{
			"achievements": []map[string]any{
				{"name": "ACH_A", "displayName": "成就甲", "description": "描述甲", "icon": "a.jpg"},
				{"name": "ACH_B", "displayName": "成就乙", "description": "描述乙", "icon": "b.jpg"},
			},
		},
	}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
