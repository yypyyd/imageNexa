package service

import (
	"context"
	"errors"
	"testing"

	"backend/internal/model"
)

type fakeSettingReader struct {
	values map[string]string
	err    error
}

func (f fakeSettingReader) GetValue(_ context.Context, key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.values[key], nil
}

func TestParseProviderSwitchDefaultsToEnabled(t *testing.T) {
	for _, value := range []string{"", "true", "1", "on", "yes", "garbage"} {
		if !parseProviderSwitch(value) {
			t.Fatalf("parseProviderSwitch(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"false", "0", "off", "no", "disabled", " FALSE "} {
		if parseProviderSwitch(value) {
			t.Fatalf("parseProviderSwitch(%q) = true, want false", value)
		}
	}
}

func TestProviderEnabledReadsSettingKey(t *testing.T) {
	ctx := context.Background()
	settings := fakeSettingReader{values: map[string]string{
		providerSettingKey("adobe"): "false",
	}}
	if enabled, err := providerEnabled(ctx, settings, "adobe"); err != nil || enabled {
		t.Fatalf("providerEnabled(adobe) = (%v, %v), want (false, nil)", enabled, err)
	}
	if enabled, err := providerEnabled(ctx, settings, "chatgpt"); err != nil || !enabled {
		t.Fatalf("providerEnabled(chatgpt) = (%v, %v), want (true, nil) when unset", enabled, err)
	}
	// Unknown pools and a nil reader never block scheduling.
	if enabled, err := providerEnabled(ctx, settings, "not-a-pool"); err != nil || !enabled {
		t.Fatalf("providerEnabled(unknown) = (%v, %v), want (true, nil)", enabled, err)
	}
	if enabled, err := providerEnabled(ctx, nil, "adobe"); err != nil || !enabled {
		t.Fatalf("providerEnabled(nil reader) = (%v, %v), want (true, nil)", enabled, err)
	}
}

func TestProviderEnabledSurfacesStoreErrors(t *testing.T) {
	boom := errors.New("redis down")
	_, err := providerEnabled(context.Background(), fakeSettingReader{err: boom}, "adobe")
	if !errors.Is(err, boom) {
		t.Fatalf("providerEnabled() error = %v, want %v", err, boom)
	}
}

func TestFilterEnabledProviderRoutesDropsDisabledPoolsInOrder(t *testing.T) {
	routes := []model.ModelRoute{
		{ID: "image.gpt-image-2.chatgpt", Provider: "chatgpt", Priority: 100},
		{ID: "image.gpt-image-2.byteplus", Provider: "byteplus", Priority: 90},
		{ID: "image.gpt-image-2.adobe", Provider: "adobe", Priority: 80},
	}
	settings := fakeSettingReader{values: map[string]string{
		providerSettingKey("adobe"): "false",
	}}
	got, err := filterEnabledProviderRoutes(context.Background(), settings, routes)
	if err != nil {
		t.Fatalf("filterEnabledProviderRoutes() error = %v", err)
	}
	if len(got) != 2 || got[0].Provider != "chatgpt" || got[1].Provider != "byteplus" {
		t.Fatalf("filterEnabledProviderRoutes() = %+v, want chatgpt then byteplus", got)
	}
}

func TestFilterEnabledProviderRoutesKeepsEverythingWhenUnset(t *testing.T) {
	routes := []model.ModelRoute{
		{ID: "a", Provider: "chatgpt"},
		{ID: "b", Provider: "adobe"},
	}
	got, err := filterEnabledProviderRoutes(context.Background(), fakeSettingReader{}, routes)
	if err != nil || len(got) != 2 {
		t.Fatalf("filterEnabledProviderRoutes() = (%d routes, %v), want all 2 kept", len(got), err)
	}
	got, err = filterEnabledProviderRoutes(context.Background(), nil, routes)
	if err != nil || len(got) != 2 {
		t.Fatalf("filterEnabledProviderRoutes(nil) = (%d routes, %v), want all 2 kept", len(got), err)
	}
}

func TestSchedulableProvidersIsStableAndCoversPools(t *testing.T) {
	first := SchedulableProviders()
	second := SchedulableProviders()
	if len(first) != len(validTokenPools) {
		t.Fatalf("SchedulableProviders() has %d entries, want %d", len(first), len(validTokenPools))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("SchedulableProviders() order unstable at %d: %q vs %q", i, first[i], second[i])
		}
		if i > 0 && first[i-1] >= first[i] {
			t.Fatalf("SchedulableProviders() not sorted: %q before %q", first[i-1], first[i])
		}
	}
}
