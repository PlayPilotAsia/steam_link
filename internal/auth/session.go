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
