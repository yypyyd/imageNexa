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

func TestV1ParameterErrorIsActionableWithoutEchoingInput(t *testing.T) {
	err := &service.ParameterError{Param: "n", Message: "n must be 1.", Supported: []string{"1"}}
	status, body := v1ErrorResponse(err, nil)
	data, _ := json.Marshal(body)
	for _, value := range []string{`"param":"n"`, `"code":"unsupported_parameter"`, `"supported_values":["1"]`} {
		if !strings.Contains(string(data), value) {
			t.Fatalf("missing %s", value)
		}
	}
	if status != 400 {
		t.Fatalf("status=%d", status)
	}
	_, body = v1ErrorResponse(errors.Join(service.ErrUnsupportedParams, errors.New("secret-token")), nil)
	data, _ = json.Marshal(body)
	if strings.Contains(string(data), "secret-token") {
		t.Fatal("raw failure leaked")
	}
}

func TestAdminProviderAuthErrorIsNotReportedAsOriginFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	adminTestError(ctx, service.ErrProviderAuth)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "所选账号凭据失效") {
		t.Fatalf("body=%s", recorder.Body.String())
	}
}
