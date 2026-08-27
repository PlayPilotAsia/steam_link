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
