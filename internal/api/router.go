package api

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/PlayPilotAsia/steam_link/internal/auth"
	"github.com/PlayPilotAsia/steam_link/internal/steam"
	"github.com/PlayPilotAsia/steam_link/internal/store"
	"github.com/PlayPilotAsia/steam_link/internal/task"
)

type Deps struct {
	Links       *store.LinkRepo
	Games       *store.GameRepo
	Probes      *store.ProbeRepo
	Sessions    *store.SessionRepo // 游戏会话与成就解锁，勿与 Auth（登录态）混淆
	Tasks       task.Queue
	Steam       steam.Client
	Verifier    *auth.Verifier
	BaseURL     string
	StateSecret []byte
	Logger      *slog.Logger
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

	r.GET("/noauth/steam/login", d.handleLogin)
	r.GET("/noauth/steam/callback", d.handleCallback)

	api := r.Group("/api/steam")
	{
		api.POST("/link/recheck", d.handleRecheck)
		api.DELETE("/link", d.handleUnlink)
		api.GET("/library", d.handleLibrary)
		api.GET("/games/:appid/achievements", d.handleGameAchievements)
		api.GET("/achievements/recent", d.handleRecentAchievements)
	}
	return r
}
