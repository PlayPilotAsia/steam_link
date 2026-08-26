# Steam 账号关联与游戏数据采集 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建一个 Go 服务，让用户通过 Steam OpenID 绑定账号，并持续采集其游戏库、全库成就与带起止时刻的游戏会话事件流。

**Architecture:** 双进程（Gin API + 采集 worker）共享 MySQL 8 与 Redis 7。采集采用四层管线：`GetPlayerSummaries` 批量探针（100 人/次）驱动一个纯函数会话状态机，状态机产出的事件经 MySQL 本地事务表异步下钻到时长结算与成就同步，每日 `GetOwnedGames` 全量校准兜底。不使用消息队列，异步与补偿全部由本地事务表 + `SELECT ... FOR UPDATE SKIP LOCKED` + 租约超时承担。

**Tech Stack:** Go 1.24、Gin、GORM、MySQL 8、Redis 7、spf13/viper、log/slog、testify

**设计依据:** 本计划实现 `docs/01-design.md`。任务中引用设计文档章节号处，实现前应先读该章节。

## Global Constraints

- Go 版本 **1.24 或更高**；module 名为 `steamlink`，所有内部包导入路径形如 `steamlink/internal/steam`
- MySQL **8.0.1 或更高**（`SELECT ... FOR UPDATE SKIP LOCKED` 自该版本起支持，低于此版本整个任务表方案失效）
- 所有 MySQL 表使用 `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`。**禁止使用 `utf8`**（实为 utf8mb3），Steam 游戏名含 emoji 会写入失败
- 所有对外 JSON 中的 SteamID64 必须序列化为字符串：`json:"steam_id,string"`。SteamID64 约 7.6×10¹⁶，超过 JavaScript `Number.MAX_SAFE_INTEGER`（9.007×10¹⁵），以数字返回会在前端静默丢精度
- Steam API base URL 常量：`https://api.steampowered.com`；OpenID endpoint 常量：`https://steamcommunity.com/openid/login`
- 所有 Steam 时长字段单位为**分钟**；`unlocktime`、`rtime_last_played` 为 Unix 秒级时间戳
- **只有 `internal/steam` 包可以发起对 Steam 的 HTTP 请求。** 其他包一律通过 `steam.Client` 接口，禁止直接构造 Steam URL
- `internal/domain` 包**禁止**导入 `gorm.io/*`、`net/http`、`time.Now`。时钟一律作为参数传入
- 表结构由 `scripts/db/init.sql` 定义，**禁止使用 GORM AutoMigrate**（生成列、复合唯一键、`DECIMAL` 精度等 AutoMigrate 处理不正确）。后续变更放入 `scripts/db/migrations/`，按 `NNN_描述.sql` 命名，不修改 `init.sql`
- 配置放 `configs/*.yaml`，用 `spf13/viper` 加载。**敏感项（`steam.api_key`、`mysql.password`、`auth.state_secret`）在 YAML 中必须留空**，仅由 `STEAMLINK_*` 环境变量注入 —— `configs/` 会进仓库
- 日志一律使用标准库 `log/slog`。**禁止 `fmt.Printf`、`log.Printf`、`log.Fatalf`**，也禁止依赖 `slog.Default()`；`*slog.Logger` 通过依赖注入传递
- 日志中的 SteamID 用 `slog.String("steam_id", strconv.FormatUint(id, 10))` 输出，理由同上（日志采集链路多经 JSON，数字会丢精度）
- 每个任务结束时必须 `go vet ./...` 与 `go test ./...` 全绿才能提交

---

## 文件结构

```
go.mod                                  module steamlink
cmd/api/main.go                         Gin HTTP 服务入口
cmd/worker/main.go                      采集 worker 入口
configs/
  config.yaml                           基础配置，所有环境共享
  config.dev.yaml                       开发环境覆盖项
  config.prod.yaml                      生产环境覆盖项
scripts/
  db/
    init.sql                            全部建表 DDL
    migrations/                         后续增量变更（初期为空）
  dev/
    up.sh                               起本地依赖并初始化数据库
    test.sh                             跑全量测试
internal/
  config/config.go                      viper 加载、合并与校验
  logging/logging.go                    slog Logger 构造
  store/
    db.go                               GORM 连接构造
    redis.go                            Redis 客户端构造
    models.go                           GORM 模型定义（全部表）
    link_repo.go                        steam_links 读写
    game_repo.go                        user_games / apps / app_achievements 读写
    session_repo.go                     play_sessions / achievement_unlocks 写入
    probe_repo.go                       probe_state 读写
  steam/
    client.go                           Client 接口 + HTTP 实现
    errors.go                           哨兵错误与响应归一化
    types.go                            领域化的返回类型
    limiter.go                          Redis 令牌桶 + 配额守卫 + 熔断
  domain/
    session.go                          会话状态机（纯函数）
    tier.go                             分层规则（纯函数）
  task/
    task.go                             任务类型与状态常量、Task 结构体
    queue.go                            入队、领取、成功、失败
    runner.go                           worker 主循环与 handler 注册
  collector/
    probe.go                            L0 探针调度器
    settle.go                           L1 会话结算 handler
    achievement.go                      L2 成就下钻 handler
    schema.go                           Schema 同步 handler
    reconcile.go                        L3 每日校准 handler
    heal.go                             worker 启动自愈
  auth/
    openid.go                           OpenID 2.0 构造与验证
    session.go                          Redis 登录会话
  api/
    router.go                           Gin 路由注册
    auth_handler.go                     登录、回调、绑定、解绑
    library_handler.go                  游戏库与成就查询
    dto.go                              响应结构体
```

**分包依据：** 按职责而非技术分层切分。`internal/steam` 是唯一网络出口，`internal/domain` 是唯一无 IO 的纯逻辑包 —— 这两条边界是可测试性的基础，任何任务都不得跨越。`collector` 下每个文件对应管线的一层，它们变更的原因各不相同，因此分开。

---

## Task 1: 项目骨架、配置、日志与数据库初始化

**Files:**
- Create: `go.mod`, `docker-compose.yml`
- Create: `configs/config.yaml`, `configs/config.dev.yaml`, `configs/config.prod.yaml`
- Create: `internal/config/config.go`, `internal/logging/logging.go`
- Create: `internal/store/db.go`, `internal/store/redis.go`
- Create: `scripts/db/init.sql`, `scripts/dev/up.sh`, `scripts/dev/test.sh`
- Test: `internal/config/config_test.go`, `internal/logging/logging_test.go`

**Interfaces:**
- Consumes: 无（首个任务）
- Produces:
  - `config.Config` 及其子结构：`AppConfig{Env string}`、`HTTPConfig{Addr, BaseURL string}`、`MySQLConfig{Host string, Port int, User, Password, Database string}`、`RedisConfig{Addr, Password string, DB int}`、`SteamConfig{APIKey string, RatePerSec, Burst int}`、`AuthConfig{StateSecret string, SessionTTL time.Duration}`、`WorkerConfig{Concurrency int, PollInterval time.Duration}`、`LogConfig{Level, Format string}`
  - `config.Load(dir string) (Config, error)` — 按 `APP_ENV` 合并 YAML 并叠加环境变量，校验失败返回错误
  - `(MySQLConfig).DSN() string`
  - `config.ErrMissingSecret`
  - `logging.New(level, format string) *slog.Logger`
  - `logging.SteamID(id uint64) slog.Attr` — 统一的 SteamID 日志属性
  - `store.NewDB(dsn string, lg *slog.Logger) (*gorm.DB, error)`
  - `store.NewRedis(addr, password string, db int) (*redis.Client, error)`
  - `scripts/db/init.sql` 中的全部表结构（后续所有任务的 GORM 模型必须与之一致）

- [ ] **Step 1: 初始化 module 与依赖**

```bash
cd /Users/aiden/coworkspace/steam_link
go mod init steamlink
go get gorm.io/gorm gorm.io/driver/mysql
go get github.com/redis/go-redis/v9
go get github.com/gin-gonic/gin
go get github.com/spf13/viper
go get github.com/stretchr/testify
```

- [ ] **Step 2: 写分环境配置文件**

创建 `configs/config.yaml`（基础配置，所有环境共享）：

```yaml
# 敏感项一律留空，由 STEAMLINK_* 环境变量注入。
# 本文件会提交到仓库，任何写在这里的密钥都等同于公开。
app:
  env: dev

http:
  addr: ":8080"
  # 站点根地址，派生出 OpenID 的两个参数：
  #   openid.realm     = {base_url}
  #   openid.return_to = {base_url}/auth/steam/callback?state=...
  # Steam 会校验 return_to 落在 realm 之下（同 scheme/host/port）。
  #
  # 它需要「用户的浏览器」能访问，而非「Steam 的服务器」能访问 ——
  # Steam 是用 302 把用户浏览器重定向回来的，不存在服务端回调。
  # 因此本地开发用 localhost 完全可行，无需内网穿透。
  base_url: "http://localhost:8080"

mysql:
  host: "127.0.0.1"
  port: 3306
  user: "root"
  password: ""          # ← STEAMLINK_MYSQL_PASSWORD
  database: "steamlink"

redis:
  addr: "127.0.0.1:6379"
  password: ""
  db: 0

steam:
  api_key: ""           # ← STEAMLINK_STEAM_API_KEY
  # 令牌桶参数。社区实测持续 1 req/s 绝对安全，突发 10+ 会触发 429，
  # 5 req/s 是留了余量的取值。
  rate_per_sec: 5
  burst: 20

auth:
  state_secret: ""      # ← STEAMLINK_AUTH_STATE_SECRET
  session_ttl: 24h

worker:
  concurrency: 4
  poll_interval: 2s

log:
  level: info
  format: json
```

创建 `configs/config.dev.yaml`：

```yaml
mysql:
  password: "root"

log:
  level: debug
  format: text
```

创建 `configs/config.prod.yaml`：

```yaml
# base_url 刻意留空：生产域名必须由 STEAMLINK_HTTP_BASE_URL 显式提供。
# 若沿用默认的 localhost，OpenID 回调会静默失败且难以排查。
http:
  base_url: ""

log:
  level: info
  format: json
```

> **`configs/` 必须加入 `.gitignore` 的例外管理**：本目录要提交，但绝不能有真实密钥。dev 环境的 `mysql.password: "root"` 是本地容器的固定密码，不构成泄漏。

- [ ] **Step 3: 写配置加载的测试**

创建 `internal/config/config_test.go`：

```go
package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 测试统一指向仓库中的真实配置目录，避免测试与生产读到不同的结构。
const testConfigDir = "../../configs"

func setSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("STEAMLINK_STEAM_API_KEY", "TESTKEY")
	t.Setenv("STEAMLINK_MYSQL_PASSWORD", "testpass")
	t.Setenv("STEAMLINK_AUTH_STATE_SECRET", "test-state-secret")
}

func TestLoad_MergesBaseAndEnvFiles(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	setSecrets(t)

	cfg, err := Load(testConfigDir)
	require.NoError(t, err)

	// 来自基础配置
	require.Equal(t, ":8080", cfg.HTTP.Addr)
	require.Equal(t, 3306, cfg.MySQL.Port)
	require.Equal(t, 5, cfg.Steam.RatePerSec)
	// 被 dev 覆盖
	require.Equal(t, "debug", cfg.Log.Level)
	require.Equal(t, "text", cfg.Log.Format)
}

// 环境变量优先级最高，覆盖两个 YAML。
func TestLoad_EnvOverridesYAML(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	setSecrets(t)
	t.Setenv("STEAMLINK_HTTP_ADDR", ":9999")
	t.Setenv("STEAMLINK_LOG_LEVEL", "warn")

	cfg, err := Load(testConfigDir)
	require.NoError(t, err)
	require.Equal(t, ":9999", cfg.HTTP.Addr)
	require.Equal(t, "warn", cfg.Log.Level)
}

// 这是 viper 最经典的坑：AutomaticEnv 只在 Get 时生效，
// Unmarshal 走 AllKeys，若某个 key 不在任何 YAML 中就读不到环境变量。
// 三个敏感项在 YAML 中留空正是为了让它们出现在 AllKeys 里，
// 同时实现里还要显式 BindEnv 作为双保险。此用例守住这个行为。
func TestLoad_SecretsComeFromEnvNotYAML(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	setSecrets(t)

	cfg, err := Load(testConfigDir)
	require.NoError(t, err)
	require.Equal(t, "TESTKEY", cfg.Steam.APIKey)
	require.Equal(t, "test-state-secret", cfg.Auth.StateSecret)
	require.Equal(t, "testpass", cfg.MySQL.Password)
}

// 缺失敏感项必须启动即失败，而不是等到第一次调用 Steam 才暴露。
func TestLoad_MissingSecretFails(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	t.Setenv("STEAMLINK_STEAM_API_KEY", "")
	t.Setenv("STEAMLINK_MYSQL_PASSWORD", "testpass")
	t.Setenv("STEAMLINK_AUTH_STATE_SECRET", "test-state-secret")

	_, err := Load(testConfigDir)
	require.ErrorIs(t, err, ErrMissingSecret)
	require.Contains(t, err.Error(), "STEAMLINK_STEAM_API_KEY")
}

// 生产环境必须显式提供 base_url 且为 https ——
// 沿用 localhost 会让 OpenID 回调静默失败，极难排查。
func TestLoad_ProdRequiresHTTPSBaseURL(t *testing.T) {
	t.Setenv("APP_ENV", "prod")
	setSecrets(t)

	_, err := Load(testConfigDir)
	require.Error(t, err, "prod 下 base_url 为空必须失败")

	t.Setenv("STEAMLINK_HTTP_BASE_URL", "http://example.com")
	_, err = Load(testConfigDir)
	require.Error(t, err, "prod 下必须是 https")

	t.Setenv("STEAMLINK_HTTP_BASE_URL", "https://example.com")
	cfg, err := Load(testConfigDir)
	require.NoError(t, err)
	require.Equal(t, "https://example.com", cfg.HTTP.BaseURL)
}

// duration 字符串必须能被解析成 time.Duration。
func TestLoad_ParsesDurations(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	setSecrets(t)

	cfg, err := Load(testConfigDir)
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, cfg.Auth.SessionTTL)
	require.Equal(t, 2*time.Second, cfg.Worker.PollInterval)
}

// DSN 必须强制带上 parseTime 与 UTC，不允许配置文件改动它们。
func TestMySQLDSN_ForcesParseTimeAndUTC(t *testing.T) {
	c := MySQLConfig{
		Host: "db.internal", Port: 3306,
		User: "app", Password: "pw", Database: "steamlink",
	}
	dsn := c.DSN()

	require.Contains(t, dsn, "app:pw@tcp(db.internal:3306)/steamlink")
	require.Contains(t, dsn, "parseTime=true")
	require.Contains(t, dsn, "loc=UTC")
	require.Contains(t, dsn, "charset=utf8mb4")
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/config/ -v`
Expected: FAIL —— `undefined: Load`

- [ ] **Step 5: 实现配置加载**

创建 `internal/config/config.go`：

```go
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// ErrMissingSecret 表示必需的敏感配置项为空。
var ErrMissingSecret = errors.New("config: required secret is empty")

// EnvPrefix 是环境变量前缀。steam.api_key 对应 STEAMLINK_STEAM_API_KEY。
const EnvPrefix = "STEAMLINK"

type Config struct {
	App    AppConfig    `mapstructure:"app"`
	HTTP   HTTPConfig   `mapstructure:"http"`
	MySQL  MySQLConfig  `mapstructure:"mysql"`
	Redis  RedisConfig  `mapstructure:"redis"`
	Steam  SteamConfig  `mapstructure:"steam"`
	Auth   AuthConfig   `mapstructure:"auth"`
	Worker WorkerConfig `mapstructure:"worker"`
	Log    LogConfig    `mapstructure:"log"`
}

type AppConfig struct {
	Env string `mapstructure:"env"`
}

type HTTPConfig struct {
	Addr    string `mapstructure:"addr"`
	BaseURL string `mapstructure:"base_url"`
}

type MySQLConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	Database string `mapstructure:"database"`
}

// DSN 拼装连接串。
//
// parseTime=true 与 loc=UTC 是硬编码的、不可配置的：
// 前者缺失会导致 DATETIME 列无法扫描进 time.Time，
// 后者缺失会让 worker 与数据库时区不一致，直接产生错误的会话时刻。
// 这是正确性要求，不是可调项。
func (c MySQLConfig) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&loc=UTC&charset=utf8mb4",
		c.User, c.Password, c.Host, c.Port, c.Database)
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type SteamConfig struct {
	APIKey     string `mapstructure:"api_key"`
	RatePerSec int    `mapstructure:"rate_per_sec"`
	Burst      int    `mapstructure:"burst"`
}

type AuthConfig struct {
	StateSecret string        `mapstructure:"state_secret"`
	SessionTTL  time.Duration `mapstructure:"session_ttl"`
}

type WorkerConfig struct {
	Concurrency  int           `mapstructure:"concurrency"`
	PollInterval time.Duration `mapstructure:"poll_interval"`
}

type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
}

// secretBindings 是必须由环境变量提供的敏感项。
// 显式 BindEnv 而非只依赖 AutomaticEnv：后者只在 Get 时生效，
// Unmarshal 走 AllKeys，一旦某个 key 不在 YAML 中就会被静默跳过。
var secretBindings = map[string]string{
	"steam.api_key":     EnvPrefix + "_STEAM_API_KEY",
	"mysql.password":    EnvPrefix + "_MYSQL_PASSWORD",
	"auth.state_secret": EnvPrefix + "_AUTH_STATE_SECRET",
}

// Load 按三层优先级加载配置：
//
//	configs/config.yaml        基础值
//	configs/config.{env}.yaml  环境覆盖
//	STEAMLINK_* 环境变量        最高优先级
//
// env 取自 APP_ENV，缺省为 dev。
func Load(dir string) (Config, error) {
	env := os.Getenv("APP_ENV")
	if env == "" {
		env = "dev"
	}

	v := viper.New()
	v.SetConfigType("yaml")
	v.AddConfigPath(dir)

	v.SetConfigName("config")
	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("config: 读取基础配置失败: %w", err)
	}

	v.SetConfigName("config." + env)
	if err := v.MergeInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) && !os.IsNotExist(err) {
			return Config{}, fmt.Errorf("config: 合并 %s 环境配置失败: %w", env, err)
		}
		// 环境专属文件可以不存在，此时仅使用基础配置
	}

	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	for key, envName := range secretBindings {
		if err := v.BindEnv(key, envName); err != nil {
			return Config{}, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: 解析配置失败: %w", err)
	}
	cfg.App.Env = env

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate 在启动时一次性暴露所有配置问题。
// 启动即失败远好于运行时才发现 —— 后者会让服务带着残缺配置对外提供服务。
func (c Config) validate() error {
	for key, envName := range secretBindings {
		var val string
		switch key {
		case "steam.api_key":
			val = c.Steam.APIKey
		case "mysql.password":
			val = c.MySQL.Password
		case "auth.state_secret":
			val = c.Auth.StateSecret
		}
		if val == "" {
			return fmt.Errorf("%w: %s（请设置环境变量 %s）", ErrMissingSecret, key, envName)
		}
	}

	if c.App.Env == "prod" {
		if c.HTTP.BaseURL == "" {
			return fmt.Errorf("config: prod 环境必须设置 %s_HTTP_BASE_URL", EnvPrefix)
		}
		if !strings.HasPrefix(c.HTTP.BaseURL, "https://") {
			return fmt.Errorf("config: prod 环境的 http.base_url 必须是 https，当前为 %q", c.HTTP.BaseURL)
		}
	}

	if c.Steam.RatePerSec <= 0 || c.Steam.Burst <= 0 {
		return fmt.Errorf("config: steam.rate_per_sec 与 steam.burst 必须为正数")
	}
	if c.Worker.Concurrency <= 0 {
		return fmt.Errorf("config: worker.concurrency 必须为正数")
	}
	return nil
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/config/ -v`
Expected: PASS（7 个用例）

若 `TestLoad_SecretsComeFromEnvNotYAML` 失败，检查 `configs/config.yaml` 中三个敏感 key 是否确实存在（值为空字符串），以及 `BindEnv` 是否对全部三项都调用了。

- [ ] **Step 7: 写日志包的测试与实现**

创建 `internal/logging/logging_test.go`：

```go
package logging

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNew_RespectsLevelAndFormat(t *testing.T) {
	lg := New("debug", "text")
	require.True(t, lg.Enabled(nil, slog.LevelDebug))

	lg = New("warn", "json")
	require.False(t, lg.Enabled(nil, slog.LevelInfo))
	require.True(t, lg.Enabled(nil, slog.LevelWarn))
}

// 未知级别退化到 Info，而不是 panic 或静默丢弃全部日志。
func TestNew_UnknownLevelFallsBackToInfo(t *testing.T) {
	lg := New("verbose", "json")
	require.True(t, lg.Enabled(nil, slog.LevelInfo))
	require.False(t, lg.Enabled(nil, slog.LevelDebug))
}

// SteamID 必须以字符串输出：日志采集链路多经 JSON，
// 7.6×10^16 的数字会丢精度，变成一个不存在的账号。
func TestSteamID_IsString(t *testing.T) {
	attr := SteamID(76561197960287930)
	require.Equal(t, "steam_id", attr.Key)
	require.Equal(t, slog.KindString, attr.Value.Kind())
	require.Equal(t, "76561197960287930", attr.Value.String())
}
```

创建 `internal/logging/logging.go`：

```go
// Package logging 构造标准库 slog 的 Logger。
// 全项目禁止 fmt.Printf / log.Printf，也不依赖 slog.Default()：
// Logger 一律通过依赖注入传递，便于测试静默与附加组件标识。
package logging

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// New 按级别与格式构造 Logger。format 为 "text" 时用 TextHandler，
// 其余情况一律 JSONHandler（生产默认）。
func New(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// parseLevel 把配置字符串映射到 slog 级别，未知值退化到 Info。
func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// SteamID 返回统一的 SteamID 日志属性。
//
// 必须以字符串输出：SteamID64 约 7.6×10^16，超过 JavaScript 与多数
// JSON 日志处理链路的安全整数范围，以数字记录会静默丢精度。
func SteamID(id uint64) slog.Attr {
	return slog.String("steam_id", strconv.FormatUint(id, 10))
}
```

Run: `go test ./internal/logging/ -v`
Expected: PASS（3 个用例）

- [ ] **Step 8: 写 DDL 与开发脚本**

创建 `scripts/db/init.sql`，内容为 `docs/01-design.md` §5.1 的完整 DDL（八张表：`steam_links`、`apps`、`app_achievements`、`user_games`、`play_sessions`、`achievement_unlocks`、`probe_state`、`sync_tasks`），逐字照抄，不得改动字段名、类型或索引。文件开头加一行：

```sql
-- 表结构初始化。后续变更请新增 scripts/db/migrations/NNN_描述.sql，
-- 不要修改本文件 —— 否则已部署环境与新建环境会产生结构漂移。
```

创建空目录 `scripts/db/migrations/`（放一个 `.gitkeep`）。

创建 `docker-compose.yml`：

```yaml
services:
  mysql:
    image: mysql:8.4
    environment:
      MYSQL_ROOT_PASSWORD: root
      MYSQL_DATABASE: steamlink
    command: --character-set-server=utf8mb4 --collation-server=utf8mb4_0900_ai_ci
    ports: ["3306:3306"]
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "-h", "127.0.0.1", "-uroot", "-proot"]
      interval: 3s
      retries: 20
  redis:
    image: redis:7-alpine
    ports: ["6379:6379"]
```

创建 `scripts/dev/up.sh`：

```bash
#!/usr/bin/env bash
# 起本地依赖并初始化数据库。可重复执行。
set -euo pipefail

cd "$(dirname "$0")/../.."

docker compose up -d --wait

echo "==> 应用 scripts/db/init.sql"
docker compose exec -T mysql mysql -uroot -proot steamlink < scripts/db/init.sql

echo "==> 应用增量脚本"
for f in scripts/db/migrations/*.sql; do
  [ -e "$f" ] || continue
  echo "    $f"
  docker compose exec -T mysql mysql -uroot -proot steamlink < "$f"
done

echo "==> 完成"
```

创建 `scripts/dev/test.sh`：

```bash
#!/usr/bin/env bash
# 跑全量测试。集成测试需要本地 MySQL 与 Redis 已就绪。
set -euo pipefail

cd "$(dirname "$0")/../.."

go vet ./...
go test ./... "$@"
```

赋予执行权限：

```bash
chmod +x scripts/dev/up.sh scripts/dev/test.sh
```

> `init.sql` 中的 `CREATE TABLE` 建议写成 `CREATE TABLE IF NOT EXISTS`，让 `up.sh` 可以重复执行而不报错。

- [ ] **Step 9: 实现数据库与 Redis 连接**

创建 `internal/store/db.go`：

```go
package store

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// NewDB 构造 GORM 连接，并把 GORM 自身的日志桥接到 slog。
func NewDB(dsn string, lg *slog.Logger) (*gorm.DB, error) {
	return gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: newGormSlogLogger(lg),
		// 全部时间统一按 UTC 处理，避免 worker 与 DB 时区不一致导致会话时刻错乱
		NowFunc: nowUTC,
	})
}

func nowUTC() time.Time { return time.Now().UTC() }

// gormSlogLogger 把 GORM 的日志接口适配到 slog，
// 避免 GORM 绕过项目的日志规范直接写 stdout。
type gormSlogLogger struct {
	lg            *slog.Logger
	slowThreshold time.Duration
}

func newGormSlogLogger(lg *slog.Logger) gormlogger.Interface {
	return &gormSlogLogger{
		lg:            lg.With("component", "gorm"),
		slowThreshold: 200 * time.Millisecond,
	}
}

func (l *gormSlogLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }

func (l *gormSlogLogger) Info(ctx context.Context, msg string, args ...any) {
	l.lg.InfoContext(ctx, msg, slog.Any("args", args))
}

func (l *gormSlogLogger) Warn(ctx context.Context, msg string, args ...any) {
	l.lg.WarnContext(ctx, msg, slog.Any("args", args))
}

func (l *gormSlogLogger) Error(ctx context.Context, msg string, args ...any) {
	l.lg.ErrorContext(ctx, msg, slog.Any("args", args))
}

func (l *gormSlogLogger) Trace(ctx context.Context, begin time.Time,
	fc func() (string, int64), err error) {

	elapsed := time.Since(begin)
	sql, rows := fc()

	switch {
	case err != nil && err != gorm.ErrRecordNotFound:
		// ErrRecordNotFound 是正常的业务分支，不该记为错误
		l.lg.ErrorContext(ctx, "SQL 执行失败",
			slog.String("sql", sql), slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed), slog.String("err", err.Error()))
	case elapsed > l.slowThreshold:
		l.lg.WarnContext(ctx, "慢查询",
			slog.String("sql", sql), slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed))
	default:
		l.lg.DebugContext(ctx, "SQL",
			slog.String("sql", sql), slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed))
	}
}
```

创建 `internal/store/redis.go`：

```go
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedis 构造客户端并做一次连通性探测 ——
// Redis 承担限流闸门，连不上时应当启动即失败而非运行时才暴露。
func NewRedis(addr, password string, db int) (*redis.Client, error) {
	c := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("store: 连接 Redis 失败: %w", err)
	}
	return c, nil
}
```

- [ ] **Step 10: 启动依赖并验证建表**

```bash
./scripts/dev/up.sh
```

Expected: 输出 `==> 完成`，无错误

验证生成列与字符集确实生效：

```bash
docker compose exec mysql mysql -uroot -proot steamlink -e "SHOW CREATE TABLE steam_links\G"
```

Expected: 输出中包含 `active_steam_id ... GENERATED ALWAYS AS` 与 `utf8mb4_0900_ai_ci`。若生成列缺失，说明 MySQL 版本过低，停止后续任务并升级。

- [ ] **Step 11: 运行全部测试并提交**

```bash
STEAMLINK_STEAM_API_KEY=x STEAMLINK_MYSQL_PASSWORD=root \
STEAMLINK_AUTH_STATE_SECRET=x ./scripts/dev/test.sh
```

Expected: 全绿

```bash
git init && git add -A
git commit -m "feat: 项目骨架、YAML 分环境配置、slog 日志与数据库初始化"
```

---

## Task 2: Steam 客户端 —— 响应归一化与错误分类

这是全项目最关键的防御性代码。Steam 用 HTTP 200 返回失败语义（见设计文档 §2.3），归一化错了会导致上层静默处理错误数据。

**Files:**
- Create: `internal/steam/types.go`, `internal/steam/errors.go`, `internal/steam/client.go`
- Create: `internal/steam/testdata/` 下 5 个 fixture 文件
- Test: `internal/steam/client_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - 类型：`steam.PlayerSummary`、`steam.OwnedGame`、`steam.PlayerAchievement`、`steam.SchemaAchievement`、`steam.GameSchema`
  - 哨兵错误：`steam.ErrProfilePrivate`、`steam.ErrAppHasNoStats`、`steam.ErrRateLimited`
  - 接口 `steam.Client`（五个方法，签名见下）
  - 构造器 `steam.New(apiKey string, opts ...Option) *HTTPClient`
  - 选项 `steam.WithBaseURL(string) Option`、`steam.WithHTTPClient(*http.Client) Option`

- [ ] **Step 1: 定义类型与接口**

创建 `internal/steam/types.go`：

```go
package steam

import (
	"context"
	"time"
)

type PlayerSummary struct {
	SteamID                  uint64
	PersonaName              string
	Avatar                   string
	CommunityVisibilityState int    // 1=非公开 3=公开
	PersonaState             int    // 0=离线 1=在线 ...
	GameID                   uint32 // 0 表示当前不在玩游戏
	GameExtraInfo            string
}

type OwnedGame struct {
	AppID              uint32
	Name               string
	ImgIconURL         string
	PlaytimeForeverMin uint32
	Playtime2WeeksMin  uint32
	RtimeLastPlayed    time.Time // 零值表示从未游玩
}

type PlayerAchievement struct {
	APIName    string
	Achieved   bool
	UnlockTime time.Time // 仅 Achieved 为 true 时有意义
}

type SchemaAchievement struct {
	APIName     string
	DisplayName string
	Description string
	Icon        string
	IconGray    string
	Hidden      bool
}

type GameSchema struct {
	AppID        uint32
	Name         string
	Achievements []SchemaAchievement
}

// Client 是访问 Steam 的唯一抽象。其他包不得自行构造 Steam 请求。
type Client interface {
	GetPlayerSummaries(ctx context.Context, ids []uint64) ([]PlayerSummary, error)
	GetOwnedGames(ctx context.Context, id uint64) ([]OwnedGame, error)
	GetRecentlyPlayedGames(ctx context.Context, id uint64) ([]OwnedGame, error)
	GetPlayerAchievements(ctx context.Context, id uint64, appID uint32) ([]PlayerAchievement, error)
	GetSchemaForGame(ctx context.Context, appID uint32) (GameSchema, error)
}
```

- [ ] **Step 2: 准备真实畸形响应 fixture**

创建以下五个文件，内容为 Steam 实际返回的响应体：

`internal/steam/testdata/owned_games_private.json`：
```json
{"response":{}}
```

`internal/steam/testdata/achievements_profile_private.json`：
```json
{"playerstats":{"error":"Profile is not public","success":false}}
```

`internal/steam/testdata/achievements_no_stats.json`：
```json
{"playerstats":{"error":"Requested app has no stats","success":false}}
```

`internal/steam/testdata/summaries_mixed.json`（第一个玩家在玩游戏，第二个不在玩 —— 注意第二个对象**没有** `gameid` 字段，这正是需要正确处理的形态）：
```json
{"response":{"players":[
  {"steamid":"76561197960287930","communityvisibilitystate":3,"personaname":"Player One","avatar":"https://a.example/1.jpg","personastate":1,"gameid":"440","gameextrainfo":"Team Fortress 2"},
  {"steamid":"76561197960287931","communityvisibilitystate":3,"personaname":"Player Two","avatar":"https://a.example/2.jpg","personastate":1}
]}}
```

`internal/steam/testdata/owned_games_emoji.json`（游戏名含 emoji 与全角字符，用于验证 utf8mb4 链路）：
```json
{"response":{"game_count":2,"games":[
  {"appid":620,"name":"Portal 2 🧪","playtime_forever":1234,"playtime_2weeks":60,"img_icon_url":"abc123","rtime_last_played":1756180800},
  {"appid":730,"name":"反恐精英：全球攻势 ⚡","playtime_forever":5000,"img_icon_url":"def456","rtime_last_played":0}
]}}
```

- [ ] **Step 3: 写错误分类的失败测试**

创建 `internal/steam/client_test.go`：

```go
package steam

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func serveFixture(t *testing.T, name string, status int) *HTTPClient {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return New("testkey", WithBaseURL(srv.URL))
}

// 游戏详情私密时 Steam 返回 HTTP 200 + 空对象，必须归一化为 ErrProfilePrivate，
// 而不是「成功但游戏数为 0」。
func TestGetOwnedGames_PrivateProfileIsError(t *testing.T) {
	c := serveFixture(t, "owned_games_private.json", 200)

	_, err := c.GetOwnedGames(context.Background(), 76561197960287930)
	require.ErrorIs(t, err, ErrProfilePrivate)
}

