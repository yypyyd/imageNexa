package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func testConcurrency(t *testing.T) (*ConcurrencyService, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	server.SetTime(time.Now())
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	return NewConcurrencyService(client), server
}

func TestConcurrencyReleaseSurvivesCanceledWork(t *testing.T) {
	gate, _ := testConcurrency(t)
	ctx, cancel := context.WithCancel(context.Background())
	if ok, err := gate.Acquire(ctx, "conc:a:test", 1, "finished"); err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	cancel()
	gate.Release(ctx, "conc:a:test", "finished")
	if count := gate.redis.ZCard(context.Background(), "conc:a:test").Val(); count != 0 {
		t.Fatalf("canceled cleanup retained %d slots", count)
	}
	if ok, err := gate.Acquire(context.Background(), "conc:a:test", 1, "next"); err != nil || !ok {
		t.Fatalf("next generation could not acquire released slot: %v, %v", ok, err)
	}
}

func TestConcurrencyAtomicLimitUnderContention(t *testing.T) {
	gate, _ := testConcurrency(t)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := gate.Acquire(context.Background(), "conc:a:test", 3, fmt.Sprint(i))
			if err != nil {
				t.Errorf("acquire: %v", err)
			}
			if ok {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 3 || gate.ActiveCount(context.Background(), "conc:a:test") != 3 {
		t.Fatalf("admitted %d requests into a three-slot account", admitted.Load())
	}
}

func TestConcurrencyObservationUsesRedisTimeAndBatches(t *testing.T) {
	gate, server := testConcurrency(t)
	// A worker clock ahead of Redis must not erase valid leases.
	serverTime := time.Now().Add(-2 * time.Hour)
	server.SetTime(serverTime)
	keys := make([]string, 260)
	for i := range keys {
		keys[i] = fmt.Sprintf("conc:a:%d", i)
		if ok, err := gate.Acquire(context.Background(), keys[i], 1, "held"); err != nil || !ok {
			t.Fatalf("acquire %d: %v, %v", i, ok, err)
		}
	}
	counts, ok := gate.ActiveCounts(context.Background(), append(keys, keys[0], ""))
	if !ok || len(counts) != len(keys) {
		t.Fatalf("batched observation = %d entries, ok=%v", len(counts), ok)
	}
	for _, key := range keys {
		if counts[key] != 1 {
			t.Fatalf("live lease missing for %s", key)
		}
	}
	server.SetTime(serverTime.Add(16 * time.Minute))
	if n := gate.ActiveCount(context.Background(), keys[0]); n != 0 {
		t.Fatalf("expired lease counted: %d", n)
	}
	if n := gate.redis.ZCard(context.Background(), keys[0]).Val(); n != 1 {
		t.Fatal("observation mutated the lease set")
	}
	if ok, err := gate.Acquire(context.Background(), keys[0], 1, "after-expiry"); err != nil || !ok {
		t.Fatalf("expired lease blocked acquisition: %v, %v", ok, err)
	}
}
