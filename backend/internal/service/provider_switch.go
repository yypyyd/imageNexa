package service

import (
	"context"
	"sort"
	"strings"

	"backend/internal/model"
)

// providerSettingKey is the site-setting key that stores whether a provider
// pool may be scheduled. An absent or unparseable value means enabled so that
// existing deployments keep every pool live until an administrator turns one
// off explicitly.
func providerSettingKey(pool string) string {
	return "provider." + pool + ".enabled"
}

// deferredAccountProviders stay in the token pool map so leftover adapter code
// can compile, but they are not console-visible, importable, or schedulable.
var deferredAccountProviders = map[string]bool{
	"runway": true,
	"custom": true,
	"oreate": true,
}

func isDeferredAccountProvider(pool string) bool {
	return deferredAccountProviders[strings.ToLower(strings.TrimSpace(pool))]
}

// SchedulableProviders lists every provider pool an administrator may toggle.
// The order is stable so the settings API and the console render consistently.
func SchedulableProviders() []string {
	out := make([]string, 0, len(validTokenPools))
	for pool := range validTokenPools {
		if deferredAccountProviders[pool] {
			continue
		}
		out = append(out, pool)
	}
	sort.Strings(out)
	return out
}

type settingValueReader interface {
	GetValue(ctx context.Context, key string) (string, error)
}

// providerEnabled reads the administrator switch for one pool. A settings-store
// error is reported so callers can fail closed on their own terms instead of
// silently treating a pool as enabled or disabled.
func providerEnabled(ctx context.Context, settings settingValueReader, pool string) (bool, error) {
	pool = normalizePool(pool)
	if pool == "" || settings == nil {
		return true, nil
	}
	value, err := settings.GetValue(ctx, providerSettingKey(pool))
	if err != nil {
		return false, err
	}
	return parseProviderSwitch(value), nil
}

// parseProviderSwitch treats only an explicit false-like value as disabled.
func parseProviderSwitch(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}

// filterEnabledProviderRoutes drops routes whose provider pool an administrator
// has switched off. Routes are preserved in their incoming priority order.
func filterEnabledProviderRoutes(ctx context.Context, settings settingValueReader, routes []model.ModelRoute) ([]model.ModelRoute, error) {
	if settings == nil || len(routes) == 0 {
		return routes, nil
	}
	decisions := map[string]bool{}
	out := make([]model.ModelRoute, 0, len(routes))
	for _, route := range routes {
		if isDeferredAccountProvider(route.Provider) {
			continue
		}
		enabled, seen := decisions[route.Provider]
		if !seen {
			var err error
			enabled, err = providerEnabled(ctx, settings, route.Provider)
			if err != nil {
				return nil, err
			}
			decisions[route.Provider] = enabled
		}
		if enabled {
			out = append(out, route)
		}
	}
	return out, nil
}
