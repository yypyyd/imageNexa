package service

import (
	"context"
	"strings"

	"backend/internal/provider/adobe"
	"backend/internal/provider/byteplus"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/dola"
	"backend/internal/provider/grok"
	"backend/internal/provider/oreate"
	"backend/internal/provider/proxypool"
)

const (
	settingProxyURL        = "proxy.url"
	settingExtractAPI      = "proxy.extract_api"
	settingDolaSessionAPI  = "proxy.dola.session_api"
	settingProviderProxyNS = "proxy."
)

// extractManager is the process-wide residential lease table. Configure is
// idempotent: unchanged extract APIs keep in-memory assignments.
var extractManager = proxypool.NewManager()

// providerProxySnapshot is one read of the administrator residential routes.
type providerProxySnapshot struct {
	Default           string
	ByProvider        map[string]string
	Stored            map[string]string
	DefaultExtract    string
	ExtractByProvider map[string]string
	ExtractStored     map[string]string
	DolaSessionAPI    string
}

func providerProxySettingKey(pool string) string {
	return settingProviderProxyNS + strings.ToLower(strings.TrimSpace(pool)) + ".url"
}

func providerExtractSettingKey(pool string) string {
	return settingProviderProxyNS + strings.ToLower(strings.TrimSpace(pool)) + ".extract_api"
}

func proxyAwareProviders() []string {
	return []string{"chatgpt", "grok", "dola", "oreate", "adobe", "byteplus"}
}

// inheritDefaultProxy reports whether an empty per-channel URL should fall back
// to proxy.url. Adobe and BytePlus stay direct unless their own URL is set, so a
// Dola JP/KR pool is not silently attached to Firefly or Lumina.
func inheritDefaultProxy(pool string) bool {
	switch strings.ToLower(strings.TrimSpace(pool)) {
	case "chatgpt", "grok", "dola", "oreate":
		return true
	default:
		return false
	}
}

func inheritDefaultExtract(pool string) bool {
	return inheritDefaultProxy(pool)
}

func resolveProviderProxy(pool, stored, fallback string) string {
	stored = strings.TrimSpace(stored)
	if stored != "" {
		return stored
	}
	if inheritDefaultProxy(pool) {
		return strings.TrimSpace(fallback)
	}
	return ""
}

func resolveProviderExtract(pool, stored, fallback string) string {
	stored = strings.TrimSpace(stored)
	if stored != "" {
		return stored
	}
	if inheritDefaultExtract(pool) {
		return strings.TrimSpace(fallback)
	}
	return ""
}

func loadProviderProxies(ctx context.Context, settings settingValueReader) (providerProxySnapshot, error) {
	snap := providerProxySnapshot{
		ByProvider:        map[string]string{},
		Stored:            map[string]string{},
		ExtractByProvider: map[string]string{},
		ExtractStored:     map[string]string{},
	}
	if settings == nil {
		return snap, nil
	}
	fallback, err := settings.GetValue(ctx, settingProxyURL)
	if err != nil {
		return snap, err
	}
	snap.Default = strings.TrimSpace(fallback)
	defaultExtract, err := settings.GetValue(ctx, settingExtractAPI)
	if err != nil {
		return snap, err
	}
	snap.DefaultExtract = strings.TrimSpace(defaultExtract)
	sessionAPI, err := settings.GetValue(ctx, settingDolaSessionAPI)
	if err != nil {
		return snap, err
	}
	snap.DolaSessionAPI = strings.TrimSpace(sessionAPI)
	for _, pool := range proxyAwareProviders() {
		raw, getErr := settings.GetValue(ctx, providerProxySettingKey(pool))
		if getErr != nil {
			return snap, getErr
		}
		raw = strings.TrimSpace(raw)
		snap.Stored[pool] = raw
		snap.ByProvider[pool] = resolveProviderProxy(pool, raw, snap.Default)
		extract, getErr := settings.GetValue(ctx, providerExtractSettingKey(pool))
		if getErr != nil {
			return snap, getErr
		}
		extract = strings.TrimSpace(extract)
		if pool == "dola" && extract == "" {
			extract = snap.DolaSessionAPI
		}
		snap.ExtractStored[pool] = extract
		snap.ExtractByProvider[pool] = resolveProviderExtract(pool, extract, snap.DefaultExtract)
	}
	if snap.ExtractStored["dola"] == "" {
		snap.ExtractStored["dola"] = snap.DolaSessionAPI
	}
	if snap.DolaSessionAPI == "" {
		snap.DolaSessionAPI = snap.ExtractStored["dola"]
	}
	return snap, nil
}

func assignProviderProxies(snap providerProxySnapshot, chatgptClient *chatgpt.Client, grokClient *grok.Client, oreateClient *oreate.Client, dolaClient *dola.Client, adobeClient *adobe.Client, byteplusClient *byteplus.Client) {
	for _, pool := range proxyAwareProviders() {
		extractManager.Configure(pool, snap.ExtractByProvider[pool])
	}
	if chatgptClient != nil {
		chatgptClient.SetProxy(snap.ByProvider["chatgpt"])
		chatgptClient.SetAssigner(extractManager.Bind("chatgpt"))
	}
	if grokClient != nil {
		grokClient.SetProxy(snap.ByProvider["grok"])
		grokClient.SetAssigner(extractManager.Bind("grok"))
	}
	if oreateClient != nil {
		oreateClient.SetProxy(snap.ByProvider["oreate"])
	}
	if dolaClient != nil {
		dolaClient.SetProxy(snap.ByProvider["dola"])
		dolaClient.SetAssigner(extractManager.Bind("dola"))
	}
	if adobeClient != nil {
		adobeClient.SetProxy(snap.ByProvider["adobe"])
		adobeClient.SetAssigner(extractManager.Bind("adobe"))
	}
	if byteplusClient != nil {
		byteplusClient.SetProxy(snap.ByProvider["byteplus"])
		byteplusClient.SetAssigner(extractManager.Bind("byteplus"))
	}
}