func TestGetPlayerAchievements_ProfilePrivate(t *testing.T) {
	c := serveFixture(t, "achievements_profile_private.json", 200)

	_, err := c.GetPlayerAchievements(context.Background(), 76561197960287930, 440)
	require.ErrorIs(t, err, ErrProfilePrivate)
}

// 「该游戏没有成就系统」必须是一个与隐私墙不同的错误 —— 上层据此永久跳过该游戏。
// 若二者混淆，无成就的游戏会陷入无限重试并持续消耗配额。
func TestGetPlayerAchievements_AppHasNoStats(t *testing.T) {
	c := serveFixture(t, "achievements_no_stats.json", 200)

	_, err := c.GetPlayerAchievements(context.Background(), 76561197960287930, 440)
	require.ErrorIs(t, err, ErrAppHasNoStats)
	require.False(t, errors.Is(err, ErrProfilePrivate), "两类错误必须可区分")
}

func TestGetPlayerSummaries_MissingGameIDMeansNotPlaying(t *testing.T) {
	c := serveFixture(t, "summaries_mixed.json", 200)

	got, err := c.GetPlayerSummaries(context.Background(),
		[]uint64{76561197960287930, 76561197960287931})
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, uint32(440), got[0].GameID)
	require.Equal(t, uint32(0), got[1].GameID, "缺失 gameid 字段应为 0，表示不在玩")
}

func TestGetOwnedGames_ParsesEmojiAndTimestamps(t *testing.T) {
	c := serveFixture(t, "owned_games_emoji.json", 200)

	got, err := c.GetOwnedGames(context.Background(), 76561197960287930)
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, "Portal 2 🧪", got[0].Name)
	require.Equal(t, uint32(1234), got[0].PlaytimeForeverMin)
	require.Equal(t, int64(1756180800), got[0].RtimeLastPlayed.Unix())
	require.True(t, got[1].RtimeLastPlayed.IsZero(), "rtime 为 0 应转为零值时间")
}

func TestRateLimitedMapsToSentinel(t *testing.T) {
	c := serveFixture(t, "owned_games_private.json", http.StatusTooManyRequests)

	_, err := c.GetOwnedGames(context.Background(), 76561197960287930)
	require.ErrorIs(t, err, ErrRateLimited)
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/steam/ -v`
Expected: FAIL —— `undefined: New`、`undefined: ErrProfilePrivate`

- [ ] **Step 5: 实现哨兵错误**

创建 `internal/steam/errors.go`：

```go
package steam

import (
	"errors"
	"fmt"
)

var (
	// ErrProfilePrivate 表示用户的资料或游戏详情未公开。上层应停止该用户的同步
	// 并引导其修改隐私设置，不应重试。
	ErrProfilePrivate = errors.New("steam: profile is not public")

	// ErrAppHasNoStats 表示该游戏没有成就系统。上层应把该 appid 永久标记为无成就，
	// 并将任务置为成功 —— 这不是失败。
	ErrAppHasNoStats = errors.New("steam: app has no achievement stats")

	// ErrRateLimited 表示触发了 Steam 的速率限制，上层应全局退避。
	ErrRateLimited = errors.New("steam: rate limited")
)

// classifyPlayerStatsError 把 playerstats.error 的文案映射到哨兵错误。
// Steam 用 HTTP 200 + success:false 表达这些失败，文案是唯一的判别依据。
func classifyPlayerStatsError(msg string) error {
	switch {
	case containsFold(msg, "not public"):
		return ErrProfilePrivate
	case containsFold(msg, "no stats"):
		return ErrAppHasNoStats
	default:
		return fmt.Errorf("steam: playerstats failed: %s", msg)
	}
}
```

补上 `containsFold` 辅助函数（大小写不敏感包含）：

```go
import "strings"

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
```

- [ ] **Step 6: 实现 HTTP 客户端**

创建 `internal/steam/client.go`：

```go
package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.steampowered.com"

// MaxSummariesBatch 是 GetPlayerSummaries 单次请求的 SteamID 上限，由 Steam 规定。
const MaxSummariesBatch = 100

type HTTPClient struct {
	apiKey  string
	baseURL string
	hc      *http.Client
}

type Option func(*HTTPClient)

func WithBaseURL(u string) Option        { return func(c *HTTPClient) { c.baseURL = u } }
func WithHTTPClient(h *http.Client) Option { return func(c *HTTPClient) { c.hc = h } }

func New(apiKey string, opts ...Option) *HTTPClient {
	c := &HTTPClient{
		apiKey:  apiKey,
		baseURL: DefaultBaseURL,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// getJSON 发起请求并解码。HTTP 层的失败在此统一归一化。
func (c *HTTPClient) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	q.Set("key", c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("steam: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		// Steam 对私密资料的部分接口直接返回 401/403
		return ErrProfilePrivate
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("steam: unexpected status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// ---------- GetPlayerSummaries ----------

type rawSummaries struct {
	Response struct {
		Players []struct {
			SteamID                  string `json:"steamid"`
			PersonaName              string `json:"personaname"`
			Avatar                   string `json:"avatar"`
			CommunityVisibilityState int    `json:"communityvisibilitystate"`
			PersonaState             int    `json:"personastate"`
			// gameid 在不玩游戏时字段缺失，且类型为字符串
			GameID        string `json:"gameid"`
			GameExtraInfo string `json:"gameextrainfo"`
		} `json:"players"`
	} `json:"response"`
}

func (c *HTTPClient) GetPlayerSummaries(ctx context.Context, ids []uint64) ([]PlayerSummary, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxSummariesBatch {
		return nil, fmt.Errorf("steam: batch size %d exceeds limit %d", len(ids), MaxSummariesBatch)
	}

	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(id, 10)
	}
	q := url.Values{"steamids": {strings.Join(parts, ",")}}

	var raw rawSummaries
	if err := c.getJSON(ctx, "/ISteamUser/GetPlayerSummaries/v0002/", q, &raw); err != nil {
		return nil, err
	}

	out := make([]PlayerSummary, 0, len(raw.Response.Players))
	for _, p := range raw.Response.Players {
		sid, _ := strconv.ParseUint(p.SteamID, 10, 64)
		gid, _ := strconv.ParseUint(p.GameID, 10, 32) // 缺失时为空串，解析得 0
		out = append(out, PlayerSummary{
			SteamID:                  sid,
			PersonaName:              p.PersonaName,
			Avatar:                   p.Avatar,
			CommunityVisibilityState: p.CommunityVisibilityState,
			PersonaState:             p.PersonaState,
			GameID:                   uint32(gid),
			GameExtraInfo:            p.GameExtraInfo,
		})
	}
	return out, nil
}

// ---------- GetOwnedGames / GetRecentlyPlayedGames ----------

type rawGames struct {
	Response struct {
		GameCount *int `json:"game_count"` // 指针：用于区分「字段缺失」与「值为 0」
		Games     []struct {
			AppID           uint32 `json:"appid"`
			Name            string `json:"name"`
			ImgIconURL      string `json:"img_icon_url"`
			PlaytimeForever uint32 `json:"playtime_forever"`
			Playtime2Weeks  uint32 `json:"playtime_2weeks"`
			RtimeLastPlayed int64  `json:"rtime_last_played"`
		} `json:"games"`
	} `json:"response"`
}

func (r rawGames) toDomain() []OwnedGame {
	out := make([]OwnedGame, 0, len(r.Response.Games))
	for _, g := range r.Response.Games {
		var last time.Time
		if g.RtimeLastPlayed > 0 {
			last = time.Unix(g.RtimeLastPlayed, 0).UTC()
		}
		out = append(out, OwnedGame{
			AppID:              g.AppID,
			Name:               g.Name,
			ImgIconURL:         g.ImgIconURL,
			PlaytimeForeverMin: g.PlaytimeForever,
			Playtime2WeeksMin:  g.Playtime2Weeks,
			RtimeLastPlayed:    last,
		})
	}
	return out
}

func (c *HTTPClient) GetOwnedGames(ctx context.Context, id uint64) ([]OwnedGame, error) {
	q := url.Values{
		"steamid":                  {strconv.FormatUint(id, 10)},
		"include_appinfo":          {"1"},
		"include_played_free_games": {"1"},
	}

	var raw rawGames
	if err := c.getJSON(ctx, "/IPlayerService/GetOwnedGames/v0001/", q, &raw); err != nil {
		return nil, err
	}

	// 关键：游戏详情非公开时 Steam 返回 {"response":{}}，game_count 字段整个缺失。
	// 一个真正拥有 0 款游戏的公开账号会返回 "game_count":0。用指针区分二者。
	if raw.Response.GameCount == nil {
		return nil, ErrProfilePrivate
	}
	return raw.toDomain(), nil
}

func (c *HTTPClient) GetRecentlyPlayedGames(ctx context.Context, id uint64) ([]OwnedGame, error) {
	q := url.Values{"steamid": {strconv.FormatUint(id, 10)}}

	var raw rawGames
	if err := c.getJSON(ctx, "/IPlayerService/GetRecentlyPlayedGames/v0001/", q, &raw); err != nil {
		return nil, err
	}
	if raw.Response.GameCount == nil {
		return nil, ErrProfilePrivate
	}
	return raw.toDomain(), nil
}

// ---------- GetPlayerAchievements ----------

type rawPlayerAch struct {
	PlayerStats struct {
		Success      bool   `json:"success"`
		Error        string `json:"error"`
		Achievements []struct {
			APIName    string `json:"apiname"`
			Achieved   int    `json:"achieved"`
			UnlockTime int64  `json:"unlocktime"`
		} `json:"achievements"`
	} `json:"playerstats"`
}

func (c *HTTPClient) GetPlayerAchievements(ctx context.Context, id uint64, appID uint32) ([]PlayerAchievement, error) {
	q := url.Values{
		"steamid": {strconv.FormatUint(id, 10)},
		"appid":   {strconv.FormatUint(uint64(appID), 10)},
		"l":       {"schinese"},
	}

	var raw rawPlayerAch
	if err := c.getJSON(ctx, "/ISteamUserStats/GetPlayerAchievements/v0001/", q, &raw); err != nil {
		return nil, err
	}
	if !raw.PlayerStats.Success {
		return nil, classifyPlayerStatsError(raw.PlayerStats.Error)
	}

	out := make([]PlayerAchievement, 0, len(raw.PlayerStats.Achievements))
	for _, a := range raw.PlayerStats.Achievements {
		var ut time.Time
		if a.UnlockTime > 0 {
			ut = time.Unix(a.UnlockTime, 0).UTC()
		}
		out = append(out, PlayerAchievement{
			APIName:    a.APIName,
			Achieved:   a.Achieved == 1,
			UnlockTime: ut,
		})
	}
	return out, nil
}

// ---------- GetSchemaForGame ----------

type rawSchema struct {
	Game struct {
		GameName           string `json:"gameName"`
		AvailableGameStats struct {
			Achievements []struct {
				Name        string `json:"name"` // 即 apiname
				DisplayName string `json:"displayName"`
				Description string `json:"description"`
				Icon        string `json:"icon"`
				IconGray    string `json:"icongray"`
				Hidden      int    `json:"hidden"`
			} `json:"achievements"`
		} `json:"availableGameStats"`
	} `json:"game"`
}

func (c *HTTPClient) GetSchemaForGame(ctx context.Context, appID uint32) (GameSchema, error) {
	q := url.Values{
		"appid": {strconv.FormatUint(uint64(appID), 10)},
		"l":     {"schinese"},
	}

	var raw rawSchema
	if err := c.getJSON(ctx, "/ISteamUserStats/GetSchemaForGame/v2/", q, &raw); err != nil {
		return GameSchema{}, err
	}

	s := GameSchema{AppID: appID, Name: raw.Game.GameName}
	for _, a := range raw.Game.AvailableGameStats.Achievements {
		s.Achievements = append(s.Achievements, SchemaAchievement{
			APIName:     a.Name,
			DisplayName: a.DisplayName,
			Description: a.Description,
			Icon:        a.Icon,
			IconGray:    a.IconGray,
			Hidden:      a.Hidden == 1,
		})
	}
	return s, nil
}

// 编译期断言：HTTPClient 必须满足 Client 接口
var _ Client = (*HTTPClient)(nil)
```

- [ ] **Step 7: 运行测试确认全部通过**

Run: `go test ./internal/steam/ -v`
Expected: PASS（6 个用例）

特别确认 `TestGetOwnedGames_PrivateProfileIsError` 通过 —— 它验证了 `game_count` 用指针区分「缺失」与「0」这个关键细节。

- [ ] **Step 8: 提交**

```bash
git add internal/steam/
git commit -m "feat(steam): API 客户端与三类错误归一化"
```

---

## Task 3: Redis 令牌桶限流与配额守卫

**Files:**
- Create: `internal/steam/limiter.go`
- Modify: `internal/steam/client.go`（在 `getJSON` 中接入限流）
- Test: `internal/steam/limiter_test.go`

**Interfaces:**
- Consumes: `steam.ErrRateLimited`（Task 2）
- Produces:
  - `steam.Limiter` 接口：`Acquire(ctx context.Context) error`
  - `steam.NewRedisLimiter(rdb *redis.Client, ratePerSec, burst int) *RedisLimiter`
  - `(*RedisLimiter).QuotaUsed(ctx) (int64, error)` — 返回当日已用调用数
  - `(*RedisLimiter).TripBreaker(ctx, d time.Duration) error` — 置全局熔断标志
  - 选项 `steam.WithLimiter(Limiter) Option`
  - 错误 `steam.ErrQuotaExhausted`、`steam.ErrCircuitOpen`

- [ ] **Step 1: 写限流器测试**

创建 `internal/steam/limiter_test.go`：

```go
package steam

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	require.NoError(t, c.Ping(context.Background()).Err(),
		"需要本地 Redis：docker compose up -d redis")
	require.NoError(t, c.FlushDB(context.Background()).Err())
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// 桶容量耗尽后必须拒绝，而不是无限放行。
func TestRedisLimiter_BurstThenReject(t *testing.T) {
	l := NewRedisLimiter(testRedis(t), 1, 3) // 1 req/s，桶容量 3
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, l.Acquire(ctx), "前 3 次应放行（burst）")
	}
	require.ErrorIs(t, l.Acquire(ctx), ErrRateLimited, "第 4 次应被限流")
}

// 令牌按速率回填。
func TestRedisLimiter_Refills(t *testing.T) {
	l := NewRedisLimiter(testRedis(t), 10, 1) // 10 req/s
	ctx := context.Background()

	require.NoError(t, l.Acquire(ctx))
	require.ErrorIs(t, l.Acquire(ctx), ErrRateLimited)

	time.Sleep(150 * time.Millisecond) // 10 req/s 下 100ms 回填 1 个
	require.NoError(t, l.Acquire(ctx), "回填后应放行")
}

// 熔断期间一律拒绝，与令牌数无关。
func TestRedisLimiter_CircuitBreaker(t *testing.T) {
	l := NewRedisLimiter(testRedis(t), 100, 100)
	ctx := context.Background()

	require.NoError(t, l.Acquire(ctx))
	require.NoError(t, l.TripBreaker(ctx, 2*time.Second))
	require.ErrorIs(t, l.Acquire(ctx), ErrCircuitOpen)
}

// 每次放行都要计入当日配额。
func TestRedisLimiter_CountsQuota(t *testing.T) {
	l := NewRedisLimiter(testRedis(t), 100, 100)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, l.Acquire(ctx))
	}

	used, err := l.QuotaUsed(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(5), used)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/steam/ -run Limiter -v`
Expected: FAIL —— `undefined: NewRedisLimiter`

- [ ] **Step 3: 实现令牌桶**

创建 `internal/steam/limiter.go`：

```go
package steam

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	// ErrQuotaExhausted 表示当日 Steam 调用配额已耗尽。
	ErrQuotaExhausted = errors.New("steam: daily quota exhausted")
	// ErrCircuitOpen 表示因收到 429 而处于全局熔断期。
	ErrCircuitOpen = errors.New("steam: circuit breaker open")
)

// DailyQuotaLimit 是单个 API Key 的日调用上限，由 Steam API 使用条款规定。
const DailyQuotaLimit = 100_000

type Limiter interface {
	Acquire(ctx context.Context) error
}

// tokenBucketScript 是原子的令牌桶实现。
// KEYS[1]=桶 key  ARGV[1]=速率(个/秒)  ARGV[2]=容量  ARGV[3]=当前时间(毫秒)
// 返回 1 表示放行，0 表示限流。
var tokenBucketScript = redis.NewScript(`
local rate     = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now_ms   = tonumber(ARGV[3])

local state    = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens   = tonumber(state[1])
local last_ms  = tonumber(state[2])

if tokens == nil then
  tokens  = capacity
  last_ms = now_ms
end

-- 按经过的时间回填令牌，上限为容量
local delta = math.max(0, now_ms - last_ms) / 1000.0 * rate
tokens = math.min(capacity, tokens + delta)

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call('HMSET', KEYS[1], 'tokens', tokens, 'ts', now_ms)
-- 桶闲置足够久后自动过期，避免僵尸 key
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity / rate * 1000) + 10000)
return allowed
`)

type RedisLimiter struct {
	rdb     *redis.Client
	rate    int
	burst   int
	nowFunc func() time.Time
}

func NewRedisLimiter(rdb *redis.Client, ratePerSec, burst int) *RedisLimiter {
	return &RedisLimiter{
		rdb:     rdb,
		rate:    ratePerSec,
		burst:   burst,
		nowFunc: func() time.Time { return time.Now().UTC() },
	}
}

const (
	bucketKey    = "steam:bucket"
	breakerKey   = "steam:breaker"
	quotaKeyBase = "steam:quota:"
)

func (l *RedisLimiter) quotaKey() string {
	return quotaKeyBase + l.nowFunc().Format("20060102")
}

func (l *RedisLimiter) Acquire(ctx context.Context) error {
	// 1. 熔断优先：熔断期内一律拒绝，不消耗令牌
	n, err := l.rdb.Exists(ctx, breakerKey).Result()
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrCircuitOpen
	}

	// 2. 日配额守卫
	used, err := l.QuotaUsed(ctx)
	if err != nil {
		return err
	}
	if used >= DailyQuotaLimit {
		return ErrQuotaExhausted
	}

	// 3. 令牌桶
	now := l.nowFunc().UnixMilli()
	allowed, err := tokenBucketScript.Run(ctx, l.rdb,
		[]string{bucketKey}, l.rate, l.burst, now).Int()
	if err != nil {
		return err
	}
	if allowed == 0 {
		return ErrRateLimited
	}

	// 4. 计入配额。放行后才计数，保证与实际调用数一致。
	key := l.quotaKey()
	if err := l.rdb.Incr(ctx, key).Err(); err != nil {
		return err
	}
	return l.rdb.Expire(ctx, key, 48*time.Hour).Err()
}

func (l *RedisLimiter) QuotaUsed(ctx context.Context) (int64, error) {
	v, err := l.rdb.Get(ctx, l.quotaKey()).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return v, err
}

// TripBreaker 在收到 429 后调用，令所有 worker 实例统一退避。
func (l *RedisLimiter) TripBreaker(ctx context.Context, d time.Duration) error {
	return l.rdb.Set(ctx, breakerKey, "1", d).Err()
}

var _ Limiter = (*RedisLimiter)(nil)
```

- [ ] **Step 4: 运行测试确认通过**

```bash
docker compose up -d redis
go test ./internal/steam/ -run Limiter -v
```

Expected: PASS（4 个用例）

- [ ] **Step 5: 把限流接入客户端**

修改 `internal/steam/client.go`。在 `HTTPClient` 结构体中增加字段：

```go
type HTTPClient struct {
	apiKey  string
	baseURL string
	hc      *http.Client
	limiter Limiter // 可为 nil，表示不限流（仅测试使用）
}

func WithLimiter(l Limiter) Option { return func(c *HTTPClient) { c.limiter = l } }
```

在 `getJSON` 开头插入限流，并在收到 429 时触发熔断：

```go
func (c *HTTPClient) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	if c.limiter != nil {
		if err := c.limiter.Acquire(ctx); err != nil {
			return err
		}
	}

	q.Set("key", c.apiKey)
	// ... 中间不变 ...

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// 收到 429 说明本地限流阈值仍然过高，立即全局熔断 60 秒
		if b, ok := c.limiter.(interface {
			TripBreaker(context.Context, time.Duration) error
		}); ok {
			_ = b.TripBreaker(ctx, 60*time.Second)
		}
		return ErrRateLimited
	// ... 其余不变 ...
	}
}
```

- [ ] **Step 6: 运行全包测试**

Run: `go test ./internal/steam/ -v`
Expected: PASS（全部 10 个用例。Task 2 的用例不传 limiter，因此不受影响）

- [ ] **Step 7: 提交**

```bash
git add internal/steam/
git commit -m "feat(steam): Redis 令牌桶限流、日配额守卫与熔断"
```

---

## Task 4: OpenID 2.0 验证

安全生命线。见设计文档 §7.1 —— 跳过 `check_authentication` 会导致任何人可冒充任意 SteamID 登录。

**Files:**
- Create: `internal/auth/openid.go`
- Test: `internal/auth/openid_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `auth.BuildRedirectURL(realm, returnTo string) string`
  - `auth.Verifier` 结构体与 `auth.NewVerifier(opts ...VerifierOption) *Verifier`
  - `(*Verifier).Verify(ctx context.Context, params url.Values) (uint64, error)`
  - `auth.ErrOpenIDInvalid`、`auth.ErrClaimedIDMalformed`
  - 选项 `auth.WithOpenIDEndpoint(string) VerifierOption`

- [ ] **Step 1: 写验证测试**

创建 `internal/auth/openid_test.go`：

```go
package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func callbackParams(claimedID string) url.Values {
	return url.Values{
		"openid.ns":        {"http://specs.openid.net/auth/2.0"},
		"openid.mode":      {"id_res"},
		"openid.op_endpoint": {"https://steamcommunity.com/openid/login"},
		"openid.claimed_id": {claimedID},
		"openid.identity":   {claimedID},
		"openid.return_to":  {"https://app.example/auth/steam/callback"},
		"openid.response_nonce": {"2026-08-26T10:00:00Zabc"},
		"openid.assoc_handle":   {"1234567890"},
		"openid.signed":         {"signed,op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle"},
		"openid.sig":            {"fakesignature"},
		"not_openid_param":      {"should_not_be_forwarded"},
	}
}

// Steam 认可时返回 is_valid:true，此时才可信任 claimed_id。
func TestVerify_ValidReturnsSteamID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ns:http://specs.openid.net/auth/2.0\nis_valid:true\n")
	}))
	defer srv.Close()

	v := NewVerifier(WithOpenIDEndpoint(srv.URL))
	id, err := v.Verify(context.Background(),
		callbackParams("https://steamcommunity.com/openid/id/76561197960287930"))

	require.NoError(t, err)
	require.Equal(t, uint64(76561197960287930), id)
}

// 这是最关键的安全测试：Steam 说 false 就必须拒绝，
// 即便 claimed_id 本身格式完全合法。
func TestVerify_InvalidIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ns:http://specs.openid.net/auth/2.0\nis_valid:false\n")
	}))
	defer srv.Close()

	v := NewVerifier(WithOpenIDEndpoint(srv.URL))
	_, err := v.Verify(context.Background(),
		callbackParams("https://steamcommunity.com/openid/id/76561197960287930"))

	require.ErrorIs(t, err, ErrOpenIDInvalid)
}

// 验证请求必须原样转发所有 openid.* 参数，且 mode 改为 check_authentication。
func TestVerify_ForwardsAllOpenIDParamsWithCheckAuthentication(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		got = r.PostForm
		_, _ = io.WriteString(w, "is_valid:true\n")
	}))
	defer srv.Close()

	v := NewVerifier(WithOpenIDEndpoint(srv.URL))
	_, err := v.Verify(context.Background(),
		callbackParams("https://steamcommunity.com/openid/id/76561197960287930"))
	require.NoError(t, err)

	require.Equal(t, "check_authentication", got.Get("openid.mode"))
	require.Equal(t, "fakesignature", got.Get("openid.sig"), "签名必须原样转发")
	require.Equal(t, "https://steamcommunity.com/openid/id/76561197960287930",
		got.Get("openid.claimed_id"))
	require.Empty(t, got.Get("not_openid_param"), "非 openid.* 参数不得转发")
}

// claimed_id 必须校验完整前缀，不能简单按 / 分割取末段 ——
// 否则攻击者可用 https://evil.com/openid/id/765... 绕过。
func TestVerify_RejectsForeignClaimedIDHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "is_valid:true\n")
	}))
	defer srv.Close()

	v := NewVerifier(WithOpenIDEndpoint(srv.URL))

	for _, bad := range []string{
		"https://evil.example/openid/id/76561197960287930",
		"http://steamcommunity.com/openid/id/76561197960287930", // 非 https
		"https://steamcommunity.com/openid/id/123",              // 位数不足
		"https://steamcommunity.com/openid/id/76561197960287930x",
	} {
		_, err := v.Verify(context.Background(), callbackParams(bad))
		require.ErrorIs(t, err, ErrClaimedIDMalformed, "应拒绝：%s", bad)
	}
}

func TestBuildRedirectURL(t *testing.T) {
	u := BuildRedirectURL("https://app.example",
		"https://app.example/auth/steam/callback?state=xyz")

	require.True(t, strings.HasPrefix(u, SteamOpenIDEndpoint+"?"))

	parsed, err := url.Parse(u)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, "checkid_setup", q.Get("openid.mode"))
	require.Equal(t, "http://specs.openid.net/auth/2.0/identifier_select", q.Get("openid.identity"))
	require.Equal(t, "http://specs.openid.net/auth/2.0/identifier_select", q.Get("openid.claimed_id"))
	require.Equal(t, "https://app.example", q.Get("openid.realm"))
	require.Equal(t, "https://app.example/auth/steam/callback?state=xyz", q.Get("openid.return_to"))
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/auth/ -v`
Expected: FAIL —— `undefined: NewVerifier`

- [ ] **Step 3: 实现 OpenID**

创建 `internal/auth/openid.go`：

```go
package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	SteamOpenIDEndpoint = "https://steamcommunity.com/openid/login"
	openIDNS            = "http://specs.openid.net/auth/2.0"
	identifierSelect    = "http://specs.openid.net/auth/2.0/identifier_select"
)

var (
	// ErrOpenIDInvalid 表示 Steam 未认可这次断言，必须拒绝登录。
	ErrOpenIDInvalid = errors.New("auth: steam rejected the openid assertion")
	// ErrClaimedIDMalformed 表示 claimed_id 不是合法的 Steam 身份 URL。
	ErrClaimedIDMalformed = errors.New("auth: malformed claimed_id")
)

// claimedIDRe 强制完整匹配 https + steamcommunity.com + 17 位数字。
// 只取末段数字是不安全的：攻击者可托管 https://evil.com/openid/id/765...
var claimedIDRe = regexp.MustCompile(
	`^https://steamcommunity\.com/openid/id/(\d{17})$`)

// BuildRedirectURL 构造第一步的跳转地址。
// returnTo 应携带签名过的 state 参数用于 CSRF 防护 —— Steam 会原样回传它。
func BuildRedirectURL(realm, returnTo string) string {
	q := url.Values{
		"openid.ns":         {openIDNS},
		"openid.mode":       {"checkid_setup"},
		"openid.return_to":  {returnTo},
		"openid.realm":      {realm},
		"openid.identity":   {identifierSelect},
		"openid.claimed_id": {identifierSelect},
	}
	return SteamOpenIDEndpoint + "?" + q.Encode()
}

type Verifier struct {
	endpoint string
	hc       *http.Client
}

type VerifierOption func(*Verifier)

func WithOpenIDEndpoint(u string) VerifierOption {
	return func(v *Verifier) { v.endpoint = u }
}

func NewVerifier(opts ...VerifierOption) *Verifier {
	v := &Verifier{
		endpoint: SteamOpenIDEndpoint,
		hc:       &http.Client{Timeout: 10 * time.Second},
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Verify 执行 OpenID 2.0 的第三步。这一步不可省略：
// 没有它，任何人手工构造回调 URL 即可冒充任意 SteamID。
func (v *Verifier) Verify(ctx context.Context, params url.Values) (uint64, error) {
	// 先做本地格式校验，避免把垃圾请求转发给 Steam
	m := claimedIDRe.FindStringSubmatch(params.Get("openid.claimed_id"))
	if m == nil {
		return 0, ErrClaimedIDMalformed
	}

	// 原样转发所有 openid.* 参数，仅把 mode 改为 check_authentication。
	// 签名覆盖了这些字段，任何增删改都会导致验证失败。
	form := url.Values{}
	for k, vs := range params {
		if strings.HasPrefix(k, "openid.") {
			form[k] = vs
		}
	}
	form.Set("openid.mode", "check_authentication")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("auth: check_authentication failed: %w", err)
	}
	defer resp.Body.Close()

	if !scanIsValid(resp.Body) {
		return 0, ErrOpenIDInvalid
	}

	id, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, ErrClaimedIDMalformed
	}
	return id, nil
}

