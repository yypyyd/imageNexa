package service

import "strings"

// providerSnapshotScope names the only bucket a provider-wide quota probe can
// authoritatively update. ChatGPT's endpoint is image_gen-only and Grok's is
// media-only; neither may poison text routing with a zero media balance.
func providerSnapshotScope(pool string) (bucketKey, unit string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(pool)) {
	case "chatgpt":
		return "chatgpt.image", "generations", true
	case "byteplus":
		return "byteplus.computing_points", "points", true
	case "adobe":
		return "adobe.credits", "credits", true
	case "runway":
		return "runway.credits", "credits", true
	case "grok":
		return "grok.media", "credits", true
	case "oreate":
		return "oreate.points", "points", true
	default:
		return "", "", false
	}
}
