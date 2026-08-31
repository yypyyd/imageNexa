package adobe

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

var imageSeedCounter atomic.Uint64

func init() {
	imageSeedCounter.Store(uint64(time.Now().UnixNano()))
}

// nextImageSeed is process-unique for 999,999 consecutive calls. Adobe accepts
// seeds in this range; a monotonic atomic counter avoids the duplicate seeds
// produced by the old second-resolution timestamp during immediate retries.
func nextImageSeed() int {
	return int(imageSeedCounter.Add(1) % 999999)
}

type modelSpec struct {
	UpstreamModelID      string
	UpstreamModelVersion string
}

var lumaSize = map[string]map[string][2]int{
	"720p": {
		"21:9": {1280, 548}, "16:9": {1280, 720}, "4:3": {960, 720},
		"1:1": {720, 720}, "3:4": {720, 960}, "9:16": {720, 1280}, "9:21": {548, 1280},
	},
	"1080p": {
		"21:9": {1920, 822}, "16:9": {1920, 1080}, "4:3": {1440, 1080},
		"1:1": {1080, 1080}, "3:4": {1080, 1440}, "9:16": {1080, 1920}, "9:21": {822, 1920},
	},
	"4k": {
		"21:9": {3840, 1646}, "16:9": {3840, 2160}, "4:3": {2880, 2160},
		"1:1": {2160, 2160}, "3:4": {2160, 2880}, "9:16": {2160, 3840}, "9:21": {1646, 3840},
	},
}

// GPT Image 2 keeps one canonical size per aspect ratio. Output quality is a
// separate detailLevel (1/3/5), rather than a multiplier on these dimensions.
// These values mirror Adobe Firefly's current gpt-image-2 model configuration.
var gptImage2Size = map[string][2]int{
	"21:9": {1584, 672}, "16:9": {1376, 768}, "5:4": {1152, 928},
	"4:3": {1200, 896}, "3:2": {1264, 848}, "1:1": {1024, 1024},
	"4:5": {928, 1152}, "3:4": {896, 1200}, "2:3": {848, 1264},
	"9:16": {768, 1376},
}

var gptImage15Size = map[string][2]int{
	"1:1": {1024, 1024},
	"3:2": {1536, 1024},
	"2:3": {1024, 1536},
}

var nanoBananaSize = map[string][2]int{
	"1:1": {1024, 1024}, "3:2": {1248, 832}, "2:3": {832, 1248},
	"4:3": {1184, 864}, "3:4": {864, 1184}, "5:4": {1152, 896},
	"4:5": {896, 1152}, "9:16": {768, 1344}, "16:9": {1344, 768},
	"21:9": {1536, 672},
}

// Nano Banana Pro and Gemini 3.1 use canonical dimensions for an explicit
// aspect ratio. Negative sentinel dimensions are valid only for the Auto
// aspect-ratio option; sending them with an explicit ratio is rejected by the
// current 3P endpoint because width/height must then be positive.
var nanoBananaTierSize = map[string]map[string][2]int{
	"1K": {
		"4:3": {1200, 896}, "1:1": {1024, 1024}, "9:16": {768, 1376},
		"16:9": {1376, 768}, "3:4": {896, 1200}, "21:9": {1584, 672},
		"3:2": {1264, 848}, "5:4": {1152, 928}, "4:5": {928, 1152},
		"2:3": {848, 1264}, "8:1": {3072, 384}, "4:1": {2048, 512},
		"1:4": {512, 2048}, "1:8": {384, 3072}, "auto": {-1, -1},
	},
	"2K": {
		"4:3": {2400, 1792}, "1:1": {2048, 2048}, "9:16": {1536, 2752},
		"16:9": {2752, 1536}, "3:4": {1792, 2400}, "21:9": {3168, 1344},
		"3:2": {2528, 1696}, "5:4": {2304, 1856}, "4:5": {1856, 2304},
		"2:3": {1696, 2528}, "8:1": {6144, 768}, "4:1": {4096, 1024},
		"1:4": {1024, 4096}, "1:8": {768, 6144}, "auto": {-2, -2},
	},
	"4K": {
		"4:3": {4800, 3584}, "1:1": {4096, 4096}, "9:16": {3072, 5504},
		"16:9": {5504, 3072}, "3:4": {3584, 4800}, "21:9": {6336, 2688},
		"3:2": {5056, 3392}, "5:4": {4608, 3712}, "4:5": {3712, 4608},
		"2:3": {3392, 5056}, "8:1": {12288, 1536}, "4:1": {8192, 2048},
		"1:4": {2048, 8192}, "1:8": {1536, 12288}, "auto": {-4, -4},
	},
}

