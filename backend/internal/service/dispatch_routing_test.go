package service

import (
	"errors"
	"math"
	"testing"

	"backend/internal/model"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/custom"
	"backend/internal/provider/grok"
)

func canonicalRouteForTest(t *testing.T, routeID string) model.ModelRoute {
	t.Helper()
	for _, definition := range model.CanonicalRoutingCatalog() {
		for _, route := range definition.Routes {
			if route.ID == routeID {
				return route
			}
		}
	}
	t.Fatalf("canonical route %q not found", routeID)
	return model.ModelRoute{}
}

func TestEvaluateRouteCostUsesRequestSpecificProviderRules(t *testing.T) {
	tests := []struct {
		name    string
		routeID string
		req     model.RouteRequirements
		want    float64
		known   bool
	}{
		{name: "chatgpt generation", routeID: "image.gpt-image-2.chatgpt", req: model.RouteRequirements{Operation: "generation", Resolution: "1K"}, want: 1, known: true},
		{name: "byteplus gpt low", routeID: "image.gpt-image-2.byteplus", req: model.RouteRequirements{Operation: "generation", Ratio: "16:9", Resolution: "1K"}, want: 1, known: true},
		{name: "byteplus gpt medium", routeID: "image.gpt-image-2.byteplus", req: model.RouteRequirements{Operation: "generation", Ratio: "16:9", Resolution: "2K", Quality: "medium"}, want: 25, known: true},
		{name: "byteplus gpt high", routeID: "image.gpt-image-2.byteplus", req: model.RouteRequirements{Operation: "generation", Ratio: "16:9", Resolution: "4K", Quality: "high"}, want: 230, known: true},
		{name: "byteplus gpt omitted quality defaults low", routeID: "image.gpt-image-2.byteplus", req: model.RouteRequirements{Operation: "generation", Ratio: "1:1", Resolution: "4K"}, want: 3, known: true},
		{name: "byteplus gpt low 4k square compatibility canvas", routeID: "image.gpt-image-2.byteplus", req: model.RouteRequirements{Operation: "generation", Ratio: "1:1", Resolution: "4K", Quality: "low"}, want: 3, known: true},
		{name: "seedream pro wide with refs", routeID: "image.seedream-5.0-pro.byteplus", req: model.RouteRequirements{Operation: "edit", Ratio: "16:9", Resolution: "2K", ReferenceImages: 3}, want: 9.6, known: true},
		{name: "seedream pro square with refs", routeID: "image.seedream-5.0-pro.byteplus", req: model.RouteRequirements{Operation: "edit", Ratio: "1:1", Resolution: "2K", ReferenceImages: 3}, want: 18.6, known: true},
		{name: "nano 4k", routeID: "image.nano-banana-2.byteplus", req: model.RouteRequirements{Operation: "generation", Ratio: "1:1", Resolution: "4K"}, want: 24, known: true},
		{name: "oreate tier and audio", routeID: "video.seedance-2.0.oreate", req: model.RouteRequirements{Operation: "generation", Resolution: "720p", Duration: "10s", GenerateAudio: true}, want: 270, known: true},
		{name: "oreate reference duration remains unknown until decode", routeID: "video.seedance-2.0.oreate", req: model.RouteRequirements{Operation: "generation", Resolution: "720p", Duration: "10s", ReferenceVideos: 1}, want: 0, known: false},
		{name: "adobe does not invent cost", routeID: "video.kling-3.adobe", req: model.RouteRequirements{Operation: "generation", Resolution: "1080p", Duration: "8s", GenerateAudio: true}, want: 0, known: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, known := evaluateRouteCost(canonicalRouteForTest(t, test.routeID), test.req)
			if known != test.known || math.Abs(got-test.want) > 1e-9 {
				t.Fatalf("evaluateRouteCost() = (%v, %v), want (%v, %v)", got, known, test.want, test.known)
			}
		})
	}
}

func TestQuotaCanServeDistinguishesUnknownFromUnmetered(t *testing.T) {
	trackedUnknownCost := dispatchPlan{QuotaTracked: true}
	if quotaCanServe(0, true, trackedUnknownCost) {
		t.Fatal("known zero balance served a quota-tracked route with unknown request cost")
	}
	if !quotaCanServe(0, false, trackedUnknownCost) {
		t.Fatal("unknown upstream balance was excluded")
	}
	if !quotaCanServe(0, true, dispatchPlan{QuotaTracked: false}) {
		t.Fatal("unmetered route was excluded by a zero balance")
	}
	if quotaCanServe(24, true, dispatchPlan{QuotaTracked: true, Cost: 25}) {
		t.Fatal("known-insufficient balance served a metered request")
	}
	if !quotaCanServe(25, true, dispatchPlan{QuotaTracked: true, Cost: 25}) {
		t.Fatal("exact sufficient balance was excluded")
	}
}

func TestRunTextRouteFailoverTriesSecondRouteAfterSafeFailure(t *testing.T) {
	routes := []model.ModelRoute{{ID: "first"}, {ID: "second"}}
	tests := []struct {
		name string
		err  error
	}{
		{name: "auth", err: chatgpt.ErrAuth},
		{name: "quota", err: custom.ErrQuotaExhausted},
		{name: "temporary", err: grok.ErrTemporaryUpstream},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var called []string
			want := &V1ChatResponse{}
			got, err := runTextRouteFailover(routes, func(route model.ModelRoute) (*V1ChatResponse, error) {
				called = append(called, route.ID)
				if route.ID == "first" {
					return nil, test.err
				}
				return want, nil
			})
			if err != nil {
				t.Fatalf("runTextRouteFailover() error = %v", err)
			}
			if got != want || len(called) != 2 || called[0] != "first" || called[1] != "second" {
				t.Fatalf("runTextRouteFailover() = (%p, calls %v), want (%p, [first second])", got, called, want)
			}
		})
	}
}

func TestRunTextRouteFailoverStopsOnUnsafeFailure(t *testing.T) {
	routes := []model.ModelRoute{{ID: "first"}, {ID: "second"}}
	calls := 0
	_, err := runTextRouteFailover(routes, func(model.ModelRoute) (*V1ChatResponse, error) {
		calls++
		return nil, ErrUnsupportedParams
	})
	if !errors.Is(err, ErrUnsupportedParams) || calls != 1 {
		t.Fatalf("runTextRouteFailover() = (%v, calls %d), want unsupported params after one call", err, calls)
	}
}

func TestRunTextRouteFailoverDoesNotResubmitReturnedStream(t *testing.T) {
	routes := []model.ModelRoute{{ID: "first"}, {ID: "second"}}
	want := &V1ChatResponse{Stream: true}
	calls := 0
	got, err := runTextRouteFailover(routes, func(model.ModelRoute) (*V1ChatResponse, error) {
		calls++
		return want, nil
	})
	if err != nil || got != want || calls != 1 {
		t.Fatalf("runTextRouteFailover() = (%p, %v, calls %d), want first stream response only", got, err, calls)
	}
}
