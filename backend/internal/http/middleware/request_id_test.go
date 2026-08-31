package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestIDAcceptsOnlyBoundedLogSafeValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name  string
		value string
		keep  bool
	}{
		{name: "uuid-like", value: "request_1234-abcd.example:v1", keep: true},
		{name: "spaces", value: "Bearer sk-secret value"},
		{name: "punctuation", value: "request/key?secret=true"},
		{name: "too long", value: strings.Repeat("a", 129)},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := gin.New()
			engine.Use(RequestID())
			engine.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("X-Request-Id", test.value)
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			got := recorder.Header().Get("X-Request-Id")
			if test.keep && got != test.value {
				t.Fatalf("request ID = %q, want original %q", got, test.value)
			}
			if !test.keep && (got == "" || got == test.value) {
				t.Fatalf("unsafe request ID was not replaced: %q", got)
			}
		})
	}
}
