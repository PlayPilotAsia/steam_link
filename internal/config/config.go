package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// ErrMissingSecret 表示必需的敏感配置项为空。
var ErrMissingSecret = errors.New("config: required secret is empty")

// ErrUnknownEnv 表示 APP_ENV 不在允许的取值内。
var ErrUnknownEnv = errors.New("config: unknown APP_ENV")

// EnvPrefix 是环境变量前缀。steam.api_key 对应 STEAMLINK_STEAM_API_KEY。
const EnvPrefix = "STEAMLINK"

// 三套环境。取值必须白名单校验：写错一个字母时，viper 只会静默地
// 跳过不存在的 config.{env}.yaml，服务带着一份基础配置照常启动 ——
// 用本地地址连生产、或反过来，都是这么发生的。
const (
	EnvLocal = "local" // 本地开发，连本机 docker
	EnvTest  = "test"  // 本地运行，连阿里云实例（公网 IP）
	EnvProd  = "prod"  // 部署在阿里云，连同 VPC 实例（内网 IP）
)

var knownEnvs = map[string]bool{EnvLocal: true, EnvTest: true, EnvProd: true}

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

// Load 按四层优先级加载配置：
//
//	configs/config.yaml        基础值
//	configs/config.{env}.yaml  环境覆盖
//	configs/{env}.env          本机注入的地址与密钥（不进仓库）
//	真实环境变量                最高优先级
//
// env 取自 APP_ENV，缺省为 local。
func Load(dir string) (Config, error) {
	env := os.Getenv("APP_ENV")
	if env == "" {
		env = EnvLocal
	}
	if !knownEnvs[env] {
		return Config{}, fmt.Errorf("%w: %q（可选：%s / %s / %s）",
			ErrUnknownEnv, env, EnvLocal, EnvTest, EnvProd)
	}

	fileVars, err := readEnvFile(filepath.Join(dir, env+".env"))
	if err != nil {
		return Config{}, err
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

	// .env 的优先级介于 YAML 与真实环境变量之间，必须在两个 YAML
	// 都读完之后再注入，否则会被 MergeInConfig 覆盖。
	applyEnvFile(v, fileVars)

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

	if c.App.Env == EnvProd {
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