// scanIsValid 解析 key-value 换行格式的响应，查找 is_valid:true。
func scanIsValid(r interface{ Read([]byte) (int, error) }) bool {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "is_valid:true" {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/auth/ -v`
Expected: PASS（5 个用例，其中 `TestVerify_RejectsForeignClaimedIDHost` 覆盖 4 个恶意输入）

- [ ] **Step 5: 提交**

```bash
git add internal/auth/
git commit -m "feat(auth): OpenID 2.0 验证与 claimed_id 严格校验"
```

---

## Task 5: GORM 模型与仓储层

**Files:**
- Create: `internal/store/models.go`, `internal/store/link_repo.go`, `internal/store/game_repo.go`
- Create: `internal/store/testutil_test.go`
- Test: `internal/store/link_repo_test.go`, `internal/store/game_repo_test.go`

**Interfaces:**
- Consumes: `store.NewDB`（Task 1）、`steam.OwnedGame`（Task 2）
- Produces:
  - 模型：`store.SteamLink`、`store.App`、`store.AppAchievement`、`store.UserGame`、`store.PlaySession`、`store.AchievementUnlock`、`store.ProbeState`、`store.SyncTask`
  - 可见性常量：`store.VisibilityUnknown/OK/ProfilePrivate/GameDetailsPrivate`（值 0/1/2/3）
  - 会话来源常量：`store.SourceProbe = 1`、`store.SourceReconcile = 2`
  - `store.NewLinkRepo(db *gorm.DB) *LinkRepo`，方法：
    - `Link(ctx, userID, steamID uint64) error` — 返回 `store.ErrSteamIDTaken` 或 `store.ErrAlreadyLinked`
    - `Unlink(ctx, userID uint64) error`
    - `ByUserID(ctx, userID uint64) (SteamLink, error)`
    - `UpdateVisibility(ctx, steamID uint64, state int8) error`
    - `BumpPrivateStrikes(ctx, steamID uint64) (int8, error)`
    - `ResetPrivateStrikes(ctx, steamID uint64) error`
    - `ActiveSteamIDs(ctx) ([]uint64, error)`
  - `store.NewGameRepo(db *gorm.DB) *GameRepo`，方法：
    - `UpsertApps(ctx, games []steam.OwnedGame) error`
    - `UpsertUserGames(ctx, steamID uint64, games []steam.OwnedGame, now time.Time) error`
    - `ListUserGames(ctx, steamID uint64) ([]UserGame, error)`
    - `PlaytimeMap(ctx, steamID uint64) (map[uint32]uint32, error)` — appid → 已记录的累计分钟数
  - `store.ErrNotLinked`

- [ ] **Step 1: 写模型定义**

创建 `internal/store/models.go`：

```go
package store

import "time"

// 可见性状态，对应 steam_links.visibility_state
const (
	VisibilityUnknown            int8 = 0
	VisibilityOK                 int8 = 1
	VisibilityProfilePrivate     int8 = 2
	VisibilityGameDetailsPrivate int8 = 3
)

// 会话来源，对应 play_sessions.source
const (
	SourceProbe     int8 = 1 // 探针实测，起止时刻可信
	SourceReconcile int8 = 2 // 每日校准推断，仅时长可信
)

type SteamLink struct {
	UserID         uint64    `gorm:"primaryKey;column:user_id"`
	SteamID        uint64    `gorm:"column:steam_id64"`
	VisibilityState int8     `gorm:"column:visibility_state"`
	PrivateStrikes int8      `gorm:"column:private_strikes"`
	LinkedAt       time.Time `gorm:"column:linked_at"`
	LastVerifiedAt *time.Time `gorm:"column:last_verified_at"`
	UnlinkedAt     *time.Time `gorm:"column:unlinked_at"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// ActiveSteamID 是数据库生成列，只读。GORM 必须标记为 "->" 否则会尝试写入并报错。
	ActiveSteamID *uint64 `gorm:"column:active_steam_id;->"`
}

func (SteamLink) TableName() string { return "steam_links" }

type App struct {
	AppID           uint32     `gorm:"primaryKey;column:appid"`
	Name            string     `gorm:"column:name"`
	ImgIconURL      string     `gorm:"column:img_icon_url"`
	HasAchievements int8       `gorm:"column:has_achievements"` // -1 未知 0 无 1 有
	AchTotal        uint16     `gorm:"column:ach_total"`
	SchemaSyncedAt  *time.Time `gorm:"column:schema_synced_at"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (App) TableName() string { return "apps" }

type AppAchievement struct {
	AppID       uint32  `gorm:"primaryKey;column:appid"`
	APIName     string  `gorm:"primaryKey;column:api_name"`
	DisplayName string  `gorm:"column:display_name"`
	Description string  `gorm:"column:description"`
	Icon        string  `gorm:"column:icon"`
	IconGray    string  `gorm:"column:icon_gray"`
	Hidden      int8    `gorm:"column:hidden"`
	GlobalPct   float64 `gorm:"column:global_pct"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (AppAchievement) TableName() string { return "app_achievements" }

type UserGame struct {
	SteamID            uint64     `gorm:"primaryKey;column:steam_id64"`
	AppID              uint32     `gorm:"primaryKey;column:appid"`
	PlaytimeForeverMin uint32     `gorm:"column:playtime_forever_min"`
	Playtime2WeeksMin  uint32     `gorm:"column:playtime_2weeks_min"`
	RtimeLastPlayed    *time.Time `gorm:"column:rtime_last_played"`
	AchUnlocked        uint16     `gorm:"column:ach_unlocked"`
	AchTotal           uint16     `gorm:"column:ach_total"`
	AchSyncedAt        *time.Time `gorm:"column:ach_synced_at"`
	FirstSeenAt        time.Time  `gorm:"column:first_seen_at"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (UserGame) TableName() string { return "user_games" }

type PlaySession struct {
	ID          uint64    `gorm:"primaryKey;column:id"`
	SteamID     uint64    `gorm:"column:steam_id64"`
	AppID       uint32    `gorm:"column:appid"`
	StartedAt   time.Time `gorm:"column:started_at"`
	EndedAt     time.Time `gorm:"column:ended_at"`
	DurationMin uint32    `gorm:"column:duration_min"`
	Source      int8      `gorm:"column:source"`
	CreatedAt   time.Time
}

func (PlaySession) TableName() string { return "play_sessions" }

type AchievementUnlock struct {
	SteamID    uint64    `gorm:"primaryKey;column:steam_id64"`
	AppID      uint32    `gorm:"primaryKey;column:appid"`
	APIName    string    `gorm:"primaryKey;column:api_name"`
	UnlockedAt time.Time `gorm:"column:unlocked_at"`
	CreatedAt  time.Time
}

func (AchievementUnlock) TableName() string { return "achievement_unlocks" }

type ProbeState struct {
	SteamID           uint64     `gorm:"primaryKey;column:steam_id64"`
	CurrentAppID      *uint32    `gorm:"column:current_appid"` // nil = Idle
	SessionStartedAt  *time.Time `gorm:"column:session_started_at"`
	LastSeenPlayingAt *time.Time `gorm:"column:last_seen_playing_at"`
	MissCount         int8       `gorm:"column:miss_count"`
	Tier              int8       `gorm:"column:tier"`
	LastProbeAt       *time.Time `gorm:"column:last_probe_at"`
	NextProbeAt       time.Time  `gorm:"column:next_probe_at"`
	UpdatedAt         time.Time
}

func (ProbeState) TableName() string { return "probe_state" }

type SyncTask struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	Type      int8      `gorm:"column:task_type"`
	SteamID   uint64    `gorm:"column:steam_id64"`
	AppID     uint32    `gorm:"column:appid"`
	Payload   []byte    `gorm:"column:payload;type:json"`
	Priority  int8      `gorm:"column:priority"`
	Status    int8      `gorm:"column:status"`
	Attempts  uint16    `gorm:"column:attempts"`
	NextRunAt time.Time `gorm:"column:next_run_at"`
	LastError string    `gorm:"column:last_error"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (SyncTask) TableName() string { return "sync_tasks" }
```

- [ ] **Step 2: 写测试基座**

创建 `internal/store/testutil_test.go`。所有仓储测试打真实 MySQL —— `SKIP LOCKED`、生成列、`ON DUPLICATE KEY UPDATE` 的行为在 SQLite 上完全不同，用它测等于没测。

```go
package store

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// testDSN 指向本地 docker compose 起的 MySQL。
// 必须带 parseTime=true 与 loc=UTC，否则 DATETIME 扫描进 time.Time 会失败或带错时区。
func testDSN() string {
	if v := os.Getenv("TEST_MYSQL_DSN"); v != "" {
		return v
	}
	return "root:root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"
}

// testLogger 静默日志输出，避免测试被 SQL trace 淹没。
func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := NewDB(testDSN(), testLogger())
	require.NoError(t, err, "需要本地 MySQL 且已初始化：./scripts/dev/up.sh")

	// 每个用例前清空，保证互不干扰
	for _, tbl := range []string{
		"sync_tasks", "probe_state", "achievement_unlocks",
		"play_sessions", "user_games", "app_achievements", "apps", "steam_links",
	} {
		require.NoError(t, db.Exec("DELETE FROM "+tbl).Error)
	}
	return db
}
```

- [ ] **Step 3: 写绑定仓储的测试**

创建 `internal/store/link_repo_test.go`：

```go
package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLinkRepo_LinkAndFetch(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))

	got, err := r.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Equal(t, uint64(76561197960287930), got.SteamID)
	require.Equal(t, VisibilityUnknown, got.VisibilityState)
}

// 同一个 Steam 账号不能同时被两个本站账号绑定。
func TestLinkRepo_SteamIDTaken(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))
	err := r.Link(ctx, 1002, 76561197960287930)
	require.ErrorIs(t, err, ErrSteamIDTaken)
}

// 生成列的核心价值：解绑后该 Steam 账号可被他人重新绑定。
func TestLinkRepo_UnlinkFreesSteamIDForOthers(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))
	require.NoError(t, r.Unlink(ctx, 1001))

	require.NoError(t, r.Link(ctx, 1002, 76561197960287930),
		"解绑后 active_steam_id 变为 NULL，不再占用唯一键")
}

// 同一用户重新绑定同一账号应恢复原记录，历史数据自动可见。
func TestLinkRepo_RelinkSameAccountRestores(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))
	require.NoError(t, r.Unlink(ctx, 1001))
	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))

	got, err := r.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Nil(t, got.UnlinkedAt)
}

func TestLinkRepo_ByUserID_NotLinked(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	_, err := r.ByUserID(context.Background(), 9999)
	require.ErrorIs(t, err, ErrNotLinked)
}

// 连续私密探测累加，探测成功后清零。
func TestLinkRepo_PrivateStrikes(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()
	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))

	n, err := r.BumpPrivateStrikes(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Equal(t, int8(1), n)

	n, err = r.BumpPrivateStrikes(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Equal(t, int8(2), n)

	require.NoError(t, r.ResetPrivateStrikes(ctx, 76561197960287930))
	got, err := r.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Equal(t, int8(0), got.PrivateStrikes)
}

// 已解绑的账号不应出现在采集名单中。
func TestLinkRepo_ActiveSteamIDsExcludesUnlinked(t *testing.T) {
	r := NewLinkRepo(testDB(t))
	ctx := context.Background()

	require.NoError(t, r.Link(ctx, 1001, 76561197960287930))
	require.NoError(t, r.Link(ctx, 1002, 76561197960287931))
	require.NoError(t, r.Unlink(ctx, 1002))

	ids, err := r.ActiveSteamIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []uint64{76561197960287930}, ids)
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/store/ -run LinkRepo -v`
Expected: FAIL —— `undefined: NewLinkRepo`

- [ ] **Step 5: 实现绑定仓储**

创建 `internal/store/link_repo.go`：

```go
package store

import (
	"context"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

var (
	// ErrSteamIDTaken 表示该 Steam 账号已被另一个本站账号绑定。
	ErrSteamIDTaken = errors.New("store: steam account already linked by another user")
	// ErrAlreadyLinked 表示该本站账号已绑定了别的 Steam 账号。
	ErrAlreadyLinked = errors.New("store: user already linked to a different steam account")
	// ErrNotLinked 表示该用户没有有效绑定。
	ErrNotLinked = errors.New("store: user has no active steam link")
)

type LinkRepo struct{ db *gorm.DB }

func NewLinkRepo(db *gorm.DB) *LinkRepo { return &LinkRepo{db: db} }

func (r *LinkRepo) Link(ctx context.Context, userID, steamID uint64) error {
	now := time.Now().UTC()

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing SteamLink
		err := tx.Where("user_id = ?", userID).Take(&existing).Error

		switch {
		case err == nil:
			// 已有记录：只允许重新绑定同一个 Steam 账号
			if existing.SteamID != steamID {
				return ErrAlreadyLinked
			}
			return tx.Model(&SteamLink{}).Where("user_id = ?", userID).
				Updates(map[string]any{
					"unlinked_at": nil,
					"linked_at":   now,
					"updated_at":  now,
				}).Error

		case errors.Is(err, gorm.ErrRecordNotFound):
			link := SteamLink{
				UserID:          userID,
				SteamID:         steamID,
				VisibilityState: VisibilityUnknown,
				LinkedAt:        now,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			// active_steam_id 是生成列，Omit 掉避免 GORM 尝试写入
			if err := tx.Omit("ActiveSteamID").Create(&link).Error; err != nil {
				if isDuplicateKey(err) {
					return ErrSteamIDTaken
				}
				return err
			}
			return nil

		default:
			return err
		}
	})
}

func (r *LinkRepo) Unlink(ctx context.Context, userID uint64) error {
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("user_id = ? AND unlinked_at IS NULL", userID).
		Updates(map[string]any{"unlinked_at": now, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotLinked
	}
	return nil
}

func (r *LinkRepo) ByUserID(ctx context.Context, userID uint64) (SteamLink, error) {
	var l SteamLink
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND unlinked_at IS NULL", userID).Take(&l).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return SteamLink{}, ErrNotLinked
	}
	return l, err
}

func (r *LinkRepo) UpdateVisibility(ctx context.Context, steamID uint64, state int8) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("steam_id64 = ? AND unlinked_at IS NULL", steamID).
		Updates(map[string]any{
			"visibility_state": state,
			"last_verified_at": now,
			"updated_at":       now,
		}).Error
}

// BumpPrivateStrikes 累加连续私密探测次数并返回新值。
// 连续 3 次后调用方应降级该用户的采集频率（见设计文档 §8.3）。
func (r *LinkRepo) BumpPrivateStrikes(ctx context.Context, steamID uint64) (int8, error) {
	var n int8
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SteamLink{}).
			Where("steam_id64 = ? AND unlinked_at IS NULL", steamID).
			UpdateColumn("private_strikes", gorm.Expr("private_strikes + 1")).Error; err != nil {
			return err
		}
		return tx.Model(&SteamLink{}).
			Where("steam_id64 = ? AND unlinked_at IS NULL", steamID).
			Select("private_strikes").Take(&n).Error
	})
	return n, err
}

func (r *LinkRepo) ResetPrivateStrikes(ctx context.Context, steamID uint64) error {
	return r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("steam_id64 = ? AND unlinked_at IS NULL", steamID).
		UpdateColumn("private_strikes", 0).Error
}

func (r *LinkRepo) ActiveSteamIDs(ctx context.Context) ([]uint64, error) {
	var ids []uint64
	err := r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("unlinked_at IS NULL").
		Order("steam_id64").
		Pluck("steam_id64", &ids).Error
	return ids, err
}

// isDuplicateKey 识别 MySQL 的 1062 错误码。
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
```

安装 driver 依赖（GORM 的 mysql driver 已间接引入，此处显式声明以便直接引用错误类型）：

```bash
go get github.com/go-sql-driver/mysql
```

- [ ] **Step 6: 运行绑定仓储测试**

Run: `go test ./internal/store/ -run LinkRepo -v`
Expected: PASS（7 个用例）

若 `TestLinkRepo_UnlinkFreesSteamIDForOthers` 失败，说明生成列未正确创建，回到 Task 1 Step 8 检查建表。

- [ ] **Step 7: 写游戏仓储的测试**

创建 `internal/store/game_repo_test.go`：

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"steamlink/internal/steam"
)

func sampleGames() []steam.OwnedGame {
	return []steam.OwnedGame{
		{AppID: 620, Name: "Portal 2 🧪", ImgIconURL: "abc",
			PlaytimeForeverMin: 100, Playtime2WeeksMin: 30,
			RtimeLastPlayed: time.Unix(1756180800, 0).UTC()},
		{AppID: 730, Name: "反恐精英 ⚡", ImgIconURL: "def",
			PlaytimeForeverMin: 5000},
	}
}

// emoji 必须能完整往返 —— 这验证了 utf8mb4 链路端到端可用。
func TestGameRepo_UpsertPreservesEmoji(t *testing.T) {
	r := NewGameRepo(testDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, r.UpsertApps(ctx, sampleGames()))
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, sampleGames(), now))

	got, err := r.ListUserGames(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Len(t, got, 2)

	var apps []App
	require.NoError(t, testDBHandle(t).Order("appid").Find(&apps).Error)
	require.Equal(t, "Portal 2 🧪", apps[0].Name)
	require.Equal(t, "反恐精英 ⚡", apps[1].Name)
}

// 重复 upsert 只更新不新增，且 first_seen_at 保持首次值。
func TestGameRepo_UpsertIsIdempotent(t *testing.T) {
	db := testDB(t)
	r := NewGameRepo(db)
	ctx := context.Background()

	first := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.UpsertApps(ctx, sampleGames()))
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, sampleGames(), first))

	updated := sampleGames()
	updated[0].PlaytimeForeverMin = 999
	later := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, updated, later))

	got, err := r.ListUserGames(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Len(t, got, 2, "重复 upsert 不应新增行")

	require.Equal(t, uint32(999), got[0].PlaytimeForeverMin, "时长应被更新")
	require.Equal(t, first.Unix(), got[0].FirstSeenAt.Unix(),
		"first_seen_at 必须保持首次入库时间，否则无法识别新购入的游戏")
}

func TestGameRepo_PlaytimeMap(t *testing.T) {
	r := NewGameRepo(testDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, r.UpsertApps(ctx, sampleGames()))
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, sampleGames(), now))

	m, err := r.PlaytimeMap(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Equal(t, map[uint32]uint32{620: 100, 730: 5000}, m)
}
```

在 `testutil_test.go` 中补一个句柄辅助函数：

```go
func testDBHandle(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := NewDB(testDSN(), testLogger())
	require.NoError(t, err)
	return db
}
```

- [ ] **Step 8: 运行测试确认失败**

Run: `go test ./internal/store/ -run GameRepo -v`
Expected: FAIL —— `undefined: NewGameRepo`

- [ ] **Step 9: 实现游戏仓储**

创建 `internal/store/game_repo.go`：

```go
package store

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"steamlink/internal/steam"
)

// upsertBatchSize 控制单条 INSERT 的行数。一个用户的游戏库可达数千款，
// 必须批量写入 —— 逐条 INSERT 会产生数千次数据库往返。
const upsertBatchSize = 200

type GameRepo struct{ db *gorm.DB }

func NewGameRepo(db *gorm.DB) *GameRepo { return &GameRepo{db: db} }

// UpsertApps 写入全局游戏元数据。这些数据跨用户共享，不带用户维度。
// 注意不要覆盖 has_achievements 与 ach_total —— 它们由 Schema 同步任务维护。
func (r *GameRepo) UpsertApps(ctx context.Context, games []steam.OwnedGame) error {
	if len(games) == 0 {
		return nil
	}
	now := time.Now().UTC()

	rows := make([]App, 0, len(games))
	for _, g := range games {
		rows = append(rows, App{
			AppID:           g.AppID,
			Name:            g.Name,
			ImgIconURL:      g.ImgIconURL,
			HasAchievements: -1, // 仅新建时生效，见下方 DoUpdates 列表
			CreatedAt:       now,
			UpdatedAt:       now,
		})
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "appid"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "img_icon_url", "updated_at"}),
	}).CreateInBatches(&rows, upsertBatchSize).Error
}

// UpsertUserGames 写入用户的游戏库快照。
// first_seen_at 在冲突时不更新，用于识别新购入的游戏。
func (r *GameRepo) UpsertUserGames(ctx context.Context, steamID uint64,
	games []steam.OwnedGame, now time.Time) error {

	if len(games) == 0 {
		return nil
	}

	rows := make([]UserGame, 0, len(games))
	for _, g := range games {
		var last *time.Time
		if !g.RtimeLastPlayed.IsZero() {
			t := g.RtimeLastPlayed
			last = &t
		}
		rows = append(rows, UserGame{
			SteamID:            steamID,
			AppID:              g.AppID,
			PlaytimeForeverMin: g.PlaytimeForeverMin,
			Playtime2WeeksMin:  g.Playtime2WeeksMin,
			RtimeLastPlayed:    last,
			FirstSeenAt:        now,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "steam_id64"}, {Name: "appid"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"playtime_forever_min", "playtime_2weeks_min",
			"rtime_last_played", "updated_at",
		}),
	}).CreateInBatches(&rows, upsertBatchSize).Error
}

func (r *GameRepo) ListUserGames(ctx context.Context, steamID uint64) ([]UserGame, error) {
	var out []UserGame
	err := r.db.WithContext(ctx).
		Where("steam_id64 = ?", steamID).
		Order("appid").
		Find(&out).Error
	return out, err
}

// PlaytimeMap 返回 appid → 库中已记录的累计分钟数，供差分计算使用。
func (r *GameRepo) PlaytimeMap(ctx context.Context, steamID uint64) (map[uint32]uint32, error) {
	type row struct {
		AppID uint32
		Mins  uint32
	}
	var rows []row
	err := r.db.WithContext(ctx).Model(&UserGame{}).
		Select("appid AS app_id, playtime_forever_min AS mins").
		Where("steam_id64 = ?", steamID).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	m := make(map[uint32]uint32, len(rows))
	for _, r := range rows {
		m[r.AppID] = r.Mins
	}
	return m, nil
}

var _ = gorm.ErrRecordNotFound // 保持 gorm 导入
```

- [ ] **Step 10: 运行全部仓储测试**

Run: `go test ./internal/store/ -v`
Expected: PASS（10 个用例）

- [ ] **Step 11: 提交**

```bash
git add internal/store/ go.mod go.sum
git commit -m "feat(store): GORM 模型与绑定/游戏仓储"
```

---

## Task 6: 账号绑定 API 与隐私探测

**Files:**
- Create: `internal/auth/session.go`, `internal/api/dto.go`, `internal/api/auth_handler.go`, `internal/api/router.go`, `cmd/api/main.go`
- Create: `internal/api/probe_visibility.go`
- Test: `internal/api/probe_visibility_test.go`, `internal/auth/session_test.go`

**Interfaces:**
- Consumes: `auth.BuildRedirectURL`/`auth.Verifier`（Task 4）、`store.LinkRepo`/`store.GameRepo`/可见性常量（Task 5）、`steam.Client`/`steam.ErrProfilePrivate`（Task 2）
- Produces:
  - `auth.NewSessionStore(rdb *redis.Client, ttl time.Duration) *SessionStore`，方法 `Issue(ctx, userID uint64) (string, error)`、`Resolve(ctx, token string) (uint64, error)`
  - `auth.SignState(secret []byte, userID uint64, now time.Time) string`、`auth.VerifyState(secret []byte, s string, now time.Time) (uint64, error)`、`auth.ErrStateInvalid`
  - `api.ProbeVisibility(ctx, c steam.Client, steamID uint64) (int8, []steam.OwnedGame, error)`
  - `api.NewRouter(deps Deps) *gin.Engine`，`api.Deps` 结构体字段：`Links *store.LinkRepo`、`Games *store.GameRepo`、`Steam steam.Client`、`Verifier *auth.Verifier`、`Auth *auth.SessionStore`、`Tasks task.Queue`、`BaseURL string`、`StateSecret []byte`

> 登录会话字段命名为 `Auth` 而非 `Sessions`：Task 17 会加入 `Sessions *store.SessionRepo`（游戏会话仓储），两者含义完全不同，同名会造成混淆。
  - 路由：`GET /auth/steam/login`、`GET /auth/steam/callback`、`POST /api/link/recheck`、`DELETE /api/link`

> `api.Deps.Tasks` 字段的类型 `task.Queue` 在 Task 8 定义。本任务先把该字段留为接口类型并在 `cmd/api/main.go` 中传 `nil`，Task 8 完成后接上。绑定流程中的入队调用在 Task 9 补全。

- [ ] **Step 1: 写 state 签名与会话的测试**

创建 `internal/auth/session_test.go`：

```go
package auth

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSignVerifyState_RoundTrip(t *testing.T) {
	secret := []byte("test-secret-key")
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	s := SignState(secret, 1001, now)
	got, err := VerifyState(secret, s, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, uint64(1001), got)
}

// 篡改后签名不匹配，必须拒绝 —— 否则攻击者可让受害者绑定到自己的账号。
func TestVerifyState_RejectsTampered(t *testing.T) {
	secret := []byte("test-secret-key")
	now := time.Now().UTC()

	s := SignState(secret, 1001, now)
	_, err := VerifyState(secret, s+"x", now)
	require.ErrorIs(t, err, ErrStateInvalid)
}

func TestVerifyState_RejectsExpired(t *testing.T) {
	secret := []byte("test-secret-key")
	now := time.Now().UTC()

	s := SignState(secret, 1001, now)
	_, err := VerifyState(secret, s, now.Add(20*time.Minute))
	require.ErrorIs(t, err, ErrStateInvalid)
}

func TestSessionStore_IssueResolve(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 14})
	require.NoError(t, rdb.FlushDB(context.Background()).Err())
	defer rdb.Close()

	ss := NewSessionStore(rdb, time.Hour)
	ctx := context.Background()

	tok, err := ss.Issue(ctx, 1001)
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	got, err := ss.Resolve(ctx, tok)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), got)

	_, err = ss.Resolve(ctx, "nonexistent")
	require.Error(t, err)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/auth/ -run "State|SessionStore" -v`
Expected: FAIL —— `undefined: SignState`

- [ ] **Step 3: 实现 state 签名与会话存储**

创建 `internal/auth/session.go`：

```go
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrStateInvalid 表示 CSRF state 签名不匹配或已过期。
var ErrStateInvalid = errors.New("auth: invalid or expired state")

// stateTTL 是 OpenID 往返的有效期。用户在 Steam 页面停留超过它需要重新发起。
const stateTTL = 15 * time.Minute

// SignState 生成 "userID.unix.hmac" 形式的 CSRF state。
// Steam 会原样回传 return_to，我们据此在回调时确认发起者身份。
func SignState(secret []byte, userID uint64, now time.Time) string {
	payload := fmt.Sprintf("%d.%d", userID, now.Unix())
	return payload + "." + sign(secret, payload)
}

func VerifyState(secret []byte, s string, now time.Time) (uint64, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, ErrStateInvalid
	}

	payload := parts[0] + "." + parts[1]
	// 常数时间比较，避免时序侧信道
	if !hmac.Equal([]byte(parts[2]), []byte(sign(secret, payload))) {
		return 0, ErrStateInvalid
	}

	issued, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, ErrStateInvalid
	}
	if now.Sub(time.Unix(issued, 0)) > stateTTL {
		return 0, ErrStateInvalid
	}

	userID, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, ErrStateInvalid
	}
	return userID, nil
}

func sign(secret []byte, payload string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// ---------- 登录会话 ----------

type SessionStore struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewSessionStore(rdb *redis.Client, ttl time.Duration) *SessionStore {
	return &SessionStore{rdb: rdb, ttl: ttl}
}

func (s *SessionStore) key(token string) string { return "session:" + token }

func (s *SessionStore) Issue(ctx context.Context, userID uint64) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	if err := s.rdb.Set(ctx, s.key(token),
		strconv.FormatUint(userID, 10), s.ttl).Err(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *SessionStore) Resolve(ctx context.Context, token string) (uint64, error) {
	v, err := s.rdb.Get(ctx, s.key(token)).Result()
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(v, 10, 64)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/auth/ -v`
Expected: PASS（9 个用例，含 Task 4 的 5 个）

- [ ] **Step 5: 写隐私探测的测试**

隐私探测是绑定流程的核心，必须能区分三种状态（设计文档 §8.1）。创建 `internal/api/probe_visibility_test.go`：

```go
package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"steamlink/internal/steam"
	"steamlink/internal/store"
)

// fakeSteam 让我们精确构造三种隐私状态。
type fakeSteam struct {
	steam.Client
	summaries []steam.PlayerSummary
	sumErr    error
	games     []steam.OwnedGame
	gamesErr  error
}

func (f *fakeSteam) GetPlayerSummaries(context.Context, []uint64) ([]steam.PlayerSummary, error) {
	return f.summaries, f.sumErr
}

func (f *fakeSteam) GetOwnedGames(context.Context, uint64) ([]steam.OwnedGame, error) {
	return f.games, f.gamesErr
}

func TestProbeVisibility_OK(t *testing.T) {
	f := &fakeSteam{
		summaries: []steam.PlayerSummary{{SteamID: 1, CommunityVisibilityState: 3}},
		games:     []steam.OwnedGame{{AppID: 620, Name: "Portal 2"}},
	}

	state, games, err := ProbeVisibility(context.Background(), f, 1)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityOK, state)
	require.Len(t, games, 1)
}

// 资料整体不公开。
func TestProbeVisibility_ProfilePrivate(t *testing.T) {
	f := &fakeSteam{
		summaries: []steam.PlayerSummary{{SteamID: 1, CommunityVisibilityState: 1}},
	}

	state, games, err := ProbeVisibility(context.Background(), f, 1)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityProfilePrivate, state)
	require.Empty(t, games, "资料私密时不应继续拉游戏库，省一次调用")
}

// 关键场景：资料公开但「游戏详情」单独设为不公开。
// 这是最容易被误判为「用户没有游戏」的情况。
func TestProbeVisibility_GameDetailsPrivate(t *testing.T) {
	f := &fakeSteam{
		summaries: []steam.PlayerSummary{{SteamID: 1, CommunityVisibilityState: 3}},
		gamesErr:  steam.ErrProfilePrivate,
	}

	state, games, err := ProbeVisibility(context.Background(), f, 1)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityGameDetailsPrivate, state)
	require.Empty(t, games)
}

// 账号真的存在但一款游戏都没有 —— 必须判定为正常，不是私密。
func TestProbeVisibility_PublicButEmptyLibrary(t *testing.T) {
	f := &fakeSteam{
		summaries: []steam.PlayerSummary{{SteamID: 1, CommunityVisibilityState: 3}},
		games:     []steam.OwnedGame{},
	}

	state, _, err := ProbeVisibility(context.Background(), f, 1)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityOK, state)
}

// SteamID 不存在时 Steam 返回空 players 数组。
func TestProbeVisibility_UnknownSteamID(t *testing.T) {
	f := &fakeSteam{summaries: []steam.PlayerSummary{}}

	_, _, err := ProbeVisibility(context.Background(), f, 1)
	require.ErrorIs(t, err, ErrSteamAccountNotFound)
}
```

- [ ] **Step 6: 运行测试确认失败**

Run: `go test ./internal/api/ -v`
Expected: FAIL —— `undefined: ProbeVisibility`

- [ ] **Step 7: 实现隐私探测**

创建 `internal/api/probe_visibility.go`：

```go
package api

import (
	"context"
	"errors"

	"steamlink/internal/steam"
	"steamlink/internal/store"
)

// ErrSteamAccountNotFound 表示该 SteamID 在 Steam 侧不存在。
var ErrSteamAccountNotFound = errors.New("api: steam account not found")

// ProbeVisibility 探测两个互相独立的隐私开关（见设计文档 §2.3），
// 返回可见性状态与已拉取到的游戏库（状态为 OK 时非空）。
//
// 顺利时只消耗 2 次 Steam 调用；资料私密时提前返回，只消耗 1 次。
func ProbeVisibility(ctx context.Context, c steam.Client, steamID uint64) (int8, []steam.OwnedGame, error) {
	sums, err := c.GetPlayerSummaries(ctx, []uint64{steamID})
	if err != nil {
		return store.VisibilityUnknown, nil, err
	}
	if len(sums) == 0 {
		return store.VisibilityUnknown, nil, ErrSteamAccountNotFound
	}

	// 开关一：个人资料公开性
	if sums[0].CommunityVisibilityState != 3 {
		return store.VisibilityProfilePrivate, nil, nil
	}

	// 开关二：游戏详情公开性。注意这里的 ErrProfilePrivate 语义是
	// 「游戏详情不公开」而非「整个资料不公开」—— 上面已经确认资料是公开的。
	games, err := c.GetOwnedGames(ctx, steamID)
	if errors.Is(err, steam.ErrProfilePrivate) {
		return store.VisibilityGameDetailsPrivate, nil, nil
	}
	if err != nil {
		return store.VisibilityUnknown, nil, err
	}

	return store.VisibilityOK, games, nil
}
```

- [ ] **Step 8: 运行测试确认通过**

Run: `go test ./internal/api/ -v`
Expected: PASS（5 个用例）

- [ ] **Step 9: 实现 DTO 与路由**

创建 `internal/api/dto.go`：

```go
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

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
```

创建 `internal/api/auth_handler.go`：

```go
package api

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"steamlink/internal/auth"
	"steamlink/internal/store"
)

// currentUserID 从登录会话解析本站用户。
// 本项目假设已有账号体系，此处以 Bearer token 为例。
func (d Deps) currentUserID(c *gin.Context) (uint64, bool) {
	tok := c.GetHeader("Authorization")
	if len(tok) > 7 && tok[:7] == "Bearer " {
		tok = tok[7:]
	}
	id, err := d.Auth.Resolve(c.Request.Context(), tok)
	if err != nil {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Code: "unauthorized", Message: "请先登录"})
		return 0, false
	}
	return id, true
}

// handleLogin 发起 OpenID 跳转。
func (d Deps) handleLogin(c *gin.Context) {
	userID, ok := d.currentUserID(c)
	if !ok {
		return
	}

	state := auth.SignState(d.StateSecret, userID, time.Now().UTC())
	returnTo := d.BaseURL + "/auth/steam/callback?state=" + url.QueryEscape(state)

	c.Redirect(http.StatusFound, auth.BuildRedirectURL(d.BaseURL, returnTo))
}

