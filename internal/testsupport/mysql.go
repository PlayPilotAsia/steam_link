// Package testsupport 为集成测试提供共享的测试库设施。
//
// Go 默认并行执行不同包的测试，而 store / task / collector / e2e 的集成测试
// 都会在每个用例前清表。它们若共用同一个库，就会互相清掉对方刚写入的数据，
// 表现为「should have 3 item(s), but has 4」「record not found」这类随机失败。
//
// 这里让每个测试包拿到一个以包路径命名的独立库，从而在默认并行下互不干扰，
// 不必再靠 `go test -p 1` 串行规避。
package testsupport

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// defaultDSN 指向本地开发用的 MySQL（默认复用常驻容器 dev-mysql）。
// 必须带 parseTime=true 与 loc=UTC，否则 DATETIME 扫描进 time.Time 会失败或带错时区。
const defaultDSN = "root:localdev-root@tcp(127.0.0.1:3306)/steamlink?parseTime=true&loc=UTC&charset=utf8mb4"

var (
	prepareOnce sync.Once
	preparedDSN string
	prepareErr  error
)

// DSN 返回当前测试包专属的 MySQL DSN，并保证该库已存在、表结构已就绪。
//
// 基础 DSN 取自环境变量 TEST_MYSQL_DSN（未设置时用 defaultDSN），其中的库名会
// 按测试包所在目录派生出后缀：internal/store 得到 steamlink_internal_store，
// internal/e2e 得到 steamlink_internal_e2e，依此类推。
//
// 建库与建表在每个测试二进制内只做一次（Go 为每个包单独编译一个测试二进制）。
// 表结构直接取自 scripts/db/init.sql 与 scripts/db/migrations/*.sql，因此跑测试
// 前不需要先执行 ./scripts/dev/up.sh，只要 MySQL 服务在跑就行。
func DSN(t *testing.T) string {
	t.Helper()
	prepareOnce.Do(func() { preparedDSN, prepareErr = prepare() })
	if prepareErr != nil {
		t.Fatalf("准备测试库失败（需要本地 MySQL：./scripts/dev/up.sh）：%v", prepareErr)
	}
	return preparedDSN
}

func prepare() (string, error) {
	base := os.Getenv("TEST_MYSQL_DSN")
	if base == "" {
		base = defaultDSN
	}
	cfg, err := mysql.ParseDSN(base)
	if err != nil {
		return "", fmt.Errorf("解析 DSN：%w", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("取工作目录：%w", err)
	}
	root, err := moduleRoot(wd)
	if err != nil {
		return "", err
	}
	suffix, err := packageSuffix(root, wd)
	if err != nil {
		return "", err
	}
	cfg.DBName += "_" + suffix

	if err := createDatabase(cfg); err != nil {
		return "", err
	}
	if err := applySchema(cfg, root); err != nil {
		return "", err
	}
	return cfg.FormatDSN(), nil
}

func createDatabase(cfg *mysql.Config) error {
	admin := cfg.Clone()
	admin.DBName = "" // 库还不存在，先连到服务器而不是某个库
	db, err := sql.Open("mysql", admin.FormatDSN())
	if err != nil {
		return fmt.Errorf("连接 MySQL：%w", err)
	}
	defer db.Close()

	stmt := fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci",
		cfg.DBName)
	if _, err := db.Exec(stmt); err != nil {
		return fmt.Errorf("建库 %s：%w", cfg.DBName, err)
	}
	return nil
}

func applySchema(cfg *mysql.Config, root string) error {
	exec := cfg.Clone()
	// init.sql 是多条语句，开 MultiStatements 就不必在客户端拆分 SQL
	// （拆分要正确处理注释与字符串字面量，交给服务端解析更稳妥）。
	exec.MultiStatements = true
	db, err := sql.Open("mysql", exec.FormatDSN())
	if err != nil {
		return fmt.Errorf("连接测试库 %s：%w", cfg.DBName, err)
	}
	defer db.Close()

	migrations, err := filepath.Glob(filepath.Join(root, "scripts", "db", "migrations", "*.sql"))
	if err != nil {
		return fmt.Errorf("扫描增量脚本：%w", err)
	}
	sort.Strings(migrations)

	files := append([]string{filepath.Join(root, "scripts", "db", "init.sql")}, migrations...)
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("读取 %s：%w", f, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			continue // MultiStatements 下空语句会报错
		}
		if _, err := db.Exec(string(content)); err != nil {
			return fmt.Errorf("应用 %s：%w", filepath.Base(f), err)
		}
	}
	return nil
}

// moduleRoot 从测试进程的工作目录向上找 go.mod。go test 保证测试二进制运行在
// 所属包的源码目录，所以这里能稳定定位到仓库根。
func moduleRoot(wd string) (string, error) {
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("从 %s 向上未找到 go.mod", wd)
		}
		dir = parent
	}
}

// packageSuffix 把测试包相对仓库根的路径转成库名后缀，例如 internal/store
// 得到 internal_store。用完整相对路径而非目录名，是为了避免不同目录下的同名
// 包（比如两个 util）撞进同一个库——那正是本包要消除的那类静默串扰。
func packageSuffix(root, wd string) (string, error) {
	rel, err := filepath.Rel(root, wd)
	if err != nil {
		return "", fmt.Errorf("计算包路径：%w", err)
	}
	if rel == "." {
		return "root", nil
	}

	var b strings.Builder
	for _, r := range strings.ToLower(filepath.ToSlash(rel)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String(), nil
}
