package steam

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrProfilePrivate 表示用户的资料或游戏详情未公开。上层应停止该用户的同步
	// 并引导其修改隐私设置，不应重试。
	ErrProfilePrivate = errors.New("steam: profile is not public")

	// ErrAppHasNoStats 表示该游戏没有成就系统。上层应把该 appid 永久标记为无成就，
	// 并将任务置为成功 —— 这不是失败。
	ErrAppHasNoStats = errors.New("steam: app has no achievement stats")

	// ErrRateLimited 表示 Steam 返回了 429，即真实触发了平台侧的速率限制。
	// 上层应全局退避。
	ErrRateLimited = errors.New("steam: rate limited by steam")

	// ErrThrottled 表示请求被本地令牌桶拦下，还没有发出去。
	//
	// 必须与 ErrRateLimited 分开：前者是「我们自己主动节流」，属于正常的
	// 流控行为，任务应当推迟而非计为失败；后者是「Steam 真的拒绝了我们」，
	// 需要触发全局熔断。二者混用会让回填高峰期的任务因反复被本地限流
	// 累加 attempts，最终无谓地进入死信。
	ErrThrottled = errors.New("steam: throttled by local rate limiter")
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

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
