package service

import (
	"context"
	"testing"

	"backend/internal/model"
)

func TestRetiredRoutesExcludedWithoutSettings(t *testing.T) {
	routes := []model.ModelRoute{
		{ID: "retired-oreate", Provider: "oreate"},
		{ID: "live-byteplus", Provider: "byteplus"},
		{ID: "retired-runway", Provider: "runway"},
		{ID: "retired-custom", Provider: "custom"},
		{ID: "live-dola", Provider: "dola"},
	}
	got, err := filterEnabledProviderRoutes(context.Background(), nil, routes)
	if err != nil || len(got) != 2 || got[0].ID != "live-byteplus" || got[1].ID != "live-dola" {
		t.Fatalf("retired routes survived or live route order changed: %+v, %v", got, err)
	}
}