// handleCallback 处理 Steam 回跳：验证断言 → 校验 state → 建立绑定 → 探测隐私。
func (d Deps) handleCallback(c *gin.Context) {
	ctx := c.Request.Context()

	userID, err := auth.VerifyState(d.StateSecret, c.Query("state"), time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Code: "invalid_state", Message: "登录请求已过期，请重试"})
		return
	}

	// 安全生命线：必须向 Steam 确认这次断言，见设计文档 §7.1
	steamID, err := d.Verifier.Verify(ctx, c.Request.URL.Query())
	if err != nil {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Code: "openid_invalid", Message: "Steam 验证失败"})
		return
	}

	switch err := d.Links.Link(ctx, userID, steamID); {
	case errors.Is(err, store.ErrSteamIDTaken):
		c.JSON(http.StatusConflict, ErrorResponse{Code: "steam_id_taken", Message: "该 Steam 账号已被其他用户绑定"})
		return
	case errors.Is(err, store.ErrAlreadyLinked):
		c.JSON(http.StatusConflict, ErrorResponse{Code: "already_linked", Message: "请先解绑当前 Steam 账号"})
		return
	case err != nil:
		c.JSON(http.StatusInternalServerError, ErrorResponse{Code: "internal", Message: "绑定失败"})
		return
	}

	c.JSON(http.StatusOK, d.probeAndPersist(c, steamID))
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
	}

	slug, hint := visibilityHint(state)
	return LinkStatusResponse{
		SteamID:    steamID,
		Visibility: slug,
		GameCount:  len(games),
		Hint:       hint,
	}
}
```

创建 `internal/api/router.go`：

```go
package api

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"steamlink/internal/auth"
	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type Deps struct {
	Links       *store.LinkRepo
	Games       *store.GameRepo
	Steam       steam.Client
	Verifier    *auth.Verifier
	Auth        *auth.SessionStore // 登录态，勿与 Task 17 的 Sessions（游戏会话）混淆
	Tasks       task.Queue
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

	r.GET("/auth/steam/login", d.handleLogin)
	r.GET("/auth/steam/callback", d.handleCallback)

	api := r.Group("/api")
	{
		api.POST("/link/recheck", d.handleRecheck)
		api.DELETE("/link", d.handleUnlink)
	}
	return r
}
```

> `task.Queue` 尚未定义，本步骤会编译失败。这是预期的 —— 先注释掉 `Deps.Tasks` 字段与 `task` 导入，在 Task 8 完成后取消注释。

- [ ] **Step 10: 写 API 入口**

创建 `cmd/api/main.go`：

```go
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
		Steam:       sc,
		Verifier:    auth.NewVerifier(),
		Auth:        auth.NewSessionStore(rdb, cfg.Auth.SessionTTL),
		BaseURL:     cfg.HTTP.BaseURL,
		StateSecret: []byte(cfg.Auth.StateSecret),
		Logger:      lg,
	})

	lg.Info("API 启动", slog.String("addr", cfg.HTTP.Addr),
		slog.String("base_url", cfg.HTTP.BaseURL))

	if err := r.Run(cfg.HTTP.Addr); err != nil {
		lg.Error("HTTP 服务退出", slog.String("err", err.Error()))
		os.Exit(1)
	}
}
```

> `api.Deps` 需要增加 `Logger *slog.Logger` 字段。同时把 `handleLogin` 等 handler 中可能的错误日志改为 `d.Logger.Error(...)`。

- [ ] **Step 11: 编译并运行全部测试**

```bash
go build ./...
go vet ./...
go test ./... 
```

Expected: 编译通过，全部测试 PASS

- [ ] **Step 12: 提交**

```bash
git add internal/auth/ internal/api/ cmd/api/
git commit -m "feat(api): OpenID 绑定流程与隐私三态探测"
```

---

## Task 7: 游戏库查询 API

至此 P1 完成，产出可演示的成果：绑定 → 看到游戏库。

**Files:**
- Create: `internal/api/library_handler.go`
- Modify: `internal/api/router.go`（注册新路由）
- Modify: `internal/store/game_repo.go`（增加联表查询）
- Test: `internal/store/game_repo_test.go`（追加用例）

**Interfaces:**
- Consumes: `store.GameRepo`、`store.LinkRepo`（Task 5）、`api.GameItem`（Task 6）
- Produces:
  - `store.GameRepo.ListLibrary(ctx, steamID uint64) ([]LibraryRow, error)`
  - `store.LibraryRow` 结构体：`AppID uint32`、`Name string`、`ImgIconURL string`、`PlaytimeForeverMin uint32`、`Playtime2WeeksMin uint32`、`RtimeLastPlayed *time.Time`、`AchUnlocked uint16`、`AchTotal uint16`
  - 路由 `GET /api/library`

- [ ] **Step 1: 写联表查询的测试**

在 `internal/store/game_repo_test.go` 末尾追加：

```go
// 游戏库列表需要联 apps 表拿名称与图标 —— user_games 本身不存这些。
func TestGameRepo_ListLibraryJoinsAppMetadata(t *testing.T) {
	r := NewGameRepo(testDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, r.UpsertApps(ctx, sampleGames()))
	require.NoError(t, r.UpsertUserGames(ctx, 76561197960287930, sampleGames(), now))

	rows, err := r.ListLibrary(ctx, 76561197960287930)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// 按累计时长倒序：730 有 5000 分钟，620 有 100 分钟
	require.Equal(t, uint32(730), rows[0].AppID)
	require.Equal(t, "反恐精英 ⚡", rows[0].Name)
	require.Equal(t, uint32(5000), rows[0].PlaytimeForeverMin)

	require.Equal(t, uint32(620), rows[1].AppID)
	require.Equal(t, "abc", rows[1].ImgIconURL)
	require.NotNil(t, rows[1].RtimeLastPlayed)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/store/ -run ListLibrary -v`
Expected: FAIL —— `r.ListLibrary undefined`

- [ ] **Step 3: 实现联表查询**

在 `internal/store/game_repo.go` 末尾追加：

```go
// LibraryRow 是 user_games 联 apps 后的展示行。
type LibraryRow struct {
	AppID              uint32     `gorm:"column:appid"`
	Name               string     `gorm:"column:name"`
	ImgIconURL         string     `gorm:"column:img_icon_url"`
	PlaytimeForeverMin uint32     `gorm:"column:playtime_forever_min"`
	Playtime2WeeksMin  uint32     `gorm:"column:playtime_2weeks_min"`
	RtimeLastPlayed    *time.Time `gorm:"column:rtime_last_played"`
	AchUnlocked        uint16     `gorm:"column:ach_unlocked"`
	AchTotal           uint16     `gorm:"column:ach_total"`
}

// ListLibrary 返回按累计时长倒序的游戏库。
// 名称与图标存在全局的 apps 表中，此处联表取出。
func (r *GameRepo) ListLibrary(ctx context.Context, steamID uint64) ([]LibraryRow, error) {
	var rows []LibraryRow
	err := r.db.WithContext(ctx).
		Table("user_games AS ug").
		Select(`ug.appid, a.name, a.img_icon_url,
		        ug.playtime_forever_min, ug.playtime_2weeks_min,
		        ug.rtime_last_played, ug.ach_unlocked, ug.ach_total`).
		Joins("LEFT JOIN apps AS a ON a.appid = ug.appid").
		Where("ug.steam_id64 = ?", steamID).
		Order("ug.playtime_forever_min DESC, ug.appid").
		Scan(&rows).Error
	return rows, err
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/store/ -run ListLibrary -v`
Expected: PASS

- [ ] **Step 5: 实现查询接口**

创建 `internal/api/library_handler.go`：

```go
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (d Deps) handleLibrary(c *gin.Context) {
	userID, ok := d.currentUserID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	link, err := d.Links.ByUserID(ctx, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Code: "not_linked", Message: "尚未绑定 Steam 账号"})
		return
	}

	rows, err := d.Games.ListLibrary(ctx, link.SteamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Code: "internal", Message: "查询失败"})
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
	c.JSON(http.StatusOK, gin.H{
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
```

在文件顶部补上 `"strconv"` 导入。

在 `internal/api/router.go` 的 `api` 分组中注册路由：

```go
	api := r.Group("/api")
	{
		api.POST("/link/recheck", d.handleRecheck)
		api.DELETE("/link", d.handleUnlink)
		api.GET("/library", d.handleLibrary)
	}
```

- [ ] **Step 6: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 7: 提交**

```bash
git add internal/api/ internal/store/
git commit -m "feat(api): 游戏库查询接口"
```

**P1 完成。** 此时应部署一次并观察隐私墙的真实命中率（设计文档 附录 B）—— 若多数目标用户的游戏详情非公开，后续阶段的价值需要重新评估。

---

## Task 8: 本地事务表 —— 入队与幂等

**Files:**
- Create: `internal/task/task.go`, `internal/task/queue.go`
- Test: `internal/task/queue_test.go`, `internal/task/testutil_test.go`

**Interfaces:**
- Consumes: `store.SyncTask` 模型、`store.NewDB`（Task 1、5）
- Produces:
  - 类型常量：`task.TypeLibrarySync = 1`、`task.TypeAchievementSync = 2`、`task.TypeSchemaSync = 3`、`task.TypeSessionSettle = 4`
  - 状态常量：`task.StatusPending = 0`、`task.StatusRunning = 1`、`task.StatusSucceeded = 2`、`task.StatusRetrying = 3`、`task.StatusDead = 4`
  - 优先级常量：`task.PriorityRealtime = 1`、`task.PriorityNormal = 5`、`task.PriorityBackfill = 9`
  - `task.Task` 结构体（字段同 `store.SyncTask`）
  - `task.Queue` 接口，四个方法见下
  - `task.NewMySQLQueue(db *gorm.DB) *MySQLQueue`
  - `task.SessionPayload` 结构体：`StartedAt time.Time`、`EndedAt time.Time`

- [ ] **Step 1: 定义类型与接口**

创建 `internal/task/task.go`：

```go
package task

import (
	"context"
	"time"
)

// 任务类型，对应 sync_tasks.task_type
const (
	TypeLibrarySync     int8 = 1 // L3 每日校准：全量拉游戏库
	TypeAchievementSync int8 = 2 // L2 成就下钻：单用户单游戏
	TypeSchemaSync      int8 = 3 // 全局成就定义同步：单游戏，与用户无关
	TypeSessionSettle   int8 = 4 // L1 会话结算
)

// 任务状态，对应 sync_tasks.status
const (
	StatusPending   int8 = 0
	StatusRunning   int8 = 1
	StatusSucceeded int8 = 2
	StatusRetrying  int8 = 3
	StatusDead      int8 = 4
)

// 优先级，数值小者优先。
// 新用户绑定会一次性产生数百条回填任务，必须用最低优先级，
// 否则会把所有用户的实时会话结算拖垮（设计文档 §6.8）。
const (
	PriorityRealtime int8 = 1
	PriorityNormal   int8 = 5
	PriorityBackfill int8 = 9
)

type Task struct {
	ID        uint64
	Type      int8
	SteamID   uint64
	AppID     uint32
	Payload   []byte
	Priority  int8
	Status    int8
	Attempts  uint16
	NextRunAt time.Time
	LastError string
}

// SessionPayload 是 TypeSessionSettle 任务携带的数据。
type SessionPayload struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

type Queue interface {
	// Enqueue 入队。同一 (Type, SteamID, AppID) 只保留一行，重复入队会合并。
	Enqueue(ctx context.Context, t Task) error
	// Claim 领取到期任务并续租。
	Claim(ctx context.Context, limit int, lease time.Duration) ([]Task, error)
	// Succeed 标记成功。
	Succeed(ctx context.Context, id uint64) error
	// Fail 记录失败并按指数退避重排，超过上限后转入死信。
	Fail(ctx context.Context, id uint64, cause error) error
}
```

- [ ] **Step 2: 写入队幂等的测试**

创建 `internal/task/testutil_test.go`：

```go
package task

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"steamlink/internal/store"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"
	}
	db, err := store.NewDB(dsn, testLogger())
	require.NoError(t, err, "需要本地 MySQL 并已初始化：./scripts/dev/up.sh")
	require.NoError(t, db.Exec("DELETE FROM sync_tasks").Error)
	return db
}
```

创建 `internal/task/queue_test.go`：

```go
package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/store"
)

func TestEnqueue_Insert(t *testing.T) {
	q := NewMySQLQueue(testDB(t))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 76561197960287930, AppID: 620,
		Priority: PriorityNormal, NextRunAt: time.Now().UTC(),
	}))

	got, err := q.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, uint32(620), got[0].AppID)
}

// 唯一键保证同一任务标识只有一行，重复入队不会堆积。
func TestEnqueue_IdempotentOnUniqueKey(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, q.Enqueue(ctx, Task{
			Type: TypeAchievementSync, SteamID: 76561197960287930, AppID: 620,
			Priority: PriorityNormal, NextRunAt: time.Now().UTC(),
		}))
	}

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Equal(t, int64(1), n, "重复入队必须合并为一行")
}

// 重复入队时取更早的执行时刻 —— 新的紧急需求不应被旧的远期排期压住。
func TestEnqueue_TakesEarlierNextRunAt(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: base.Add(6 * time.Hour),
	}))
	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: base.Add(time.Minute),
	}))

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, base.Add(time.Minute).Unix(), row.NextRunAt.Unix())
}

// 已成功的任务再次入队应复活为待执行 —— 这是「状态表而非日志表」的关键行为。
func TestEnqueue_RevivesSucceededTask(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityNormal, NextRunAt: time.Now().UTC(),
	}))
	claimed, err := q.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.Succeed(ctx, claimed[0].ID))

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityNormal, NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	again, err := q.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, again, 1, "已成功的任务再次入队应可被重新领取")
	require.Equal(t, uint16(0), again[0].Attempts, "重试次数应清零")
}

// 未到期的任务不应被领取。
func TestClaim_SkipsFutureTasks(t *testing.T) {
	q := NewMySQLQueue(testDB(t))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(time.Hour),
	}))

	got, err := q.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Empty(t, got)
}

// 优先级必须先于时间生效：回填任务不能插队到实时任务前面。
func TestClaim_PriorityBeatsTime(t *testing.T) {
	q := NewMySQLQueue(testDB(t))
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)

	// 回填任务更早入队
	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityBackfill, NextRunAt: past.Add(-time.Hour),
	}))
	// 实时任务更晚，但优先级更高
	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeSessionSettle, SteamID: 1, AppID: 730,
		Priority: PriorityRealtime, NextRunAt: past,
	}))

	got, err := q.Claim(ctx, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, TypeSessionSettle, got[0].Type, "实时任务必须优先")
}

func TestFail_BackoffThenDead(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))
	claimed, err := q.Claim(ctx, 1, time.Minute)
	require.NoError(t, err)

	require.NoError(t, q.Fail(ctx, claimed[0].ID, errors.New("boom")))

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusRetrying, row.Status)
	require.Equal(t, uint16(1), row.Attempts)
	require.Contains(t, row.LastError, "boom")
	require.True(t, row.NextRunAt.After(time.Now().UTC()), "应退避到未来")

	// 连续失败超过上限转入死信
	require.NoError(t, db.Model(&store.SyncTask{}).
		Where("id = ?", row.ID).Update("attempts", MaxAttempts).Error)
	require.NoError(t, q.Fail(ctx, row.ID, errors.New("boom again")))

	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusDead, row.Status)
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/task/ -v`
Expected: FAIL —— `undefined: NewMySQLQueue`

- [ ] **Step 4: 实现入队与失败处理**

创建 `internal/task/queue.go`：

```go
package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"steamlink/internal/store"
)

// MaxAttempts 是转入死信前的最大重试次数。
const MaxAttempts = 8

// LeaseDuration 是默认租约时长。worker 崩溃后，任务在此时长后被自动回收。
const LeaseDuration = 5 * time.Minute

type MySQLQueue struct {
	db      *gorm.DB
	nowFunc func() time.Time
}

func NewMySQLQueue(db *gorm.DB) *MySQLQueue {
	return &MySQLQueue{db: db, nowFunc: func() time.Time { return time.Now().UTC() }}
}

// Enqueue 依赖 uk_task(task_type, steam_id64, appid) 唯一键实现幂等。
// sync_tasks 是状态表而非日志表：每个任务标识永远只有一行，反复复用。
func (q *MySQLQueue) Enqueue(ctx context.Context, t Task) error {
	now := q.nowFunc()

	row := store.SyncTask{
		Type:      t.Type,
		SteamID:   t.SteamID,
		AppID:     t.AppID,
		Payload:   t.Payload,
		Priority:  t.Priority,
		Status:    StatusPending,
		Attempts:  0,
		NextRunAt: t.NextRunAt,
		CreatedAt: now,
		UpdatedAt: now,
	}

	return q.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "task_type"}, {Name: "steam_id64"}, {Name: "appid"},
		},
		DoUpdates: clause.Assignments(map[string]any{
			"status":   StatusPending,
			"attempts": 0,
			// 取更早的执行时刻：新的紧急需求不应被旧的远期排期压住
			"next_run_at": gorm.Expr("LEAST(next_run_at, VALUES(next_run_at))"),
			"payload":     row.Payload,
			"priority":    row.Priority,
			"last_error":  "",
			"updated_at":  now,
		}),
	}).Create(&row).Error
}

func (q *MySQLQueue) Succeed(ctx context.Context, id uint64) error {
	now := q.nowFunc()
	return q.db.WithContext(ctx).Model(&store.SyncTask{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     StatusSucceeded,
			"last_error": "",
			"updated_at": now,
		}).Error
}

// Fail 按指数退避重排，超过 MaxAttempts 后转入死信。
func (q *MySQLQueue) Fail(ctx context.Context, id uint64, cause error) error {
	now := q.nowFunc()

	return q.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row store.SyncTask
		if err := tx.Where("id = ?", id).Take(&row).Error; err != nil {
			return err
		}

		attempts := row.Attempts + 1
		msg := truncate(cause.Error(), 512)

		if attempts >= MaxAttempts {
			return tx.Model(&store.SyncTask{}).Where("id = ?", id).
				Updates(map[string]any{
					"status":     StatusDead,
					"attempts":   attempts,
					"last_error": msg,
					"updated_at": now,
				}).Error
		}

		return tx.Model(&store.SyncTask{}).Where("id = ?", id).
			Updates(map[string]any{
				"status":      StatusRetrying,
				"attempts":    attempts,
				"last_error":  msg,
				"next_run_at": now.Add(backoff(attempts)),
				"updated_at":  now,
			}).Error
	})
}

// backoff 计算指数退避间隔：30s、60s、120s…… 上限 6 小时。
func backoff(attempts uint16) time.Duration {
	d := 30 * time.Second << (attempts - 1)
	if d > 6*time.Hour || d <= 0 {
		return 6 * time.Hour
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var _ = errors.New
var _ = fmt.Sprintf
```

> `Claim` 方法在 Task 9 实现。本步骤先让 `Enqueue`/`Succeed`/`Fail` 的测试通过。

- [ ] **Step 5: 临时跳过 Claim 相关用例并验证**

Run: `go test ./internal/task/ -run "Enqueue_Insert|Idempotent|EarlierNextRunAt" -v`
Expected: `TestEnqueue_Idempotent` 与 `TestEnqueue_TakesEarlierNextRunAt` PASS；`TestEnqueue_Insert` 因缺少 `Claim` 而编译失败 —— 这是预期的，进入 Task 9 补齐。

- [ ] **Step 6: 提交**

```bash
git add internal/task/
git commit -m "feat(task): 本地事务表入队幂等与指数退避"
```

---

## Task 9: 本地事务表 —— 领取、租约与并发安全

这是补偿方案的核心。没有租约，任何一次 worker 异常退出都会留下永久卡在「执行中」的任务。

**Files:**
- Modify: `internal/task/queue.go`（新增 `Claim`）
- Test: `internal/task/claim_test.go`

**Interfaces:**
- Consumes: Task 8 的全部类型与常量
- Produces: `(*MySQLQueue).Claim(ctx, limit int, lease time.Duration) ([]Task, error)`

- [ ] **Step 1: 写租约与并发的测试**

创建 `internal/task/claim_test.go`：

```go
package task

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/store"
)

// 领取后任务转为执行中，且 next_run_at 被推到租约到期时刻 ——
// 这使得扫描条件无需 OR，租约过期即自动可领。
func TestClaim_SetsRunningAndExtendsLease(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	got, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusRunning, row.Status)
	require.True(t, row.NextRunAt.After(time.Now().UTC().Add(4*time.Minute)),
		"next_run_at 应被推到租约到期时刻")
}

// 租约未过期时不可被重复领取。
func TestClaim_DoesNotStealLiveLease(t *testing.T) {
	q := NewMySQLQueue(testDB(t))
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	first, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 1)

	second, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Empty(t, second, "租约有效期内不得被其他 worker 领走")
}

// 这是整个补偿方案的关键测试：worker 崩溃后任务必须能被回收，
// 否则会永久卡在「执行中」。
func TestClaim_ReclaimsExpiredLease(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	claimed, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	// 模拟 worker 崩溃：租约到期但状态仍是「执行中」
	require.NoError(t, db.Model(&store.SyncTask{}).
		Where("id = ?", claimed[0].ID).
		Update("next_run_at", time.Now().UTC().Add(-time.Minute)).Error)

	reclaimed, err := q.Claim(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1, "租约过期的任务必须能被回收")
	require.Equal(t, claimed[0].ID, reclaimed[0].ID)
}

