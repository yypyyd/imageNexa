package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// The administrator control plane only accepts small JSON documents. Keeping
// this limit independent from the much larger media endpoints prevents an
// unauthenticated login/initialization request from inheriting a proxy's video
// upload allowance and forcing the process to buffer an attacker-sized body.
const maxAdminJSONBytes int64 = 1 << 20

func bindAdminJSON(c *gin.Context, target any, invalidMessage string) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminJSONBytes)
	if err := c.ShouldBindJSON(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"detail": "request body too large"})
			return false
		}
		if strings.TrimSpace(invalidMessage) == "" {
			invalidMessage = "invalid request body"
		}
		c.JSON(http.StatusBadRequest, gin.H{"detail": invalidMessage})
		return false
	}
	return true
}
