package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type AchievementItem struct {
	APIName     string  `json:"api_name"`
	DisplayName string  `json:"display_name"`
	Description string  `json:"description"`
	Icon        string  `json:"icon"`
	Hidden      bool    `json:"hidden"`
	GlobalPct   float64 `json:"global_pct"` // 全球解锁率，由 Task 15 的 Schema 同步填充
	Achieved    bool    `json:"achieved"`
	UnlockedAt  *int64  `json:"unlocked_at,omitempty"`
}

// handleGameAchievements 返回某款游戏的全部成就，含未解锁的。
func (d Deps) handleGameAchievements(c *gin.Context) {
	userID, ok := d.currentUserID(c)
	if !ok {
		return
	}

	appID64, err := strconv.ParseUint(c.Param("appid"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Code: "bad_appid", Message: "无效的 appid"})
		return
	}
	appID := uint32(appID64)

	ctx := c.Request.Context()
	link, err := d.Links.ByUserID(ctx, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Code: "not_linked", Message: "尚未绑定 Steam 账号"})
		return
	}

	defs, err := d.Games.ListAchievementDefs(ctx, appID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Code: "internal", Message: "查询失败"})
		return
	}

	unlocks, err := d.Sessions.ListUnlocks(ctx, link.SteamID, appID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Code: "internal", Message: "查询失败"})
		return
	}

	unlockedAt := make(map[string]int64, len(unlocks))
	for _, u := range unlocks {
		unlockedAt[u.APIName] = u.UnlockedAt.Unix()
	}

	items := make([]AchievementItem, 0, len(defs))
	for _, d := range defs {
		item := AchievementItem{
			APIName:     d.APIName,
			DisplayName: d.DisplayName,
			Description: d.Description,
			Icon:        d.Icon,
			Hidden:      d.Hidden == 1,
			GlobalPct:   d.GlobalPct,
		}
		if ts, ok := unlockedAt[d.APIName]; ok {
			item.Achieved = true
			item.UnlockedAt = &ts
		}
		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{
		"appid":    appID,
		"total":    len(defs),
		"unlocked": len(unlocks),
		"items":    items,
	})
}

// handleRecentAchievements 返回最近解锁的成就时间线。
func (d Deps) handleRecentAchievements(c *gin.Context) {
	userID, ok := d.currentUserID(c)
	if !ok {
		return
	}

	limit := 50
	if v, err := strconv.Atoi(c.DefaultQuery("limit", "50")); err == nil && v > 0 && v <= 200 {
		limit = v
	}

	ctx := c.Request.Context()
	link, err := d.Links.ByUserID(ctx, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Code: "not_linked", Message: "尚未绑定 Steam 账号"})
		return
	}

	rows, err := d.Sessions.RecentUnlocks(ctx, link.SteamID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Code: "internal", Message: "查询失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"items": rows})
}