var fluxSize = map[string][2]int{
	"1:1":  {1024, 1024},
	"16:9": {1408, 768},
	"9:16": {768, 1408},
	"4:3":  {1280, 896},
	"3:4":  {896, 1280},
}

var defaultSize = map[string]map[string][2]int{
	"1K": {"1:1": {1024, 1024}, "1:8": {384, 3072}, "1:4": {512, 2048}, "16:9": {1360, 768}, "9:16": {768, 1360}, "4:1": {2048, 512}, "4:3": {1152, 864}, "3:4": {864, 1152}, "8:1": {3072, 384}},
	"2K": {"1:1": {2048, 2048}, "1:8": {768, 6144}, "1:4": {1024, 4096}, "16:9": {2752, 1536}, "9:16": {1536, 2752}, "4:1": {4096, 1024}, "4:3": {2048, 1536}, "3:4": {1536, 2048}, "8:1": {6144, 768}},
	"4K": {"1:1": {4096, 4096}, "1:8": {1536, 12288}, "1:4": {2048, 8192}, "16:9": {5504, 3072}, "9:16": {3072, 5504}, "4:1": {8192, 2048}, "4:3": {4096, 3072}, "3:4": {3072, 4096}, "8:1": {12288, 1536}},
}

func ResolveModelSpec(modelID string) modelSpec {
	switch modelID {
	case "firefly-gpt-image", "firefly-gpt-image-2":
		return modelSpec{UpstreamModelID: "gpt-image", UpstreamModelVersion: "2"}
	case "firefly-gpt-image-1.5":
		return modelSpec{UpstreamModelID: "gpt-image", UpstreamModelVersion: "1.5"}
	case "firefly-nano-banana":
		return modelSpec{UpstreamModelID: "gemini-flash", UpstreamModelVersion: "nano-banana"}
	case "firefly-nano-banana-pro":
		return modelSpec{UpstreamModelID: "gemini-flash", UpstreamModelVersion: "nano-banana-2"}
	case "firefly-nano-banana-2":
		return modelSpec{UpstreamModelID: "gemini-flash", UpstreamModelVersion: "nano-banana-3"}
	case "flux-kontext-max":
		return modelSpec{UpstreamModelID: "flux", UpstreamModelVersion: "fluxKontextMax"}
	default:
		return modelSpec{UpstreamModelID: "gemini-flash", UpstreamModelVersion: "nano-banana-3"}
	}
}

// buildImage5Payload builds the Adobe Firefly Image 5 request. It uses a distinct
// schema from the firefly-3p models: NO modelId/size, a top-level aspectRatio
// string label and a resolutionLevel (1K→1MP, 2K→4MP). Mirrors a captured
// working image-v5.ff.adobe.io request.
func buildImage5Payload(prompt, aspectRatio, resolution string, blobIDs []string) map[string]any {
	p := map[string]any{
		"n":                    1,
		"seeds":                []int{nextImageSeed()},
		"output":               map[string]any{"storeInputs": true},
		"prompt":               prompt,
		"referenceBlobs":       []any{},
		"modelSpecificPayload": map[string]any{"locale": "en-US", "prompt_reasoner": "quality"},
		"modelVersion":         "image5",
		"resolutionLevel":      image5ResolutionLevel(resolution),
		"generationMetadata":   map[string]any{"module": "text2image", "submodule": "ff-image-generate"},
	}
	if len(blobIDs) > 0 {
		// Instruct-edit: aspect ratio is derived from the reference image; sending
		// aspectRatio is rejected with a validation_error.
		p["referenceBlobs"] = blobRefs(blobIDs, "general")
	} else {
		p["aspectRatio"] = defaultString(aspectRatio, "1:1")
	}
	return p
}

// image5ResolutionLevel maps the UI resolution tier to Image 5's megapixel level.
func image5ResolutionLevel(resolution string) string {
	switch strings.ToUpper(strings.TrimSpace(resolution)) {
	case "1K":
		return "1MP"
	case "2K":
		return "4MP"
	default:
		return "4MP"
	}
}

