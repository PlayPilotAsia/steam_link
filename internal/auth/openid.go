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
