package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// readEnvFile 解析 configs/{env}.env，返回 环境变量名 → 值。
//
// 刻意不调用 os.Setenv：那会永久改写进程环境，让 Load 变成有副作用、
// 且依赖调用顺序的函数 —— 同一进程里先后加载两套环境时，前一套的地址
// 会残留下来污染后一套。
//
// 文件不存在不是错误：生产通常只注入容器环境变量，不落盘密钥文件。
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: 打开 %s 失败: %w", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)

	for line := 1; sc.Scan(); line++ {
		key, val, ok := parseEnvLine(sc.Text())
		if !ok {
			continue
		}
		if key == "" {
			return nil, fmt.Errorf("config: %s 第 %d 行格式错误（应为 KEY=VALUE）", path, line)
		}
		out[key] = val
	}
	return out, sc.Err()
}

// parseEnvLine 解析一行 KEY=VALUE。ok 为 false 表示该行应被跳过
//（空行或注释）；ok 为 true 但 key 为空表示格式非法。
func parseEnvLine(raw string) (key, val string, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	s = strings.TrimPrefix(s, "export ")

	k, v, found := strings.Cut(s, "=")
	if !found {
		return "", "", true
	}

	k = strings.TrimSpace(k)
	v = strings.TrimSpace(v)

	// 去掉成对的引号：密码里常见 # 与空格，写进 .env 时通常要加引号
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return k, v, true
}

// applyEnvFile 把 .env 中的值注入 viper，优先级介于 YAML 与真实环境变量之间。
//
// 遍历方向是「已知的配置项 → 推导出它的环境变量名」，而不是反过来解析
// 环境变量名。后者无法还原带下划线的键：STEAMLINK_STEAM_RATE_PER_SEC
// 究竟对应 steam.rate_per_sec 还是 steam.rate.per.sec，只看名字无从判断。
//
// 已存在于真实环境的键一律跳过，交给 viper 的 AutomaticEnv 处理 ——
// 于是部署时用容器变量覆盖文件里的任意一项，不必先删掉文件。
func applyEnvFile(v *viper.Viper, fileVars map[string]string) {
	if len(fileVars) == 0 {
		return
	}

	for _, key := range v.AllKeys() {
		envName := EnvPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
		if _, inRealEnv := os.LookupEnv(envName); inRealEnv {
			continue
		}
		if val, ok := fileVars[envName]; ok {
			v.Set(key, val)
		}
	}
}