func BuildImagePayloadCandidates(modelID, prompt, aspectRatio, outputResolution string, blobIDs []string) []map[string]any {
	spec := ResolveModelSpec(modelID)
	ratio := defaultString(aspectRatio, "1:1")
	resolution := defaultString(outputResolution, "2K")

	switch spec.UpstreamModelID {
	case "gpt-image":
		if spec.UpstreamModelVersion == "1.5" {
			return buildGPTImage15Payloads(spec, prompt, ratio, blobIDs)
		}
		return buildGPTImagePayloads(spec, prompt, ratio, resolution, blobIDs)
	case "gemini-flash":
		return buildGeminiPayloads(spec, prompt, ratio, resolution, blobIDs)
	case "flux":
		return buildFluxPayloads(spec, prompt, ratio, blobIDs)
	default:
		return buildDefaultPayloads(spec, prompt, ratio, resolution, blobIDs)
	}
}

func buildGPTImage15Payloads(spec modelSpec, prompt, ratio string, blobIDs []string) []map[string]any {
	size, ok := gptImage15Size[ratio]
	if !ok {
		size = gptImage15Size["1:1"]
	}
	base := map[string]any{
		"modelId":              spec.UpstreamModelID,
		"modelVersion":         spec.UpstreamModelVersion,
		"n":                    1,
		"prompt":               prompt,
		"size":                 map[string]any{"width": size[0], "height": size[1]},
		"seeds":                []int{nextImageSeed()},
		"output":               map[string]any{"storeInputs": true},
		"referenceBlobs":       []any{},
		"generationMetadata":   map[string]any{"module": "text2image", "submodule": "ff-image-generate"},
		"modelSpecificPayload": map[string]any{},
	}
	if len(blobIDs) == 0 {
		return []map[string]any{base}
	}
	edited := cloneMap(base)
	edited["referenceBlobs"] = blobRefs(blobIDs, "subject")
	return []map[string]any{edited}
}

func buildGeminiPayloads(spec modelSpec, prompt, ratio, resolution string, blobIDs []string) []map[string]any {
	var size [2]int
	isOriginal := spec.UpstreamModelVersion == "nano-banana"
	if isOriginal {
		size = nanoBananaSize[ratio]
		if size == [2]int{} {
			size = nanoBananaSize["1:1"]
		}
	} else {
		level := strings.ToUpper(strings.TrimSpace(resolution))
		levelSizes := nanoBananaTierSize[level]
		if levelSizes == nil {
			levelSizes = nanoBananaTierSize["2K"]
		}
		size = levelSizes[strings.ToLower(strings.TrimSpace(ratio))]
		if size == [2]int{} {
			size = levelSizes["1:1"]
		}
	}
	modelSpecific := map[string]any{
		"parameters": map[string]any{"addWatermark": false},
	}
	if !strings.EqualFold(strings.TrimSpace(ratio), "auto") {
		modelSpecific["aspectRatio"] = ratio
	}
	base := map[string]any{
		"modelId":      spec.UpstreamModelID,
		"modelVersion": spec.UpstreamModelVersion,
		"n":            1,
		"prompt":       prompt,
		"size":         map[string]any{"width": size[0], "height": size[1]},
		"seeds":        []int{nextImageSeed()},
		"output":       map[string]any{"storeInputs": true},
		"generationMetadata": map[string]any{
			"module":    "text2image",
			"submodule": "ff-image-generate",
		},
		"modelSpecificPayload": modelSpecific,
		"referenceBlobs":       []any{},
	}
	if !isOriginal {
		base["groundSearch"] = false
	}
	if len(blobIDs) == 0 {
		return []map[string]any{base}
	}
	edited := cloneMap(base)
	edited["referenceBlobs"] = blobRefs(blobIDs, "general")
	return []map[string]any{edited}
}

