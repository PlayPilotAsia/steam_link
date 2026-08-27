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
