package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
)

// LogicalModel is the only model identity visible to API consumers. Provider
// names and provider-specific model ids live exclusively on ModelRoute.
type LogicalModel struct {
	ID              string `gorm:"primaryKey;size:191"`
	Kind            string `gorm:"size:32;index;not null"`
	Name            string `gorm:"size:255;not null"`
	Enabled         bool   `gorm:"not null;default:true;index"`
	Weight          int    `gorm:"not null;default:0;index"`
	GenerationCount int64  `gorm:"not null;default:0"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ModelRoute maps one public logical model to one real provider implementation.
// RuntimeModel is the legacy/provider adapter selector. UpstreamModel is the id
// sent over the wire when it differs from RuntimeModel.
type ModelRoute struct {
	ID             string         `gorm:"primaryKey;size:191"`
	LogicalModelID string         `gorm:"size:191;not null;uniqueIndex:ux_route_identity,priority:1;index"`
	Provider       string         `gorm:"size:64;not null;uniqueIndex:ux_route_identity,priority:2;index"`
	RuntimeModel   string         `gorm:"size:255;not null;uniqueIndex:ux_route_identity,priority:3"`
	UpstreamModel  string         `gorm:"size:255;not null;default:''"`
	Enabled        bool           `gorm:"not null;default:true;index"`
	Priority       int            `gorm:"not null;default:0;index"`
	Weight         int            `gorm:"not null;default:1"`
	QuotaBucketKey string         `gorm:"size:128;not null;default:''"`
	QuotaCosts     datatypes.JSON `gorm:"type:jsonb;not null;default:'{}'"`
	Capabilities   datatypes.JSON `gorm:"type:jsonb;not null"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CapabilityProfile is deliberately evaluated as a whole. Matching each field
// against a model-wide union would invent unsupported ratio/resolution/duration
// combinations when multiple routes expose different capabilities.
type CapabilityProfile struct {
	Operations          []string `json:"operations,omitempty"`
	Ratios              []string `json:"ratios,omitempty"`
	Resolutions         []string `json:"resolutions,omitempty"`
	Durations           []string `json:"durations,omitempty"`
	MaxReferenceImages  int      `json:"max_reference_images,omitempty"`
	MaxReferenceVideos  int      `json:"max_reference_videos,omitempty"`
	MaxReferenceAudios  int      `json:"max_reference_audios,omitempty"`
	MaxReferenceMedia   int      `json:"max_reference_media,omitempty"`
	SupportsAudioOutput bool     `json:"supports_audio_output,omitempty"`
	ReferenceMode       string   `json:"reference_mode,omitempty"`
	RequiresReference   bool     `json:"requires_reference,omitempty"`
}

type RouteRequirements struct {
	Operation       string
	Ratio           string
	Resolution      string
	Duration        string
	ReferenceImages int
	ReferenceVideos int
	ReferenceAudios int
	GenerateAudio   bool
}

// QuotaCostPolicy describes how a route translates one normalized request
// into the provider allowance units stored in AccountQuotaBucket. Unknown is
// deliberately distinct from unmetered: both avoid inventing a reservation,
// but unknown providers are still refreshed after every real attempt.
type QuotaCostPolicy struct {
	Mode       string  `json:"mode"`                 // metered|unknown|unmetered
	Unit       string  `json:"unit,omitempty"`       // credits|points|generations
	Calculator string  `json:"calculator,omitempty"` // fixed|per_second|byteplus_image|oreate_seedance
	Value      float64 `json:"value,omitempty"`
	PerSecond  float64 `json:"per_second,omitempty"`
}

// AccountModelRoute scopes provider entitlements to an account+route pair. An
// account can be disabled for one model without removing it from the provider.
type AccountModelRoute struct {
	ID               string     `gorm:"primaryKey;size:191"`
	AccountID        string     `gorm:"size:64;not null;uniqueIndex:ux_account_route,priority:1;index"`
	ModelRouteID     string     `gorm:"size:191;not null;uniqueIndex:ux_account_route,priority:2;index"`
	Enabled          bool       `gorm:"not null;default:true;index"`
	Entitled         bool       `gorm:"not null;default:true"`
	QuotaBucketKey   string     `gorm:"size:128;not null;default:''"`
	ConsecutiveFails int        `gorm:"not null;default:0"`
	SuccessTotal     int64      `gorm:"not null;default:0"`
	CooldownUntil    *time.Time `gorm:"index"`
	LastFailureClass string     `gorm:"size:32"`
	LastFailureAt    *time.Time
	LastSuccessAt    *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// AccountQuotaBucket is the durable source of truth for schedulable upstream
// allowance. Remaining excludes Reserved; both are updated under a row lock.
type AccountQuotaBucket struct {
	ID          string `gorm:"primaryKey;size:191"`
	AccountID   string `gorm:"size:64;not null;uniqueIndex:ux_account_bucket,priority:1;index"`
	BucketKey   string `gorm:"size:128;not null;uniqueIndex:ux_account_bucket,priority:2;index"`
	Unit        string `gorm:"size:32;not null;default:'credits'"`
	Total       *float64
	Remaining   *float64
	Reserved    float64    `gorm:"not null;default:0"`
	ResetAt     *time.Time `gorm:"index"`
	Revision    int64      `gorm:"not null;default:0"`
	RefreshedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type QuotaReservation struct {
	ID                string  `gorm:"primaryKey;size:64"`
	EventID           string  `gorm:"size:64;not null;index"`
	DispatchAttemptID string  `gorm:"size:64;index"`
	QuotaBucketID     string  `gorm:"size:191;not null;index"`
	Amount            float64 `gorm:"not null"`
	Status            string  `gorm:"size:32;not null;index"` // held|settled|released|uncertain
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SettledAt         *time.Time
}

type DispatchAttempt struct {
	ID             string    `gorm:"primaryKey;size:64"`
	EventID        string    `gorm:"size:64;not null;index"`
	ModelRouteID   string    `gorm:"size:191;not null;index"`
	AccountID      string    `gorm:"size:64;index"`
	State          string    `gorm:"size:32;not null;index"` // created|submitting|accepted|unknown|succeeded|failed
	FailureClass   string    `gorm:"size:32;index"`
	UpstreamTaskID string    `gorm:"size:255"`
	Error          string    `gorm:"type:text"`
	StartedAt      time.Time `gorm:"index"`
	FinishedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type CanonicalModelDefinition struct {
	Model  LogicalModel
	Routes []ModelRoute
}

func capabilityJSON(profiles ...CapabilityProfile) datatypes.JSON {
	b, err := json.Marshal(profiles)
	if err != nil {
		panic(err)
	}
	return datatypes.JSON(b)
}

func quotaCostJSON(policy QuotaCostPolicy) datatypes.JSON {
	b, err := json.Marshal(policy)
	if err != nil {
		panic(err)
	}
	return datatypes.JSON(b)
}

func route(id, logical, provider, runtime, upstream string, priority int, profiles ...CapabilityProfile) ModelRoute {
	return ModelRoute{
		ID: id, LogicalModelID: logical, Provider: provider, RuntimeModel: runtime,
		UpstreamModel: upstream, Enabled: true, Priority: priority, Weight: 1,
		QuotaBucketKey: routeQuotaBucket(id, logical, provider), QuotaCosts: routeQuotaCosts(id, provider),
		Capabilities: capabilityJSON(profiles...),
	}
}

func routeQuotaCosts(routeID, provider string) datatypes.JSON {
	policy := QuotaCostPolicy{Mode: "unknown", Unit: "credits"}
	switch {
	case provider == "custom":
		policy = QuotaCostPolicy{Mode: "unmetered"}
	case provider == "byteplus" && strings.HasPrefix(routeID, "image."):
		policy = QuotaCostPolicy{Mode: "metered", Unit: "points", Calculator: "byteplus_image"}
	case provider == "oreate" && strings.HasPrefix(routeID, "video."):
		policy = QuotaCostPolicy{Mode: "metered", Unit: "points", Calculator: "oreate_seedance"}
	case provider == "runway" && strings.HasPrefix(routeID, "video."):
		policy = QuotaCostPolicy{Mode: "metered", Unit: "credits", Calculator: "per_second", PerSecond: 5}
	case provider == "chatgpt" && strings.HasPrefix(routeID, "image."):
		policy = QuotaCostPolicy{Mode: "metered", Unit: "generations", Calculator: "fixed", Value: 1}
	}
	return quotaCostJSON(policy)
}

func routeQuotaBucket(routeID, logicalID, provider string) string {
	switch provider {
	case "byteplus":
		return "byteplus.computing_points"
	case "chatgpt":
		if strings.HasPrefix(routeID, "image.") {
			return "chatgpt.image"
		}
		return "chatgpt.text"
	case "adobe":
		// Adobe exposes one account-level Firefly allowance. Image and video
		// routes must share it or the same upstream credits become spendable
		// twice in the scheduler.
		return "adobe.credits"
	case "runway":
		return "runway.credits"
	case "grok":
		if strings.HasPrefix(routeID, "text.") {
			return "grok.text"
		}
		return "grok.media"
	case "oreate":
		return "oreate.points"
	case "custom":
		return "custom." + logicalID
	default:
		return provider + "." + logicalID
	}
}

func textProfile() CapabilityProfile {
	return CapabilityProfile{Operations: []string{"completion"}}
}

func imageProfile(ratios, resolutions []string, maxRefs int) CapabilityProfile {
	ops := []string{"generation"}
	if maxRefs > 0 {
		ops = append(ops, "edit")
	}
	return CapabilityProfile{Operations: ops, Ratios: ratios, Resolutions: resolutions, MaxReferenceImages: maxRefs, ReferenceMode: "asset"}
}

func videoProfile(ratios, resolutions, durations []string, maxImages, maxVideos, maxAudios, maxMedia int, audio bool, mode string) CapabilityProfile {
	return CapabilityProfile{Operations: []string{"generation"}, Ratios: ratios, Resolutions: resolutions, Durations: durations,
		MaxReferenceImages: maxImages, MaxReferenceVideos: maxVideos, MaxReferenceAudios: maxAudios,
		MaxReferenceMedia: maxMedia, SupportsAudioOutput: audio, ReferenceMode: mode}
}

func requiredVideoProfile(ratios, resolutions, durations []string, maxImages int) CapabilityProfile {
	profile := videoProfile(ratios, resolutions, durations, maxImages, 0, 0, 0, false, "frame")
	profile.RequiresReference = true
	return profile
}

func secondsRange(first, last int) []string {
	out := make([]string, 0, last-first+1)
	for i := first; i <= last; i++ {
		out = append(out, strconv.Itoa(i)+"s")
	}
	return out
}

// CanonicalRoutingCatalog is the closed public model set. Provider-specific
// ids below are internal adapter selectors and must never be returned by /v1.
func CanonicalRoutingCatalog() []CanonicalModelDefinition {
	commonImageRatios := []string{"1:1", "16:9", "9:16", "4:3", "3:4"}
	adobeWideRatios := []string{"1:1", "5:4", "9:16", "21:9", "16:9", "4:3", "3:2", "4:5", "3:4", "2:3"}
	runwayWideRatios := []string{"1:1", "1:4", "1:8", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "8:1", "9:16", "16:9", "21:9"}
	seedanceRatios := []string{"16:9", "1:1", "3:4", "4:3", "9:16", "21:9"}
	return []CanonicalModelDefinition{
		{Model: LogicalModel{ID: "gpt-5-5-mini", Kind: "text", Name: "GPT-5.5 Mini", Enabled: true}, Routes: []ModelRoute{
			route("text.gpt-5-5-mini.chatgpt", "gpt-5-5-mini", "chatgpt", "gpt-5-5-mini", "gpt-5-5-mini", 100, textProfile()),
		}},
		{Model: LogicalModel{ID: "gpt-5-5-thinking", Kind: "text", Name: "GPT-5.5 Thinking", Enabled: true}, Routes: []ModelRoute{
			route("text.gpt-5-5-thinking.chatgpt", "gpt-5-5-thinking", "chatgpt", "gpt-5-5-thinking", "gpt-5-5-thinking", 100, textProfile()),
		}},
		{Model: LogicalModel{ID: "grok-4.5", Kind: "text", Name: "Grok 4.5", Enabled: true}, Routes: []ModelRoute{
			route("text.grok-4.5.grok", "grok-4.5", "grok", "grok-4.5", "grok-4.5", 100, textProfile()),
		}},
		{Model: LogicalModel{ID: "grok-chat-fast", Kind: "text", Name: "Grok Chat Fast", Enabled: true}, Routes: []ModelRoute{
			route("text.grok-chat-fast.grok", "grok-chat-fast", "grok", "grok-chat-fast", "grok-chat-fast", 100, textProfile()),
		}},

		{Model: LogicalModel{ID: "gpt-image-2", Kind: "image", Name: "GPT Image 2", Enabled: true}, Routes: []ModelRoute{
			route("image.gpt-image-2.chatgpt", "gpt-image-2", "chatgpt", "gpt-image-2", "gpt-image-2", 100, imageProfile(commonImageRatios, []string{"1K"}, 6)),
			route("image.gpt-image-2.byteplus", "gpt-image-2", "byteplus", "lumina-gpt-image-2", "6824519374061285743", 90, imageProfile(commonImageRatios, []string{"1K", "2K", "4K"}, 14)),
			route("image.gpt-image-2.adobe", "gpt-image-2", "adobe", "firefly-gpt-image-2", "", 80, imageProfile(adobeWideRatios, []string{"1K", "2K", "4K"}, 6)),
		}},
		{Model: LogicalModel{ID: "seedream-5.0-pro", Kind: "image", Name: "Seedream 5.0 Pro", Enabled: true}, Routes: []ModelRoute{
			route("image.seedream-5.0-pro.byteplus", "seedream-5.0-pro", "byteplus", "lumina-seedream-5.0-pro", "7657401949175693322", 100, imageProfile(commonImageRatios, []string{"1K", "2K"}, 10)),
		}},
		{Model: LogicalModel{ID: "seedream-5.0-lite", Kind: "image", Name: "Seedream 5.0 Lite", Enabled: true}, Routes: []ModelRoute{
			route("image.seedream-5.0-lite.byteplus", "seedream-5.0-lite", "byteplus", "lumina-seedream-5.0-lite", "7604761017696141358", 100, imageProfile(commonImageRatios, []string{"2K"}, 10)),
		}},
		{Model: LogicalModel{ID: "nano-banana-2", Kind: "image", Name: "Nano Banana 2", Enabled: true}, Routes: []ModelRoute{
			route("image.nano-banana-2.byteplus", "nano-banana-2", "byteplus", "lumina-nano-banana-2", "8162745039814627354", 100, imageProfile(commonImageRatios, []string{"1K", "2K", "4K"}, 14)),
			route("image.nano-banana-2.runway", "nano-banana-2", "runway", "nano-banana-2", "nano-banana-2", 90, imageProfile(runwayWideRatios, []string{"1K", "2K", "4K"}, 6)),
			route("image.nano-banana-2.adobe", "nano-banana-2", "adobe", "firefly-nano-banana-2", "", 80, imageProfile(adobeWideRatios, []string{"1K", "2K", "4K"}, 6)),
		}},
		{Model: LogicalModel{ID: "nano-banana-pro", Kind: "image", Name: "Nano Banana Pro", Enabled: true}, Routes: []ModelRoute{
			route("image.nano-banana-pro.byteplus", "nano-banana-pro", "byteplus", "lumina-nano-banana-pro", "8162745039814627353", 100, imageProfile(commonImageRatios, []string{"1K", "2K", "4K"}, 14)),
			route("image.nano-banana-pro.runway", "nano-banana-pro", "runway", "nano-banana-pro", "nano-banana-pro", 90, imageProfile(runwayWideRatios, []string{"1K", "2K", "4K"}, 6)),
			route("image.nano-banana-pro.adobe", "nano-banana-pro", "adobe", "firefly-nano-banana-pro", "", 80, imageProfile(adobeWideRatios, []string{"1K", "2K", "4K"}, 6)),
		}},
		{Model: LogicalModel{ID: "grok-imagine-image", Kind: "image", Name: "Grok Imagine Image", Enabled: true}, Routes: []ModelRoute{
			route("image.grok-imagine-image.grok", "grok-imagine-image", "grok", "grok-imagine-image", "grok-imagine-image", 100, imageProfile([]string{"2:3", "3:2", "1:1", "9:16", "16:9"}, []string{"1K"}, 0)),
		}},

		{Model: LogicalModel{ID: "veo-3.1", Kind: "video", Name: "Veo 3.1", Enabled: true}, Routes: []ModelRoute{
			route("video.veo-3.1.adobe", "veo-3.1", "adobe", "gemini-veo31", "", 100, videoProfile([]string{"16:9", "9:16"}, []string{"720p", "1080p"}, []string{"4s", "6s", "8s"}, 2, 0, 0, 0, true, "frame")),
		}},
		{Model: LogicalModel{ID: "veo-3.1-lite", Kind: "video", Name: "Veo 3.1 Lite", Enabled: true}, Routes: []ModelRoute{
			route("video.veo-3.1-lite.adobe", "veo-3.1-lite", "adobe", "gemini-veo31-lite", "", 100, videoProfile([]string{"16:9", "9:16"}, []string{"720p", "1080p"}, []string{"4s", "6s", "8s"}, 2, 0, 0, 0, false, "frame")),
		}},
		{Model: LogicalModel{ID: "kling-3", Kind: "video", Name: "Kling 3", Enabled: true}, Routes: []ModelRoute{
			route("video.kling-3.adobe", "kling-3", "adobe", "firefly-kling-3", "", 100, videoProfile([]string{"16:9", "9:16"}, []string{"720p", "1080p"}, secondsRange(3, 15), 1, 1, 0, 0, true, "frame")),
		}},
		{Model: LogicalModel{ID: "kling-o3", Kind: "video", Name: "Kling O3", Enabled: true}, Routes: []ModelRoute{
			route("video.kling-o3.adobe", "kling-o3", "adobe", "firefly-kling-o3", "", 100, videoProfile([]string{"16:9", "9:16"}, []string{"720p", "1080p"}, secondsRange(3, 15), 1, 1, 0, 0, true, "frame")),
		}},
		{Model: LogicalModel{ID: "runway-gen-4.5", Kind: "video", Name: "Runway Gen-4.5", Enabled: true}, Routes: []ModelRoute{
			route("video.runway-gen-4.5.adobe", "runway-gen-4.5", "adobe", "firefly-runway-4.5", "", 100, videoProfile([]string{"16:9"}, []string{"720p"}, []string{"5s", "8s", "10s"}, 1, 0, 0, 0, false, "frame")),
		}},
		{Model: LogicalModel{ID: "runway-gen-4-turbo", Kind: "video", Name: "Runway Gen-4 Turbo", Enabled: true}, Routes: []ModelRoute{
			route("video.runway-gen-4-turbo.runway", "runway-gen-4-turbo", "runway", "runway-gen4-turbo", "gen4_turbo", 100, requiredVideoProfile([]string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9"}, []string{"720p"}, []string{"5s", "10s"}, 1)),
		}},
		{Model: LogicalModel{ID: "seedance-2.0", Kind: "video", Name: "Seedance 2.0", Enabled: true}, Routes: []ModelRoute{
			route("video.seedance-2.0.adobe", "seedance-2.0", "adobe", "firefly-seedance-2", "", 100, videoProfile([]string{"16:9", "9:16"}, []string{"480p", "720p", "1080p"}, secondsRange(4, 15), 9, 3, 3, 9, true, "asset")),
			route("video.seedance-2.0.oreate", "seedance-2.0", "oreate", "oreate-seedance-2.0", "seedance-2.0", 90, videoProfile(seedanceRatios, []string{"480p", "720p", "1080p"}, []string{"5s", "10s"}, 9, 3, 0, 12, true, "asset")),
		}},
		{Model: LogicalModel{ID: "seedance-2.0-fast", Kind: "video", Name: "Seedance 2.0 Fast", Enabled: true}, Routes: []ModelRoute{
			route("video.seedance-2.0-fast.adobe", "seedance-2.0-fast", "adobe", "firefly-seedance-2-fast", "", 100, videoProfile([]string{"16:9", "9:16"}, []string{"480p", "720p", "1080p"}, secondsRange(4, 15), 9, 3, 3, 9, true, "asset")),
			route("video.seedance-2.0-fast.oreate", "seedance-2.0-fast", "oreate", "oreate-seedance-2.0-fast", "seedance-2.0-fast", 90, videoProfile(seedanceRatios, []string{"480p", "720p"}, []string{"5s", "10s"}, 9, 3, 0, 12, true, "asset")),
		}},
		{Model: LogicalModel{ID: "seedance-2.0-mini", Kind: "video", Name: "Seedance 2.0 Mini", Enabled: true}, Routes: []ModelRoute{
			route("video.seedance-2.0-mini.oreate", "seedance-2.0-mini", "oreate", "oreate-seedance-2.0-mini", "seedance-2.0-mini", 100, videoProfile(seedanceRatios, []string{"480p", "720p"}, []string{"5s", "10s"}, 9, 3, 0, 12, true, "asset")),
		}},
		{Model: LogicalModel{ID: "seedance-1.5-pro", Kind: "video", Name: "Seedance 1.5 Pro", Enabled: true}, Routes: []ModelRoute{
			route("video.seedance-1.5-pro.oreate", "seedance-1.5-pro", "oreate", "oreate-seedance-1.5-pro", "seedance-1.5-pro", 100, videoProfile(seedanceRatios, []string{"480p", "720p", "1080p"}, []string{"5s", "10s"}, 2, 0, 0, 2, true, "frame")),
		}},
		{Model: LogicalModel{ID: "seedance-2.5", Kind: "video", Name: "Seedance 2.5", Enabled: true}, Routes: []ModelRoute{
			route("video.seedance-2.5.oreate", "seedance-2.5", "oreate", "oreate-seedance-2.5", "seedance-2.5", 100, videoProfile(seedanceRatios, []string{"480p", "720p"}, []string{"5s", "10s", "20s", "30s"}, 9, 3, 0, 12, true, "asset")),
		}},
		{Model: LogicalModel{ID: "grok-imagine-video", Kind: "video", Name: "Grok Imagine Video", Enabled: true}, Routes: []ModelRoute{
			route("video.grok-imagine-video.grok", "grok-imagine-video", "grok", "grok-video", "grok-imagine-video", 100, videoProfile([]string{"2:3", "3:2", "1:1", "9:16", "16:9"}, []string{"720p"}, []string{"6s", "10s"}, 6, 0, 0, 0, false, "asset")),
		}},
		{Model: LogicalModel{ID: "luma-ray", Kind: "video", Name: "Luma Ray", Enabled: true}, Routes: []ModelRoute{
			route("video.luma-ray.adobe", "luma-ray", "adobe", "firefly-ray", "", 100, videoProfile([]string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16", "9:21"}, []string{"720p", "1080p", "4K"}, []string{"5s"}, 2, 1, 0, 0, false, "frame")),
		}},
		{Model: LogicalModel{ID: "firefly-video", Kind: "video", Name: "Firefly Video", Enabled: true}, Routes: []ModelRoute{
			route("video.firefly-video.adobe", "firefly-video", "adobe", "firefly-video", "", 100, videoProfile([]string{"16:9", "1:1", "9:16"}, []string{"540p", "720p", "1080p"}, []string{"5s"}, 2, 1, 0, 0, false, "frame")),
		}},
	}
}

func IsCanonicalModelID(id string) bool {
	id = strings.TrimSpace(id)
	for _, definition := range CanonicalRoutingCatalog() {
		if definition.Model.ID == id {
			return true
		}
	}
	return false
}

func IsCanonicalRoute(route ModelRoute) bool {
	if route.Provider == "custom" {
		return IsCanonicalModelID(route.LogicalModelID) && route.ID == "custom."+route.LogicalModelID
	}
	for _, definition := range CanonicalRoutingCatalog() {
		for _, candidate := range definition.Routes {
			if candidate.ID == route.ID && candidate.LogicalModelID == route.LogicalModelID && candidate.Provider == route.Provider {
				return true
			}
		}
	}
	return false
}

func DecodeCapabilityProfiles(raw datatypes.JSON) []CapabilityProfile {
	var profiles []CapabilityProfile
	if len(raw) == 0 || json.Unmarshal(raw, &profiles) != nil {
		return nil
	}
	return profiles
}

func DecodeQuotaCostPolicy(raw datatypes.JSON) (QuotaCostPolicy, bool) {
	var policy QuotaCostPolicy
	if len(raw) == 0 || json.Unmarshal(raw, &policy) != nil {
		return policy, false
	}
	policy.Mode = strings.ToLower(strings.TrimSpace(policy.Mode))
	policy.Calculator = strings.ToLower(strings.TrimSpace(policy.Calculator))
	return policy, policy.Mode == "metered" || policy.Mode == "unknown" || policy.Mode == "unmetered"
}

func (r ModelRoute) Supports(req RouteRequirements) bool {
	for _, profile := range DecodeCapabilityProfiles(r.Capabilities) {
		if capabilitySupports(profile, req) {
			return true
		}
	}
	return false
}

func capabilitySupports(profile CapabilityProfile, req RouteRequirements) bool {
	contains := func(values []string, wanted string) bool {
		if strings.TrimSpace(wanted) == "" {
			return true
		}
		for _, value := range values {
			if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(wanted)) {
				return true
			}
		}
		return false
	}
	if !contains(profile.Operations, req.Operation) || !contains(profile.Ratios, req.Ratio) ||
		!contains(profile.Resolutions, req.Resolution) || !contains(profile.Durations, req.Duration) {
		return false
	}
	if req.ReferenceImages > profile.MaxReferenceImages || req.ReferenceVideos > profile.MaxReferenceVideos || req.ReferenceAudios > profile.MaxReferenceAudios {
		return false
	}
	if profile.MaxReferenceMedia > 0 && req.ReferenceImages+req.ReferenceVideos+req.ReferenceAudios > profile.MaxReferenceMedia {
		return false
	}
	if profile.RequiresReference && req.ReferenceImages+req.ReferenceVideos+req.ReferenceAudios == 0 {
		return false
	}
	return !req.GenerateAudio || profile.SupportsAudioOutput
}