func buildGPTImagePayloads(spec modelSpec, prompt, ratio, resolution string, blobIDs []string) []map[string]any {
	size := gptImage2Size[ratio]
	if size == [2]int{} {
		size = gptImage2Size["1:1"]
	}
	// Adobe moved GPT Image 2 dimensions from modelSpecificPayload.size to the
	// top-level size object. The configured 1K/2K/4K tiers now select the
	// provider's low/medium/high detail level while the canonical ratio size
	// stays fixed.
	base := map[string]any{
		"modelId":              spec.UpstreamModelID,
		"modelVersion":         spec.UpstreamModelVersion,
		"n":                    1,
		"prompt":               prompt,
		"size":                 map[string]any{"width": size[0], "height": size[1]},
		"seeds":                []int{nextImageSeed()},
		"output":               map[string]any{"storeInputs": true},
		"referenceBlobs":       []any{},
		"generationMetadata":   map[string]any{"module": "text2image", "submodule": "ff-image-generate"},
		"modelSpecificPayload": map[string]any{},
		"generationSettings":   map[string]any{"detailLevel": gptImage2DetailLevel(resolution)},
	}
	if len(blobIDs) == 0 {
		return []map[string]any{base}
	}
	subject := cloneMap(base)
	subject["referenceBlobs"] = blobRefs(blobIDs, "subject")
	return []map[string]any{subject}
}

func gptImage2DetailLevel(resolution string) int {
	switch strings.ToUpper(strings.TrimSpace(resolution)) {
	case "1K":
		return 1
	case "4K":
		return 5
	default:
		return 3
	}
}

func buildFluxPayloads(spec modelSpec, prompt, ratio string, blobIDs []string) []map[string]any {
	size := fluxSize[ratio]
	if size == [2]int{} {
		size = fluxSize["1:1"]
	}
	base := map[string]any{
		"modelId":        spec.UpstreamModelID,
		"modelVersion":   spec.UpstreamModelVersion,
		"n":              1,
		"prompt":         prompt,
		"size":           map[string]any{"width": size[0], "height": size[1]},
		"seeds":          []int{nextImageSeed()},
		"output":         map[string]any{"storeInputs": true},
		"referenceBlobs": []any{},
		"modelSpecificPayload": map[string]any{
			"prompt_upsampling": true,
			"safety_tolerance":  2,
			"aspect_ratio":      ratio,
		},
		"generationMetadata": map[string]any{"module": "text2image", "submodule": "ff-image-generate"},
	}
	if len(blobIDs) == 0 {
		return []map[string]any{base}
	}
	edited := cloneMap(base)
	edited["generationMetadata"] = map[string]any{"module": "image2image", "submodule": "ff-image-generate"}
	edited["referenceBlobs"] = blobRefs(blobIDs, "general")
	return []map[string]any{edited}
}

func buildDefaultPayloads(spec modelSpec, prompt, ratio, resolution string, blobIDs []string) []map[string]any {
	size := getSize(defaultSize, resolution, ratio, "16:9")
	// Shape mirrors a captured working firefly.adobe.com request exactly: top-level
	// size object, modelSpecificPayload only {parameters:{addWatermark:false}},
	// groundSearch:false, module "text2image" (even with a reference blob). NO
	// skipCai and NO modelSpecificPayload.aspectRatio — sending those got 403.
	base := map[string]any{
		"modelId":      spec.UpstreamModelID,
		"modelVersion": spec.UpstreamModelVersion,
		"n":            1,
		"prompt":       prompt,
		"size":         map[string]any{"width": size[0], "height": size[1]},
		"seeds":        []int{nextImageSeed()},
		"groundSearch": false,
		"output":       map[string]any{"storeInputs": true},
		"generationMetadata": map[string]any{
			"module":    "text2image",
			"submodule": "ff-image-generate",
		},
		"modelSpecificPayload": map[string]any{
			"parameters": map[string]any{"addWatermark": false},
		},
	}
	if len(blobIDs) == 0 {
		base["referenceBlobs"] = []any{}
		return []map[string]any{base}
	}
	edited := cloneMap(base)
	edited["referenceBlobs"] = blobRefs(blobIDs, "general")
	return []map[string]any{edited}
}

func getSize(table map[string]map[string][2]int, resolution, ratio, fallbackRatio string) [2]int {
	level := defaultString(resolution, "2K")
	levelTable, ok := table[level]
	if !ok {
		levelTable = table["2K"]
	}
	size, ok := levelTable[ratio]
	if !ok {
		size = levelTable[fallbackRatio]
	}
	return size
}

