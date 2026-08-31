package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"backend/internal/service"
	"github.com/gin-gonic/gin"
)

func TestCreateVideoRejectsSpoofedJSONInputReference(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	spoof := base64.StdEncoding.EncodeToString([]byte("not an image"))
	body := `{"model":"veo-3.1","prompt":"test","seconds":8,"input_reference":["` + spoof + `"]}`
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")

	handler := &V1Handler{}
	handler.CreateVideo(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "reference media content does not match its field") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestImageGenerationsRejectsOversizedJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := `{"model":"gpt-image-2","prompt":"` + strings.Repeat("x", maxImageJSONBytes) + `"}`
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")

	handler := &V1Handler{}
	handler.ImageGenerations(context)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "request_too_large") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestImageEditsRejectsUnsupportedMask(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	mask, err := writer.CreateFormFile("mask", "mask.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = mask.Write([]byte("not inspected because masks are unsupported"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	context.Request.Header.Set("Content-Type", writer.FormDataContentType())

	(&V1Handler{}).ImageEdits(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "unsupported_parameter") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestCreateVideoRejectsOversizedBodiesFromContentLength(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, contentType := range []string{"application/json", "multipart/form-data; boundary=test"} {
		t.Run(contentType, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", http.NoBody)
			context.Request.Header.Set("Content-Type", contentType)
			context.Request.ContentLength = maxGenerateMultipartBytes + 1

			handler := &V1Handler{}
			handler.CreateVideo(context)
			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413; body=%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "request_too_large") {
				t.Fatalf("body = %s", recorder.Body.String())
			}
		})
	}
}

func TestV1ErrorResponseSanitizesUnexpectedInternalErrors(t *testing.T) {
	secret := "postgres dial failed Authorization: Bearer sk-secret-value"
	status, body := v1ErrorResponse(errors.New(secret), nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "sk-secret-value") {
		t.Fatalf("response leaked internal error: %s", raw)
	}
	if !strings.Contains(string(raw), `"type":"server_error"`) {
		t.Fatalf("response is not an OpenAI server_error: %s", raw)
	}
}

func TestV1KnownErrorDoesNotLeakJoinedCause(t *testing.T) {
	secret := "Cookie: csrfToken=secret; session=secret"
	status, body := v1ErrorResponse(errors.Join(service.ErrUnknownModel, errors.New(secret)), nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "csrfToken") {
		t.Fatalf("response leaked joined cause: %s", raw)
	}
}

func TestGeneratedImageRequestIDHasRecoverablePollURL(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("X-Request-Id", "server-generated-request-id")
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "https://api.example.test/v1/images/generations", http.NoBody)

	requestID := ensureImageRequestID(context, "")
	if requestID != "server-generated-request-id" {
		t.Fatalf("request id = %q", requestID)
	}
	if got := imageTaskPollURL(context, requestID); got != "/v1/images/tasks?request_id=server-generated-request-id" {
		t.Fatalf("poll URL = %q", got)
	}
}
