package adobe

import (
	"fmt"
	"testing"
)

func TestResolveNewPartnerModels(t *testing.T) {
	tests := []struct {
		id, upstream, version string
	}{
		{"firefly-gpt-image-1.5", "gpt-image", "1.5"},
		{"firefly-nano-banana", "gemini-flash", "nano-banana"},
		{"firefly-nano-banana-pro", "gemini-flash", "nano-banana-2"},
		{"firefly-nano-banana-2", "gemini-flash", "nano-banana-3"},
	}
	for _, tt := range tests {
		spec := ResolveModelSpec(tt.id)
		if spec.UpstreamModelID != tt.upstream || spec.UpstreamModelVersion != tt.version {
			t.Fatalf("%s resolved to %s/%s", tt.id, spec.UpstreamModelID, spec.UpstreamModelVersion)
		}
	}
}

func TestNewPartnerPayloadShapes(t *testing.T) {
	gpt := BuildImagePayloadCandidates("firefly-gpt-image-1.5", "test", "3:2", "1K", nil)[0]
	if gpt["modelVersion"] != "1.5" {
		t.Fatalf("gpt modelVersion = %#v", gpt["modelVersion"])
	}
	if size := gpt["size"].(map[string]any); size["width"] != 1536 || size["height"] != 1024 {
		t.Fatalf("gpt size = %#v", size)
	}
	if _, ok := gpt["generationSettings"]; ok {
		t.Fatalf("GPT Image 1.5 generationSettings = %#v, want absent", gpt["generationSettings"])
	}
	if seeds, ok := gpt["seeds"].([]int); !ok || len(seeds) != 1 {
		t.Fatalf("GPT Image 1.5 seeds = %#v, want one seed", gpt["seeds"])
	}
	if specific := gpt["modelSpecificPayload"].(map[string]any); len(specific) != 0 {
		t.Fatalf("GPT Image 1.5 modelSpecificPayload = %#v, want empty object", specific)
	}

	banana := BuildImagePayloadCandidates("firefly-nano-banana-pro", "test", "16:9", "2K", nil)[0]
	if banana["modelVersion"] != "nano-banana-2" {
		t.Fatalf("banana modelVersion = %#v", banana["modelVersion"])
	}
	if size := banana["size"].(map[string]any); size["width"] != 2752 || size["height"] != 1536 {
		t.Fatalf("banana size = %#v", size)
	}
}

func TestNanoBananaModelsUseCurrentAdobePayloadShape(t *testing.T) {
	tests := []struct {
		model            string
		resolution       string
		ratio            string
		wantWidth        int
		wantHeight       int
		wantGroundSearch bool
	}{
		{model: "firefly-nano-banana", resolution: "1K", ratio: "4:3", wantWidth: 1184, wantHeight: 864},
		{model: "firefly-nano-banana", resolution: "1K", ratio: "3:4", wantWidth: 864, wantHeight: 1184},
		{model: "firefly-nano-banana", resolution: "1K", ratio: "5:4", wantWidth: 1152, wantHeight: 896},
		{model: "firefly-nano-banana", resolution: "1K", ratio: "4:5", wantWidth: 896, wantHeight: 1152},
		{model: "firefly-nano-banana-pro", resolution: "1K", ratio: "16:9", wantWidth: 1376, wantHeight: 768, wantGroundSearch: true},
		{model: "firefly-nano-banana-pro", resolution: "4K", ratio: "1:1", wantWidth: 4096, wantHeight: 4096, wantGroundSearch: true},
		{model: "firefly-nano-banana-2", resolution: "2K", ratio: "9:16", wantWidth: 1536, wantHeight: 2752, wantGroundSearch: true},
		{model: "firefly-nano-banana-2", resolution: "4K", ratio: "8:1", wantWidth: 12288, wantHeight: 1536, wantGroundSearch: true},
		{model: "firefly-nano-banana-2", resolution: "1K", ratio: "auto", wantWidth: -1, wantHeight: -1, wantGroundSearch: true},
	}

	for _, tt := range tests {
		t.Run(tt.model+"/"+tt.resolution+"/"+tt.ratio, func(t *testing.T) {
			payload := BuildImagePayloadCandidates(tt.model, "test", tt.ratio, tt.resolution, nil)[0]
			size := payload["size"].(map[string]any)
			if size["width"] != tt.wantWidth || size["height"] != tt.wantHeight {
				t.Fatalf("size = %#v, want %dx%d", size, tt.wantWidth, tt.wantHeight)
			}
			specific := payload["modelSpecificPayload"].(map[string]any)
			if tt.ratio == "auto" {
				if _, ok := specific["aspectRatio"]; ok {
					t.Fatalf("modelSpecificPayload.aspectRatio = %#v, want absent for auto", specific["aspectRatio"])
				}
			} else if specific["aspectRatio"] != tt.ratio {
				t.Fatalf("modelSpecificPayload.aspectRatio = %#v, want %q", specific["aspectRatio"], tt.ratio)
			}
			if _, ok := payload["generationSettings"]; ok {
				t.Fatalf("generationSettings = %#v, want absent", payload["generationSettings"])
			}
			_, hasGroundSearch := payload["groundSearch"]
			if hasGroundSearch != tt.wantGroundSearch {
				t.Fatalf("groundSearch presence = %v, want %v", hasGroundSearch, tt.wantGroundSearch)
			}
		})
	}
}