func sizeString(size [2]int) string {
	return itoa(size[0]) + "x" + itoa(size[1])
}

func blobRefs(ids []string, usage string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "usage": usage})
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SupportsVideoDuration mirrors the duration controls currently exposed by
// Adobe Firefly for each upstream engine. Keep this validation at the provider
// boundary as a second line of defence: a stale/admin-edited model price table
// must not make us submit a duration Adobe will reject.
func SupportsVideoDuration(engine string, durationSeconds int) bool {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "veo31-fast", "veo31-standard", "veo31-lite":
		return durationSeconds == 4 || durationSeconds == 6 || durationSeconds == 8
	case "kling-v3", "kling-o3":
		return durationSeconds >= 3 && durationSeconds <= 15
	case "runway45":
		return durationSeconds == 5 || durationSeconds == 8 || durationSeconds == 10
	case "seedance20", "seedance20-fast":
		return durationSeconds >= 4 && durationSeconds <= 15
	case "luma", "firefly-video":
		// This integration uses Ray 3.14 (modelVersion 3.14-ray), whose current
		// Firefly duration is fixed at 5 seconds. Native Firefly Video is also 5s.
		return durationSeconds == 5
	default:
		// Preserve legacy/custom Adobe engines whose capability is not described
		// here; their configured duration_prices table remains the support gate.
		return durationSeconds > 0
	}
}

// SupportsVideoResolution mirrors Adobe Firefly's Generate video resolution
// controls for the engines integrated here. This deliberately follows Adobe's
// route (which can differ from the same model on another API provider).
func SupportsVideoResolution(engine, resolution string) bool {
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "veo31-fast", "veo31-standard", "veo31-lite", "kling-v3", "kling-o3":
		return resolution == "720p" || resolution == "1080p"
	case "runway45":
		return resolution == "720p"
	case "seedance20", "seedance20-fast":
		return resolution == "480p" || resolution == "720p" || resolution == "1080p"
	case "luma":
		// firefly-ray maps to Ray 3.14 Generate video (not Modify/HDR).
		return resolution == "720p" || resolution == "1080p" || resolution == "4k"
	case "firefly-video":
		return resolution == "540p" || resolution == "720p" || resolution == "1080p"
	default:
		return resolution != ""
	}
}

type VideoInputs struct {
	ImageBlobIDs  []string
	VideoBlobIDs  []string
	AudioBlobIDs  []string
	GenerateAudio bool
}