// SKIP LOCKED 保证多 worker 并发扫描不会取到同一条任务。
// 这个行为在 SQLite 或 mock 上无法验证，必须打真实 MySQL。
func TestClaim_ConcurrentWorkersDoNotOverlap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	const total = 30
	q := NewMySQLQueue(db)
	for i := 0; i < total; i++ {
		require.NoError(t, q.Enqueue(ctx, Task{
			Type: TypeAchievementSync, SteamID: 1, AppID: uint32(1000 + i),
			Priority: PriorityNormal, NextRunAt: time.Now().UTC().Add(-time.Second),
		}))
	}

	const workers = 4
	var (
		mu     sync.Mutex
		seen   = map[uint64]int{}
		wg     sync.WaitGroup
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wq := NewMySQLQueue(db)
			for {
				got, err := wq.Claim(ctx, 5, 5*time.Minute)
				if err != nil || len(got) == 0 {
					return
				}
				mu.Lock()
				for _, tk := range got {
					seen[tk.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	require.Len(t, seen, total, "所有任务都应被领取")
	for id, n := range seen {
		require.Equal(t, 1, n, "任务 %d 被领取了 %d 次，SKIP LOCKED 未生效", id, n)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/task/ -run Claim -v`
Expected: FAIL —— `q.Claim undefined`

- [ ] **Step 3: 实现 Claim**

在 `internal/task/queue.go` 末尾追加：

```go
// Claim 领取到期任务。
//
// 设计要点：统一使用 next_run_at 作为唯一的调度时间轴。领取时把 status 置为
// StatusRunning 并把 next_run_at 推到租约到期时刻，因此扫描条件不需要 OR：
//
//	status=0/3 且到期 → 正常待执行
//	status=1   且到期 → 租约已过期，持有它的 worker 已崩溃，自动回收
//
// FOR UPDATE SKIP LOCKED 保证多 worker 并发扫描互不阻塞、互不重复。
func (q *MySQLQueue) Claim(ctx context.Context, limit int, lease time.Duration) ([]Task, error) {
	now := q.nowFunc()
	var out []Task

	err := q.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []store.SyncTask
		if err := tx.Clauses(clause.Locking{
			Strength: "UPDATE",
			Options:  "SKIP LOCKED",
		}).
			Where("status IN ? AND next_run_at <= ?",
				[]int8{StatusPending, StatusRunning, StatusRetrying}, now).
			Order("priority, next_run_at").
			Limit(limit).
			Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		ids := make([]uint64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}

		// 续租：状态置为执行中，next_run_at 推到租约到期时刻
		if err := tx.Model(&store.SyncTask{}).
			Where("id IN ?", ids).
			Updates(map[string]any{
				"status":      StatusRunning,
				"next_run_at": now.Add(lease),
				"updated_at":  now,
			}).Error; err != nil {
			return err
		}

		for _, r := range rows {
			out = append(out, Task{
				ID:        r.ID,
				Type:      r.Type,
				SteamID:   r.SteamID,
				AppID:     r.AppID,
				Payload:   r.Payload,
				Priority:  r.Priority,
				Status:    StatusRunning,
				Attempts:  r.Attempts,
				NextRunAt: now.Add(lease),
				LastError: r.LastError,
			})
		}
		return nil
	})

	return out, err
}

var _ Queue = (*MySQLQueue)(nil)
```

- [ ] **Step 4: 运行全部任务表测试**

Run: `go test ./internal/task/ -v`
Expected: PASS（11 个用例）

`TestClaim_ConcurrentWorkersDoNotOverlap` 若失败，检查 MySQL 版本是否 ≥ 8.0.1 —— `SKIP LOCKED` 在更低版本会被静默忽略，导致任务被重复领取。

- [ ] **Step 5: 接上 API 的 Tasks 字段**

取消 `internal/api/router.go` 中 `Deps.Tasks` 字段与 `task` 导入的注释（Task 6 Step 9 遗留），然后编译验证：

```bash
go build ./... && go vet ./...
```

Expected: 通过

- [ ] **Step 6: 提交**

```bash
git add internal/task/ internal/api/
git commit -m "feat(task): SKIP LOCKED 领取与租约回收"
```

---

## Task 10: 会话状态机

纯函数，无 IO。全项目覆盖率要求最高的地方 —— 它出错会静默产生错误数据。

**Files:**
- Create: `internal/domain/session.go`
- Test: `internal/domain/session_test.go`

**Interfaces:**
- Consumes: 无（纯函数包，不依赖任何其他内部包）
- Produces:
  - `domain.State` 结构体：`AppID uint32`（0 表示 Idle）、`StartedAt time.Time`、`LastSeenPlayingAt time.Time`、`MissCount uint8`
  - `domain.Probe` 结构体：`GameID uint32`（0 表示不在玩）
  - `domain.EventKind`，常量 `domain.SessionStarted`、`domain.SessionEnded`
  - `domain.Event` 结构体：`Kind EventKind`、`AppID uint32`、`StartedAt time.Time`、`EndedAt time.Time`
  - `domain.Advance(prev State, p Probe, now time.Time) (State, []Event)`
  - `domain.MaxSessionDuration = 24 * time.Hour`
  - `domain.MissThreshold uint8 = 1`

- [ ] **Step 1: 写状态机的穷举测试**

创建 `internal/domain/session_test.go`：

```go
package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func playing(appID uint32, started, lastSeen time.Time, miss uint8) State {
	return State{AppID: appID, StartedAt: started, LastSeenPlayingAt: lastSeen, MissCount: miss}
}

func TestAdvance_TransitionTable(t *testing.T) {
	cases := []struct {
		name       string
		prev       State
		probe      Probe
		now        time.Time
		wantState  State
		wantEvents []EventKind
	}{
		{
			name:      "Idle 观测不到游戏，保持 Idle",
			prev:      State{},
			probe:     Probe{GameID: 0},
			now:       t0,
			wantState: State{},
		},
		{
			name:       "Idle 观测到游戏，开始会话",
			prev:       State{},
			probe:      Probe{GameID: 440},
			now:        t0,
			wantState:  playing(440, t0, t0, 0),
			wantEvents: []EventKind{SessionStarted},
		},
		{
			name:      "持续游玩同一游戏，起始时刻不变",
			prev:      playing(440, t0, t0, 0),
			probe:     Probe{GameID: 440},
			now:       t0.Add(2 * time.Minute),
			wantState: playing(440, t0, t0.Add(2*time.Minute), 0),
		},
		{
			name: "首次观测不到游戏，仅累加 miss 不结束会话",
			prev: playing(440, t0, t0, 0),
			probe: Probe{GameID: 0},
			now:  t0.Add(2 * time.Minute),
			// 关键：起始与最后在玩时刻都保持不变，只有 MissCount 变化
			wantState: playing(440, t0, t0, 1),
		},
		{
			name:       "连续两次观测不到，结束会话",
			prev:       playing(440, t0, t0, 1),
			probe:      Probe{GameID: 0},
			now:        t0.Add(4 * time.Minute),
			wantState:  State{},
			wantEvents: []EventKind{SessionEnded},
		},
		{
			name:       "切换游戏，同时产出结束与开始",
			prev:       playing(440, t0, t0, 0),
			probe:      Probe{GameID: 730},
			now:        t0.Add(2 * time.Minute),
			wantState:  playing(730, t0.Add(2*time.Minute), t0.Add(2*time.Minute), 0),
			wantEvents: []EventKind{SessionEnded, SessionStarted},
		},
		{
			name: "miss 后恢复游玩，计数归零且不产出事件",
			prev: playing(440, t0, t0, 1),
			probe: Probe{GameID: 440},
			now:  t0.Add(4 * time.Minute),
			wantState: playing(440, t0, t0.Add(4*time.Minute), 0),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, events := Advance(tc.prev, tc.probe, tc.now)
			require.Equal(t, tc.wantState, got)

			kinds := make([]EventKind, 0, len(events))
			for _, e := range events {
				kinds = append(kinds, e.Kind)
			}
			if tc.wantEvents == nil {
				require.Empty(t, kinds)
			} else {
				require.Equal(t, tc.wantEvents, kinds)
			}
		})
	}
}

// 会话结束时刻必须回填为最后一次观测到在玩的时刻，
// 而不是当前时刻 —— 否则会把两个探针周期的空档算进游玩时长。
func TestAdvance_EndedAtUsesLastSeenPlaying(t *testing.T) {
	lastSeen := t0.Add(10 * time.Minute)
	prev := playing(440, t0, lastSeen, 1)

	_, events := Advance(prev, Probe{GameID: 0}, t0.Add(14*time.Minute))
	require.Len(t, events, 1)

	require.Equal(t, SessionEnded, events[0].Kind)
	require.Equal(t, t0, events[0].StartedAt)
	require.Equal(t, lastSeen, events[0].EndedAt, "结束时刻应为最后在玩时刻，非当前时刻")
}

// 切换游戏时，被结束的那局同样用 LastSeenPlayingAt 作为结束时刻。
func TestAdvance_SwitchGameEndsPreviousAtLastSeen(t *testing.T) {
	lastSeen := t0.Add(6 * time.Minute)
	prev := playing(440, t0, lastSeen, 0)

	_, events := Advance(prev, Probe{GameID: 730}, t0.Add(8*time.Minute))
	require.Len(t, events, 2)

	require.Equal(t, uint32(440), events[0].AppID)
	require.Equal(t, lastSeen, events[0].EndedAt)
	require.Equal(t, uint32(730), events[1].AppID)
	require.Equal(t, t0.Add(8*time.Minute), events[1].StartedAt)
}

// 超长挂机强制结算并开启新会话，避免单条异常记录污染统计。
func TestAdvance_ForcesRolloverAfterMaxDuration(t *testing.T) {
	prev := playing(440, t0, t0.Add(23*time.Hour), 0)
	now := t0.Add(25 * time.Hour)

	got, events := Advance(prev, Probe{GameID: 440}, now)

	require.Len(t, events, 2)
	require.Equal(t, SessionEnded, events[0].Kind)
	require.Equal(t, SessionStarted, events[1].Kind)
	require.Equal(t, now, got.StartedAt, "应以当前时刻开启新会话")
	require.Equal(t, uint32(440), got.AppID)
}

// 状态机必须是纯函数：同样的输入永远得到同样的输出，且不修改入参。
func TestAdvance_IsPure(t *testing.T) {
	prev := playing(440, t0, t0, 0)
	snapshot := prev

	for i := 0; i < 3; i++ {
		got, events := Advance(prev, Probe{GameID: 0}, t0.Add(2*time.Minute))
		require.Equal(t, playing(440, t0, t0, 1), got)
		require.Empty(t, events)
	}
	require.Equal(t, snapshot, prev, "入参不得被修改")
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/domain/ -v`
Expected: FAIL —— `undefined: Advance`

- [ ] **Step 3: 实现状态机**

创建 `internal/domain/session.go`：

```go
// Package domain 存放不依赖任何 IO 的业务规则。
// 本包禁止导入 gorm、net/http，也禁止调用 time.Now —— 时钟一律由调用方传入。
// 这使得所有边界情况都能用表驱动测试穷举验证。
package domain

import "time"

// MissThreshold 是判定会话结束前允许的连续「观测不到」次数。
//
// 去抖不是优化，是必需的：Steam 的 GetPlayerSummaries 偶发返回不完整数据，
// 没有去抖时一次网络抖动就会把一局连续游戏切割成多段碎片会话。
const MissThreshold uint8 = 1

// MaxSessionDuration 是单条会话的时长上限，超过后强制结算并开启新会话。
const MaxSessionDuration = 24 * time.Hour

type EventKind int

const (
	SessionStarted EventKind = iota + 1
	SessionEnded
)

// State 是单个用户的会话状态。AppID 为 0 表示 Idle（当前不在玩）。
type State struct {
	AppID             uint32
	StartedAt         time.Time
	LastSeenPlayingAt time.Time
	MissCount         uint8
}

func (s State) isIdle() bool { return s.AppID == 0 }

// Probe 是一次探针观测结果。GameID 为 0 表示当前不在玩游戏。
//
// 重要：调用方绝不能在「探针请求失败」时构造 Probe{GameID: 0}。
// 请求失败与「用户没在玩」是两回事，前者应当直接跳过本轮、保持状态不变。
type Probe struct {
	GameID uint32
}

type Event struct {
	Kind      EventKind
	AppID     uint32
	StartedAt time.Time
	EndedAt   time.Time
}

// Advance 推进状态机一步，返回新状态与产出的事件。
// 纯函数：不修改入参，相同输入永远产生相同输出。
func Advance(prev State, p Probe, now time.Time) (State, []Event) {
	switch {
	case prev.isIdle() && p.GameID == 0:
		return prev, nil

	case prev.isIdle():
		return start(p.GameID, now), []Event{{
			Kind: SessionStarted, AppID: p.GameID, StartedAt: now,
		}}

	case p.GameID == prev.AppID:
		// 持续游玩。先检查是否超过时长上限，需要强制翻篇。
		if now.Sub(prev.StartedAt) > MaxSessionDuration {
			return start(prev.AppID, now), []Event{
				endEvent(prev),
				{Kind: SessionStarted, AppID: prev.AppID, StartedAt: now},
			}
		}
		next := prev
		next.LastSeenPlayingAt = now
		next.MissCount = 0
		return next, nil

	case p.GameID == 0 && prev.MissCount < MissThreshold:
		// 去抖：仅累加计数，不改动任何时刻，不产出事件
		next := prev
		next.MissCount++
		return next, nil

	case p.GameID == 0:
		return State{}, []Event{endEvent(prev)}

	default:
		// 切换到了另一款游戏
		return start(p.GameID, now), []Event{
			endEvent(prev),
			{Kind: SessionStarted, AppID: p.GameID, StartedAt: now},
		}
	}
}

func start(appID uint32, now time.Time) State {
	return State{AppID: appID, StartedAt: now, LastSeenPlayingAt: now, MissCount: 0}
}

// endEvent 用 LastSeenPlayingAt 而非当前时刻作为结束时刻 ——
// 当前时刻已经晚了一到两个探针周期，用它会把空档算进游玩时长。
func endEvent(s State) Event {
	return Event{
		Kind:      SessionEnded,
		AppID:     s.AppID,
		StartedAt: s.StartedAt,
		EndedAt:   s.LastSeenPlayingAt,
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/domain/ -v -cover`
Expected: PASS（11 个子用例 + 4 个独立用例），覆盖率应达到 100%

- [ ] **Step 5: 验证包的纯净性**

确认 `internal/domain` 没有引入任何 IO 依赖：

```bash
go list -deps ./internal/domain | grep -E "gorm|redis|net/http" || echo "OK: domain 包无 IO 依赖"
```

Expected: 输出 `OK: domain 包无 IO 依赖`

- [ ] **Step 6: 提交**

```bash
git add internal/domain/
git commit -m "feat(domain): 会话状态机与去抖逻辑"
```

---

## Task 11: 探针状态仓储与 L0 调度器

**Files:**
- Create: `internal/store/probe_repo.go`, `internal/collector/probe.go`
- Test: `internal/store/probe_repo_test.go`, `internal/collector/probe_test.go`

**Interfaces:**
- Consumes: `domain.State`/`domain.Probe`/`domain.Advance`（Task 10）、`steam.Client`/`steam.MaxSummariesBatch`（Task 2）、`task.Queue`（Task 8、9）、`store.ProbeState`（Task 5）
- Produces:
  - `store.NewProbeRepo(db *gorm.DB) *ProbeRepo`，方法：
    - `Ensure(ctx, steamID uint64, now time.Time) error` — 绑定时初始化
    - `Due(ctx, now time.Time, limit int) ([]ProbeState, error)`
    - `Save(ctx, steamID uint64, s domain.State, tier int8, nextProbeAt, now time.Time) error`
    - `Stale(ctx, before time.Time) ([]ProbeState, error)` — 供启动自愈使用
  - `store.ToDomain(p ProbeState) domain.State`、`store.FromDomain(s domain.State) (appID *uint32, started, lastSeen *time.Time, miss int8)`
  - `collector.NewProber(deps ProberDeps) *Prober`，`collector.ProberDeps` 字段：`Steam steam.Client`、`Probes *store.ProbeRepo`、`Tasks task.Queue`、`Now func() time.Time`
  - `(*Prober).RunOnce(ctx context.Context) error`

- [ ] **Step 1: 写探针仓储的测试**

创建 `internal/store/probe_repo_test.go`：

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/domain"
)

func TestProbeRepo_EnsureAndDue(t *testing.T) {
	r := NewProbeRepo(testDB(t))
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 76561197960287930, now))

	due, err := r.Due(ctx, now, 100)
	require.NoError(t, err)
	require.Len(t, due, 1, "新建的探针状态应立即到期，让新用户马上被采集")
}

// Ensure 必须幂等 —— 重新绑定时不能重置正在进行的会话。
func TestProbeRepo_EnsureIsIdempotent(t *testing.T) {
	db := testDB(t)
	r := NewProbeRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, now))
	require.NoError(t, r.Save(ctx, 1,
		domain.State{AppID: 440, StartedAt: now, LastSeenPlayingAt: now},
		0, now.Add(2*time.Minute), now))

	require.NoError(t, r.Ensure(ctx, 1, now.Add(time.Hour)))

	var row ProbeState
	require.NoError(t, db.Where("steam_id64 = ?", uint64(1)).Take(&row).Error)
	require.NotNil(t, row.CurrentAppID, "已有的会话状态不得被 Ensure 覆盖")
	require.Equal(t, uint32(440), *row.CurrentAppID)
}

// 状态在 domain 与存储之间往返必须无损。
func TestProbeRepo_SaveLoadRoundTrip(t *testing.T) {
	r := NewProbeRepo(testDB(t))
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, now))

	want := domain.State{
		AppID:             730,
		StartedAt:         now,
		LastSeenPlayingAt: now.Add(4 * time.Minute),
		MissCount:         1,
	}
	require.NoError(t, r.Save(ctx, 1, want, 0, now.Add(6*time.Minute), now))

	due, err := r.Due(ctx, now.Add(10*time.Minute), 100)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, want, ToDomain(due[0]))
}

// Idle 状态必须把 current_appid 写回 NULL，不能残留旧 appid。
func TestProbeRepo_SaveIdleClearsAppID(t *testing.T) {
	db := testDB(t)
	r := NewProbeRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, now))
	require.NoError(t, r.Save(ctx, 1,
		domain.State{AppID: 440, StartedAt: now, LastSeenPlayingAt: now},
		0, now, now))
	require.NoError(t, r.Save(ctx, 1, domain.State{}, 0, now.Add(time.Minute), now))

	var row ProbeState
	require.NoError(t, db.Where("steam_id64 = ?", uint64(1)).Take(&row).Error)
	require.Nil(t, row.CurrentAppID)
	require.Nil(t, row.SessionStartedAt)
}

// 供启动自愈使用：找出长时间未被探测但仍标记为「在玩」的僵尸会话。
func TestProbeRepo_StaleFindsZombieSessions(t *testing.T) {
	r := NewProbeRepo(testDB(t))
	ctx := context.Background()
	old := time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, r.Ensure(ctx, 1, old))
	require.NoError(t, r.Save(ctx, 1,
		domain.State{AppID: 440, StartedAt: old, LastSeenPlayingAt: old},
		0, old.Add(2*time.Minute), old))

	require.NoError(t, r.Ensure(ctx, 2, old))
	require.NoError(t, r.Save(ctx, 2, domain.State{}, 0, old, old))

	stale, err := r.Stale(ctx, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, stale, 1, "只有仍在 Playing 的僵尸会话需要自愈")
	require.Equal(t, uint64(1), stale[0].SteamID)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/store/ -run ProbeRepo -v`
Expected: FAIL —— `undefined: NewProbeRepo`

- [ ] **Step 3: 实现探针仓储**

创建 `internal/store/probe_repo.go`：

```go
package store

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"steamlink/internal/domain"
)

type ProbeRepo struct{ db *gorm.DB }

func NewProbeRepo(db *gorm.DB) *ProbeRepo { return &ProbeRepo{db: db} }

// Ensure 在用户绑定时初始化探针状态。
// 使用 DO NOTHING 保证幂等 —— 重新绑定不得重置正在进行的会话。
func (r *ProbeRepo) Ensure(ctx context.Context, steamID uint64, now time.Time) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).
		Create(&ProbeState{
			SteamID:     steamID,
			Tier:        0, // 新用户按最高频率采集，后续由分层规则调整
			NextProbeAt: now,
			UpdatedAt:   now,
		}).Error
}

// Due 返回到期待探测的用户，按到期时刻升序。
func (r *ProbeRepo) Due(ctx context.Context, now time.Time, limit int) ([]ProbeState, error) {
	var out []ProbeState
	err := r.db.WithContext(ctx).
		Where("next_probe_at <= ?", now).
		Order("next_probe_at").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// Save 落库状态机推进后的新状态。
// Idle 时必须把 current_appid 等字段写回 NULL，不能残留旧值。
func (r *ProbeRepo) Save(ctx context.Context, steamID uint64, s domain.State,
	tier int8, nextProbeAt, now time.Time) error {

	appID, started, lastSeen, miss := FromDomain(s)

	return r.db.WithContext(ctx).Model(&ProbeState{}).
		Where("steam_id64 = ?", steamID).
		Updates(map[string]any{
			"current_appid":        appID,
			"session_started_at":   started,
			"last_seen_playing_at": lastSeen,
			"miss_count":           miss,
			"tier":                 tier,
			"last_probe_at":        now,
			"next_probe_at":        nextProbeAt,
			"updated_at":           now,
		}).Error
}

// Stale 返回 last_probe_at 早于 before 且仍标记为「在玩」的记录。
// worker 长时间宕机后，这些会话的时长已不可信，需要强制结算（设计文档 §9.4）。
func (r *ProbeRepo) Stale(ctx context.Context, before time.Time) ([]ProbeState, error) {
	var out []ProbeState
	err := r.db.WithContext(ctx).
		Where("current_appid IS NOT NULL AND last_probe_at < ?", before).
		Find(&out).Error
	return out, err
}

// ToDomain 把存储行转成状态机可用的纯值。
func ToDomain(p ProbeState) domain.State {
	var s domain.State
	if p.CurrentAppID != nil {
		s.AppID = *p.CurrentAppID
	}
	if p.SessionStartedAt != nil {
		s.StartedAt = *p.SessionStartedAt
	}
	if p.LastSeenPlayingAt != nil {
		s.LastSeenPlayingAt = *p.LastSeenPlayingAt
	}
	if p.MissCount > 0 {
		s.MissCount = uint8(p.MissCount)
	}
	return s
}

// FromDomain 把状态机的值拆成可写入的可空字段。
func FromDomain(s domain.State) (appID *uint32, started, lastSeen *time.Time, miss int8) {
	if s.AppID == 0 {
		return nil, nil, nil, 0
	}
	a, st, ls := s.AppID, s.StartedAt, s.LastSeenPlayingAt
	return &a, &st, &ls, int8(s.MissCount)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/store/ -run ProbeRepo -v`
Expected: PASS（5 个用例）

- [ ] **Step 5: 写 L0 调度器的测试**

创建 `internal/collector/probe_test.go`：

```go
package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/domain"
	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type stubSteam struct {
	steam.Client
	calls   [][]uint64
	results map[uint64]uint32 // steamID → gameID
	err     error
}

func (s *stubSteam) GetPlayerSummaries(_ context.Context, ids []uint64) ([]steam.PlayerSummary, error) {
	s.calls = append(s.calls, append([]uint64(nil), ids...))
	if s.err != nil {
		return nil, s.err
	}
	out := make([]steam.PlayerSummary, 0, len(ids))
	for _, id := range ids {
		out = append(out, steam.PlayerSummary{
			SteamID:                  id,
			CommunityVisibilityState: 3,
			GameID:                   s.results[id],
		})
	}
	return out, nil
}

func newProbeFixture(t *testing.T, now time.Time, ids ...uint64) (*store.ProbeRepo, task.Queue, *gorm.DB) {
	t.Helper()
	db := storeTestDB(t)
	pr := store.NewProbeRepo(db)
	for _, id := range ids {
		require.NoError(t, pr.Ensure(context.Background(), id, now))
	}
	return pr, task.NewMySQLQueue(db), db
}

// 100 个以内的用户应合并为一次请求 —— 这是整个方案的成本支点。
func TestProber_BatchesUpToLimit(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	ids := make([]uint64, 0, 150)
	for i := 0; i < 150; i++ {
		ids = append(ids, uint64(76561197960287930+i))
	}
	pr, q, _ := newProbeFixture(t, now, ids...)

	st := &stubSteam{results: map[uint64]uint32{}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})

	require.NoError(t, p.RunOnce(context.Background()))

	require.Len(t, st.calls, 2, "150 个用户应拆成 2 批")
	require.Len(t, st.calls[0], steam.MaxSummariesBatch)
	require.Len(t, st.calls[1], 50)
}

// 观测到用户在玩游戏 → 状态转为 Playing，但此时不产生任何任务。
func TestProber_StartsSession(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, db := newProbeFixture(t, now, 1)

	st := &stubSteam{results: map[uint64]uint32{1: 440}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})
	require.NoError(t, p.RunOnce(context.Background()))

	due, err := pr.Due(context.Background(), now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, uint32(440), store.ToDomain(due[0]).AppID)

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n, "会话开始不应产生任务，只有结束才需要结算")
}

// 会话结束 → 入队结算任务，且延迟 5 分钟执行。
func TestProber_EnqueuesSettleTaskOnSessionEnd(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, db := newProbeFixture(t, now, 1)
	ctx := context.Background()

	// 预置一个已累计 1 次 miss 的进行中会话，下一轮观测不到即结束
	require.NoError(t, pr.Save(ctx, 1, domain.State{
		AppID: 440, StartedAt: now.Add(-30 * time.Minute),
		LastSeenPlayingAt: now.Add(-2 * time.Minute), MissCount: 1,
	}, 0, now, now))

	st := &stubSteam{results: map[uint64]uint32{1: 0}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})
	require.NoError(t, p.RunOnce(ctx))

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, task.TypeSessionSettle, row.Type)
	require.Equal(t, uint32(440), row.AppID)
	require.Equal(t, task.PriorityRealtime, row.Priority)
	require.Equal(t, now.Add(SettleDelay).Unix(), row.NextRunAt.Unix(),
		"Steam 的 playtime 在退出后才结算，必须延迟查询")
}

// 关键安全测试：请求失败绝不能被当成「所有人都没在玩」。
// 否则一次网络抖动会让整批用户的会话被同时误判结束。
func TestProber_RequestFailureDoesNotEndSessions(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, db := newProbeFixture(t, now, 1)
	ctx := context.Background()

	require.NoError(t, pr.Save(ctx, 1, domain.State{
		AppID: 440, StartedAt: now.Add(-30 * time.Minute),
		LastSeenPlayingAt: now.Add(-2 * time.Minute), MissCount: 1,
	}, 0, now, now))

	st := &stubSteam{err: errors.New("connection reset")}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})

	require.Error(t, p.RunOnce(ctx))

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n, "请求失败不得产生任何结算任务")

	due, err := pr.Due(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, uint32(440), store.ToDomain(due[0]).AppID, "会话状态必须保持不变")
}

// Steam 返回的 players 数组可能缺少某些 SteamID（账号被封等），
// 这些用户同样不能被当作「没在玩」。
func TestProber_MissingPlayerInResponseIsSkipped(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, db := newProbeFixture(t, now, 1, 2)
	ctx := context.Background()

	require.NoError(t, pr.Save(ctx, 2, domain.State{
		AppID: 440, StartedAt: now.Add(-time.Hour),
		LastSeenPlayingAt: now.Add(-2 * time.Minute), MissCount: 1,
	}, 0, now, now))

	// stub 只返回 id=1，id=2 缺失
	st := &partialSteam{present: map[uint64]uint32{1: 0}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})
	require.NoError(t, p.RunOnce(ctx))

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n, "响应中缺失的用户应跳过，不得判定会话结束")
}

type partialSteam struct {
	steam.Client
	present map[uint64]uint32
}

func (s *partialSteam) GetPlayerSummaries(_ context.Context, ids []uint64) ([]steam.PlayerSummary, error) {
	var out []steam.PlayerSummary
	for _, id := range ids {
		if g, ok := s.present[id]; ok {
			out = append(out, steam.PlayerSummary{
				SteamID: id, CommunityVisibilityState: 3, GameID: g,
			})
		}
	}
	return out, nil
}
```

在 `internal/collector/` 下创建 `testutil_test.go` 提供共享的数据库句柄：

```go
package collector

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"steamlink/internal/store"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func storeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"
	}
	db, err := store.NewDB(dsn, testLogger())
	require.NoError(t, err, "需要本地 MySQL 并已初始化：./scripts/dev/up.sh")

	for _, tbl := range []string{
		"sync_tasks", "probe_state", "achievement_unlocks",
		"play_sessions", "user_games", "app_achievements", "apps", "steam_links",
	} {
		require.NoError(t, db.Exec("DELETE FROM "+tbl).Error)
	}
	return db
}
```

并在 `probe_test.go` 顶部补上 `"gorm.io/gorm"` 导入。

- [ ] **Step 6: 运行测试确认失败**

Run: `go test ./internal/collector/ -v`
Expected: FAIL —— `undefined: NewProber`

- [ ] **Step 7: 实现 L0 调度器**

创建 `internal/collector/probe.go`：

```go
package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"steamlink/internal/domain"
	"steamlink/internal/logging"
	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

// SettleDelay 是会话结束到查询时长之间的等待时间。
// Steam 的 playtime_forever 在游戏退出后才最终结算，立刻查询会拿到旧值。
const SettleDelay = 5 * time.Minute

// DefaultProbeInterval 是探针的基础间隔。分层调整在 Task 18 引入。
const DefaultProbeInterval = 2 * time.Minute

// maxDuePerRun 限制单轮取出的用户数，避免一次拉取过多。
const maxDuePerRun = 1000

type ProberDeps struct {
	Steam  steam.Client
	Probes *store.ProbeRepo
	Tasks  task.Queue
	Now    func() time.Time
	Logger *slog.Logger
}

type Prober struct {
	d  ProberDeps
	lg *slog.Logger
}

func NewProber(d ProberDeps) *Prober {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Prober{d: d, lg: d.Logger.With("component", "prober")}
}

// RunOnce 执行一轮探测：取出到期用户 → 分批调用 → 推进状态机 → 落库并入队。
func (p *Prober) RunOnce(ctx context.Context) error {
	now := p.d.Now()

	due, err := p.d.Probes.Due(ctx, now, maxDuePerRun)
	if err != nil {
		return fmt.Errorf("collector: 查询到期用户失败: %w", err)
	}
	if len(due) == 0 {
		return nil
	}

	var firstErr error
	for _, batch := range chunk(due, steam.MaxSummariesBatch) {
		if err := p.runBatch(ctx, batch, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *Prober) runBatch(ctx context.Context, batch []store.ProbeState, now time.Time) error {
	ids := make([]uint64, 0, len(batch))
	for _, b := range batch {
		ids = append(ids, b.SteamID)
	}

	sums, err := p.d.Steam.GetPlayerSummaries(ctx, ids)
	if err != nil {
		// 关键：请求失败与「用户没在玩」是两回事。直接返回，
		// 保持所有状态不变，等下一轮重试。绝不能把空结果喂给状态机。
		p.lg.Warn("探针批次请求失败，本轮跳过且状态不变",
			slog.Int("batch_size", len(ids)), slog.String("err", err.Error()))
		return fmt.Errorf("collector: 探针请求失败: %w", err)
	}

	observed := make(map[uint64]uint32, len(sums))
	for _, s := range sums {
		observed[s.SteamID] = s.GameID
	}

	var missing int
	for _, row := range batch {
		gameID, present := observed[row.SteamID]
		if !present {
			// 响应中缺失该用户（账号被封、SteamID 失效等）。
			// 同样不能判定为「没在玩」，跳过本轮。
			missing++
			p.lg.Debug("响应中缺失该用户，跳过本轮", logging.SteamID(row.SteamID))
			continue
		}
		if err := p.advanceOne(ctx, row, gameID, now); err != nil {
			return err
		}
	}

	// 持续性的缺失说明有一批账号已失效，值得在运维层面关注
	if missing > 0 {
		p.lg.Info("探针批次存在缺失用户",
			slog.Int("missing", missing), slog.Int("batch_size", len(ids)))
	}
	return nil
}

func (p *Prober) advanceOne(ctx context.Context, row store.ProbeState,
	gameID uint32, now time.Time) error {

	prev := store.ToDomain(row)
	next, events := domain.Advance(prev, domain.Probe{GameID: gameID}, now)

	for _, e := range events {
		if e.Kind != domain.SessionEnded {
			continue // 会话开始无需任何后续动作
		}
		if err := p.enqueueSettle(ctx, row.SteamID, e, now); err != nil {
			return err
		}
	}

	nextAt := now.Add(DefaultProbeInterval)
	return p.d.Probes.Save(ctx, row.SteamID, next, row.Tier, nextAt, now)
}

func (p *Prober) enqueueSettle(ctx context.Context, steamID uint64,
	e domain.Event, now time.Time) error {

	payload, err := json.Marshal(task.SessionPayload{
		StartedAt: e.StartedAt,
		EndedAt:   e.EndedAt,
	})
	if err != nil {
		return err
	}

	return p.d.Tasks.Enqueue(ctx, task.Task{
		Type:      task.TypeSessionSettle,
		SteamID:   steamID,
		AppID:     e.AppID,
		Payload:   payload,
		Priority:  task.PriorityRealtime,
		NextRunAt: now.Add(SettleDelay),
	})
}

func chunk[T any](s []T, size int) [][]T {
	var out [][]T
	for size < len(s) {
		s, out = s[size:], append(out, s[0:size:size])
	}
	return append(out, s)
}
```

- [ ] **Step 8: 运行测试确认通过**

Run: `go test ./internal/collector/ -v`
Expected: PASS（5 个用例）

`TestProber_RequestFailureDoesNotEndSessions` 是最关键的一个 —— 它验证了错误与空结果的严格区分。

- [ ] **Step 9: 在绑定流程中初始化探针状态**

修改 `internal/api/auth_handler.go` 的 `probeAndPersist`，在写入游戏库后追加：

```go
	if state == store.VisibilityOK && len(games) > 0 {
		_ = d.Games.UpsertApps(ctx, games)
		_ = d.Games.UpsertUserGames(ctx, steamID, games, now)
		// 初始化探针状态，让新用户立即进入采集范围
		_ = d.Probes.Ensure(ctx, steamID, now)
	}
```

在 `internal/api/router.go` 的 `Deps` 中增加字段：

```go
	Probes *store.ProbeRepo
```

在 `cmd/api/main.go` 的 `api.Deps{...}` 中补上：

```go
		Probes: store.NewProbeRepo(db),
```

- [ ] **Step 10: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 11: 提交**

```bash
git add internal/store/ internal/collector/ internal/api/ cmd/api/
git commit -m "feat(collector): L0 批量探针与会话事件入队"
```

---

## Task 12: worker 主循环

**Files:**
- Create: `internal/task/runner.go`, `cmd/worker/main.go`
- Test: `internal/task/runner_test.go`

**Interfaces:**
- Consumes: `task.Queue`（Task 8、9）、`collector.Prober`（Task 11）
- Produces:
  - `task.Handler` 函数类型：`func(ctx context.Context, t Task) error`
  - `task.NewRunner(q Queue, opts RunnerOptions) *Runner`
  - `task.RunnerOptions` 结构体：`Concurrency int`、`PollInterval time.Duration`、`Lease time.Duration`、`Logger *slog.Logger`
  - `(*Runner).Register(typ int8, h Handler)`
  - `(*Runner).RunOnce(ctx context.Context) (int, error)` — 返回本轮处理的任务数
  - `(*Runner).Start(ctx context.Context)` — 阻塞直到 ctx 取消
  - `task.ErrPermanent` — handler 返回它的包装错误时直接置为成功，不重试

- [ ] **Step 1: 写主循环的测试**

创建 `internal/task/runner_test.go`：

```go
package task

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/store"
)

func TestRunner_DispatchesByType(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	var got atomic.Int64
	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	r.Register(TypeLibrarySync, func(_ context.Context, tk Task) error {
		got.Store(int64(tk.SteamID))
		return nil
	})

	n, err := r.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, int64(1), got.Load())

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusSucceeded, row.Status)
}

// handler 返回普通错误 → 退避重试。
func TestRunner_FailureSchedulesRetry(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	r.Register(TypeLibrarySync, func(context.Context, Task) error {
		return errors.New("transient failure")
	})

	_, err := r.RunOnce(ctx)
	require.NoError(t, err)

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusRetrying, row.Status)
	require.Equal(t, uint16(1), row.Attempts)
}

// 包装了 ErrPermanent 的错误 → 直接置为成功，不重试。
// 这是「该游戏没有成就系统」等场景的处理方式：
// 它不是失败，重试永远不会成功，只会白白消耗配额。
func TestRunner_PermanentErrorMarksSucceeded(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeAchievementSync, SteamID: 1, AppID: 620,
		Priority: PriorityNormal, NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	r.Register(TypeAchievementSync, func(context.Context, Task) error {
		return fmt.Errorf("app has no stats: %w", ErrPermanent)
	})

	_, err := r.RunOnce(ctx)
	require.NoError(t, err)

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusSucceeded, row.Status)
	require.Equal(t, uint16(0), row.Attempts, "永久错误不应累加重试次数")
}

// 未注册的任务类型不能让 worker 崩溃，也不能无限重试。
func TestRunner_UnregisteredTypeGoesToRetry(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeSchemaSync, AppID: 620,
		Priority: PriorityNormal, NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	_, err := r.RunOnce(ctx)
	require.NoError(t, err)

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusRetrying, row.Status)
	require.Contains(t, row.LastError, "no handler")
}

// handler panic 必须被捕获，不能带崩整个 worker。
func TestRunner_RecoversFromPanic(t *testing.T) {
	db := testDB(t)
	q := NewMySQLQueue(db)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, Task{
		Type: TypeLibrarySync, SteamID: 1, Priority: PriorityNormal,
		NextRunAt: time.Now().UTC().Add(-time.Second),
	}))

	r := NewRunner(q, RunnerOptions{Concurrency: 1})
	r.Register(TypeLibrarySync, func(context.Context, Task) error {
		panic("boom")
	})

	require.NotPanics(t, func() {
		_, _ = r.RunOnce(ctx)
	})

	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, StatusRetrying, row.Status)
	require.Contains(t, row.LastError, "panic")
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/task/ -run Runner -v`
Expected: FAIL —— `undefined: NewRunner`

- [ ] **Step 3: 实现主循环**

创建 `internal/task/runner.go`：

```go
package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"steamlink/internal/logging"
)

// ErrPermanent 标记不可恢复的失败。handler 用 %w 包装它返回时，
// 任务会被直接置为成功而非重试 —— 例如「该游戏没有成就系统」，
// 重试永远不会成功，只会持续消耗配额。
var ErrPermanent = errors.New("task: permanent failure, do not retry")

type Handler func(ctx context.Context, t Task) error

type RunnerOptions struct {
	Concurrency  int
	PollInterval time.Duration
	Lease        time.Duration
	Logger       *slog.Logger
}

type Runner struct {
	q        Queue
	handlers map[int8]Handler
	opts     RunnerOptions
	lg       *slog.Logger
}

func NewRunner(q Queue, opts RunnerOptions) *Runner {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.Lease <= 0 {
		opts.Lease = LeaseDuration
	}
	// 未注入 Logger 时静默，而不是回退到 slog.Default() ——
	// 全局默认 Logger 会绕过项目的日志配置，且让测试输出变脏。
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		q:        q,
		handlers: map[int8]Handler{},
		opts:     opts,
		lg:       opts.Logger.With("component", "task-runner"),
	}
}

func (r *Runner) Register(typ int8, h Handler) { r.handlers[typ] = h }

// RunOnce 领取一批任务并并发执行，返回处理的任务数。
func (r *Runner) RunOnce(ctx context.Context) (int, error) {
	tasks, err := r.q.Claim(ctx, r.opts.Concurrency*4, r.opts.Lease)
	if err != nil {
		return 0, err
	}
	if len(tasks) == 0 {
		return 0, nil
	}

	sem := make(chan struct{}, r.opts.Concurrency)
	var wg sync.WaitGroup

	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t Task) {
			defer wg.Done()
			defer func() { <-sem }()
			r.execute(ctx, t)
		}(t)
	}
	wg.Wait()

	return len(tasks), nil
}

func (r *Runner) execute(ctx context.Context, t Task) {
	lg := r.lg.With(
		slog.Uint64("task_id", t.ID),
		slog.Int("task_type", int(t.Type)),
		logging.SteamID(t.SteamID),
		slog.Uint64("appid", uint64(t.AppID)),
	)

	err := r.invoke(ctx, t)

	switch {
	case err == nil:
		lg.Debug("任务执行成功")
		if e := r.q.Succeed(ctx, t.ID); e != nil {
			lg.Error("标记任务成功失败", slog.String("err", e.Error()))
		}

	case errors.Is(err, ErrPermanent):
		// 永久失败也算「处理完毕」：重试没有意义
		lg.Info("任务永久失败，不再重试", slog.String("err", err.Error()))
		if e := r.q.Succeed(ctx, t.ID); e != nil {
			lg.Error("标记任务成功失败", slog.String("err", e.Error()))
		}

	default:
		lg.Warn("任务执行失败，将退避重试",
			slog.Int("attempts", int(t.Attempts)),
			slog.String("err", err.Error()))
		if e := r.q.Fail(ctx, t.ID, err); e != nil {
			lg.Error("标记任务失败失败", slog.String("err", e.Error()))
		}
	}
}

// invoke 调用 handler 并捕获 panic —— 单个任务的 bug 不应带崩整个 worker。
func (r *Runner) invoke(ctx context.Context, t Task) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("task: handler panic: %v", p)
		}
	}()

	h, ok := r.handlers[t.Type]
	if !ok {
		return fmt.Errorf("task: no handler registered for type %d", t.Type)
	}
	return h(ctx, t)
}

// Start 持续轮询直到 ctx 取消。无任务时按 PollInterval 休眠，
// 有任务时立即继续下一轮，保证积压能被快速消化。
func (r *Runner) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := r.RunOnce(ctx)
		if err != nil {
			r.lg.Error("任务轮询失败", slog.String("err", err.Error()))
		}
		if n > 0 {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(r.opts.PollInterval):
		}
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/task/ -v`
Expected: PASS（16 个用例）

- [ ] **Step 5: 写 worker 入口**

创建 `cmd/worker/main.go`：

```go
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

	prober := collector.NewProber(collector.ProberDeps{
		Steam: sc, Probes: probes, Tasks: queue, Logger: lg,
	})

	runner := task.NewRunner(queue, task.RunnerOptions{
		Concurrency:  cfg.Worker.Concurrency,
		PollInterval: cfg.Worker.PollInterval,
		Logger:       lg,
	})
	// handler 在 Task 13、14、15、16 中逐步注册

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 探针独立于任务队列，按固定节拍运行
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
```

> `collector.ProberDeps` 需要增加 `Logger *slog.Logger` 字段；`NewProber` 中为 nil 时回退到 `slog.New(slog.DiscardHandler)`，并 `With("component", "prober")`。

> 探针的 ticker 设为 30 秒而非 2 分钟：`next_probe_at` 才是真正的节流阀，ticker 只是驱动检查的节拍，更密的节拍能让分层中不同间隔的用户按时被采集。

- [ ] **Step 6: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 7: 提交**

```bash
git add internal/task/ cmd/worker/
git commit -m "feat(task): worker 主循环、永久错误与 panic 隔离"
```

---

## Task 13: L1 会话结算

**Files:**
- Create: `internal/store/session_repo.go`, `internal/collector/settle.go`
- Modify: `cmd/worker/main.go`（注册 handler）
- Test: `internal/store/session_repo_test.go`, `internal/collector/settle_test.go`

**Interfaces:**
- Consumes: `task.Task`/`task.SessionPayload`/`task.ErrPermanent`（Task 8、12）、`store.GameRepo.PlaytimeMap`（Task 5）、`steam.Client`（Task 2）
- Produces:
  - `store.NewSessionRepo(db *gorm.DB) *SessionRepo`，方法：
    - `Insert(ctx, s PlaySession) (bool, error)` — 返回 `false` 表示唯一键冲突（重复结算，非错误）
    - `SetPlaytime(ctx, steamID uint64, appID uint32, minutes uint32, lastPlayed *time.Time, now time.Time) error`
    - `HasSessionOn(ctx, steamID uint64, appID uint32, day time.Time) (bool, error)`
  - `store.GameRepo.HasAchievements(ctx, appID uint32) (int8, error)` — 返回 -1 未知 / 0 无成就 / 1 有成就
  - `collector.NewSettler(deps SettlerDeps) *Settler`，`collector.SettlerDeps` 字段：`Steam steam.Client`、`Games *store.GameRepo`、`Sessions *store.SessionRepo`、`Tasks task.Queue`、`Now func() time.Time`
  - `(*Settler).Handle(ctx context.Context, t task.Task) error`

- [ ] **Step 1: 写会话仓储的测试**

创建 `internal/store/session_repo_test.go`：

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sampleSession(started time.Time) PlaySession {
	return PlaySession{
		SteamID: 76561197960287930, AppID: 440,
		StartedAt: started, EndedAt: started.Add(30 * time.Minute),
		DurationMin: 30, Source: SourceProbe,
		CreatedAt: started,
	}
}

func TestSessionRepo_Insert(t *testing.T) {
	r := NewSessionRepo(testDB(t))
	started := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	ok, err := r.Insert(context.Background(), sampleSession(started))
	require.NoError(t, err)
	require.True(t, ok)
}

// 租约回收会导致任务重跑。重复写入必须被唯一键挡住，
// 且不能当作错误上报 —— 否则任务会被无谓地重试到死信。
func TestSessionRepo_InsertIsIdempotent(t *testing.T) {
	db := testDB(t)
	r := NewSessionRepo(db)
	ctx := context.Background()
	started := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	ok, err := r.Insert(ctx, sampleSession(started))
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = r.Insert(ctx, sampleSession(started))
	require.NoError(t, err, "重复写入不是错误")
	require.False(t, ok, "应报告未插入")

	var n int64
	require.NoError(t, db.Model(&PlaySession{}).Count(&n).Error)
	require.Equal(t, int64(1), n)
}

// 同一游戏的不同会话（起始时刻不同）互不冲突。
func TestSessionRepo_DifferentStartTimesCoexist(t *testing.T) {
	db := testDB(t)
	r := NewSessionRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	_, err := r.Insert(ctx, sampleSession(base))
	require.NoError(t, err)
	_, err = r.Insert(ctx, sampleSession(base.Add(2*time.Hour)))
	require.NoError(t, err)

	var n int64
	require.NoError(t, db.Model(&PlaySession{}).Count(&n).Error)
	require.Equal(t, int64(2), n)
}

// 供 L3 校准判断某天是否已有实测会话。
func TestSessionRepo_HasSessionOn(t *testing.T) {
	r := NewSessionRepo(testDB(t))
	ctx := context.Background()
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	_, err := r.Insert(ctx, sampleSession(day.Add(14*time.Hour)))
	require.NoError(t, err)

	has, err := r.HasSessionOn(ctx, 76561197960287930, 440, day)
	require.NoError(t, err)
	require.True(t, has)

	has, err = r.HasSessionOn(ctx, 76561197960287930, 440, day.AddDate(0, 0, -1))
	require.NoError(t, err)
	require.False(t, has)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/store/ -run SessionRepo -v`
Expected: FAIL —— `undefined: NewSessionRepo`

- [ ] **Step 3: 实现会话仓储**

创建 `internal/store/session_repo.go`：

```go
package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

type SessionRepo struct{ db *gorm.DB }

func NewSessionRepo(db *gorm.DB) *SessionRepo { return &SessionRepo{db: db} }

// Insert 写入一条游戏会话。
//
// play_sessions 是自增主键的追加表，本身不幂等，而租约回收必然导致任务重跑。
// uk_session(steam_id64, appid, started_at) 唯一键挡住重复写入，
// 此处把冲突翻译成 (false, nil) 而非错误 —— 重复不是失败，
// 若当作失败上报，任务会被无谓重试直到进入死信。
func (r *SessionRepo) Insert(ctx context.Context, s PlaySession) (bool, error) {
	res := r.db.WithContext(ctx).
		Set("gorm:insert_option", "").
		Clauses(ignoreConflict{}).
		Create(&s)

	if res.Error != nil {
		if isDuplicateKey(res.Error) {
			return false, nil
		}
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// SetPlaytime 更新用户某款游戏的累计时长与最后游玩时刻。
func (r *SessionRepo) SetPlaytime(ctx context.Context, steamID uint64, appID uint32,
	minutes uint32, lastPlayed *time.Time, now time.Time) error {

	updates := map[string]any{
		"playtime_forever_min": minutes,
		"updated_at":           now,
	}
	if lastPlayed != nil {
		updates["rtime_last_played"] = *lastPlayed
	}

	return r.db.WithContext(ctx).Model(&UserGame{}).
		Where("steam_id64 = ? AND appid = ?", steamID, appID).
		Updates(updates).Error
}

// HasSessionOn 判断指定自然日（UTC）是否已有该游戏的会话记录。
// L3 校准据此避免为已被探针捕获的游玩重复补录推断会话。
func (r *SessionRepo) HasSessionOn(ctx context.Context, steamID uint64,
	appID uint32, day time.Time) (bool, error) {

	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)

	var n int64
	err := r.db.WithContext(ctx).Model(&PlaySession{}).
		Where("steam_id64 = ? AND appid = ? AND started_at >= ? AND started_at < ?",
			steamID, appID, start, start.AddDate(0, 0, 1)).
		Count(&n).Error
	return n > 0, err
}
```

在 `internal/store/link_repo.go` 中已有 `isDuplicateKey`，此处直接复用。再补上 `ignoreConflict` 子句：

```go
// 追加到 session_repo.go
import "gorm.io/gorm/clause"

// ignoreConflict 生成 INSERT IGNORE，让唯一键冲突静默跳过。
type ignoreConflict struct{}

func (ignoreConflict) Name() string { return "INSERT" }

func (ignoreConflict) Build(builder clause.Builder) {
	_, _ = builder.WriteString("INSERT IGNORE")
}

func (ignoreConflict) MergeClause(*clause.Clause) {}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/store/ -run SessionRepo -v`
Expected: PASS（4 个用例）

若 `INSERT IGNORE` 子句方式在你的 GORM 版本上不生效，退回到显式冲突检测：把 `Create` 的错误传给 `isDuplicateKey` 判断即可，测试断言不变。

- [ ] **Step 5: 写结算逻辑的测试**

创建 `internal/collector/settle_test.go`：

```go
package collector

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type recentStub struct {
	steam.Client
	games []steam.OwnedGame
	err   error
}

func (s *recentStub) GetRecentlyPlayedGames(context.Context, uint64) ([]steam.OwnedGame, error) {
	return s.games, s.err
}

func settleTask(t *testing.T, started, ended time.Time) task.Task {
	t.Helper()
	p, err := json.Marshal(task.SessionPayload{StartedAt: started, EndedAt: ended})
	require.NoError(t, err)
	return task.Task{
		Type: task.TypeSessionSettle, SteamID: 1, AppID: 440, Payload: p,
	}
}

// 核心行为：时长取 Steam 的真实增量，起始时刻由结束时刻反推 ——
// 探针推算的起始时刻最多有一个轮询周期的误差，不可信。
func TestSettler_UsesSteamDeltaAndBackdatesStart(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, Name: "TF2", PlaytimeForeverMin: 100}}, now))

	// Steam 侧现在是 147 分钟，比库中记录多 47 分钟
	st := &recentStub{games: []steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 147}}}

	s := NewSettler(SettlerDeps{
		Steam: st, Games: games, Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	// 探针认为玩了 12:00–12:50（50 分钟），但真实增量是 47 分钟
	require.NoError(t, s.Handle(ctx, settleTask(t,
		now.Add(-time.Hour), now.Add(-10*time.Minute))))

	var sess store.PlaySession
	require.NoError(t, db.Take(&sess).Error)
	require.Equal(t, uint32(47), sess.DurationMin, "时长应取 Steam 增量而非探针推算")
	require.Equal(t, now.Add(-10*time.Minute).Unix(), sess.EndedAt.Unix())
	require.Equal(t, now.Add(-10*time.Minute).Add(-47*time.Minute).Unix(),
		sess.StartedAt.Unix(), "起始时刻应由结束时刻减去真实时长反推")
	require.Equal(t, store.SourceProbe, sess.Source)

	// 累计时长必须同步更新，否则下次差分会重复计算
	m, err := games.PlaytimeMap(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, uint32(147), m[440])
}

// 时长没有增长（探针误判、Steam 尚未结算）→ 不写会话，不算失败。
func TestSettler_NoDeltaWritesNothing(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))

	st := &recentStub{games: []steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}}
	s := NewSettler(SettlerDeps{
		Steam: st, Games: games, Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	require.NoError(t, s.Handle(ctx, settleTask(t, now.Add(-time.Hour), now)))

	var n int64
	require.NoError(t, db.Model(&store.PlaySession{}).Count(&n).Error)
	require.Zero(t, n)
}

// 该游戏有成就时应入队 L2 下钻。
func TestSettler_EnqueuesAchievementSync(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))
	// 标记该游戏有成就
	require.NoError(t, db.Model(&store.App{}).Where("appid = ?", 440).
		Update("has_achievements", 1).Error)

	st := &recentStub{games: []steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 130}}}
	s := NewSettler(SettlerDeps{
		Steam: st, Games: games, Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	require.NoError(t, s.Handle(ctx, settleTask(t, now.Add(-time.Hour), now)))

	var row store.SyncTask
	require.NoError(t, db.Where("task_type = ?", task.TypeAchievementSync).
		Take(&row).Error)
	require.Equal(t, uint32(440), row.AppID)
}

// 隐私墙 → 永久错误，不重试。持续重试只会白烧配额。
func TestSettler_PrivateProfileIsPermanent(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	st := &recentStub{err: steam.ErrProfilePrivate}
	s := NewSettler(SettlerDeps{
		Steam: st, Games: store.NewGameRepo(db), Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	err := s.Handle(ctx, settleTask(t, now.Add(-time.Hour), now))
	require.ErrorIs(t, err, task.ErrPermanent)
}

// 重复结算（租约回收后重跑）必须幂等。
func TestSettler_RerunIsIdempotent(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))

	st := &recentStub{games: []steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 140}}}
	s := NewSettler(SettlerDeps{
		Steam: st, Games: games, Sessions: store.NewSessionRepo(db),
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now },
	})

	tk := settleTask(t, now.Add(-time.Hour), now)
	require.NoError(t, s.Handle(ctx, tk))
	require.NoError(t, s.Handle(ctx, tk), "重跑不应报错")

	var n int64
	require.NoError(t, db.Model(&store.PlaySession{}).Count(&n).Error)
	require.Equal(t, int64(1), n, "重跑不应产生重复会话")
}
```

- [ ] **Step 6: 运行测试确认失败**

Run: `go test ./internal/collector/ -run Settler -v`
Expected: FAIL —— `undefined: NewSettler`

- [ ] **Step 7: 实现结算逻辑**

创建 `internal/collector/settle.go`：

```go
package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type SettlerDeps struct {
	Steam    steam.Client
	Games    *store.GameRepo
	Sessions *store.SessionRepo
	Tasks    task.Queue
	Now      func() time.Time
}

