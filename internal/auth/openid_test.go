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
		"openid.ns":             {"http://specs.openid.net/auth/2.0"},
		"openid.mode":           {"id_res"},
		"openid.op_endpoint":    {"https://steamcommunity.com/openid/login"},
		"openid.claimed_id":     {claimedID},
		"openid.identity":       {claimedID},
		"openid.return_to":      {"https://app.example/auth/steam/callback"},
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