func BuildVideoPayload(engine, prompt, aspectRatio string, durationSeconds int, resolution, referenceMode, upstreamModel string, in VideoInputs) map[string]any {
	seedVal := int(time.Now().Unix()) % 999999
	engine = defaultString(engine, "sora2")
	resolution = defaultString(resolution, "720p")
	aspectRatio = defaultString(aspectRatio, "16:9")
	if durationSeconds <= 0 {
		durationSeconds = 5
	}

	switch engine {
	case "firefly-video":
		// Firefly-native video model — a distinct schema (mirrors a captured
		// working video-v1.ff.adobe.io request): sizes[] carries width/height +
		// numFrames (numFrames encodes duration, ~25.6fps so 5s = 128), and
		// reference frames go under image.conditions with placement.start
		// (0 = first frame / 首帧, 1 = last frame / 末帧). NO modelId / version /
		// engine / duration / referenceBlobs fields.
		w, h, frames := fireflyVideoSize(aspectRatio, resolution, durationSeconds)
		payload := map[string]any{
			"addOnTransparentBackground": false,
			"prompt":                     prompt,
			"seeds":                      []int{seedVal},
			"sizes":                      []any{map[string]any{"width": w, "height": h, "numFrames": frames}},
			"videoSettings":              map[string]any{},
			"locale":                     "en-US",
			"generationMetadata":         map[string]any{"module": "text2video", "submodule": "ff-video-generate"},
			"output":                     map[string]any{"storeInputs": true},
		}
		if len(in.ImageBlobIDs) > 0 {
			conds := make([]any, 0, 2)
			conds = append(conds, map[string]any{
				"source":    map[string]any{"id": in.ImageBlobIDs[0]},
				"placement": map[string]any{"start": 0},
			})
			if len(in.ImageBlobIDs) > 1 {
				conds = append(conds, map[string]any{
					"source":    map[string]any{"id": in.ImageBlobIDs[1]},
					"placement": map[string]any{"start": 1},
				})
			}
			payload["image"] = map[string]any{"conditions": conds}
		}
		if len(in.VideoBlobIDs) > 0 {
			payload["controlData"] = map[string]any{
				"structureData": map[string]any{"referenceVideo": map[string]any{"source": map[string]any{"id": in.VideoBlobIDs[0]}}},
			}
		}
		return payload
	case "veo31-fast", "veo31-standard", "veo31-lite":
		modelVersion := "3.1-fast-generate"
		if engine == "veo31-standard" {
			modelVersion = "3.1-generate"
		} else if engine == "veo31-lite" {
			modelVersion = "3.1-lite-generate"
		}
		// Shape mirrors a captured working firefly.adobe.com video request: flat
		// top-level duration / negativePrompt / generateAudio, submodule set, and
		// NO `n` / NO modelSpecificPayload (sending those got 403).
		payload := map[string]any{
			"modelId":        "veo",
			"modelVersion":   modelVersion,
			"size":           videoSize(aspectRatio, resolution),
			"seeds":          []int{seedVal},
			"prompt":         prompt,
			"negativePrompt": "",
			"duration":       durationSeconds,
			"generateAudio":  in.GenerateAudio,
			"generationMetadata": map[string]any{
				"module":    "text2video",
				"submodule": "ff-video-generate",
			},
			"output":         map[string]any{"storeInputs": true},
			"referenceBlobs": []any{},
		}
		if len(in.ImageBlobIDs) > 0 {
			payload["generationMetadata"] = map[string]any{"module": "image2video", "submodule": "ff-video-generate"}
			refs := make([]any, 0, min(len(in.ImageBlobIDs), 2))
			for idx, id := range in.ImageBlobIDs[:min(len(in.ImageBlobIDs), 2)] {
				refs = append(refs, map[string]any{"id": id, "usage": "general", "promptReference": idx + 1})
			}
			payload["referenceBlobs"] = refs
		}
		return payload
	case "kling-v3", "kling-o3":
		isPro := strings.EqualFold(resolution, "1080p")
		isImage := len(in.ImageBlobIDs) > 0
		isVideo := len(in.VideoBlobIDs) > 0
		prefix := "kling_v3"
		if engine == "kling-o3" {
			prefix = "kling_o3"
		}
		tier := "standard"
		if isPro {
			tier = "pro"
		}
		workflow := "t2v"
		module := "text2video"
		if isVideo {
			workflow = "v2v"
			module = "video2video"
		} else if isImage {
			workflow = "i2v"
			module = "image2video"
		}
		modelVersion := prefix + "_" + tier + "_" + workflow
		if isVideo {
			if engine == "kling-v3" {
				modelVersion = "kling_v3_omni_v2v_edit"
				if isImage {
					modelVersion = "kling_v3_omni_v2v_create"
				}
			} else if isImage {
				modelVersion = prefix + "_" + tier + "_v2v_reference"
			} else {
				modelVersion = prefix + "_" + tier + "_v2v_edit"
			}
		}
		payload := map[string]any{
			"modelId":            "kling",
			"modelVersion":       modelVersion,
			"size":               videoSize(aspectRatio, resolution),
			"duration":           durationSeconds,
			"seeds":              []int{seedVal},
			"prompt":             prompt,
			"negativePrompt":     "",
			"generateAudio":      in.GenerateAudio,
			"generationSettings": map[string]any{"aspectRatio": aspectRatio},
			"generationMetadata": map[string]any{"module": module, "submodule": "ff-video-generate"},
			"output":             map[string]any{"storeInputs": true},
			"referenceBlobs":     []any{},
		}
		if isVideo {
			refs := []any{map[string]any{"id": in.VideoBlobIDs[0], "usage": "source"}}
			if isImage {
				refs = append(refs, map[string]any{"id": in.ImageBlobIDs[0], "usage": "style"})
			}
			payload["referenceBlobs"] = refs
		} else if isImage {
			payload["referenceBlobs"] = []any{map[string]any{"id": in.ImageBlobIDs[0], "usage": "frame", "order": 1}}
		}
		return payload
	case "runway45":
		payload := map[string]any{
			"modelId":              "runway",
			"modelVersion":         "gen4.5",
			"size":                 videoSize(aspectRatio, "720p"),
			"duration":             durationSeconds,
			"seeds":                []int{seedVal},
			"prompt":               prompt,
			"negativePrompt":       "",
			"generationMetadata":   map[string]any{"module": "text2video", "submodule": "ff-video-generate"},
			"modelSpecificPayload": map[string]any{"duration": durationSeconds},
			"output":               map[string]any{"storeInputs": true},
			"referenceBlobs":       []any{},
		}
		if len(in.ImageBlobIDs) > 0 {
			payload["generationMetadata"] = map[string]any{"module": "image2video", "submodule": "ff-video-generate"}
			payload["referenceBlobs"] = []any{map[string]any{"id": in.ImageBlobIDs[0], "usage": "frame", "order": 1}}
		}
		return payload
	case "seedance20", "seedance20-fast":
		version := "seedance_2.0"
		if engine == "seedance20-fast" {
			version = "seedance_2.0_fast"
		}
		payload := map[string]any{
			"modelId":            "seedance",
			"modelVersion":       version,
			"size":               videoSize(aspectRatio, resolution),
			"duration":           durationSeconds,
			"seeds":              []int{seedVal},
			"prompt":             prompt,
			"negativePrompt":     "",
			"generateAudio":      in.GenerateAudio,
			"generationSettings": map[string]any{"aspectRatio": aspectRatio},
			"generationMetadata": map[string]any{"module": "text2video", "submodule": "ff-video-generate"},
			"output":             map[string]any{"storeInputs": true},
			"referenceBlobs":     []any{},
		}
		if len(in.ImageBlobIDs) > 0 || len(in.VideoBlobIDs) > 0 || len(in.AudioBlobIDs) > 0 {
			if len(in.ImageBlobIDs) > 0 {
				payload["generationMetadata"] = map[string]any{"module": "image2video", "submodule": "ff-video-generate"}
			}
			refs := blobRefs(in.ImageBlobIDs, "style")
			for idx, id := range in.VideoBlobIDs {
				refs = append(refs, map[string]any{"id": id, "usage": "source", "mention": map[string]any{"id": fmt.Sprintf("video%016d", idx+1), "label": fmt.Sprintf("Video %d", idx+1)}})
			}
			for idx, id := range in.AudioBlobIDs {
				refs = append(refs, map[string]any{"id": id, "usage": "source", "mention": map[string]any{"id": fmt.Sprintf("audio%016d", idx+1), "label": fmt.Sprintf("Audio %d", idx+1)}})
			}
			payload["referenceBlobs"] = refs
		}
		return payload
	case "luma":
		payload := map[string]any{
			"modelId":        "luma",
			"modelVersion":   "3.14-ray",
			"size":           lumaVideoSize(aspectRatio, resolution),
			"mode":           "flex_2",
			"prompt":         prompt,
			"negativePrompt": "",
			"duration":       durationSeconds,
			"generationMetadata": map[string]any{
				"module":    "text2video",
				"submodule": "ff-video-generate",
			},
			"modelSpecificPayload": map[string]any{
				"resolution":   strings.ToLower(resolution),
				"aspect_ratio": aspectRatio,
			},
			"output": map[string]any{"storeInputs": true},
		}
		if len(in.VideoBlobIDs) > 0 {
			payload["generationType"] = "modify_video"
			payload["generationMetadata"] = map[string]any{"module": "video2video", "submodule": "ff-video-generate"}
			payload["referenceBlobs"] = []any{map[string]any{"id": in.VideoBlobIDs[0], "usage": "source"}}
			payload["modelSpecificPayload"].(map[string]any)["generation_type"] = "modify_video"
		} else if len(in.ImageBlobIDs) > 0 {
			payload["generationMetadata"] = map[string]any{
				"module":    "image2video",
				"submodule": "ff-video-generate",
			}
			refs := make([]any, 0, min(len(in.ImageBlobIDs), 2))
			for idx, id := range in.ImageBlobIDs[:min(len(in.ImageBlobIDs), 2)] {
				refs = append(refs, map[string]any{"id": id, "usage": "frame", "order": idx + 1})
			}
			payload["referenceBlobs"] = refs
		}
		return payload
	default:
		upstream := defaultString(upstreamModel, "openai:firefly:colligo:sora2")
		payload := map[string]any{
			"n":                          1,
			"seeds":                      []int{seedVal},
			"modelId":                    "sora",
			"modelVersion":               "sora-2",
			"size":                       videoSize(aspectRatio, resolution),
			"duration":                   durationSeconds,
			"fps":                        24,
			"prompt":                     buildVideoPromptJSON(prompt, durationSeconds),
			"generationMetadata":         map[string]any{"module": "text2video"},
			"model":                      upstream,
			"generateAudio":              true,
			"generateLoop":               false,
			"transparentBackground":      false,
			"seed":                       itoa(seedVal),
			"locale":                     "en-US",
			"camera":                     map[string]any{"angle": "none", "shotSize": "none", "motion": nil, "promptStyle": nil},
			"negativePrompt":             "",
			"jobMode":                    "standard",
			"debugGenerationEndpoint":    "",
			"referenceBlobs":             []any{},
			"referenceFrames":            []any{},
			"referenceVideo":             nil,
			"cameraMotionReferenceVideo": nil,
			"characterReference":         nil,
			"editReferenceVideo":         nil,
			"output":                     map[string]any{"storeInputs": true},
		}
		if len(in.ImageBlobIDs) > 0 {
			firstID := in.ImageBlobIDs[0]
			payload["generationMetadata"] = map[string]any{"module": "image2video"}
			payload["referenceBlobs"] = []any{
				map[string]any{"id": firstID, "usage": "general", "promptReference": 1},
			}
			payload["referenceFrames"] = []any{map[string]any{"localBlobRef": firstID}, nil}
		}
		return payload
	}
}

