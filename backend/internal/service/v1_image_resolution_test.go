package service

import (
	"testing"

	"backend/internal/model"
	"gorm.io/datatypes"
)

func TestResolveImageSizeModelSemantics(t *testing.T) {
	gptImage2 := &model.ModelConfig{ID: "firefly-gpt-image-2", Prices: datatypes.JSONMap{"1K": 1, "2K": 2, "4K": 4}}
	luminaGPTImage2 := &model.ModelConfig{ID: "lumina-gpt-image-2", Prices: datatypes.JSONMap{"1K": 1, "2K": 2, "4K": 4}}
	otherModel := &model.ModelConfig{ID: "seedream-4.5", Prices: datatypes.JSONMap{"1K": 1, "2K": 2, "4K": 4}}
	tests := []struct {
		name string
		item *model.ModelConfig
		in   V1ImageRequest
		want string
	}{
		{name: "other model ignores quality", item: otherModel, in: V1ImageRequest{Size: "2480x3312", Quality: "high"}, want: "2K"},
		{name: "GPT Image 2 maps quality", item: gptImage2, in: V1ImageRequest{Size: "2480x3312", Quality: "high"}, want: "4K"},
		{name: "Lumina GPT Image 2 maps quality", item: luminaGPTImage2, in: V1ImageRequest{Size: "2480x3312", Quality: "high"}, want: "4K"},
		{name: "blank quality keeps size tier", item: gptImage2, in: V1ImageRequest{Size: "2480x3312"}, want: "2K"},
		{name: "explicit resolution remains authoritative", item: gptImage2, in: V1ImageRequest{Size: "2480x3312", Quality: "high", Resolution: "2K"}, want: "2K"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ratio, resolution := resolveImageSize(tt.item, tt.in)
			if ratio != "3:4" || resolution != tt.want {
				t.Fatalf("resolveImageSize() = %q, %q; want 3:4, %q", ratio, resolution, tt.want)
			}
		})
	}
}

func TestResolveImageSizeForwards4KGPTImage2Payload(t *testing.T) {
	item := &model.ModelConfig{ID: "firefly-gpt-image-2", Prices: datatypes.JSONMap{"1K": 1, "2K": 2, "4K": 4}}
	ratio, resolution := resolveImageSize(item, V1ImageRequest{Size: "2480x3312", Quality: "high"})
	if size := upstreamSize(ratio, resolution); size != "3072x4096" {
		t.Fatalf("upstreamSize() = %q, want 3072x4096", size)
	}
	if quality := upstreamQualityForModel(item.ID, resolution); quality != "high" {
		t.Fatalf("upstreamQualityForModel() = %q, want high", quality)
	}
	if quality := upstreamQualityForModel("seedream-4.5", resolution); quality != "" {
		t.Fatalf("non-GPT upstream quality = %q, want empty", quality)
	}
	if quality := upstreamQualityForModel("lumina-gpt-image-2", resolution); quality != "high" {
		t.Fatalf("Lumina GPT Image 2 upstream quality = %q, want high", quality)
	}
}
