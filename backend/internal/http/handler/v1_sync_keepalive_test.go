package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"backend/internal/service"
	"github.com/gin-gonic/gin"
)

func TestSynchronousImageResponseReturnsSuccessWithoutEarlyFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	result := imageSyncResult{response: map[string]any{
		"created": int64(123),
		"data":    []map[string]any{{"url": "https://example.test/image.png"}},
	}}

	h := &V1Handler{}
	h.finishSynchronousImageResponse(c, result)

	if recorder.Flushed {
		t.Fatal("synchronous response flushed before its final JSON")
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response with leading keepalive whitespace is not valid JSON: %v", err)
	}
	if body["created"] != float64(123) {
		t.Fatalf("created = %#v, want 123", body["created"])
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestSynchronousImageResponsePreservesErrorStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	h := &V1Handler{}
	h.finishSynchronousImageResponse(c, imageSyncResult{err: service.ErrContentRejected})

	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("slow error response is not valid JSON: %v", err)
	}
	if body.Error["code"] != "content_policy_violation" {
		t.Fatalf("error code = %#v", body.Error["code"])
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("error status = %d, want 400", recorder.Code)
	}
}

func TestProviderQuotaUsesOpenAI429WithoutBearerChallenge(t *testing.T) {
	status, body := v1ErrorResponse(service.ErrProviderQuota, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	encoded, _ := json.Marshal(body)
	if !strings.Contains(string(encoded), `"code":"insufficient_quota"`) {
		t.Fatalf("body = %s", encoded)
	}
}

func TestV1ErrorResponsePreservesFastStatus(t *testing.T) {
	status, body := v1ErrorResponse(errors.Join(service.ErrUnknownModel, errors.New("missing")), nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Error["code"] != "model_not_found" {
		t.Fatalf("error code = %#v", decoded.Error["code"])
	}
}
