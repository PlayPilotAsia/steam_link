package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"steamlink/internal/auth"
	"steamlink/internal/logging"
	"steamlink/internal/store"
)

// SessionCookieName 是承载本站登录态的 Cookie 名。
//
// 必须用 Cookie 而非 Authorization 头：OpenID 流程包含两次浏览器顶层导航
//（本站 → Steam、Steam → 本站），顶层导航无法携带自定义请求头，
// 用 fetch 又会撞上跨域重定向。Cookie 是唯一能贯穿整个流程的载体。
const SessionCookieName = "steamlink_session"

// setSessionCookie 写入登录态 Cookie。
//
// SameSite 必须是 Lax 而非 Strict：Steam 回跳是一次跨站顶层 GET 导航，
// Strict 会拒绝携带 Cookie，导致回调时读不到用户身份、绑定必然失败。
// Lax 恰好允许跨站顶层 GET 携带 Cookie，同时仍能挡住跨站 POST 的 CSRF。
func (d Deps) setSessionCookie(c *gin.Context, token string, ttl time.Duration) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   strings.HasPrefix(d.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// currentUserID 从 Cookie 解析本站用户。
//
// 本项目假设已有账号体系：该体系在用户登录时调用 auth.SessionStore.Issue
// 签发 token 并通过 setSessionCookie 下发，本项目只负责消费。
func (d Deps) currentUserID(c *gin.Context) (uint64, bool) {
	tok, err := c.Cookie(SessionCookieName)
	if err != nil || tok == "" {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Code: "unauthorized", Message: "请先登录"})
		return 0, false
	}

	id, err := d.Auth.Resolve(c.Request.Context(), tok)
	if err != nil {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Code: "unauthorized", Message: "登录已过期，请重新登录"})
		return 0, false
	}
	return id, true
}

// handleLogin 发起 OpenID 跳转。
//
// 这是一个浏览器顶层导航入口（前端用 window.location 跳转过来），
// 因此失败时也必须重定向而非返回 JSON —— 用户看到的是页面，不是接口响应。
func (d Deps) handleLogin(c *gin.Context) {
	tok, err := c.Cookie(SessionCookieName)
	if err != nil || tok == "" {
		d.redirectResult(c, "unauthorized")
		return
	}
	userID, err := d.Auth.Resolve(c.Request.Context(), tok)
	if err != nil {
		d.redirectResult(c, "unauthorized")
		return
	}

	state := auth.SignState(d.StateSecret, userID, time.Now().UTC())
	returnTo := d.BaseURL + "/auth/steam/callback?state=" + url.QueryEscape(state)

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

// handleDevLogin 仅在 dev 环境注册，直接为指定 user_id 签发登录态。
// 生产环境由既有账号体系承担同样的职责（调用 Issue + setSessionCookie）。
func (d Deps) handleDevLogin(c *gin.Context) {
	var req struct {
		UserID uint64 `json:"user_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == 0 {
		c.JSON(http.StatusBadRequest, ErrorResponse{Code: "bad_request", Message: "缺少 user_id"})
		return
	}

	tok, err := d.Auth.Issue(c.Request.Context(), req.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Code: "internal", Message: "签发失败"})
		return
	}

	d.setSessionCookie(c, tok, d.SessionTTL)
	c.JSON(http.StatusOK, gin.H{"user_id": req.UserID})
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
	}

	slug, hint := visibilityHint(state)
	return LinkStatusResponse{
		SteamID:    steamID,
		Visibility: slug,
		GameCount:  len(games),
		Hint:       hint,
	}
}