type Settler struct{ d SettlerDeps }

func NewSettler(d SettlerDeps) *Settler {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Settler{d: d}
}

// Handle 处理 TypeSessionSettle 任务。
//
// 探针给出的起止时刻最多有一个轮询周期的误差，因此这里只信任
// Steam 的 playtime_forever 增量作为时长，起始时刻由结束时刻反推。
func (s *Settler) Handle(ctx context.Context, t task.Task) error {
	now := s.d.Now()

	var payload task.SessionPayload
	if err := json.Unmarshal(t.Payload, &payload); err != nil {
		// payload 损坏，重试也不会好转
		return fmt.Errorf("collector: 解析会话载荷失败: %v: %w", err, task.ErrPermanent)
	}

	recent, err := s.d.Steam.GetRecentlyPlayedGames(ctx, t.SteamID)
	if errors.Is(err, steam.ErrProfilePrivate) {
		return fmt.Errorf("collector: 用户 %d 资料非公开: %w", t.SteamID, task.ErrPermanent)
	}
	if err != nil {
		return fmt.Errorf("collector: 拉取近期游玩失败: %w", err)
	}

	var current *steam.OwnedGame
	for i := range recent {
		if recent[i].AppID == t.AppID {
			current = &recent[i]
			break
		}
	}
	if current == nil {
		// 游戏不在近期列表中（时长过短未被 Steam 记录，或已超出两周窗口）。
		// 无法结算，但也不是失败。
		return nil
	}

	known, err := s.d.Games.PlaytimeMap(ctx, t.SteamID)
	if err != nil {
		return fmt.Errorf("collector: 读取已记录时长失败: %w", err)
	}

	prev := known[t.AppID]
	if current.PlaytimeForeverMin <= prev {
		// 时长没有增长：探针误判，或 Steam 尚未完成结算。不写会话。
		return nil
	}
	delta := current.PlaytimeForeverMin - prev

	// 时长取 Steam 的真实增量，起始时刻反推
	started := payload.EndedAt.Add(-time.Duration(delta) * time.Minute)

	if _, err := s.d.Sessions.Insert(ctx, store.PlaySession{
		SteamID:     t.SteamID,
		AppID:       t.AppID,
		StartedAt:   started,
		EndedAt:     payload.EndedAt,
		DurationMin: delta,
		Source:      store.SourceProbe,
		CreatedAt:   now,
	}); err != nil {
		return fmt.Errorf("collector: 写入会话失败: %w", err)
	}

	var lastPlayed *time.Time
	if !current.RtimeLastPlayed.IsZero() {
		lp := current.RtimeLastPlayed
		lastPlayed = &lp
	}
	if err := s.d.Sessions.SetPlaytime(ctx, t.SteamID, t.AppID,
		current.PlaytimeForeverMin, lastPlayed, now); err != nil {
		return fmt.Errorf("collector: 更新累计时长失败: %w", err)
	}

	return s.maybeEnqueueAchievements(ctx, t.SteamID, t.AppID, now)
}

// maybeEnqueueAchievements 仅对确认有成就系统的游戏入队下钻。
// has_achievements 为 -1（未知）时先入队 Schema 同步，由它来确定。
func (s *Settler) maybeEnqueueAchievements(ctx context.Context, steamID uint64,
	appID uint32, now time.Time) error {

	has, err := s.d.Games.HasAchievements(ctx, appID)
	if err != nil {
		return fmt.Errorf("collector: 查询成就标记失败: %w", err)
	}

	switch has {
	case 0:
		return nil // 确认无成就，不必下钻
	case -1:
		return s.d.Tasks.Enqueue(ctx, task.Task{
			Type: task.TypeSchemaSync, AppID: appID,
			Priority: task.PriorityNormal, NextRunAt: now,
		})
	default:
		return s.d.Tasks.Enqueue(ctx, task.Task{
			Type: task.TypeAchievementSync, SteamID: steamID, AppID: appID,
			Priority: task.PriorityNormal, NextRunAt: now,
		})
	}
}
```

在 `internal/store/game_repo.go` 末尾追加 `HasAchievements`：

```go
// HasAchievements 返回 apps.has_achievements：-1 未知、0 无成就、1 有成就。
// 游戏不存在时返回 -1。
func (r *GameRepo) HasAchievements(ctx context.Context, appID uint32) (int8, error) {
	var v int8
	err := r.db.WithContext(ctx).Model(&App{}).
		Select("has_achievements").
		Where("appid = ?", appID).
		Take(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return -1, nil
	}
	return v, err
}
```

在该文件顶部补上 `"errors"` 导入，并删除文件末尾的 `var _ = gorm.ErrRecordNotFound` 占位行。

- [ ] **Step 8: 运行测试确认通过**

Run: `go test ./internal/collector/ -v`
Expected: PASS（10 个用例）

- [ ] **Step 9: 注册 handler**

在 `cmd/worker/main.go` 中，`runner` 构造之后加入：

```go
	settler := collector.NewSettler(collector.SettlerDeps{
		Steam:    sc,
		Games:    store.NewGameRepo(db),
		Sessions: store.NewSessionRepo(db),
		Tasks:    queue,
	})
	runner.Register(task.TypeSessionSettle, settler.Handle)
```

- [ ] **Step 10: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 11: 提交**

```bash
git add internal/store/ internal/collector/ cmd/worker/
git commit -m "feat(collector): L1 会话结算与时长差分"
```

---

## Task 14: L3 每日校准与 worker 启动自愈

**Files:**
- Create: `internal/collector/reconcile.go`, `internal/collector/heal.go`
- Modify: `cmd/worker/main.go`
- Test: `internal/collector/reconcile_test.go`, `internal/collector/heal_test.go`

**Interfaces:**
- Consumes: 前序全部
- Produces:
  - `collector.NewReconciler(deps ReconcilerDeps) *Reconciler`，`ReconcilerDeps` 字段：`Steam steam.Client`、`Games *store.GameRepo`、`Sessions *store.SessionRepo`、`Links *store.LinkRepo`、`Tasks task.Queue`、`Now func() time.Time`
  - `(*Reconciler).Handle(ctx context.Context, t task.Task) error`
  - `(*Reconciler).ScheduleDaily(ctx context.Context) error` — 为所有活跃用户入队次日校准
  - `collector.NewHealer(probes *store.ProbeRepo, tasks task.Queue, now func() time.Time) *Healer`
  - `(*Healer).Run(ctx context.Context) error`
  - `collector.PrivateStrikeLimit int8 = 3`

- [ ] **Step 1: 写校准的测试**

创建 `internal/collector/reconcile_test.go`：

```go
package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type ownedStub struct {
	steam.Client
	games []steam.OwnedGame
	err   error
}

func (s *ownedStub) GetOwnedGames(context.Context, uint64) ([]steam.OwnedGame, error) {
	return s.games, s.err
}

func newReconciler(t *testing.T, st steam.Client, db *gorm.DB, now time.Time) *Reconciler {
	t.Helper()
	return NewReconciler(ReconcilerDeps{
		Steam: st, Games: store.NewGameRepo(db), Sessions: store.NewSessionRepo(db),
		Links: store.NewLinkRepo(db), Tasks: task.NewMySQLQueue(db),
		Now: func() time.Time { return now },
	})
}

// 新购入的游戏应被写入游戏库。
func TestReconciler_AddsNewlyPurchasedGames(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	st := &ownedStub{games: []steam.OwnedGame{
		{AppID: 440, Name: "TF2", PlaytimeForeverMin: 100},
		{AppID: 730, Name: "CS", PlaytimeForeverMin: 0},
	}}

	r := newReconciler(t, st, db, now)
	require.NoError(t, r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1}))

	rows, err := store.NewGameRepo(db).ListUserGames(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

// 核心兜底行为：时长增长了但当天没有实测会话（短会话、隐身游玩、
// 探针宕机窗口）→ 补一条推断会话，并明确标记来源。
func TestReconciler_BackfillsMissedSession(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)
	lastPlayed := time.Date(2026, 8, 25, 22, 30, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))

	st := &ownedStub{games: []steam.OwnedGame{{
		AppID: 440, Name: "TF2", PlaytimeForeverMin: 118,
		RtimeLastPlayed: lastPlayed,
	}}}

	r := newReconciler(t, st, db, now)
	require.NoError(t, r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1}))

	var sess store.PlaySession
	require.NoError(t, db.Take(&sess).Error)
	require.Equal(t, store.SourceReconcile, sess.Source,
		"推断出的会话必须标记来源，不能伪装成实测数据")
	require.Equal(t, uint32(18), sess.DurationMin)
	require.Equal(t, lastPlayed.Unix(), sess.EndedAt.Unix(),
		"应以 rtime_last_played 作为时间锚点")
	require.Equal(t, lastPlayed.Add(-18*time.Minute).Unix(), sess.StartedAt.Unix())
}

// 已被探针实测捕获的游玩不得重复补录。
func TestReconciler_SkipsWhenProbeSessionExists(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)
	lastPlayed := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	sessions := store.NewSessionRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))

	// 同一天已有实测会话
	_, err := sessions.Insert(ctx, store.PlaySession{
		SteamID: 1, AppID: 440,
		StartedAt: lastPlayed.Add(-30 * time.Minute), EndedAt: lastPlayed,
		DurationMin: 30, Source: store.SourceProbe, CreatedAt: now,
	})
	require.NoError(t, err)

	st := &ownedStub{games: []steam.OwnedGame{{
		AppID: 440, PlaytimeForeverMin: 130, RtimeLastPlayed: lastPlayed,
	}}}

	r := newReconciler(t, st, db, now)
	require.NoError(t, r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1}))

	var n int64
	require.NoError(t, db.Model(&store.PlaySession{}).
		Where("source = ?", store.SourceReconcile).Count(&n).Error)
	require.Zero(t, n, "当天已有实测会话时不应补录推断会话")
}

// 连续 3 次探测到私密 → 降级并停止重试（设计文档 §8.3）。
func TestReconciler_PrivateStrikesDegradeUser(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))

	st := &ownedStub{err: steam.ErrProfilePrivate}
	r := newReconciler(t, st, db, now)

	for i := 0; i < 2; i++ {
		err := r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1})
		require.Error(t, err)
		require.NotErrorIs(t, err, task.ErrPermanent, "前两次应可重试")
	}

	err := r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1})
	require.ErrorIs(t, err, task.ErrPermanent, "第三次应停止重试")

	link, err := links.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Equal(t, store.VisibilityGameDetailsPrivate, link.VisibilityState)
}

// 探测成功后 strike 计数清零。
func TestReconciler_SuccessResetsStrikes(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))
	_, err := links.BumpPrivateStrikes(ctx, 1)
	require.NoError(t, err)

	st := &ownedStub{games: []steam.OwnedGame{{AppID: 440, Name: "TF2"}}}
	r := newReconciler(t, st, db, now)
	require.NoError(t, r.Handle(ctx, task.Task{Type: task.TypeLibrarySync, SteamID: 1}))

	link, err := links.ByUserID(ctx, 1001)
	require.NoError(t, err)
	require.Equal(t, int8(0), link.PrivateStrikes)
	require.Equal(t, store.VisibilityOK, link.VisibilityState)
}

// ScheduleDaily 为所有活跃用户入队，已解绑的排除在外。
func TestReconciler_ScheduleDailyCoversActiveUsers(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))
	require.NoError(t, links.Link(ctx, 1002, 2))
	require.NoError(t, links.Unlink(ctx, 1002))

	r := newReconciler(t, &ownedStub{}, db, now)
	require.NoError(t, r.ScheduleDaily(ctx))

	var rows []store.SyncTask
	require.NoError(t, db.Where("task_type = ?", task.TypeLibrarySync).Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, uint64(1), rows[0].SteamID)
}
```

在文件顶部补上 `"gorm.io/gorm"` 导入。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/collector/ -run Reconciler -v`
Expected: FAIL —— `undefined: NewReconciler`

- [ ] **Step 3: 实现校准**

创建 `internal/collector/reconcile.go`：

```go
package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

// PrivateStrikeLimit 是连续探测到私密后停止重试的阈值。
// 用户随时可能关闭公开设置，无休止重试只会消耗配额（设计文档 §8.3）。
const PrivateStrikeLimit int8 = 3

// DailyReconcileJitter 把每日校准任务打散到一个时间窗内，
// 避免所有用户在同一秒集中触发。
const DailyReconcileJitter = 6 * time.Hour

type ReconcilerDeps struct {
	Steam    steam.Client
	Games    *store.GameRepo
	Sessions *store.SessionRepo
	Links    *store.LinkRepo
	Tasks    task.Queue
	Now      func() time.Time
}

type Reconciler struct{ d ReconcilerDeps }

func NewReconciler(d ReconcilerDeps) *Reconciler {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{d: d}
}

// Handle 处理 TypeLibrarySync 任务：同步游戏库并补录漏采的会话。
func (r *Reconciler) Handle(ctx context.Context, t task.Task) error {
	now := r.d.Now()

	owned, err := r.d.Steam.GetOwnedGames(ctx, t.SteamID)
	if errors.Is(err, steam.ErrProfilePrivate) {
		return r.handlePrivate(ctx, t.SteamID)
	}
	if err != nil {
		return fmt.Errorf("collector: 拉取游戏库失败: %w", err)
	}

	_ = r.d.Links.ResetPrivateStrikes(ctx, t.SteamID)
	_ = r.d.Links.UpdateVisibility(ctx, t.SteamID, store.VisibilityOK)

	// 先取旧快照，再写新快照 —— 顺序不能反，否则差值恒为 0
	known, err := r.d.Games.PlaytimeMap(ctx, t.SteamID)
	if err != nil {
		return fmt.Errorf("collector: 读取已记录时长失败: %w", err)
	}

	if err := r.d.Games.UpsertApps(ctx, owned); err != nil {
		return fmt.Errorf("collector: 写入游戏元数据失败: %w", err)
	}
	if err := r.d.Games.UpsertUserGames(ctx, t.SteamID, owned, now); err != nil {
		return fmt.Errorf("collector: 写入游戏库失败: %w", err)
	}

	for _, g := range owned {
		if err := r.reconcileGame(ctx, t.SteamID, g, known[g.AppID], now); err != nil {
			return err
		}
	}
	return nil
}

// reconcileGame 对单款游戏做差分补录。
func (r *Reconciler) reconcileGame(ctx context.Context, steamID uint64,
	g steam.OwnedGame, prevMin uint32, now time.Time) error {

	if g.PlaytimeForeverMin <= prevMin {
		return nil
	}
	delta := g.PlaytimeForeverMin - prevMin

	// rtime_last_played 是方案 C 的价值所在：它给推断出的会话
	// 一个可信的时间锚点，而不是笼统地归属到「某一天」。
	anchor := g.RtimeLastPlayed
	if anchor.IsZero() {
		anchor = now
	}

	has, err := r.d.Sessions.HasSessionOn(ctx, steamID, g.AppID, anchor)
	if err != nil {
		return fmt.Errorf("collector: 查询当日会话失败: %w", err)
	}
	if !has {
		if _, err := r.d.Sessions.Insert(ctx, store.PlaySession{
			SteamID:     steamID,
			AppID:       g.AppID,
			StartedAt:   anchor.Add(-time.Duration(delta) * time.Minute),
			EndedAt:     anchor,
			DurationMin: delta,
			Source:      store.SourceReconcile, // 明确标记为推断值
			CreatedAt:   now,
		}); err != nil {
			return fmt.Errorf("collector: 补录会话失败: %w", err)
		}
	}

	return r.enqueueAchievementsIfAny(ctx, steamID, g.AppID, now)
}

func (r *Reconciler) enqueueAchievementsIfAny(ctx context.Context, steamID uint64,
	appID uint32, now time.Time) error {

	has, err := r.d.Games.HasAchievements(ctx, appID)
	if err != nil {
		return fmt.Errorf("collector: 查询成就标记失败: %w", err)
	}

	switch has {
	case 0:
		return nil
	case -1:
		return r.d.Tasks.Enqueue(ctx, task.Task{
			Type: task.TypeSchemaSync, AppID: appID,
			Priority: task.PriorityNormal, NextRunAt: now,
		})
	default:
		return r.d.Tasks.Enqueue(ctx, task.Task{
			Type: task.TypeAchievementSync, SteamID: steamID, AppID: appID,
			Priority: task.PriorityNormal, NextRunAt: now,
		})
	}
}

// handlePrivate 累加 strike 计数，达到阈值后停止重试并落库可见性状态。
func (r *Reconciler) handlePrivate(ctx context.Context, steamID uint64) error {
	n, err := r.d.Links.BumpPrivateStrikes(ctx, steamID)
	if err != nil {
		return fmt.Errorf("collector: 累加私密计数失败: %w", err)
	}

	if n >= PrivateStrikeLimit {
		if err := r.d.Links.UpdateVisibility(ctx, steamID,
			store.VisibilityGameDetailsPrivate); err != nil {
			return err
		}
		return fmt.Errorf("collector: 用户 %d 连续 %d 次探测到非公开: %w",
			steamID, n, task.ErrPermanent)
	}
	return fmt.Errorf("collector: 用户 %d 资料非公开（第 %d 次）", steamID, n)
}

// ScheduleDaily 为所有活跃用户入队次日校准，执行时刻打散到一个时间窗内。
func (r *Reconciler) ScheduleDaily(ctx context.Context) error {
	now := r.d.Now()

	ids, err := r.d.Links.ActiveSteamIDs(ctx)
	if err != nil {
		return fmt.Errorf("collector: 查询活跃用户失败: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}

	step := DailyReconcileJitter / time.Duration(len(ids))
	for i, id := range ids {
		if err := r.d.Tasks.Enqueue(ctx, task.Task{
			Type:      task.TypeLibrarySync,
			SteamID:   id,
			Priority:  task.PriorityNormal,
			NextRunAt: now.Add(time.Duration(i) * step),
		}); err != nil {
			return fmt.Errorf("collector: 入队每日校准失败: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/collector/ -run Reconciler -v`
Expected: PASS（6 个用例）

- [ ] **Step 5: 写自愈的测试**

创建 `internal/collector/heal_test.go`：

```go
package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/domain"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

// worker 停机数小时后重启，probe_state 里会残留卡在 Playing 的僵尸会话。
// 这些会话的时长已不可信，必须强制结算而非继续累积。
func TestHealer_ForceSettlesZombieSessions(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	old := time.Date(2026, 8, 26, 2, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	probes := store.NewProbeRepo(db)
	require.NoError(t, probes.Ensure(ctx, 1, old))
	require.NoError(t, probes.Save(ctx, 1, domain.State{
		AppID: 440, StartedAt: old, LastSeenPlayingAt: old.Add(10 * time.Minute),
	}, 0, old.Add(2*time.Minute), old))

	h := NewHealer(probes, task.NewMySQLQueue(db), func() time.Time { return now })
	require.NoError(t, h.Run(ctx))

	// 状态应被重置为 Idle
	due, err := probes.Due(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, uint32(0), store.ToDomain(due[0]).AppID)

	// 并入队一条结算任务
	var row store.SyncTask
	require.NoError(t, db.Take(&row).Error)
	require.Equal(t, task.TypeSessionSettle, row.Type)
	require.Equal(t, uint32(440), row.AppID)
}

// 近期仍在正常探测的会话不得被自愈打断。
func TestHealer_LeavesFreshSessionsAlone(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	probes := store.NewProbeRepo(db)
	require.NoError(t, probes.Ensure(ctx, 1, now))
	require.NoError(t, probes.Save(ctx, 1, domain.State{
		AppID: 440, StartedAt: now.Add(-20 * time.Minute),
		LastSeenPlayingAt: now.Add(-2 * time.Minute),
	}, 0, now.Add(2*time.Minute), now.Add(-2*time.Minute)))

	h := NewHealer(probes, task.NewMySQLQueue(db), func() time.Time { return now })
	require.NoError(t, h.Run(ctx))

	due, err := probes.Due(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, uint32(440), store.ToDomain(due[0]).AppID, "进行中的会话不应被打断")

	var n int64
	require.NoError(t, db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n)
}
```

- [ ] **Step 6: 运行测试确认失败**

Run: `go test ./internal/collector/ -run Healer -v`
Expected: FAIL —— `undefined: NewHealer`

- [ ] **Step 7: 实现自愈**

创建 `internal/collector/heal.go`：

```go
package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"steamlink/internal/domain"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

// StaleThreshold 是判定探针状态为「僵尸」的阈值。
// 超过它仍未被探测却标记为在玩，说明 worker 曾长时间宕机。
const StaleThreshold = time.Hour

type Healer struct {
	probes *store.ProbeRepo
	tasks  task.Queue
	now    func() time.Time
}

func NewHealer(probes *store.ProbeRepo, tasks task.Queue, now func() time.Time) *Healer {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Healer{probes: probes, tasks: tasks, now: now}
}

// Run 在 worker 启动时执行一次，强制结算僵尸会话。
//
// 这些会话跨越了 worker 的宕机窗口，实际结束时刻无从得知，
// 因此结算出的时长只能是推断值 —— 后续 L1 会用 Steam 的真实增量修正时长，
// 但起止时刻仍带有不确定性。
func (h *Healer) Run(ctx context.Context) error {
	now := h.now()

	stale, err := h.probes.Stale(ctx, now.Add(-StaleThreshold))
	if err != nil {
		return fmt.Errorf("collector: 查询僵尸会话失败: %w", err)
	}

	for _, row := range stale {
		state := store.ToDomain(row)

		payload, err := json.Marshal(task.SessionPayload{
			StartedAt: state.StartedAt,
			EndedAt:   state.LastSeenPlayingAt,
		})
		if err != nil {
			return err
		}

		if err := h.tasks.Enqueue(ctx, task.Task{
			Type:      task.TypeSessionSettle,
			SteamID:   row.SteamID,
			AppID:     state.AppID,
			Payload:   payload,
			Priority:  task.PriorityNormal,
			NextRunAt: now,
		}); err != nil {
			return fmt.Errorf("collector: 入队僵尸会话结算失败: %w", err)
		}

		// 重置为 Idle，让下一轮探针从干净状态重新开始
		if err := h.probes.Save(ctx, row.SteamID, domain.State{},
			row.Tier, now, now); err != nil {
			return fmt.Errorf("collector: 重置探针状态失败: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 8: 运行全部 collector 测试**

Run: `go test ./internal/collector/ -v`
Expected: PASS（18 个用例）

- [ ] **Step 9: 在 worker 中接入自愈与每日调度**

在 `cmd/worker/main.go` 中，signal context 建立之后、启动探针之前插入：

```go
	reconciler := collector.NewReconciler(collector.ReconcilerDeps{
		Steam:    sc,
		Games:    store.NewGameRepo(db),
		Sessions: store.NewSessionRepo(db),
		Links:    store.NewLinkRepo(db),
		Tasks:    queue,
	})
	runner.Register(task.TypeLibrarySync, reconciler.Handle)

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
```

- [ ] **Step 10: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 11: 提交**

```bash
git add internal/collector/ cmd/worker/
git commit -m "feat(collector): L3 每日校准与启动自愈"
```

**P2 完成。** 时长监控链路已闭环：探针捕获实测会话，校准兜底漏采，宕机后自愈。

---

## Task 15: 成就 Schema 全局同步

成就定义跨用户共享，只按 appid 拉取一次。这是全方案最大的一笔配额节省。

**Files:**
- Create: `internal/collector/schema.go`
- Modify: `internal/store/game_repo.go`（新增成就定义写入）
- Modify: `cmd/worker/main.go`
- Test: `internal/collector/schema_test.go`

**Interfaces:**
- Consumes: `steam.GameSchema`/`steam.ErrAppHasNoStats`（Task 2）、`task.ErrPermanent`（Task 12）
- Produces:
  - `store.GameRepo.UpsertAchievementSchema(ctx, appID uint32, achs []steam.SchemaAchievement, now time.Time) error`
  - `store.GameRepo.MarkAppAchievements(ctx, appID uint32, has int8, total uint16, now time.Time) error`
  - `store.GameRepo.SchemaAchievementCount(ctx, appID uint32) (int64, error)`
  - `collector.NewSchemaSyncer(deps SchemaDeps) *SchemaSyncer`，`SchemaDeps` 字段：`Steam steam.Client`、`Games *store.GameRepo`、`Tasks task.Queue`、`Now func() time.Time`
  - `(*SchemaSyncer).Handle(ctx context.Context, t task.Task) error`

- [ ] **Step 1: 写 Schema 同步的测试**

创建 `internal/collector/schema_test.go`：

```go
package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type schemaStub struct {
	steam.Client
	schema steam.GameSchema
	err    error
	calls  int
}

func (s *schemaStub) GetSchemaForGame(context.Context, uint32) (steam.GameSchema, error) {
	s.calls++
	return s.schema, s.err
}

