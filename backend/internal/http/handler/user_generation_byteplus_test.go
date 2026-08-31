package handler

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"backend/internal/service"
	"github.com/gin-gonic/gin"
)

type bytePlusCatalogExpectation struct {
	ratios      []string
	resolutions []string
	maxRefs     int
}

var expectedBytePlusCatalog = map[string]bytePlusCatalogExpectation{
	"lumina-seedream-5.0-pro": {
		ratios:      []string{"1:1", "16:9", "9:16", "4:3", "3:4"},
		resolutions: []string{"1K", "2K"},
		maxRefs:     10,
	},
	"lumina-gpt-image-2": {
		ratios:      []string{"1:1", "16:9", "9:16", "4:3", "3:4"},
		resolutions: []string{"1K", "2K", "4K"},
		maxRefs:     14,
	},
	"lumina-seedream-5.0-lite": {
		ratios:      []string{"1:1", "16:9", "9:16", "4:3", "3:4"},
		resolutions: []string{"2K"},
		maxRefs:     10,
	},
	"lumina-nano-banana-2": {
		ratios:      []string{"16:9", "9:16", "4:3", "3:4", "1:1"},
		resolutions: []string{"1K", "2K", "4K"},
		maxRefs:     14,
	},
	"lumina-nano-banana-pro": {
		ratios:      []string{"16:9", "9:16", "4:3", "3:4", "1:1"},
		resolutions: []string{"1K", "2K", "4K"},
		maxRefs:     14,
	},
}

func TestBytePlusModelsInBothHardcodedCatalogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &UserGenerationHandler{}
	c, _ := gin.CreateTestContext(nil)

	catalog, err := h.catalogEntries(c)
	if err != nil {
		t.Fatal(err)
	}
	models, err := h.publicModels()
	if err != nil {
		t.Fatal(err)
	}

	assertBytePlusCatalog(t, catalog, "type")
	assertBytePlusCatalog(t, models, "kind")
}

func assertBytePlusCatalog(t *testing.T, items []gin.H, kindKey string) {
	t.Helper()
	got := make(map[string]gin.H)
	for _, item := range items {
		if item["provider"] == "byteplus" {
			id, ok := item["id"].(string)
			if !ok {
				t.Fatalf("BytePlus entry has invalid id: %#v", item["id"])
			}
			got[id] = item
		}
	}
	if len(got) != len(expectedBytePlusCatalog) {
		t.Fatalf("BytePlus model count = %d, want %d; entries = %#v", len(got), len(expectedBytePlusCatalog), got)
	}
	for id, want := range expectedBytePlusCatalog {
		item, ok := got[id]
		if !ok {
			t.Errorf("missing BytePlus model %q", id)
			continue
		}
		if item[kindKey] != "image" {
			t.Errorf("%s %s = %#v, want image", id, kindKey, item[kindKey])
		}
		if item["image_to_image"] != true {
			t.Errorf("%s image_to_image = %#v, want true", id, item["image_to_image"])
		}
		if item["reference_mode"] != "asset" {
			t.Errorf("%s reference_mode = %#v, want asset", id, item["reference_mode"])
		}
		if item["max_reference_images"] != want.maxRefs {
			t.Errorf("%s max_reference_images = %#v, want %d", id, item["max_reference_images"], want.maxRefs)
		}
		if !reflect.DeepEqual(item["ratios"], want.ratios) {
			t.Errorf("%s ratios = %#v, want %#v", id, item["ratios"], want.ratios)
		}
		if !reflect.DeepEqual(item["resolutions"], want.resolutions) {
			t.Errorf("%s resolutions = %#v, want %#v", id, item["resolutions"], want.resolutions)
		}
	}
}

func TestBytePlusServiceErrorsKeepHTTPContract(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "auth", err: service.ErrProviderAuth, status: http.StatusServiceUnavailable},
		{name: "quota", err: service.ErrProviderQuota, status: http.StatusUnauthorized},
		{name: "temporary", err: service.ErrProviderTemporary, status: http.StatusServiceUnavailable},
		{name: "invalid params", err: service.ErrUnsupportedParams, status: http.StatusBadRequest},
		{name: "risk control", err: service.ErrContentRejected, status: http.StatusBadRequest},
		{name: "execution", err: service.ErrProviderExecution, status: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := v1ErrorResponse(errors.Join(tt.err, errors.New("byteplus upstream detail")), nil)
			if status != tt.status {
				t.Fatalf("v1ErrorResponse() status = %d, want %d; body=%#v", status, tt.status, body)
			}
		})
	}
}
