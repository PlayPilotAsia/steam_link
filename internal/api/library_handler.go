package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/PlayPilotAsia/libra/errcode"
)

func (d Deps) handleLibrary(c *gin.Context) {
	userID, ok := d.currentUserID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	link, err := d.Links.ByUserID(ctx, userID)
	if err != nil {
		fail(c, errcode.SteamLinkNotFound)
		return
	}

	rows, err := d.Games.ListLibrary(ctx, link.SteamID)
	if err != nil {
		fail(c, errcode.SteamSystemError)
		return
	}

	items := make([]GameItem, 0, len(rows))
	for _, r := range rows {
		item := GameItem{
			AppID:              r.AppID,
			Name:               r.Name,
			IconURL:            iconURL(r.AppID, r.ImgIconURL),
			PlaytimeForeverMin: r.PlaytimeForeverMin,
			Playtime2WeeksMin:  r.Playtime2WeeksMin,
			AchUnlocked:        r.AchUnlocked,
			AchTotal:           r.AchTotal,
		}
		if r.RtimeLastPlayed != nil {
			ts := r.RtimeLastPlayed.Unix()
			item.LastPlayedAt = &ts
		}
		items = append(items, item)
	}

	slug, hint := visibilityHint(link.VisibilityState)
	succeed(c, gin.H{
		"visibility": slug,
		"hint":       hint,
		"games":      items,
	})
}

// iconURL 把 Steam 返回的 img_icon_url 哈希拼成完整 CDN 地址。
func iconURL(appID uint32, hash string) string {
	if hash == "" {
		return ""
	}
	return "https://media.steampowered.com/steamcommunity/public/images/apps/" +
		itoa(appID) + "/" + hash + ".jpg"
}

func itoa(v uint32) string {
	return strconv.FormatUint(uint64(v), 10)
}
