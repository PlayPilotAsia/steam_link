package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/PlayPilotAsia/libra/errcode"
	"github.com/PlayPilotAsia/libra/response"

	"github.com/PlayPilotAsia/steam_link/internal/auth"
)

func TestRouterRegistersGatewayPathsOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := NewRouter(Deps{})

	got := make(map[string]bool)
	for _, route := range router.Routes() {
		got[route.Method+" "+route.Path] = true
	}

	expected := []string{
		"GET /noauth/steam/login",
		"GET /noauth/steam/callback",
		"GET /api/steam/library",
		"DELETE /api/steam/link",
		"POST /api/steam/link/recheck",
		"GET /api/steam/games/:appid/achievements",
		"GET /api/steam/achievements/recent",
	}
	for _, route := range expected {
		require.True(t, got[route], "missing route %s", route)
	}
	require.Len(t, got, len(expected))
	require.False(t, got["POST /dev/login"])
	require.False(t, got["GET /auth/steam/login"])
}

func TestCurrentUserIDReadsTrustedGatewayHeader(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/steam/library", nil)
	context.Request.Header.Set(userIDHeader, "22002")

	userID, ok := (Deps{}).currentUserID(context)

	require.True(t, ok)
	require.Equal(t, uint64(22002), userID)
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestCurrentUserIDRejectsMissingOrInvalidHeader(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodGet, "/api/steam/library", nil)
			context.Request.Header.Set(requestIDHeader, "request-id")
			if value != "" {
				context.Request.Header.Set(userIDHeader, value)
			}

			_, ok := (Deps{}).currentUserID(context)

			require.False(t, ok)
			require.Equal(t, http.StatusUnauthorized, recorder.Code)

			var body response.Response
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.Equal(t, errcode.UserAuthUnauthenticated.Code(), body.Code)
			require.Equal(t, errcode.UserAuthUnauthenticated.DefaultMessage(), body.Message)
			require.Equal(t, "request-id", body.TraceID)
			require.Equal(t, map[string]any{}, body.Data)
		})
	}
}

func TestUnifiedSuccessResponseCarriesTraceIDAndData(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/steam/library", nil)
	context.Request.Header.Set(requestIDHeader, "request-id")

	succeed(context, gin.H{"value": "result"})

	require.Equal(t, http.StatusOK, recorder.Code)
	var body response.Response
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, errcode.Success, body.Code)
	require.Equal(t, "success", body.Message)
	require.Equal(t, "request-id", body.TraceID)
	require.Equal(t, map[string]any{"value": "result"}, body.Data)
}

func TestHandleLoginRedirectsWhenGatewayIdentityMissing(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/noauth/steam/login", nil)

	(Deps{BaseURL: "http://localhost:9980"}).handleLogin(context)

	require.Equal(t, http.StatusFound, recorder.Code)
	require.Equal(t,
		"http://localhost:9980/settings/steam?status=unauthorized",
		recorder.Header().Get("Location"))
}

func TestHandleLoginSignsGatewayIdentityIntoNoauthCallback(t *testing.T) {
	secret := []byte("state-secret")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/noauth/steam/login", nil)
	context.Request.Header.Set(userIDHeader, "22002")

	(Deps{BaseURL: "http://localhost:9980", StateSecret: secret}).handleLogin(context)

	require.Equal(t, http.StatusFound, recorder.Code)
	steamRedirect, err := url.Parse(recorder.Header().Get("Location"))
	require.NoError(t, err)
	returnTo, err := url.Parse(steamRedirect.Query().Get("openid.return_to"))
	require.NoError(t, err)
	require.Equal(t, "/noauth/steam/callback", returnTo.Path)
	userID, err := auth.VerifyState(secret, returnTo.Query().Get("state"), time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, uint64(22002), userID)
}