// fireflyVideoSizeTable maps the firefly-video resolution tier + aspect ratio to
// pixel dimensions. Only 1080p 9:16 (1080x1920) is HAR-confirmed; the rest follow
// the standard 540p/720p/1080p grid for each ratio.
var fireflyVideoSizeTable = map[string]map[string][2]int{
	"540p":  {"16:9": {960, 540}, "1:1": {540, 540}, "9:16": {540, 960}},
	"720p":  {"16:9": {1280, 720}, "1:1": {720, 720}, "9:16": {720, 1280}},
	"1080p": {"16:9": {1920, 1080}, "1:1": {1080, 1080}, "9:16": {1080, 1920}},
}

// fireflyVideoSize returns width, height and numFrames. numFrames encodes the
// clip length (~25.6fps; 5s = 128 frames, HAR-confirmed).
func fireflyVideoSize(aspectRatio, resolution string, durationSeconds int) (int, int, int) {
	table, ok := fireflyVideoSizeTable[strings.ToLower(defaultString(resolution, "1080p"))]
	if !ok {
		table = fireflyVideoSizeTable["1080p"]
	}
	wh, ok := table[defaultString(aspectRatio, "9:16")]
	if !ok {
		wh = table["9:16"]
	}
	frames := durationSeconds * 128 / 5
	if frames <= 0 {
		frames = 128
	}
	return wh[0], wh[1], frames
}

func videoSize(aspectRatio, resolution string) map[string]any {
	if strings.EqualFold(resolution, "480p") {
		if aspectRatio == "16:9" {
			return map[string]any{"width": 854, "height": 480}
		}
		return map[string]any{"width": 480, "height": 854}
	}
	if strings.EqualFold(resolution, "1080p") {
		if aspectRatio == "16:9" {
			return map[string]any{"width": 1920, "height": 1080}
		}
		return map[string]any{"width": 1080, "height": 1920}
	}
	if aspectRatio == "16:9" {
		return map[string]any{"width": 1280, "height": 720}
	}
	return map[string]any{"width": 720, "height": 1280}
}

func lumaVideoSize(aspectRatio, resolution string) map[string]any {
	table, ok := lumaSize[strings.ToLower(defaultString(resolution, "720p"))]
	if !ok {
		table = lumaSize["720p"]
	}
	size, ok := table[defaultString(aspectRatio, "16:9")]
	if !ok {
		size = table["16:9"]
	}
	return map[string]any{"width": size[0], "height": size[1]}
}

func buildVideoPromptJSON(prompt string, durationSeconds int) string {
	payload := map[string]any{
		"id":           1,
		"duration_sec": durationSeconds,
		"prompt_text":  prompt,
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
