package middleware

import (
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Recovery catches handler panics without dumping the request. Gin's default
// recovery middleware logs every request header, which can disclose Bearer
// credentials, administrator cookies, and CSRF tokens.
func Recovery(output io.Writer) gin.HandlerFunc {
	if output == nil {
		output = io.Discard
	}
	logger := log.New(output, "", log.LstdFlags|log.LUTC)

	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				requestID, _ := c.Get(requestIDKey)
				logger.Printf(
					"http panic recovered request_id=%q method=%q path=%q panic_type=%T",
					requestID,
					c.Request.Method,
					c.Request.URL.Path,
					recovered,
				)

				c.Abort()
				if c.Writer.Written() {
					return
				}
				if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
					c.JSON(http.StatusInternalServerError, gin.H{
						"error": gin.H{
							"message": "The server encountered an internal error.",
							"type":    "server_error",
							"param":   nil,
							"code":    "server_error",
						},
					})
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{"detail": "internal server error"})
			}
		}()

		c.Next()
	}
}