func TestSchemaSyncer_StoresDefinitionsAndMarksApp(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{
		AppID: 440, Name: "Team Fortress 2",
		Achievements: []steam.SchemaAchievement{
			{APIName: "TF_SCOUT_LONG_DISTANCE_RUNNER", DisplayName: "马拉松选手",
				Description: "累计跑动 25 公里", Icon: "a.jpg", IconGray: "a_gray.jpg"},
			{APIName: "TF_HIDDEN", DisplayName: "隐藏成就", Hidden: true},
		},
	}}

	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440}))

	var achs []store.AppAchievement
	require.NoError(t, db.Where("appid = ?", 440).Order("api_name").Find(&achs).Error)
	require.Len(t, achs, 2)
	require.Equal(t, "隐藏成就", achs[0].DisplayName)
	require.Equal(t, int8(1), achs[0].Hidden)
	require.Equal(t, "累计跑动 25 公里", achs[1].Description)

	var app store.App
	require.NoError(t, db.Where("appid = ?", 440).Take(&app).Error)
	require.Equal(t, int8(1), app.HasAchievements)
	require.Equal(t, uint16(2), app.AchTotal)
	require.NotNil(t, app.SchemaSyncedAt)
}

// 没有成就系统的游戏必须被永久标记，且任务算成功而非失败。
// 把它当失败重试会让这类游戏陷入死循环并持续消耗配额。
func TestSchemaSyncer_NoStatsMarksAppAndSucceeds(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{err: steam.ErrAppHasNoStats}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	err := s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440})
	require.ErrorIs(t, err, task.ErrPermanent, "应作为永久错误，由 runner 置为成功")

	has, err := games.HasAchievements(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int8(0), has, "必须永久标记为无成就")
}

// 返回空成就列表 = 该游戏确实没有成就，同样永久标记。
func TestSchemaSyncer_EmptyAchievementListMarksNoStats(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{AppID: 440, Name: "TF2"}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440}))

	has, err := games.HasAchievements(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int8(0), has)
}

// Schema 同步完成后，若任务携带了 SteamID，应接着入队该用户的成就下钻。
func TestSchemaSyncer_ChainsToAchievementSync(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{
		AppID: 440,
		Achievements: []steam.SchemaAchievement{{APIName: "A", DisplayName: "甲"}},
	}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{
		Type: task.TypeSchemaSync, SteamID: 1, AppID: 440}))

	var row store.SyncTask
	require.NoError(t, db.Where("task_type = ?", task.TypeAchievementSync).Take(&row).Error)
	require.Equal(t, uint64(1), row.SteamID)
	require.Equal(t, uint32(440), row.AppID)
}

// 重复同步幂等：定义被覆盖更新，不产生重复行。
func TestSchemaSyncer_RerunIsIdempotent(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{
		AppID: 440,
		Achievements: []steam.SchemaAchievement{{APIName: "A", DisplayName: "甲"}},
	}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	tk := task.Task{Type: task.TypeSchemaSync, AppID: 440}
	require.NoError(t, s.Handle(ctx, tk))
	require.NoError(t, s.Handle(ctx, tk))

	n, err := games.SchemaAchievementCount(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/collector/ -run SchemaSyncer -v`
Expected: FAIL —— `undefined: NewSchemaSyncer`

- [ ] **Step 3: 实现成就定义的写入**

在 `internal/store/game_repo.go` 末尾追加：

```go
// UpsertAchievementSchema 写入某款游戏的成就定义。
// 这张表不带用户维度：1000 个用户共玩 5000 款游戏，成就定义只需拉 5000 次。
func (r *GameRepo) UpsertAchievementSchema(ctx context.Context, appID uint32,
	achs []steam.SchemaAchievement, now time.Time) error {

	if len(achs) == 0 {
		return nil
	}

	rows := make([]AppAchievement, 0, len(achs))
	for _, a := range achs {
		var hidden int8
		if a.Hidden {
			hidden = 1
		}
		rows = append(rows, AppAchievement{
			AppID:       appID,
			APIName:     a.APIName,
			DisplayName: a.DisplayName,
			Description: a.Description,
			Icon:        a.Icon,
			IconGray:    a.IconGray,
			Hidden:      hidden,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "appid"}, {Name: "api_name"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"display_name", "description", "icon", "icon_gray", "hidden", "updated_at",
		}),
	}).CreateInBatches(&rows, upsertBatchSize).Error
}

// MarkAppAchievements 标记某款游戏是否有成就系统及成就总数。
func (r *GameRepo) MarkAppAchievements(ctx context.Context, appID uint32,
	has int8, total uint16, now time.Time) error {

	return r.db.WithContext(ctx).Model(&App{}).
		Where("appid = ?", appID).
		Updates(map[string]any{
			"has_achievements": has,
			"ach_total":        total,
			"schema_synced_at": now,
			"updated_at":       now,
		}).Error
}

func (r *GameRepo) SchemaAchievementCount(ctx context.Context, appID uint32) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&AppAchievement{}).
		Where("appid = ?", appID).Count(&n).Error
	return n, err
}
```

- [ ] **Step 4: 实现 Schema 同步 handler**

创建 `internal/collector/schema.go`：

```go
package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type SchemaDeps struct {
	Steam steam.Client
	Games *store.GameRepo
	Tasks task.Queue
	Now   func() time.Time
}

type SchemaSyncer struct{ d SchemaDeps }

func NewSchemaSyncer(d SchemaDeps) *SchemaSyncer {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &SchemaSyncer{d: d}
}

// Handle 处理 TypeSchemaSync 任务：拉取并存储某款游戏的成就定义。
//
// 任务可携带 SteamID：若携带，同步完成后会接着入队该用户的成就下钻，
// 形成「发现新游戏 → 拉定义 → 拉用户解锁状态」的链条。
func (s *SchemaSyncer) Handle(ctx context.Context, t task.Task) error {
	now := s.d.Now()

	schema, err := s.d.Steam.GetSchemaForGame(ctx, t.AppID)

	// 该游戏没有成就系统。这不是失败 —— 重试永远不会成功，
	// 必须永久标记并让任务算作完成，否则会持续消耗配额。
	if errors.Is(err, steam.ErrAppHasNoStats) {
		if e := s.d.Games.MarkAppAchievements(ctx, t.AppID, 0, 0, now); e != nil {
			return fmt.Errorf("collector: 标记无成就失败: %w", e)
		}
		return fmt.Errorf("collector: 游戏 %d 无成就系统: %w", t.AppID, task.ErrPermanent)
	}
	if err != nil {
		return fmt.Errorf("collector: 拉取成就定义失败: %w", err)
	}

	// 返回空列表同样意味着该游戏没有成就
	if len(schema.Achievements) == 0 {
		return s.d.Games.MarkAppAchievements(ctx, t.AppID, 0, 0, now)
	}

	if err := s.d.Games.UpsertAchievementSchema(ctx, t.AppID, schema.Achievements, now); err != nil {
		return fmt.Errorf("collector: 写入成就定义失败: %w", err)
	}
	if err := s.d.Games.MarkAppAchievements(ctx, t.AppID, 1,
		uint16(len(schema.Achievements)), now); err != nil {
		return fmt.Errorf("collector: 标记有成就失败: %w", err)
	}

	if t.SteamID == 0 {
		return nil
	}
	return s.d.Tasks.Enqueue(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: t.SteamID, AppID: t.AppID,
		Priority: t.Priority, NextRunAt: now,
	})
}
```

在 `internal/store/game_repo.go` 顶部确认已导入 `"steamlink/internal/steam"` 与 `"gorm.io/gorm/clause"`。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/collector/ -run SchemaSyncer -v`
Expected: PASS（5 个用例）

- [ ] **Step 6: 写全球解锁率的测试**

`app_achievements.global_pct` 用于展示成就稀有度。数据来自 `GetGlobalAchievementPercentagesForApp`，它按 appid 全局共享，且**不需要 API Key**。与成就定义同属一个 appid 维度的工作单元，因此并入同一个任务同步。

在 `internal/steam/client_test.go` 末尾追加：

```go
func TestGetGlobalAchievementPercentages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 该接口不需要 key，且参数名是 gameid 而非 appid
		require.Equal(t, "440", r.URL.Query().Get("gameid"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"achievementpercentages":{"achievements":[
			{"name":"ACH_A","percent":42.5},
			{"name":"ACH_B","percent":3.125}
		]}}`)
	}))
	defer srv.Close()

	c := New("testkey", WithBaseURL(srv.URL))
	got, err := c.GetGlobalAchievementPercentages(context.Background(), 440)

	require.NoError(t, err)
	require.InDelta(t, 42.5, got["ACH_A"], 0.001)
	require.InDelta(t, 3.125, got["ACH_B"], 0.001)
}
```

在该文件顶部补上 `"io"` 导入。

在 `internal/collector/schema_test.go` 末尾追加：

```go
func (s *schemaStub) GetGlobalAchievementPercentages(context.Context, uint32) (map[string]float64, error) {
	return map[string]float64{"A": 55.5}, nil
}

// 全球解锁率应与成就定义一并落库。
func TestSchemaSyncer_StoresGlobalPercentages(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &schemaStub{schema: steam.GameSchema{
		AppID: 440,
		Achievements: []steam.SchemaAchievement{{APIName: "A", DisplayName: "甲"}},
	}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440}))

	var ach store.AppAchievement
	require.NoError(t, db.Where("appid = ? AND api_name = ?", 440, "A").Take(&ach).Error)
	require.InDelta(t, 55.5, ach.GlobalPct, 0.001)
}

// 解锁率拉取失败不得让整个 Schema 同步失败 ——
// 成就定义是主数据，稀有度只是锦上添花。
func TestSchemaSyncer_GlobalPercentFailureIsNonFatal(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &pctFailStub{schemaStub: schemaStub{schema: steam.GameSchema{
		AppID: 440,
		Achievements: []steam.SchemaAchievement{{APIName: "A", DisplayName: "甲"}},
	}}}
	s := NewSchemaSyncer(SchemaDeps{Steam: st, Games: games,
		Tasks: task.NewMySQLQueue(db), Now: func() time.Time { return now }})

	require.NoError(t, s.Handle(ctx, task.Task{Type: task.TypeSchemaSync, AppID: 440}),
		"解锁率失败不应影响成就定义同步")

	n, err := games.SchemaAchievementCount(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

type pctFailStub struct{ schemaStub }

func (s *pctFailStub) GetGlobalAchievementPercentages(context.Context, uint32) (map[string]float64, error) {
	return nil, errors.New("service unavailable")
}
```

在该文件顶部补上 `"errors"` 导入。

- [ ] **Step 7: 实现全球解锁率拉取**

在 `internal/steam/types.go` 的 `Client` 接口中追加一个方法：

```go
	GetGlobalAchievementPercentages(ctx context.Context, appID uint32) (map[string]float64, error)
```

在 `internal/steam/client.go` 末尾追加实现：

```go
type rawGlobalPct struct {
	AchievementPercentages struct {
		Achievements []struct {
			Name    string  `json:"name"`
			Percent float64 `json:"percent"`
		} `json:"achievements"`
	} `json:"achievementpercentages"`
}

// GetGlobalAchievementPercentages 返回 apiname → 全球解锁百分比。
//
// 注意两个与其他接口不同之处：参数名是 gameid 而非 appid，
// 且该接口本身不需要 API Key（getJSON 仍会带上，无害）。
func (c *HTTPClient) GetGlobalAchievementPercentages(ctx context.Context,
	appID uint32) (map[string]float64, error) {

	q := url.Values{"gameid": {strconv.FormatUint(uint64(appID), 10)}}

	var raw rawGlobalPct
	if err := c.getJSON(ctx,
		"/ISteamUserStats/GetGlobalAchievementPercentagesForApp/v0002/", q, &raw); err != nil {
		return nil, err
	}

	out := make(map[string]float64, len(raw.AchievementPercentages.Achievements))
	for _, a := range raw.AchievementPercentages.Achievements {
		out[a.Name] = a.Percent
	}
	return out, nil
}
```

在 `internal/store/game_repo.go` 末尾追加：

```go
// UpdateGlobalPercentages 批量回填成就的全球解锁率。
// 只更新已存在的定义行，不新增 —— 定义由 UpsertAchievementSchema 负责。
func (r *GameRepo) UpdateGlobalPercentages(ctx context.Context, appID uint32,
	pcts map[string]float64, now time.Time) error {

	if len(pcts) == 0 {
		return nil
	}

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for name, pct := range pcts {
			if err := tx.Model(&AppAchievement{}).
				Where("appid = ? AND api_name = ?", appID, name).
				Updates(map[string]any{"global_pct": pct, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
```

在 `internal/collector/schema.go` 的 `Handle` 中，`MarkAppAchievements` 成功之后、入队下游任务之前插入：

```go
	// 全球解锁率是展示用的附加数据，拉取失败不影响主流程：
	// 成就定义才是主数据，不能因为稀有度拿不到就让整个任务重试。
	if pcts, err := s.d.Steam.GetGlobalAchievementPercentages(ctx, t.AppID); err == nil {
		if e := s.d.Games.UpdateGlobalPercentages(ctx, t.AppID, pcts, now); e != nil {
			return fmt.Errorf("collector: 写入全球解锁率失败: %w", e)
		}
	}
```

- [ ] **Step 8: 运行测试确认通过**

```bash
go test ./internal/steam/ -run Global -v
go test ./internal/collector/ -run SchemaSyncer -v
```

Expected: 均 PASS（Schema 同步共 7 个用例）

> 前序任务中所有实现了 `steam.Client` 的测试桩（`fakeSteam`、`stubSteam`、`recentStub`、`ownedStub`、`achStub`、`partialSteam`）都内嵌了 `steam.Client` 接口，新增方法不会导致它们编译失败——未实现的方法在被调用时才会 panic，而这些桩都不会走到该路径。

> 解锁率的对外展示无需在此处理：Task 17 的 `AchievementItem` 已包含 `GlobalPct` 字段并直接读取本任务写入的数据。

- [ ] **Step 9: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 10: 提交**

```bash
git add internal/steam/ internal/store/ internal/collector/
git commit -m "feat(collector): 成就定义与全球解锁率的全局同步"
```

---

## Task 16: L2 成就下钻

**Files:**
- Create: `internal/collector/achievement.go`
- Modify: `internal/store/session_repo.go`（新增成就解锁写入）
- Modify: `cmd/worker/main.go`
- Test: `internal/collector/achievement_test.go`

**Interfaces:**
- Consumes: `steam.PlayerAchievement`、三类哨兵错误（Task 2）、`task.ErrPermanent`（Task 12）
- Produces:
  - `store.SessionRepo.UpsertUnlocks(ctx, steamID uint64, appID uint32, unlocks []AchievementUnlock, now time.Time) error`
  - `store.SessionRepo.CountUnlocks(ctx, steamID uint64, appID uint32) (int64, error)`
  - `store.GameRepo.SetAchievementProgress(ctx, steamID uint64, appID uint32, unlocked, total uint16, now time.Time) error`
  - `collector.NewAchievementSyncer(deps AchievementDeps) *AchievementSyncer`，`AchievementDeps` 字段：`Steam steam.Client`、`Games *store.GameRepo`、`Sessions *store.SessionRepo`、`Links *store.LinkRepo`、`Tasks task.Queue`、`Now func() time.Time`
  - `(*AchievementSyncer).Handle(ctx context.Context, t task.Task) error`

- [ ] **Step 1: 写成就下钻的测试**

创建 `internal/collector/achievement_test.go`：

```go
package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type achStub struct {
	steam.Client
	achs []steam.PlayerAchievement
	err  error
}

func (s *achStub) GetPlayerAchievements(context.Context, uint64, uint32) ([]steam.PlayerAchievement, error) {
	return s.achs, s.err
}

func newAchSyncer(t *testing.T, st steam.Client, db *gorm.DB, now time.Time) *AchievementSyncer {
	t.Helper()
	return NewAchievementSyncer(AchievementDeps{
		Steam: st, Games: store.NewGameRepo(db), Sessions: store.NewSessionRepo(db),
		Links: store.NewLinkRepo(db), Tasks: task.NewMySQLQueue(db),
		Now: func() time.Time { return now },
	})
}

func seedAppWithAchievements(t *testing.T, db *gorm.DB, now time.Time) {
	t.Helper()
	ctx := context.Background()
	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))
	require.NoError(t, games.UpsertUserGames(ctx, 1,
		[]steam.OwnedGame{{AppID: 440, PlaytimeForeverMin: 100}}, now))
	require.NoError(t, games.UpsertAchievementSchema(ctx, 440, []steam.SchemaAchievement{
		{APIName: "A", DisplayName: "甲"},
		{APIName: "B", DisplayName: "乙"},
		{APIName: "C", DisplayName: "丙"},
	}, now))
	require.NoError(t, games.MarkAppAchievements(ctx, 440, 1, 3, now))
}

// 解锁时刻直接取 Steam 的 unlocktime —— 成就自带精确时间戳，
// 不需要像时长那样靠采样差分推断。
func TestAchievementSyncer_StoresUnlocksWithSteamTimestamp(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	unlockA := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	st := &achStub{achs: []steam.PlayerAchievement{
		{APIName: "A", Achieved: true, UnlockTime: unlockA},
		{APIName: "B", Achieved: false},
		{APIName: "C", Achieved: true, UnlockTime: now.Add(-time.Hour)},
	}}

	s := newAchSyncer(t, st, db, now)
	require.NoError(t, s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440}))

	var rows []store.AchievementUnlock
	require.NoError(t, db.Where("steam_id64 = ?", uint64(1)).
		Order("api_name").Find(&rows).Error)
	require.Len(t, rows, 2, "只写入已解锁的成就")
	require.Equal(t, "A", rows[0].APIName)
	require.Equal(t, unlockA.Unix(), rows[0].UnlockedAt.Unix())

	// 进度必须回写到 user_games，供列表页展示
	games, err := store.NewGameRepo(db).ListUserGames(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, uint16(2), games[0].AchUnlocked)
	require.Equal(t, uint16(3), games[0].AchTotal)
	require.NotNil(t, games[0].AchSyncedAt)
}

// 重复同步幂等：主键冲突静默跳过，不产生重复记录。
func TestAchievementSyncer_RerunIsIdempotent(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	st := &achStub{achs: []steam.PlayerAchievement{
		{APIName: "A", Achieved: true, UnlockTime: now.Add(-time.Hour)},
	}}

	s := newAchSyncer(t, st, db, now)
	tk := task.Task{Type: task.TypeAchievementSync, SteamID: 1, AppID: 440}
	require.NoError(t, s.Handle(ctx, tk))
	require.NoError(t, s.Handle(ctx, tk))

	n, err := store.NewSessionRepo(db).CountUnlocks(ctx, 1, 440)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

// 三类错误必须区分处理之一：该游戏没有成就 → 永久标记，任务算成功。
func TestAchievementSyncer_NoStatsMarksAppPermanently(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	st := &achStub{err: steam.ErrAppHasNoStats}
	s := newAchSyncer(t, st, db, now)

	err := s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440})
	require.ErrorIs(t, err, task.ErrPermanent)

	has, err := store.NewGameRepo(db).HasAchievements(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int8(0), has, "无成就的游戏必须永久标记，否则每次游玩都会重复调用")
}

// 三类错误之二：隐私墙 → 累加 strike，达阈值后停止重试。
// 注意它绝不能被误当成「该游戏没有成就」而永久标记 app —— 那会影响所有用户。
func TestAchievementSyncer_ProfilePrivateDoesNotMarkApp(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	links := store.NewLinkRepo(db)
	require.NoError(t, links.Link(ctx, 1001, 1))

	st := &achStub{err: steam.ErrProfilePrivate}
	s := newAchSyncer(t, st, db, now)

	err := s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440})
	require.Error(t, err)

	has, err := store.NewGameRepo(db).HasAchievements(ctx, 440)
	require.NoError(t, err)
	require.Equal(t, int8(1), has, "用户隐私问题不得污染全局的游戏成就标记")
}

// 三类错误之三：网络故障 → 普通错误，走退避重试。
func TestAchievementSyncer_TransientErrorIsRetryable(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedAppWithAchievements(t, db, now)

	st := &achStub{err: errors.New("connection reset by peer")}
	s := newAchSyncer(t, st, db, now)

	err := s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440})
	require.Error(t, err)
	require.NotErrorIs(t, err, task.ErrPermanent, "网络故障必须可重试")
}

// Schema 尚未同步时，应先入队 Schema 任务再处理成就。
func TestAchievementSyncer_RequiresSchemaFirst(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	games := store.NewGameRepo(db)
	require.NoError(t, games.UpsertApps(ctx, []steam.OwnedGame{{AppID: 440, Name: "TF2"}}))

	st := &achStub{}
	s := newAchSyncer(t, st, db, now)
	require.NoError(t, s.Handle(ctx, task.Task{
		Type: task.TypeAchievementSync, SteamID: 1, AppID: 440}))

	var row store.SyncTask
	require.NoError(t, db.Where("task_type = ?", task.TypeSchemaSync).Take(&row).Error)
	require.Equal(t, uint32(440), row.AppID)
}
```

在文件顶部补上 `"errors"` 与 `"gorm.io/gorm"` 导入。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/collector/ -run AchievementSyncer -v`
Expected: FAIL —— `undefined: NewAchievementSyncer`

- [ ] **Step 3: 实现成就解锁的写入**

在 `internal/store/session_repo.go` 末尾追加：

```go
// UpsertUnlocks 批量写入成就解锁记录。
//
// 主键 (steam_id64, appid, api_name) 天然幂等，解锁时刻取自 Steam 的
// unlocktime。成就与时长的本质差异在此：成就自带精确时间戳，
// 不需要 diff 逻辑，重复同步无害。
func (r *SessionRepo) UpsertUnlocks(ctx context.Context, steamID uint64,
	appID uint32, unlocks []AchievementUnlock, now time.Time) error {

	if len(unlocks) == 0 {
		return nil
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "steam_id64"}, {Name: "appid"}, {Name: "api_name"}},
		DoNothing: true,
	}).CreateInBatches(&unlocks, 200).Error
}

func (r *SessionRepo) CountUnlocks(ctx context.Context, steamID uint64, appID uint32) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&AchievementUnlock{}).
		Where("steam_id64 = ? AND appid = ?", steamID, appID).Count(&n).Error
	return n, err
}
```

在 `internal/store/game_repo.go` 末尾追加：

```go
// SetAchievementProgress 回写用户某款游戏的成就进度，供列表页直接展示，
// 避免每次查询都聚合 achievement_unlocks 表。
func (r *GameRepo) SetAchievementProgress(ctx context.Context, steamID uint64,
	appID uint32, unlocked, total uint16, now time.Time) error {

	return r.db.WithContext(ctx).Model(&UserGame{}).
		Where("steam_id64 = ? AND appid = ?", steamID, appID).
		Updates(map[string]any{
			"ach_unlocked":  unlocked,
			"ach_total":     total,
			"ach_synced_at": now,
			"updated_at":    now,
		}).Error
}
```

- [ ] **Step 4: 实现成就下钻 handler**

创建 `internal/collector/achievement.go`：

```go
package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

type AchievementDeps struct {
	Steam    steam.Client
	Games    *store.GameRepo
	Sessions *store.SessionRepo
	Links    *store.LinkRepo
	Tasks    task.Queue
	Now      func() time.Time
}

type AchievementSyncer struct{ d AchievementDeps }

func NewAchievementSyncer(d AchievementDeps) *AchievementSyncer {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &AchievementSyncer{d: d}
}

// Handle 处理 TypeAchievementSync 任务。
//
// 本函数的核心是对三类错误的严格区分（设计文档 §6.5）：
//
//	ErrAppHasNoStats  → 游戏级问题，永久标记 apps.has_achievements=0，任务算成功
//	ErrProfilePrivate → 用户级问题，累加 strike，绝不能污染全局的 app 标记
//	其他错误          → 真故障，退避重试
//
// 混淆前两者的后果最严重：把用户的隐私问题标记到 app 上，
// 会让所有用户都不再同步这款游戏的成就。
func (a *AchievementSyncer) Handle(ctx context.Context, t task.Task) error {
	now := a.d.Now()

	// Schema 是成就展示的前提，缺失时先补齐
	total, err := a.d.Games.SchemaAchievementCount(ctx, t.AppID)
	if err != nil {
		return fmt.Errorf("collector: 查询成就定义数失败: %w", err)
	}
	if total == 0 {
		return a.d.Tasks.Enqueue(ctx, task.Task{
			Type: task.TypeSchemaSync, SteamID: t.SteamID, AppID: t.AppID,
			Priority: t.Priority, NextRunAt: now,
		})
	}

	achs, err := a.d.Steam.GetPlayerAchievements(ctx, t.SteamID, t.AppID)

	switch {
	case errors.Is(err, steam.ErrAppHasNoStats):
		// 游戏级：永久标记，所有用户都不必再试
		if e := a.d.Games.MarkAppAchievements(ctx, t.AppID, 0, 0, now); e != nil {
			return fmt.Errorf("collector: 标记无成就失败: %w", e)
		}
		return fmt.Errorf("collector: 游戏 %d 无成就系统: %w", t.AppID, task.ErrPermanent)

	case errors.Is(err, steam.ErrProfilePrivate):
		// 用户级：只影响这一个用户，不得触碰 apps 表
		return a.handlePrivate(ctx, t.SteamID)

	case err != nil:
		return fmt.Errorf("collector: 拉取玩家成就失败: %w", err)
	}

	_ = a.d.Links.ResetPrivateStrikes(ctx, t.SteamID)

	rows := make([]store.AchievementUnlock, 0, len(achs))
	for _, ach := range achs {
		if !ach.Achieved {
			continue
		}
		unlocked := ach.UnlockTime
		if unlocked.IsZero() {
			// 极少数老游戏解锁时刻为 0，退化为当前时刻
			unlocked = now
		}
		rows = append(rows, store.AchievementUnlock{
			SteamID:    t.SteamID,
			AppID:      t.AppID,
			APIName:    ach.APIName,
			UnlockedAt: unlocked,
			CreatedAt:  now,
		})
	}

	if err := a.d.Sessions.UpsertUnlocks(ctx, t.SteamID, t.AppID, rows, now); err != nil {
		return fmt.Errorf("collector: 写入成就解锁失败: %w", err)
	}

	return a.d.Games.SetAchievementProgress(ctx, t.SteamID, t.AppID,
		uint16(len(rows)), uint16(total), now)
}

func (a *AchievementSyncer) handlePrivate(ctx context.Context, steamID uint64) error {
	n, err := a.d.Links.BumpPrivateStrikes(ctx, steamID)
	if err != nil {
		return fmt.Errorf("collector: 累加私密计数失败: %w", err)
	}

	if n >= PrivateStrikeLimit {
		if err := a.d.Links.UpdateVisibility(ctx, steamID,
			store.VisibilityGameDetailsPrivate); err != nil {
			return err
		}
		return fmt.Errorf("collector: 用户 %d 连续 %d 次探测到非公开: %w",
			steamID, n, task.ErrPermanent)
	}
	return fmt.Errorf("collector: 用户 %d 资料非公开（第 %d 次）", steamID, n)
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/collector/ -run AchievementSyncer -v`
Expected: PASS（6 个用例）

`TestAchievementSyncer_ProfilePrivateDoesNotMarkApp` 是最重要的一个 —— 它防止了「一个用户的隐私设置导致所有用户都失去某款游戏的成就数据」这个严重故障。

- [ ] **Step 6: 注册 handler**

在 `cmd/worker/main.go` 中追加：

```go
	gameRepo := store.NewGameRepo(db)
	sessionRepo := store.NewSessionRepo(db)
	linkRepo := store.NewLinkRepo(db)

	schemaSyncer := collector.NewSchemaSyncer(collector.SchemaDeps{
		Steam: sc, Games: gameRepo, Tasks: queue,
	})
	runner.Register(task.TypeSchemaSync, schemaSyncer.Handle)

	achSyncer := collector.NewAchievementSyncer(collector.AchievementDeps{
		Steam: sc, Games: gameRepo, Sessions: sessionRepo,
		Links: linkRepo, Tasks: queue,
	})
	runner.Register(task.TypeAchievementSync, achSyncer.Handle)
```

同时把前面 `settler`、`reconciler` 构造中重复的 `store.NewGameRepo(db)` 等替换为这里的共享变量。

- [ ] **Step 7: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 8: 提交**

```bash
git add internal/store/ internal/collector/ cmd/worker/
git commit -m "feat(collector): L2 成就下钻与三类错误分治"
```

---

## Task 17: 全库成就回填与成就查询接口

**Files:**
- Create: `internal/collector/backfill.go`, `internal/api/achievement_handler.go`
- Modify: `internal/api/auth_handler.go`（绑定后触发回填）、`internal/api/router.go`、`internal/store/session_repo.go`
- Test: `internal/collector/backfill_test.go`

**Interfaces:**
- Consumes: 前序全部
- Produces:
  - `collector.EnqueueBackfill(ctx context.Context, q task.Queue, steamID uint64, appIDs []uint32, now time.Time) error`
  - `store.SessionRepo.ListUnlocks(ctx, steamID uint64, appID uint32) ([]AchievementUnlock, error)`
  - `store.GameRepo.ListAchievementDefs(ctx, appID uint32) ([]AppAchievement, error)`
  - `store.SessionRepo.RecentUnlocks(ctx, steamID uint64, limit int) ([]UnlockRow, error)`，`store.UnlockRow` 字段：`AppID uint32`、`AppName string`、`APIName string`、`DisplayName string`、`Icon string`、`UnlockedAt time.Time`
  - 路由 `GET /api/games/:appid/achievements`、`GET /api/achievements/recent`

- [ ] **Step 1: 写回填的测试**

创建 `internal/collector/backfill_test.go`：

```go
package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/store"
	"steamlink/internal/task"
)

// 回填任务必须用最低优先级 —— 一个新用户会一次性产生数百条任务，
// 若与实时会话结算同级排队，会把所有用户的实时性拖垮。
func TestEnqueueBackfill_UsesLowestPriority(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	q := task.NewMySQLQueue(db)
	require.NoError(t, EnqueueBackfill(ctx, q, 1, []uint32{440, 620, 730}, now))

	var rows []store.SyncTask
	require.NoError(t, db.Order("appid").Find(&rows).Error)
	require.Len(t, rows, 3)

	for _, r := range rows {
		require.Equal(t, task.PriorityBackfill, r.Priority)
		require.Equal(t, task.TypeAchievementSync, r.Type)
	}
}

// 回填任务在时间上错开，避免瞬间打满限流器。
func TestEnqueueBackfill_SpreadsOverTime(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	ids := make([]uint32, 100)
	for i := range ids {
		ids[i] = uint32(1000 + i)
	}

	q := task.NewMySQLQueue(db)
	require.NoError(t, EnqueueBackfill(ctx, q, 1, ids, now))

	var first, last store.SyncTask
	require.NoError(t, db.Order("next_run_at").Take(&first).Error)
	require.NoError(t, db.Order("next_run_at DESC").Take(&last).Error)

	require.True(t, last.NextRunAt.After(first.NextRunAt.Add(time.Minute)),
		"回填任务应在时间上铺开，而非全部堆在同一时刻")
}

// 实时任务必须能插队到回填任务前面。
func TestEnqueueBackfill_RealtimeTasksJumpQueue(t *testing.T) {
	db := storeTestDB(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)

	q := task.NewMySQLQueue(db)
	require.NoError(t, EnqueueBackfill(ctx, q, 1, []uint32{440, 620}, past))

	require.NoError(t, q.Enqueue(ctx, task.Task{
		Type: task.TypeSessionSettle, SteamID: 1, AppID: 730,
		Priority: task.PriorityRealtime, NextRunAt: past,
	}))

	got, err := q.Claim(ctx, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, task.TypeSessionSettle, got[0].Type)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/collector/ -run Backfill -v`
Expected: FAIL —— `undefined: EnqueueBackfill`

- [ ] **Step 3: 实现回填入队**

创建 `internal/collector/backfill.go`：

```go
package collector

import (
	"context"
	"fmt"
	"time"

	"steamlink/internal/task"
)

// BackfillSpread 是全库回填任务铺开的时间窗。
// 一个拥有 500 款游戏的用户，任务会被摊在这个窗口内逐步执行，
// 而不是瞬间涌入限流器。
const BackfillSpread = 12 * time.Hour

// EnqueueBackfill 为用户的全部游戏入队成就同步任务。
//
// 优先级固定为 PriorityBackfill（最低）：新用户绑定会一次性产生数百条任务，
// 若与实时会话结算同级排队，会拖垮所有用户的实时性（设计文档 §6.8）。
func EnqueueBackfill(ctx context.Context, q task.Queue, steamID uint64,
	appIDs []uint32, now time.Time) error {

	if len(appIDs) == 0 {
		return nil
	}

	step := BackfillSpread / time.Duration(len(appIDs))
	if step < time.Second {
		step = time.Second
	}

	for i, appID := range appIDs {
		if err := q.Enqueue(ctx, task.Task{
			Type:      task.TypeAchievementSync,
			SteamID:   steamID,
			AppID:     appID,
			Priority:  task.PriorityBackfill,
			NextRunAt: now.Add(time.Duration(i) * step),
		}); err != nil {
			return fmt.Errorf("collector: 入队回填任务失败 (appid=%d): %w", appID, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/collector/ -run Backfill -v`
Expected: PASS（3 个用例）

- [ ] **Step 5: 在绑定流程中触发回填**

修改 `internal/api/auth_handler.go` 的 `probeAndPersist`：

