package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/PlayPilotAsia/steam_link/internal/auth"
	"github.com/PlayPilotAsia/steam_link/internal/collector"
	"github.com/PlayPilotAsia/steam_link/internal/logging"
	"github.com/PlayPilotAsia/steam_link/internal/store"
)

const userIDHeader = "X-User-Id"

// trustedUserID 只读取 Gateway 注入的可信身份头。客户端原始同名头必须由
// Gateway 无条件剥离；steam_link 自身不再解析 Cookie 或访问登录态 Redis。
func trustedUserID(c *gin.Context) (uint64, bool) {
	raw := c.GetHeader(userIDHeader)
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

func (d Deps) currentUserID(c *gin.Context) (uint64, bool) {
	id, ok := trustedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Code: "unauthorized", Message: "请先登录"})
		return 0, false
	}
	return id, true
}

// handleLogin 发起 OpenID 跳转。
//
// 这是一个浏览器顶层导航入口（前端用 window.location 跳转过来），
// 因此失败时也必须重定向而非返回 JSON —— 用户看到的是页面，不是接口响应。
func (d Deps) handleLogin(c *gin.Context) {
	userID, ok := trustedUserID(c)
	if !ok {
		d.redirectResult(c, "unauthorized")
		return
	}

	state := auth.SignState(d.StateSecret, userID, time.Now().UTC())
	returnTo := d.BaseURL + "/noauth/steam/callback?state=" + url.QueryEscape(state)

	c.Redirect(http.StatusFound, auth.BuildRedirectURL(d.BaseURL, returnTo))
}

// handleCallback 处理 Steam 回跳：验证断言 → 校验 state → 建立绑定 → 探测隐私。
//
// 全程以 302 回应：这个端点是被用户浏览器直接导航到的，返回 JSON
// 会让用户对着一屏原始 JSON 发呆。结果通过 query 参数传给前端页面。
func (d Deps) handleCallback(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := auth.VerifyState(d.StateSecret, c.Query("state"), time.Now().UTC())
	if err != nil {
		d.redirectResult(c, "invalid_state")
		return
	}

	// 安全生命线：必须向 Steam 确认这次断言，见设计文档 §7.1
	steamID, err := d.Verifier.Verify(ctx, c.Request.URL.Query())
	if err != nil {
		d.Logger.Warn("OpenID 断言验证失败", slog.String("err", err.Error()))
		d.redirectResult(c, "openid_invalid")
		return
	}

	switch err := d.Links.Link(ctx, userID, steamID); {
	case errors.Is(err, store.ErrSteamIDTaken):
		d.redirectResult(c, "steam_id_taken")
		return
	case errors.Is(err, store.ErrAlreadyLinked):
		d.redirectResult(c, "already_linked")
		return
	case err != nil:
		d.Logger.Error("建立绑定失败",
			logging.SteamID(steamID), slog.String("err", err.Error()))
		d.redirectResult(c, "internal_error")
		return
	}

	status := d.probeAndPersist(c, steamID)
	d.redirectResult(c, status.Visibility)
}

// redirectResult 把结果作为 query 参数带回前端的绑定结果页。
func (d Deps) redirectResult(c *gin.Context, status string) {
	c.Redirect(http.StatusFound,
		d.BaseURL+"/settings/steam?status="+url.QueryEscape(status))
}

// handleRecheck 供「我已修改隐私设置，重新检测」按钮调用。
func (d Deps) handleRecheck(c *gin.Context) {
	userID, ok := d.currentUserID(c)
	if !ok {
		return
	}

	link, err := d.Links.ByUserID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Code: "not_linked", Message: "尚未绑定 Steam 账号"})
		return
	}

	c.JSON(http.StatusOK, d.probeAndPersist(c, link.SteamID))
}

func (d Deps) handleUnlink(c *gin.Context) {
	userID, ok := d.currentUserID(c)
	if !ok {
		return
	}
	if err := d.Links.Unlink(c.Request.Context(), userID); err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Code: "not_linked", Message: "尚未绑定 Steam 账号"})
		return
	}
	c.Status(http.StatusNoContent)
}

// probeAndPersist 同步探测隐私并落库游戏库，让用户立刻看到结果，
// 而不是绑定完面对空白页面无从判断原因。
func (d Deps) probeAndPersist(c *gin.Context, steamID uint64) LinkStatusResponse {
	ctx := c.Request.Context()
	now := time.Now().UTC()

	state, games, err := ProbeVisibility(ctx, d.Steam, steamID)
	if err != nil {
		return LinkStatusResponse{SteamID: steamID, Visibility: "unknown",
			Hint: "暂时无法连接 Steam，请稍后重新检测"}
	}

	_ = d.Links.UpdateVisibility(ctx, steamID, state)

	if state == store.VisibilityOK && len(games) > 0 {
		_ = d.Games.UpsertApps(ctx, games)
		_ = d.Games.UpsertUserGames(ctx, steamID, games, now)
		// 初始化探针状态，让新用户立即进入采集范围
		_ = d.Probes.Ensure(ctx, steamID, now)

		// 全库成就回填，低优先级后台慢速执行
		appIDs := make([]uint32, 0, len(games))
		for _, g := range games {
			appIDs = append(appIDs, g.AppID)
		}
		if err := collector.EnqueueBackfill(ctx, d.Tasks, steamID, appIDs, now); err != nil {
			d.Logger.Error("入队成就回填失败",
				logging.SteamID(steamID), slog.String("err", err.Error()))
		}
	}

	slug, hint := visibilityHint(state)
	return LinkStatusResponse{
		SteamID:    steamID,
		Visibility: slug,
		GameCount:  len(games),
		Hint:       hint,
	}
}
