package api

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"steamlink/internal/auth"
	"steamlink/internal/steam"
	"steamlink/internal/store"
)

type Deps struct {
	Links       *store.LinkRepo
	Games       *store.GameRepo
	Steam       steam.Client
	Verifier    *auth.Verifier
	Auth        *auth.SessionStore // 登录态，勿与 Task 17 的 Sessions（游戏会话）混淆
	BaseURL     string
	StateSecret []byte
	SessionTTL  time.Duration
	DevMode     bool // 仅 dev 环境为 true，用于开放本地登录端点
	Logger      *slog.Logger
	// Probes（*store.ProbeRepo）由 Task 11 加入；
	// Tasks（task.Queue）与 Sessions（*store.SessionRepo）由 Task 17 加入
}

func NewRouter(d Deps) *gin.Engine {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	d.Logger = d.Logger.With("component", "api")

	// gin 默认的 Logger 中间件直接写 stdout，绕过项目日志规范，因此不使用。
	// 用 gin.New() 而非 gin.Default()，只保留 Recovery。
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/auth/steam/login", d.handleLogin)
	r.GET("/auth/steam/callback", d.handleCallback)

	// 本站登录态由既有账号体系签发：它在用户登录时调用
	// auth.SessionStore.Issue 拿到 token，再用 d.setSessionCookie 下发。
	// 开发环境提供一个直接签发的端点，便于本地跑通整条绑定流程。
	if d.DevMode {
		r.POST("/dev/login", d.handleDevLogin)
	}

	api := r.Group("/api")
	{
		api.POST("/link/recheck", d.handleRecheck)
		api.DELETE("/link", d.handleUnlink)
		api.GET("/library", d.handleLibrary)
	}
	return r
}