```go
	if state == store.VisibilityOK && len(games) > 0 {
		_ = d.Games.UpsertApps(ctx, games)
		_ = d.Games.UpsertUserGames(ctx, steamID, games, now)
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
```

在文件顶部补上 `"log/slog"`、`"steamlink/internal/collector"` 与 `"steamlink/internal/logging"` 导入。

- [ ] **Step 6: 实现成就查询**

在 `internal/store/session_repo.go` 末尾追加：

```go
func (r *SessionRepo) ListUnlocks(ctx context.Context, steamID uint64,
	appID uint32) ([]AchievementUnlock, error) {

	var out []AchievementUnlock
	err := r.db.WithContext(ctx).
		Where("steam_id64 = ? AND appid = ?", steamID, appID).
		Order("unlocked_at DESC").
		Find(&out).Error
	return out, err
}

// UnlockRow 是成就时间线的展示行。
type UnlockRow struct {
	AppID       uint32    `gorm:"column:appid"`
	AppName     string    `gorm:"column:app_name"`
	APIName     string    `gorm:"column:api_name"`
	DisplayName string    `gorm:"column:display_name"`
	Icon        string    `gorm:"column:icon"`
	UnlockedAt  time.Time `gorm:"column:unlocked_at"`
}

// RecentUnlocks 返回最近解锁的成就时间线。
// unlocked_at 取自 Steam 的 unlocktime，是精确值而非推断值。
func (r *SessionRepo) RecentUnlocks(ctx context.Context, steamID uint64,
	limit int) ([]UnlockRow, error) {

	var out []UnlockRow
	err := r.db.WithContext(ctx).
		Table("achievement_unlocks AS u").
		Select(`u.appid, a.name AS app_name, u.api_name,
		        d.display_name, d.icon, u.unlocked_at`).
		Joins("LEFT JOIN apps AS a ON a.appid = u.appid").
		Joins("LEFT JOIN app_achievements AS d ON d.appid = u.appid AND d.api_name = u.api_name").
		Where("u.steam_id64 = ?", steamID).
		Order("u.unlocked_at DESC").
		Limit(limit).
		Scan(&out).Error
	return out, err
}
```

在 `internal/store/game_repo.go` 末尾追加：

```go
func (r *GameRepo) ListAchievementDefs(ctx context.Context, appID uint32) ([]AppAchievement, error) {
	var out []AppAchievement
	err := r.db.WithContext(ctx).
		Where("appid = ?", appID).
		Order("api_name").
		Find(&out).Error
	return out, err
}
```

创建 `internal/api/achievement_handler.go`：

```go
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
```

在 `internal/api/router.go` 的 `Deps` 中增加 `Sessions *store.SessionRepo` 字段，并注册路由：

```go
		api.GET("/games/:appid/achievements", d.handleGameAchievements)
		api.GET("/achievements/recent", d.handleRecentAchievements)
```

在 `cmd/api/main.go` 的 `api.Deps{...}` 中补上：

```go
		Sessions: store.NewSessionRepo(db),
		Tasks:    task.NewMySQLQueue(db),
```

并补上 `"steamlink/internal/task"` 导入。

- [ ] **Step 7: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 8: 提交**

```bash
git add internal/ cmd/
git commit -m "feat: 全库成就回填与成就查询接口"
```

**P3 完成。** 成就链路已闭环：定义全局共享、解锁事件驱动、新用户后台回填。

---

## Task 18: 分层 tier 调度

**Files:**
- Create: `internal/domain/tier.go`
- Modify: `internal/collector/probe.go`（用 tier 计算 next_probe_at）
- Test: `internal/domain/tier_test.go`

**Interfaces:**
- Consumes: 无（纯函数）
- Produces:
  - `domain.Tier` 类型（`int8` 别名），常量 `domain.TierActive = 0`、`domain.TierRecent = 1`、`domain.TierDormant = 2`、`domain.TierAsleep = 3`
  - `domain.ClassifyTier(lastPlayed time.Time, now time.Time) Tier`
  - `domain.ProbeInterval(t Tier) time.Duration`
  - `domain.NextProbeAt(t Tier, playing bool, now time.Time) time.Time`

- [ ] **Step 1: 写分层规则的测试**

创建 `internal/domain/tier_test.go`：

```go
package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyTier(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		lastPlayed time.Time
		want       Tier
	}{
		{"1 小时前游玩 → 活跃", now.Add(-time.Hour), TierActive},
		{"23 小时前 → 活跃", now.Add(-23 * time.Hour), TierActive},
		{"3 天前 → 近期", now.AddDate(0, 0, -3), TierRecent},
		{"6 天前 → 近期", now.AddDate(0, 0, -6), TierRecent},
		{"20 天前 → 休眠", now.AddDate(0, 0, -20), TierDormant},
		{"60 天前 → 沉睡", now.AddDate(0, 0, -60), TierAsleep},
		{"从未游玩 → 沉睡", time.Time{}, TierAsleep},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ClassifyTier(tc.lastPlayed, now))
		})
	}
}

func TestProbeInterval(t *testing.T) {
	require.Equal(t, 2*time.Minute, ProbeInterval(TierActive))
	require.Equal(t, 15*time.Minute, ProbeInterval(TierRecent))
	require.Equal(t, 2*time.Hour, ProbeInterval(TierDormant))
	require.Equal(t, 24*time.Hour, ProbeInterval(TierAsleep))
}

// 正在游玩的用户必须按最高频率探测，无论其 tier 是什么 ——
// 否则一个沉睡用户突然开始游玩，会话要等一整天才会被结算。
func TestNextProbeAt_PlayingUserAlwaysHighFrequency(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.Equal(t, now.Add(2*time.Minute), NextProbeAt(TierAsleep, true, now))
	require.Equal(t, now.Add(2*time.Minute), NextProbeAt(TierDormant, true, now))
}

func TestNextProbeAt_IdleUserFollowsTier(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	require.Equal(t, now.Add(24*time.Hour), NextProbeAt(TierAsleep, false, now))
	require.Equal(t, now.Add(2*time.Minute), NextProbeAt(TierActive, false, now))
}

// 未知的 tier 值必须退化到最保守的频率，而不是 panic 或零间隔。
// 零间隔会造成对该用户的忙轮询，瞬间打满配额。
func TestProbeInterval_UnknownTierIsConservative(t *testing.T) {
	require.Equal(t, 24*time.Hour, ProbeInterval(Tier(99)))
	require.Equal(t, 24*time.Hour, ProbeInterval(Tier(-1)))
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/domain/ -run Tier -v`
Expected: FAIL —— `undefined: ClassifyTier`

- [ ] **Step 3: 实现分层规则**

创建 `internal/domain/tier.go`：

```go
package domain

import "time"

// Tier 决定用户的探针频率。分层是让高频轮询在配额上可行的关键：
// 只有少数活跃用户需要分钟级探测，多数用户可以降到天级。
type Tier int8

const (
	TierActive  Tier = 0 // 24 小时内有游玩
	TierRecent  Tier = 1 // 7 天内
	TierDormant Tier = 2 // 30 天内
	TierAsleep  Tier = 3 // 超过 30 天，或从未游玩
)

// ClassifyTier 根据最后游玩时刻判定分层。
func ClassifyTier(lastPlayed, now time.Time) Tier {
	if lastPlayed.IsZero() {
		return TierAsleep
	}

	switch elapsed := now.Sub(lastPlayed); {
	case elapsed < 24*time.Hour:
		return TierActive
	case elapsed < 7*24*time.Hour:
		return TierRecent
	case elapsed < 30*24*time.Hour:
		return TierDormant
	default:
		return TierAsleep
	}
}

// ProbeInterval 返回该分层的探针间隔。
// 未知值退化到最保守的间隔 —— 返回零会导致对该用户忙轮询，瞬间打满配额。
func ProbeInterval(t Tier) time.Duration {
	switch t {
	case TierActive:
		return 2 * time.Minute
	case TierRecent:
		return 15 * time.Minute
	case TierDormant:
		return 2 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// NextProbeAt 计算下次探测时刻。
//
// 正在游玩的用户一律按最高频率探测，与其 tier 无关：
// 会话正在进行时降频会让结束时刻严重失真，甚至让沉睡用户的
// 会话等上一整天才被结算。
func NextProbeAt(t Tier, playing bool, now time.Time) time.Time {
	if playing {
		return now.Add(ProbeInterval(TierActive))
	}
	return now.Add(ProbeInterval(t))
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/domain/ -v -cover`
Expected: PASS，覆盖率保持 100%

- [ ] **Step 5: 在探针中接入分层**

修改 `internal/collector/probe.go` 的 `advanceOne`，把固定间隔换成分层计算：

```go
func (p *Prober) advanceOne(ctx context.Context, row store.ProbeState,
	gameID uint32, now time.Time) error {

	prev := store.ToDomain(row)
	next, events := domain.Advance(prev, domain.Probe{GameID: gameID}, now)

	for _, e := range events {
		if e.Kind == domain.SessionStarted {
			p.lg.Info("会话开始",
				logging.SteamID(row.SteamID),
				slog.Uint64("appid", uint64(e.AppID)))
			continue
		}

		p.lg.Info("会话结束，入队结算",
			logging.SteamID(row.SteamID),
			slog.Uint64("appid", uint64(e.AppID)),
			slog.Time("started_at", e.StartedAt),
			slog.Time("ended_at", e.EndedAt))

		if err := p.enqueueSettle(ctx, row.SteamID, e, now); err != nil {
			return err
		}
	}

	// 用最后一次游玩时刻重新分层。正在游玩时以当前时刻计，
	// 保证其落入 TierActive。
	lastPlayed := lastPlayedOf(row, next, now)
	tier := domain.ClassifyTier(lastPlayed, now)
	nextAt := domain.NextProbeAt(tier, next.AppID != 0, now)

	return p.d.Probes.Save(ctx, row.SteamID, next, int8(tier), nextAt, now)
}

// lastPlayedOf 推断用于分层的「最后游玩时刻」。
func lastPlayedOf(row store.ProbeState, next domain.State, now time.Time) time.Time {
	if next.AppID != 0 {
		return now // 正在游玩
	}
	if row.LastSeenPlayingAt != nil {
		return *row.LastSeenPlayingAt
	}
	return time.Time{}
}
```

删除 `DefaultProbeInterval` 常量及其引用。

- [ ] **Step 6: 补一个分层生效的集成测试**

在 `internal/collector/probe_test.go` 末尾追加：

```go
// 沉睡用户一旦开始游玩，必须立刻升到最高探测频率。
func TestProber_PlayingUserUpgradesToActiveTier(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	pr, q, _ := newProbeFixture(t, now, 1)
	ctx := context.Background()

	// 预置一个沉睡用户
	require.NoError(t, pr.Save(ctx, 1, domain.State{}, int8(domain.TierAsleep),
		now, now))

	st := &stubSteam{results: map[uint64]uint32{1: 440}}
	p := NewProber(ProberDeps{Steam: st, Probes: pr, Tasks: q,
		Now: func() time.Time { return now }})
	require.NoError(t, p.RunOnce(ctx))

	due, err := pr.Due(ctx, now.Add(3*time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, due, 1, "开始游玩后应在 2 分钟内再次到期")
	require.Equal(t, int8(domain.TierActive), due[0].Tier)
}
```

- [ ] **Step 7: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 8: 提交**

```bash
git add internal/domain/ internal/collector/
git commit -m "feat(domain): 分层探针频率与在玩用户升频"
```

---

## Task 19: 配额守卫降级

配额逼近上限时按层次丢弃任务，保住最有价值的实时探测。

**Files:**
- Create: `internal/collector/guard.go`
- Modify: `cmd/worker/main.go`
- Test: `internal/collector/guard_test.go`

**Interfaces:**
- Consumes: `steam.RedisLimiter.QuotaUsed`/`steam.DailyQuotaLimit`（Task 3）、`task.Handler`（Task 12）
- Produces:
  - `collector.QuotaGuard` 接口：`Level(ctx context.Context) (DegradeLevel, error)`
  - `collector.DegradeLevel` 类型，常量 `collector.DegradeNone = 0`、`collector.DegradeBackfill = 1`、`collector.DegradeAll = 2`
  - `collector.NewRedisQuotaGuard(l *steam.RedisLimiter) *RedisQuotaGuard`
  - `collector.WithQuotaGuard(g QuotaGuard, minPriority int8, h task.Handler) task.Handler` — 中间件
  - `collector.ErrDeferredByQuota`

- [ ] **Step 1: 写降级的测试**

创建 `internal/collector/guard_test.go`：

```go
package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"steamlink/internal/task"
)

type fixedGuard struct{ level DegradeLevel }

func (g fixedGuard) Level(context.Context) (DegradeLevel, error) { return g.level, nil }

func TestQuotaLevelFromUsage(t *testing.T) {
	require.Equal(t, DegradeNone, quotaLevel(0))
	require.Equal(t, DegradeNone, quotaLevel(79_999))
	require.Equal(t, DegradeBackfill, quotaLevel(80_000))
	require.Equal(t, DegradeBackfill, quotaLevel(94_999))
	require.Equal(t, DegradeAll, quotaLevel(95_000))
	require.Equal(t, DegradeAll, quotaLevel(200_000))
}

// 未降级时所有任务正常执行。
func TestWithQuotaGuard_PassesThroughWhenHealthy(t *testing.T) {
	called := false
	h := WithQuotaGuard(fixedGuard{DegradeNone}, task.PriorityNormal,
		func(context.Context, task.Task) error { called = true; return nil })

	require.NoError(t, h(context.Background(), task.Task{Priority: task.PriorityBackfill}))
	require.True(t, called)
}

// 配额超过 80% 时，回填任务被推迟而非执行。
func TestWithQuotaGuard_DefersBackfillUnderPressure(t *testing.T) {
	called := false
	h := WithQuotaGuard(fixedGuard{DegradeBackfill}, task.PriorityNormal,
		func(context.Context, task.Task) error { called = true; return nil })

	err := h(context.Background(), task.Task{Priority: task.PriorityBackfill})
	require.ErrorIs(t, err, ErrDeferredByQuota)
	require.False(t, called, "回填任务不应被执行")
}

// 同样的压力下，实时任务必须照常执行。
func TestWithQuotaGuard_KeepsRealtimeUnderPressure(t *testing.T) {
	called := false
	h := WithQuotaGuard(fixedGuard{DegradeBackfill}, task.PriorityNormal,
		func(context.Context, task.Task) error { called = true; return nil })

	require.NoError(t, h(context.Background(), task.Task{Priority: task.PriorityRealtime}))
	require.True(t, called)
}

// 配额几乎耗尽时，连普通任务也让位，只保留探针（探针不走任务队列）。
func TestWithQuotaGuard_DefersEverythingWhenCritical(t *testing.T) {
	h := WithQuotaGuard(fixedGuard{DegradeAll}, task.PriorityNormal,
		func(context.Context, task.Task) error { return nil })

	require.ErrorIs(t,
		h(context.Background(), task.Task{Priority: task.PriorityRealtime}),
		ErrDeferredByQuota)
	require.ErrorIs(t,
		h(context.Background(), task.Task{Priority: task.PriorityNormal}),
		ErrDeferredByQuota)
}

// 被推迟的任务是可重试的普通错误，绝不能是永久错误 ——
// 否则次日配额重置后它永远不会再执行。
func TestWithQuotaGuard_DeferIsRetryable(t *testing.T) {
	h := WithQuotaGuard(fixedGuard{DegradeAll}, task.PriorityNormal,
		func(context.Context, task.Task) error { return nil })

	err := h(context.Background(), task.Task{Priority: task.PriorityNormal})
	require.NotErrorIs(t, err, task.ErrPermanent)
}

var _ = time.Now
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/collector/ -run Quota -v`
Expected: FAIL —— `undefined: quotaLevel`

- [ ] **Step 3: 实现配额守卫**

创建 `internal/collector/guard.go`：

```go
package collector

import (
	"context"
	"errors"
	"fmt"

	"steamlink/internal/steam"
	"steamlink/internal/task"
)

// ErrDeferredByQuota 表示任务因配额压力被推迟。
// 它是可重试的普通错误 —— 绝不能是 task.ErrPermanent，
// 否则次日配额重置后这些任务永远不会再执行。
var ErrDeferredByQuota = errors.New("collector: deferred due to quota pressure")

type DegradeLevel int

const (
	DegradeNone     DegradeLevel = 0 // 正常
	DegradeBackfill DegradeLevel = 1 // 停掉低优先级回填
	DegradeAll      DegradeLevel = 2 // 停掉全部队列任务，只保留探针
)

// 降级阈值，占日配额（100,000）的比例
const (
	backfillStopThreshold int64 = 80_000
	allStopThreshold      int64 = 95_000
)

func quotaLevel(used int64) DegradeLevel {
	switch {
	case used >= allStopThreshold:
		return DegradeAll
	case used >= backfillStopThreshold:
		return DegradeBackfill
	default:
		return DegradeNone
	}
}

type QuotaGuard interface {
	Level(ctx context.Context) (DegradeLevel, error)
}

type RedisQuotaGuard struct{ l *steam.RedisLimiter }

func NewRedisQuotaGuard(l *steam.RedisLimiter) *RedisQuotaGuard {
	return &RedisQuotaGuard{l: l}
}

func (g *RedisQuotaGuard) Level(ctx context.Context) (DegradeLevel, error) {
	used, err := g.l.QuotaUsed(ctx)
	if err != nil {
		// 读不到配额时保守放行：宁可多用一点配额，
		// 也不要因为 Redis 抖动就停掉全部采集。
		return DegradeNone, nil
	}
	return quotaLevel(used), nil
}

// WithQuotaGuard 包装 handler，在配额压力下按优先级丢弃任务。
//
// minPriority 是 DegradeBackfill 级别下仍允许执行的最低优先级数值
// （数值小者优先，因此传 task.PriorityNormal 意味着放行 Realtime 与 Normal）。
func WithQuotaGuard(g QuotaGuard, minPriority int8, h task.Handler) task.Handler {
	return func(ctx context.Context, t task.Task) error {
		level, err := g.Level(ctx)
		if err != nil {
			return err
		}

		switch level {
		case DegradeAll:
			return fmt.Errorf("配额已逼近上限，推迟全部队列任务: %w", ErrDeferredByQuota)
		case DegradeBackfill:
			if t.Priority > minPriority {
				return fmt.Errorf("配额紧张，推迟低优先级任务: %w", ErrDeferredByQuota)
			}
		}
		return h(ctx, t)
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/collector/ -run Quota -v`
Expected: PASS（6 个用例）

- [ ] **Step 5: 在 worker 中包装 handler**

修改 `cmd/worker/main.go`，把所有 `runner.Register` 调用包上守卫：

```go
	guard := collector.NewRedisQuotaGuard(limiter)

	runner.Register(task.TypeSessionSettle,
		collector.WithQuotaGuard(guard, task.PriorityNormal, settler.Handle))
	runner.Register(task.TypeLibrarySync,
		collector.WithQuotaGuard(guard, task.PriorityNormal, reconciler.Handle))
	runner.Register(task.TypeSchemaSync,
		collector.WithQuotaGuard(guard, task.PriorityNormal, schemaSyncer.Handle))
	runner.Register(task.TypeAchievementSync,
		collector.WithQuotaGuard(guard, task.PriorityNormal, achSyncer.Handle))
```

- [ ] **Step 6: 编译并运行全部测试**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: 全绿

- [ ] **Step 7: 提交**

```bash
git add internal/collector/ cmd/worker/
git commit -m "feat(collector): 配额压力下的分级降级"
```

---

## Task 20: 端到端时间线测试

用一个假 Steam server 回放完整的游玩序列，验证整条管线的最终落库结果。

**Files:**
- Create: `internal/e2e/fakesteam_test.go`, `internal/e2e/timeline_test.go`
- Test: 同上

**Interfaces:**
- Consumes: 全部包
- Produces: 无（仅测试）

- [ ] **Step 1: 写假 Steam server**

创建 `internal/e2e/fakesteam_test.go`：

```go
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
```

- [ ] **Step 2: 写端到端时间线测试**

创建 `internal/e2e/timeline_test.go`：

```go
package e2e

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"steamlink/internal/collector"
	"steamlink/internal/steam"
	"steamlink/internal/store"
	"steamlink/internal/task"
)

const testSteamID uint64 = 76561197960287930

func e2eDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"
	}
	db, err := store.NewDB(dsn, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	for _, tbl := range []string{
		"sync_tasks", "probe_state", "achievement_unlocks",
		"play_sessions", "user_games", "app_achievements", "apps", "steam_links",
	} {
		require.NoError(t, db.Exec("DELETE FROM "+tbl).Error)
	}
	return db
}

// rig 把整条管线组装起来，时钟由测试完全掌控。
type rig struct {
	db      *gorm.DB
	fake    *fakeSteam
	prober  *collector.Prober
	runner  *task.Runner
	queue   task.Queue
	now     time.Time
}

func newRig(t *testing.T, start time.Time) *rig {
	db := e2eDB(t)
	fake := newFakeSteam(t)

	r := &rig{db: db, fake: fake, now: start}
	nowFn := func() time.Time { return r.now }

	sc := steam.New("testkey", steam.WithBaseURL(fake.URL()))
	queue := task.NewMySQLQueue(db)
	probes := store.NewProbeRepo(db)
	games := store.NewGameRepo(db)
	sessions := store.NewSessionRepo(db)
	links := store.NewLinkRepo(db)

	r.queue = queue
	r.prober = collector.NewProber(collector.ProberDeps{
		Steam: sc, Probes: probes, Tasks: queue, Now: nowFn,
	})

	runner := task.NewRunner(queue, task.RunnerOptions{
		Concurrency: 1,
		// 不注入 Logger，NewRunner 会自动回退到 DiscardHandler
	})
	runner.Register(task.TypeSessionSettle, collector.NewSettler(collector.SettlerDeps{
		Steam: sc, Games: games, Sessions: sessions, Tasks: queue, Now: nowFn,
	}).Handle)
	runner.Register(task.TypeSchemaSync, collector.NewSchemaSyncer(collector.SchemaDeps{
		Steam: sc, Games: games, Tasks: queue, Now: nowFn,
	}).Handle)
	runner.Register(task.TypeAchievementSync, collector.NewAchievementSyncer(collector.AchievementDeps{
		Steam: sc, Games: games, Sessions: sessions, Links: links, Tasks: queue, Now: nowFn,
	}).Handle)
	runner.Register(task.TypeLibrarySync, collector.NewReconciler(collector.ReconcilerDeps{
		Steam: sc, Games: games, Sessions: sessions, Links: links, Tasks: queue, Now: nowFn,
	}).Handle)
	r.runner = runner

	require.NoError(t, links.Link(context.Background(), 1001, testSteamID))
	require.NoError(t, probes.Ensure(context.Background(), testSteamID, start))

	return r
}

func (r *rig) advance(d time.Duration) { r.now = r.now.Add(d) }

func (r *rig) probe(t *testing.T) {
	t.Helper()
	require.NoError(t, r.prober.RunOnce(context.Background()))
}

// drain 反复执行任务直到没有到期任务为止。
func (r *rig) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 20; i++ {
		n, err := r.runner.RunOnce(context.Background())
		require.NoError(t, err)
		if n == 0 {
			return
		}
	}
	t.Fatal("任务队列未能收敛，可能存在无限重新入队")
}

// 完整时间线：开始游玩 → 持续 30 分钟 → 退出 → Steam 延迟结算 → 落库。
func TestTimeline_FullSessionProducesAccurateRecord(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	r := newRig(t, start)

	// 20:00 用户启动游戏 440
	r.fake.StartPlaying(testSteamID, 440)
	r.probe(t)

	// 20:02 ~ 20:30 持续游玩，探针每 2 分钟一次
	for i := 0; i < 14; i++ {
		r.advance(2 * time.Minute)
		r.probe(t)
	}

	// 20:30 用户退出。Steam 侧结算 30 分钟。
	r.fake.StopPlaying(testSteamID, 440, 30)
	r.fake.Unlock(testSteamID, 440, "ACH_A", r.now.Unix())

	// 20:32 探针首次观测不到 → 去抖，尚不结束会话
	r.advance(2 * time.Minute)
	r.probe(t)

	var n int64
	require.NoError(t, r.db.Model(&store.SyncTask{}).Count(&n).Error)
	require.Zero(t, n, "首次未观测到时不应结束会话（去抖）")

	// 20:34 连续第二次观测不到 → 会话结束，入队结算
	r.advance(2 * time.Minute)
	r.probe(t)

	var settle store.SyncTask
	require.NoError(t, r.db.Where("task_type = ?", task.TypeSessionSettle).
		Take(&settle).Error)

	// 结算任务被延迟 5 分钟，此刻还不能执行
	r.drain(t)
	var sessions int64
	require.NoError(t, r.db.Model(&store.PlaySession{}).Count(&sessions).Error)
	require.Zero(t, sessions, "延迟窗口内不应结算")

	// 20:40 延迟窗口过去，执行结算
	r.advance(6 * time.Minute)
	r.drain(t)

	var sess store.PlaySession
	require.NoError(t, r.db.Take(&sess).Error)
	require.Equal(t, uint32(440), sess.AppID)
	require.Equal(t, uint32(30), sess.DurationMin, "时长应等于 Steam 的真实增量")
	require.Equal(t, store.SourceProbe, sess.Source)

	// 结束时刻应是最后一次观测到在玩的时刻（20:30），而非判定结束的时刻
	require.Equal(t, start.Add(30*time.Minute).Unix(), sess.EndedAt.Unix())
	require.Equal(t, start.Unix(), sess.StartedAt.Unix())

	// 成就应被连带同步
	var unlocks []store.AchievementUnlock
	require.NoError(t, r.db.Find(&unlocks).Error)
	require.Len(t, unlocks, 1)
	require.Equal(t, "ACH_A", unlocks[0].APIName)

	// 成就定义（全局共享）也应落库
	var defs int64
	require.NoError(t, r.db.Model(&store.AppAchievement{}).Count(&defs).Error)
	require.Equal(t, int64(2), defs, "含未解锁的成就定义")
}

// 短于探针间隔的会话被 L0 漏采，必须由 L3 校准捞回并标记为推断值。
func TestTimeline_ShortSessionRecoveredByReconcile(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	r := newRig(t, start)

	// 用户玩了 3 分钟，完整地卡在两次探针之间
	r.probe(t) // 20:00 未在玩
	r.fake.StartPlaying(testSteamID, 440)
	r.advance(time.Minute)
	r.fake.StopPlaying(testSteamID, 440, 3)
	r.advance(time.Minute)
	r.probe(t) // 20:02 已经退出了

	r.drain(t)
	var n int64
	require.NoError(t, r.db.Model(&store.PlaySession{}).Count(&n).Error)
	require.Zero(t, n, "L0 无法捕获短于探针间隔的会话")

	// 次日校准兜底
	r.advance(24 * time.Hour)
	require.NoError(t, r.queue.Enqueue(context.Background(), task.Task{
		Type: task.TypeLibrarySync, SteamID: testSteamID,
		Priority: task.PriorityNormal, NextRunAt: r.now,
	}))
	r.drain(t)

	var sess store.PlaySession
	require.NoError(t, r.db.Take(&sess).Error)
	require.Equal(t, uint32(3), sess.DurationMin)
	require.Equal(t, store.SourceReconcile, sess.Source,
		"校准补录的会话必须标记为推断值，不能伪装成实测数据")
}

// 切换游戏时，前一局结束、后一局开始，两条会话都要正确落库。
func TestTimeline_GameSwitchProducesTwoSessions(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	r := newRig(t, start)

	r.fake.StartPlaying(testSteamID, 440)
	r.probe(t)

	r.advance(20 * time.Minute)
	// 切到另一款游戏，前一局的时长结算到 Steam
	r.fake.StopPlaying(testSteamID, 440, 20)
	r.fake.StartPlaying(testSteamID, 730)
	r.probe(t)

	r.advance(15 * time.Minute)
	r.fake.StopPlaying(testSteamID, 730, 15)
	r.advance(2 * time.Minute)
	r.probe(t) // 去抖
	r.advance(2 * time.Minute)
	r.probe(t) // 结束

	r.advance(6 * time.Minute)
	r.drain(t)

	var sessions []store.PlaySession
	require.NoError(t, r.db.Order("appid").Find(&sessions).Error)
	require.Len(t, sessions, 2)

	require.Equal(t, uint32(440), sessions[0].AppID)
	require.Equal(t, uint32(20), sessions[0].DurationMin)
	require.Equal(t, uint32(730), sessions[1].AppID)
	require.Equal(t, uint32(15), sessions[1].DurationMin)
}
```

- [ ] **Step 3: 运行端到端测试**

```bash
docker compose up -d
go test ./internal/e2e/ -v
```

Expected: PASS（3 个用例）

这三个用例覆盖了设计文档 §3.2 中列出的全部可接受降级场景，是整个系统行为的最终验收标准。

- [ ] **Step 4: 运行全量测试并检查覆盖率**

```bash
go test ./... -cover
```

Expected: 全绿。`internal/domain` 覆盖率应为 100%，其余包不低于 70%。

- [ ] **Step 5: 提交**

```bash
git add internal/e2e/
git commit -m "test(e2e): 完整游玩时间线的端到端验证"
```

**P4 完成，全部功能交付。**

---

## 验收清单

实施完成后逐项确认：

- [ ] `go build ./... && go vet ./... && go test ./...` 全绿
- [ ] `internal/domain` 覆盖率 100%，且 `go list -deps ./internal/domain | grep -E "gorm|redis|net/http"` 无输出
- [ ] MySQL 版本 ≥ 8.0.1，`SHOW CREATE TABLE steam_links` 中可见 `GENERATED ALWAYS AS` 生成列
- [ ] 所有表字符集为 `utf8mb4_0900_ai_ci`，含 emoji 的游戏名可正常读写
- [ ] API 返回的 `steam_id` 为字符串类型（用 `curl` 检查实际 JSON 输出）
- [ ] 日志中的 `steam_id` 同样是字符串（`APP_ENV=prod` 跑一次，检查 JSON 输出）
- [ ] `grep -rn "log\.Print\|fmt\.Print\|slog\.Default" --include="*.go" .` 无输出（`cmd/*/main.go` 中配置加载失败的 stderr 直出除外）
- [ ] `grep -rn "api_key\|state_secret" configs/` 确认全部为空值
- [ ] 不设任何 `STEAMLINK_*` 环境变量直接启动，确认进程立即退出并提示缺失的具体变量名
- [ ] `APP_ENV=prod` 且不设 `STEAMLINK_HTTP_BASE_URL` 时启动失败
- [ ] 绑定一个游戏详情非公开的测试账号，确认返回 `game_details_private` 且带可操作的提示文案
- [ ] 手动 kill 一个执行中的 worker，确认 5 分钟后任务被另一实例回收
- [ ] 观察一天的 `steam:quota:{date}` 计数，确认稳态日调用量在 5,000 以内

## 部署

配置分三层，优先级从低到高：`configs/config.yaml` → `configs/config.{APP_ENV}.yaml` → `STEAMLINK_*` 环境变量。

**必须注入的环境变量**（这三项在 YAML 中刻意留空，不可写入仓库）：

```bash
APP_ENV=prod
STEAMLINK_STEAM_API_KEY="向 https://steamcommunity.com/dev/apikey 申请"
STEAMLINK_MYSQL_PASSWORD="数据库密码"
STEAMLINK_AUTH_STATE_SECRET="随机生成的长字符串，用于 CSRF state 签名"
STEAMLINK_HTTP_BASE_URL="https://your-domain.example"
```

**可选覆盖**（有 YAML 默认值，按需调整）：

```bash
STEAMLINK_MYSQL_HOST  STEAMLINK_MYSQL_PORT  STEAMLINK_MYSQL_DATABASE
STEAMLINK_REDIS_ADDR  STEAMLINK_REDIS_PASSWORD
STEAMLINK_WORKER_CONCURRENCY  STEAMLINK_LOG_LEVEL
CONFIG_DIR   # 配置目录路径，容器中挂载到别处时使用
```

三点部署注意：

- `STEAMLINK_HTTP_BASE_URL` 是站点根地址，OpenID 的 `realm` 与 `return_to` 都由它派生。它需要**用户浏览器**可达（Steam 用 302 重定向用户回来，不存在服务端回调），生产环境要求 https 的理由是 `return_to` 的 query 里带着 CSRF state，走 http 会被窃取或篡改。prod 环境下配置校验会强制检查这一点。
- **服务在反向代理后面时，这里必须填用户看到的外部地址**（如 `https://example.com`），不能填内部地址（如 `http://10.0.0.5:8080`）——`return_to` 是给浏览器用的，填内网地址会让用户的浏览器无处可去。
- 你的服务器需要能出网访问 `steamcommunity.com`：OpenID 第三步的 `check_authentication` 验签是由服务端主动发起的。
- `STEAMLINK_AUTH_STATE_SECRET` 泄漏等同于允许攻击者伪造 CSRF state，把受害者的 Steam 账号绑定到攻击者的本站账号上。用密码管理器或密钥服务生成并保管，不要手写。
- 数据库初始化执行 `scripts/db/init.sql`；后续版本的结构变更按顺序执行 `scripts/db/migrations/` 下的脚本，不要重跑 `init.sql`。