func TestImagePayloadSeedsChangeBetweenImmediateAttempts(t *testing.T) {
	seen := make(map[int]bool)
	for i := 0; i < 10; i++ {
		payload := BuildImagePayloadCandidates("firefly-gpt-image-2", "test", "1:1", "1K", nil)[0]
		seeds := payload["seeds"].([]int)
		if len(seeds) != 1 || seeds[0] < 0 || seeds[0] >= 999999 {
			t.Fatalf("invalid image seed: %#v", seeds)
		}
		if seen[seeds[0]] {
			t.Fatalf("duplicate image seed on immediate payload rebuild: %d", seeds[0])
		}
		seen[seeds[0]] = true
	}
}

func TestGPTImage2UsesCurrentAdobePayloadShape(t *testing.T) {
	tests := []struct {
		resolution string
		ratio      string
		wantWidth  int
		wantHeight int
		wantDetail int
	}{
		{resolution: "1K", ratio: "1:1", wantWidth: 1024, wantHeight: 1024, wantDetail: 1},
		{resolution: "2K", ratio: "16:9", wantWidth: 1376, wantHeight: 768, wantDetail: 3},
		{resolution: "4K", ratio: "9:16", wantWidth: 768, wantHeight: 1376, wantDetail: 5},
	}

	for _, tt := range tests {
		t.Run(tt.resolution+"/"+tt.ratio, func(t *testing.T) {
			payload := BuildImagePayloadCandidates("firefly-gpt-image-2", "test", tt.ratio, tt.resolution, nil)[0]
			size := payload["size"].(map[string]any)
			if size["width"] != tt.wantWidth || size["height"] != tt.wantHeight {
				t.Fatalf("size = %#v, want %dx%d", size, tt.wantWidth, tt.wantHeight)
			}
			if specific := payload["modelSpecificPayload"].(map[string]any); len(specific) != 0 {
				t.Fatalf("modelSpecificPayload = %#v, want empty object", specific)
			}
			settings := payload["generationSettings"].(map[string]any)
			if settings["detailLevel"] != tt.wantDetail {
				t.Fatalf("detailLevel = %#v, want %d", settings["detailLevel"], tt.wantDetail)
			}
		})
	}
}

