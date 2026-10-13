package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"backend/internal/service"
	"github.com/gin-gonic/gin"
)

func TestWriteVideoContentPreservesPartialResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writeVideoContent(ctx, &service.VideoContent{
		Body: io.NopCloser(strings.NewReader("test")), ContentType: "video/mp4",
		StatusCode: http.StatusPartialContent, ContentLength: 4,
		ContentRange: "bytes 4-7/12", AcceptRanges: "bytes",
	})
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "test" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	for key, want := range map[string]string{
		"Content-Type": "video/mp4", "Content-Length": "4",
		"Content-Range": "bytes 4-7/12", "Accept-Ranges": "bytes",
		"X-Accel-Buffering": "no",
	} {
		if got := recorder.Header().Get(key); got != want {
			t.Fatalf("%s=%q want %q", key, got, want)
		}
	}
}
