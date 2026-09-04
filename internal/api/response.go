package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/PlayPilotAsia/libra/errcode"
	"github.com/PlayPilotAsia/libra/response"
)

const requestIDHeader = "X-Request-Id"

// traceID 取 Gateway 注入的请求 ID。缺失时留空，避免生成一个无法与
// Gateway 日志关联的本地 ID。
func traceID(c *gin.Context) string {
	return c.GetHeader(requestIDHeader)
}

func fail(c *gin.Context, definition errcode.Definition) {
	c.JSON(definition.HTTPStatus(), response.Failure(definition, traceID(c)))
}

func succeed(c *gin.Context, data any) {
	c.JSON(http.StatusOK, response.Success(data, traceID(c)))
}
