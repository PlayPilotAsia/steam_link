package api

// LinkStatusResponse 是绑定状态的对外表示。
// SteamID 必须以字符串返回：它超过 JavaScript 的安全整数范围。
type LinkStatusResponse struct {
	SteamID    uint64 `json:"steam_id,string"`
	Visibility string `json:"visibility"` // ok / profile_private / game_details_private
	GameCount  int    `json:"game_count"`
	Hint       string `json:"hint,omitempty"`
}

type GameItem struct {
	AppID              uint32 `json:"appid"`
	Name               string `json:"name"`
	IconURL            string `json:"icon_url"`
	PlaytimeForeverMin uint32 `json:"playtime_forever_min"`
	Playtime2WeeksMin  uint32 `json:"playtime_2weeks_min"`
	LastPlayedAt       *int64 `json:"last_played_at,omitempty"` // Unix 秒
	AchUnlocked        uint16 `json:"ach_unlocked"`
	AchTotal           uint16 `json:"ach_total"`
}

// visibilityHint 给出可操作的修复指引，而不是笼统的「数据获取失败」。
func visibilityHint(state int8) (slug, hint string) {
	switch state {
	case 2:
		return "profile_private", "你的 Steam 个人资料未公开。请打开 Steam → 个人资料 → 编辑资料 → 隐私设置，将「我的个人资料」设为「公开」后重新检测。"
	case 3:
		return "game_details_private", "你的 Steam 个人资料已公开，但「游戏详情」仍是非公开。请打开 Steam → 个人资料 → 编辑资料 → 隐私设置，将「游戏详情」设为「公开」后重新检测。"
	default:
		return "ok", ""
	}
}