func TestGPTImage2SubjectReferencesKeepCurrentPayloadShape(t *testing.T) {
	payload := BuildImagePayloadCandidates("firefly-gpt-image-2", "edit", "3:2", "2K", []string{"blob-1"})[0]
	refs := payload["referenceBlobs"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["usage"] != "subject" {
		t.Fatalf("referenceBlobs = %#v", refs)
	}
	if _, ok := payload["size"]; !ok {
		t.Fatal("top-level size is missing")
	}
	if specific := payload["modelSpecificPayload"].(map[string]any); len(specific) != 0 {
		t.Fatalf("modelSpecificPayload = %#v, want empty object", specific)
	}
}

func TestVeo31LitePayload(t *testing.T) {
	payload := BuildVideoPayload("veo31-lite", "test", "16:9", 4, "720p", "frame", "", VideoInputs{})
	if payload["modelId"] != "veo" || payload["modelVersion"] != "3.1-lite-generate" {
		t.Fatalf("unexpected Veo Lite model: %v/%v", payload["modelId"], payload["modelVersion"])
	}
	if payload["duration"] != 4 {
		t.Fatalf("duration = %#v", payload["duration"])
	}
}

func TestFireflyVideoPayloadForwardsAllSupportedInputs(t *testing.T) {
	payload := BuildVideoPayload("firefly-video", "test", "9:16", 5, "720p", "frame", "", VideoInputs{
		ImageBlobIDs: []string{"first-frame", "last-frame"},
		VideoBlobIDs: []string{"control-video"},
	})
	if payload["prompt"] != "test" || payload["locale"] != "en-US" {
		t.Fatalf("Firefly payload basics = %#v", payload)
	}
	sizes := payload["sizes"].([]any)
	if len(sizes) != 1 {
		t.Fatalf("Firefly sizes = %#v", sizes)
	}
	size := sizes[0].(map[string]any)
	if size["width"] != 720 || size["height"] != 1280 || size["numFrames"] != 128 {
		t.Fatalf("Firefly size/duration = %#v", size)
	}
	conditions := payload["image"].(map[string]any)["conditions"].([]any)
	if len(conditions) != 2 || conditions[0].(map[string]any)["placement"].(map[string]any)["start"] != 0 ||
		conditions[1].(map[string]any)["placement"].(map[string]any)["start"] != 1 {
		t.Fatalf("Firefly image conditions = %#v", conditions)
	}
	control := payload["controlData"].(map[string]any)["structureData"].(map[string]any)["referenceVideo"].(map[string]any)
	if control["source"].(map[string]any)["id"] != "control-video" {
		t.Fatalf("Firefly control video = %#v", control)
	}
}

func TestNewPartnerVideoPayloads(t *testing.T) {
	tests := []struct {
		engine, modelID, version string
	}{
		{"kling-v3", "kling", "kling_v3_standard_t2v"},
		{"kling-o3", "kling", "kling_o3_standard_t2v"},
		{"runway45", "runway", "gen4.5"},
		{"seedance20", "seedance", "seedance_2.0"},
		{"seedance20-fast", "seedance", "seedance_2.0_fast"},
	}
	for _, tt := range tests {
		payload := BuildVideoPayload(tt.engine, "test", "16:9", 5, "720p", "frame", "", VideoInputs{})
		if payload["modelId"] != tt.modelID || payload["modelVersion"] != tt.version {
			t.Fatalf("%s model = %v/%v", tt.engine, payload["modelId"], payload["modelVersion"])
		}
		if payload["duration"] != 5 {
			t.Fatalf("%s duration = %#v", tt.engine, payload["duration"])
		}
	}
}

func TestSupportsVideoDuration(t *testing.T) {
	tests := []struct {
		engine  string
		seconds int
		want    bool
	}{
		{"veo31-lite", 4, true},
		{"veo31-lite", 5, false},
		{"kling-v3", 3, true},
		{"kling-o3", 14, true},
		{"kling-v3", 16, false},
		{"runway45", 8, true},
		{"runway45", 9, false},
		{"seedance20", 4, true},
		{"seedance20-fast", 15, true},
		{"seedance20", 16, false},
		{"luma", 5, true},
		{"luma", 10, false},
		{"firefly-video", 5, true},
		{"firefly-video", 10, false},
	}
	for _, tt := range tests {
		if got := SupportsVideoDuration(tt.engine, tt.seconds); got != tt.want {
			t.Errorf("SupportsVideoDuration(%q, %d) = %v, want %v", tt.engine, tt.seconds, got, tt.want)
		}
	}
}

func TestSupportsVideoResolution(t *testing.T) {
	tests := []struct {
		engine, resolution string
		want               bool
	}{
		{"veo31-lite", "1080p", true},
		{"veo31-lite", "480p", false},
		{"kling-v3", "1080p", true},
		{"runway45", "1080p", false},
		{"seedance20", "480p", true},
		{"seedance20-fast", "1080p", true},
		{"seedance20", "4K", false},
		{"luma", "4K", true},
		{"luma", "540p", false},
		{"firefly-video", "540p", true},
	}
	for _, tt := range tests {
		if got := SupportsVideoResolution(tt.engine, tt.resolution); got != tt.want {
			t.Errorf("SupportsVideoResolution(%q, %q) = %v, want %v", tt.engine, tt.resolution, got, tt.want)
		}
	}
}

func TestSeedanceAndRayResolutionPayloads(t *testing.T) {
	seedance := BuildVideoPayload("seedance20", "test", "16:9", 5, "480p", "frame", "", VideoInputs{})
	seedanceSize := seedance["size"].(map[string]any)
	if seedanceSize["width"] != 854 || seedanceSize["height"] != 480 {
		t.Fatalf("Seedance 480p size = %#v", seedanceSize)
	}

	ray := BuildVideoPayload("luma", "test", "16:9", 5, "4K", "frame", "", VideoInputs{})
	raySize := ray["size"].(map[string]any)
	if raySize["width"] != 3840 || raySize["height"] != 2160 {
		t.Fatalf("Ray 4K size = %#v", raySize)
	}
}

func TestVideoMediaAndAudioPayloads(t *testing.T) {
	veo := BuildVideoPayload("veo31-fast", "test", "16:9", 8, "1080p", "frame", "", VideoInputs{GenerateAudio: true})
	if veo["generateAudio"] != true {
		t.Fatalf("Veo generateAudio = %#v", veo["generateAudio"])
	}

	kling := BuildVideoPayload("kling-o3", "test", "16:9", 5, "1080p", "frame", "", VideoInputs{
		ImageBlobIDs: []string{"image-1"}, VideoBlobIDs: []string{"video-1"}, GenerateAudio: true,
	})
	if kling["modelVersion"] != "kling_o3_pro_v2v_reference" {
		t.Fatalf("Kling V2V modelVersion = %#v", kling["modelVersion"])
	}
	if kling["generateAudio"] != true {
		t.Fatalf("Kling generateAudio = %#v", kling["generateAudio"])
	}
	refs := kling["referenceBlobs"].([]any)
	if len(refs) != 2 || refs[0].(map[string]any)["usage"] != "source" {
		t.Fatalf("Kling references = %#v", refs)
	}

	seedance := BuildVideoPayload("seedance20", "test", "9:16", 15, "1080p", "frame", "", VideoInputs{
		VideoBlobIDs: []string{"video-1"}, AudioBlobIDs: []string{"audio-1"}, GenerateAudio: true,
	})
	seedRefs := seedance["referenceBlobs"].([]any)
	if len(seedRefs) != 2 || seedance["generateAudio"] != true {
		t.Fatalf("Seedance multimodal payload = %#v", seedance)
	}
	if seedRefs[0].(map[string]any)["mention"] == nil || seedRefs[1].(map[string]any)["mention"] == nil {
		t.Fatalf("Seedance source mentions missing: %#v", seedRefs)
	}
	for _, ref := range seedRefs {
		mention := ref.(map[string]any)["mention"].(map[string]any)
		if len(mention["id"].(string)) < 21 {
			t.Fatalf("Seedance mention id is too short: %#v", mention)
		}
	}
}

func TestSeedanceKeepsAllReferenceImagesWithinSharedLimit(t *testing.T) {
	payload := BuildVideoPayload("seedance20-fast", "test", "16:9", 15, "1080p", "asset", "", VideoInputs{
		ImageBlobIDs: []string{"image-1", "image-2", "image-3", "image-4"},
		VideoBlobIDs: []string{"video-1", "video-2", "video-3"},
		AudioBlobIDs: []string{"audio-1", "audio-2"},
	})
	refs := payload["referenceBlobs"].([]any)
	if len(refs) != 9 {
		t.Fatalf("Seedance references = %d, want 9: %#v", len(refs), refs)
	}
	for i, raw := range refs[:4] {
		ref := raw.(map[string]any)
		if ref["usage"] != "style" || ref["id"] != fmt.Sprintf("image-%d", i+1) {
			t.Fatalf("Seedance image reference %d = %#v", i, ref)
		}
	}
}
