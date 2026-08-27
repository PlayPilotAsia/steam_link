package main

import (
	"log/slog"
	"os"

	"steamlink/internal/api"
	"steamlink/internal/auth"
	"steamlink/internal/config"
	"steamlink/internal/logging"
	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

// configDir 可由 CONFIG_DIR 覆盖，便于容器中挂载到别处。
func configDir() string {
	if v := os.Getenv("CONFIG_DIR"); v != "" {
		return v
	}
	return "configs"
}

func main() {
	cfg, err := config.Load(configDir())
	if err != nil {
		// 此刻 Logger 尚未构造，配置错误用 stderr 直出并退出。
		// 这是全项目唯一允许绕过 slog 的地方。
		os.Stderr.WriteString("配置加载失败: " + err.Error() + "\n")
		os.Exit(1)
	}

	lg := logging.New(cfg.Log.Level, cfg.Log.Format).With(
		slog.String("service", "api"),
		slog.String("env", cfg.App.Env),
	)

	db, err := store.NewDB(cfg.MySQL.DSN(), lg)
	if err != nil {
		lg.Error("MySQL 连接失败", slog.String("err", err.Error()))
		os.Exit(1)
	}
	rdb, err := store.NewRedis(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		lg.Error("Redis 连接失败", slog.String("err", err.Error()))
		os.Exit(1)
	}

	limiter := steam.NewRedisLimiter(rdb, cfg.Steam.RatePerSec, cfg.Steam.Burst)
	sc := steam.New(cfg.Steam.APIKey, steam.WithLimiter(limiter))

	r := api.NewRouter(api.Deps{
		Links:       store.NewLinkRepo(db),
		Games:       store.NewGameRepo(db),
		Probes:      store.NewProbeRepo(db),
		Sessions:    store.NewSessionRepo(db),
		Tasks:       task.NewMySQLQueue(db),
		Steam:       sc,
		Verifier:    auth.NewVerifier(),
		Auth:        auth.NewSessionStore(rdb, cfg.Auth.SessionTTL),
		BaseURL:     cfg.HTTP.BaseURL,
		StateSecret: []byte(cfg.Auth.StateSecret),
		SessionTTL:  cfg.Auth.SessionTTL,
		// 本地登录端点只在非生产环境开放：local 与 test 都是开发者自己
		// 在本机运行，区别只是连哪套数据库。
		DevMode: cfg.App.Env != config.EnvProd,
		Logger:  lg,
	})

	lg.Info("API 启动", slog.String("addr", cfg.HTTP.Addr),
		slog.String("base_url", cfg.HTTP.BaseURL))

	if err := r.Run(cfg.HTTP.Addr); err != nil {
		lg.Error("HTTP 服务退出", slog.String("err", err.Error()))
		os.Exit(1)
	}
}
