package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"steamlink/internal/collector"
	"steamlink/internal/config"
	"steamlink/internal/logging"
	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

func configDir() string {
	if v := os.Getenv("CONFIG_DIR"); v != "" {
		return v
	}
	return "configs"
}

func main() {
	cfg, err := config.Load(configDir())
	if err != nil {
		os.Stderr.WriteString("配置加载失败: " + err.Error() + "\n")
		os.Exit(1)
	}

	lg := logging.New(cfg.Log.Level, cfg.Log.Format).With(
		slog.String("service", "worker"),
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

	queue := task.NewMySQLQueue(db)
	probes := store.NewProbeRepo(db)
	gameRepo := store.NewGameRepo(db)
	sessionRepo := store.NewSessionRepo(db)
	linkRepo := store.NewLinkRepo(db)

	prober := collector.NewProber(collector.ProberDeps{
		Steam: sc, Probes: probes, Tasks: queue, Logger: lg,
	})

	runner := task.NewRunner(queue, task.RunnerOptions{
		Concurrency:  cfg.Worker.Concurrency,
		PollInterval: cfg.Worker.PollInterval,
		Logger:       lg,
	})
	settler := collector.NewSettler(collector.SettlerDeps{
		Steam:    sc,
		Games:    gameRepo,
		Sessions: sessionRepo,
		Tasks:    queue,
	})
	guard := collector.NewRedisQuotaGuard(limiter)
	runner.Register(task.TypeSessionSettle,
		collector.WithQuotaGuard(guard, task.PriorityNormal, settler.Handle))

	schemaSyncer := collector.NewSchemaSyncer(collector.SchemaDeps{
		Steam: sc, Games: gameRepo, Tasks: queue,
	})
	runner.Register(task.TypeSchemaSync,
		collector.WithQuotaGuard(guard, task.PriorityNormal, schemaSyncer.Handle))

	achSyncer := collector.NewAchievementSyncer(collector.AchievementDeps{
		Steam: sc, Games: gameRepo, Sessions: sessionRepo,
		Links: linkRepo, Tasks: queue,
	})
	runner.Register(task.TypeAchievementSync,
		collector.WithQuotaGuard(guard, task.PriorityNormal, achSyncer.Handle))

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reconciler := collector.NewReconciler(collector.ReconcilerDeps{
		Steam:    sc,
		Games:    gameRepo,
		Sessions: sessionRepo,
		Links:    linkRepo,
		Tasks:    queue,
	})
	runner.Register(task.TypeLibrarySync,
		collector.WithQuotaGuard(guard, task.PriorityNormal, reconciler.Handle))

	// 启动自愈：结算 worker 宕机期间残留的僵尸会话
	healer := collector.NewHealer(probes, queue, nil)
	if err := healer.Run(ctx); err != nil {
		lg.Error("启动自愈失败", slog.String("err", err.Error()))
	}

	// 每日校准调度
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		if err := reconciler.ScheduleDaily(ctx); err != nil {
			lg.Error("每日校准调度失败", slog.String("err", err.Error()))
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := reconciler.ScheduleDaily(ctx); err != nil {
					lg.Error("每日校准调度失败", slog.String("err", err.Error()))
				}
			}
		}
	}()

	// 探针独立于任务队列，按固定节拍运行。
	// ticker 取 30 秒而非 2 分钟：next_probe_at 才是真正的节流阀，
	// ticker 只是驱动检查的节拍，更密的节拍能让分层中不同间隔的用户按时被采集。
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := prober.RunOnce(ctx); err != nil {
					lg.Warn("探针轮询失败", slog.String("err", err.Error()))
				}
			}
		}
	}()

	lg.Info("worker 已启动",
		slog.Int("concurrency", cfg.Worker.Concurrency))
	runner.Start(ctx)
	lg.Info("worker 已停止")
}
