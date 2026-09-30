package proxypool

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTakeFreeSkipsExpiredExtractedEndpoints(t *testing.T) {
	now := time.Now()
	p := &pool{
		ttl:    15 * time.Minute,
		leases: map[string]lease{},
		free: []lease{
			{url: "http://expired.example:8000", expiresAt: now.Add(-time.Second)},
			{url: "http://fresh.example:8000", expiresAt: now.Add(time.Minute)},
		},
	}

	got, ok := p.takeFree("account-1")
	if !ok || got != "http://fresh.example:8000" {
		t.Fatalf("takeFree() = %q, %v; want fresh endpoint", got, ok)
	}
	assigned := p.leases["account-1"]
	if !assigned.expiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("assignment expiry = %v; want original extraction expiry", assigned.expiresAt)
	}
}

func TestFreeEndpointDoesNotGainANewTTLWhenAssigned(t *testing.T) {
	expiresAt := time.Now().Add(30 * time.Second)
	p := &pool{
		ttl:    time.Hour,
		leases: map[string]lease{},
		free:   []lease{{url: "http://proxy.example:8000", expiresAt: expiresAt}},
	}

	if _, ok := p.takeFree("account-1"); !ok {
		t.Fatal("takeFree() did not return the fresh endpoint")
	}
	if got := p.leases["account-1"].expiresAt; !got.Equal(expiresAt) {
		t.Fatalf("assignment expiry = %v; want %v", got, expiresAt)
	}
}

func TestRefillStampsFreeEndpointsAtExtractionTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "127.0.0.1:18080")
	}))
	defer server.Close()

	p := &pool{extract: server.URL, ttl: 10 * time.Minute, leases: map[string]lease{}}
	before := time.Now().Add(p.ttl)
	p.refill(context.Background(), 1)
	after := time.Now().Add(p.ttl)

	if len(p.free) != 1 {
		t.Fatalf("free endpoint count = %d; want 1", len(p.free))
	}
	if expiry := p.free[0].expiresAt; expiry.Before(before) || expiry.After(after) {
		t.Fatalf("free endpoint expiry %v not in [%v, %v]", expiry, before, after)
	}
}
