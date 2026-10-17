package service

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"backend/internal/model"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/dola"
	"backend/internal/provider/grok"
)

func TestAcceptedGPTNeverAuthorizesFailover(t *testing.T) {
	for _, cause := range []error{chatgpt.ErrAuth, chatgpt.ErrQuotaExhausted, chatgpt.ErrTemporaryUpstream, chatgpt.ErrContentPolicy} {
		err := chatgpt.AcceptedFailure("existing-task", cause)
		if !noRouteFailover(err) || routeFailoverSafe(err) {
			t.Fatalf("accepted task allows retry: %v", cause)
		}
		state, class := dispatchFailureClass(err)
		if errors.Is(cause, chatgpt.ErrContentPolicy) {
			if state != "failed" || class != "content" {
				t.Fatal("rejection must be terminal")
			}
		} else if state != "accepted" {
			t.Fatal("accepted task must remain recoverable")
		}
		if dispatchUpstreamTaskID(err) != "existing-task" {
			t.Fatal("lost recovery handle")
		}
	}
	err := chatgpt.AcceptedFailure("", chatgpt.ErrTemporaryUpstream)
	state, _ := dispatchFailureClass(err)
	if !noRouteFailover(err) || state != "unknown" {
		t.Fatal("ambiguous submission must not fail over")
	}
}

func TestAcceptedVideoNeverAuthorizesFailover(t *testing.T) {
	taskURL := "https://assets.grok.com/users/u/generated/p/generated_video.mp4"
	for _, tc := range []struct {
		name       string
		err        error
		state      string
		class      string
		recovering bool
	}{
		{"grok temporary", grok.AcceptedFailure(taskURL, grok.ErrTemporaryUpstream), "accepted", "temporary", true},
		{"grok auth after submit", grok.AcceptedFailure(taskURL, grok.ErrAuth), "accepted", "temporary", true},
		{"grok quota rejection", grok.AcceptedFailure(taskURL, grok.ErrQuotaExhausted), "failed", "quota", false},
		{"grok fatal rejection", grok.AcceptedFailure(taskURL, grok.ErrVideoRejected), "failed", "request", false},
		{"dola temporary", dola.AcceptedFailure("conversation", dola.ErrTemporaryUpstream), "accepted", "temporary", true},
		{"dola content rejection", dola.AcceptedFailure("conversation", dola.ErrContentRejected), "failed", "request", false},
		{"dola final generation failure", dola.AcceptedFailure("conversation", dola.ErrVideoFailed), "failed", "request", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !noRouteFailover(tc.err) || routeFailoverSafe(tc.err) {
				t.Fatalf("accepted video allows failover: %v", tc.err)
			}
			state, class := dispatchFailureClass(tc.err)
			if state != tc.state || class != tc.class || acceptedVideoRecoveryPending(tc.err) != tc.recovering {
				t.Fatalf("state=%s class=%s recovering=%v", state, class, acceptedVideoRecoveryPending(tc.err))
			}
		})
	}
	if dispatchUpstreamTaskID(grok.AcceptedFailure(taskURL, grok.ErrTemporaryUpstream)) != taskURL {
		t.Fatal("lost Grok recovery handle")
	}
}

func TestParameterErrorsPreserveCapabilityCombinations(t *testing.T) {
	capabilities, _ := json.Marshal([]model.CapabilityProfile{
		{Operations: []string{"generation"}, Ratios: []string{"1:1"}, Resolutions: []string{"1K"}},
		{Operations: []string{"generation"}, Ratios: []string{"16:9"}, Resolutions: []string{"2K"}},
	})
	err := describeUnsupportedRoute([]model.ModelRoute{{Capabilities: capabilities}}, model.RouteRequirements{Operation: "generation", Ratio: "1:1", Resolution: "2K"})
	var detail *ParameterError
	if !errors.As(publicGenerationError(err), &detail) || detail.Param != "resolution" || len(detail.Supported) != 1 || detail.Supported[0] != "1K" {
		t.Fatalf("invalid capability cross-product: %#v", detail)
	}
}

func TestQuotaViewDistinguishesUnknownEmptyAndCached(t *testing.T) {
	now := time.Now()
	stale := now.Add(-time.Hour)
	positive := 2.0
	zero := 0.0
	for _, tc := range []struct {
		name    string
		buckets []model.AccountQuotaBucket
		want    string
	}{
		{"missing", nil, "unknown"},
		{"unknown", []model.AccountQuotaBucket{{BucketKey: "chatgpt.image"}}, "unknown"},
		{"empty", []model.AccountQuotaBucket{{BucketKey: "chatgpt.image", Remaining: &zero, RefreshedAt: &now}}, "empty"},
		{"positive", []model.AccountQuotaBucket{{BucketKey: "chatgpt.image", Remaining: &positive, RefreshedAt: &now}}, "available"},
		{"cached balance", []model.AccountQuotaBucket{{BucketKey: "chatgpt.image", Remaining: &positive, RefreshedAt: &stale}}, "available"},
		{"unrelated bucket", []model.AccountQuotaBucket{{BucketKey: "chatgpt.text", Remaining: &positive, RefreshedAt: &now}}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountQuotaState("chatgpt", tc.buckets, now); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
