package auth

import (
	"testing"
	"time"

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
