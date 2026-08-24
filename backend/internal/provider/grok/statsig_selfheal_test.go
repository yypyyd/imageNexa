package grok

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSelfHealStatsigE2E exercises the full browser-free self-healing path:
// fetch the homepage, derive seed+F, cache the challenge, then hit the
// anti-bot-gated conversations/new endpoint. Requires a live GROK_TOK and no
// GROK_STATSIG_* env overrides.
func TestSelfHealStatsigE2E(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("GROK_TOK"))
	if token == "" {
		t.Skip("no GROK_TOK")
	}
	c := NewClient("")
	client, err := c.newSubmitTLSClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	c.ensureChallenge(ctx, client, token)
	ch, ok := loadStatsigChallenge()
	if !ok {
		t.Fatal("challenge not cached (homepage fetch/derive failed)")
	}
	t.Logf("dynamic header[:6]=%x suffix=%s", ch.header[:6], ch.suffix)

	body, err := c.postStream(ctx, client, token, "/rest/app-chat/conversations/new", map[string]any{
		"temporary": true,
		"modelName": "grok-3",
		"message":   "hi",
	})
	if err != nil {
		t.Fatalf("conversations/new: %v", err)
	}
	t.Logf("OK bytes=%d head=%.80s", len(body), strings.ReplaceAll(body, "\n", " "))
}

// TestStatsigEngineConcurrent fires many concurrent goja signs through the pool to
// catch data races / engine cross-talk (run with -race). Requires a live GROK_TOK
// so the engine is built and the challenge cached.
func TestStatsigEngineConcurrent(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("GROK_TOK"))
	if token == "" {
		t.Skip("no GROK_TOK")
	}
	c := NewClient("")
	client, err := c.newSubmitTLSClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c.ensureChallenge(ctx, client, token)
	ch, ok := loadStatsigChallenge()
	if !ok || ch.seedB64 == "" || ch.curvesJSON == "" {
		t.Fatal("engine inputs not cached")
	}

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := signWithEngine(ch.seedB64, ch.curvesJSON, "/rest/app-chat/conversations/new", "POST")
			if err != nil {
				errs <- err
				return
			}
			if len(id) < 40 {
				errs <- errTooShort
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent sign: %v", e)
	}
}

var errTooShort = errors.New("statsig id too short")

// TestGenerateVideoE2E generates a real grok video using only the dynamic
// self-healed statsig (no env overrides). Requires a live GROK_TOK.
func TestGenerateVideoE2E(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("GROK_TOK"))
	if token == "" {
		t.Skip("no GROK_TOK")
	}
	c := NewClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	data, meta, err := c.GenerateVideo(ctx, token, "a cat playing piano", "16:9", "720p", 6, nil, true)
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	t.Logf("video bytes=%d meta=%v", len(data), meta)
	if len(data) < 1<<20 {
		t.Fatalf("video too small: %d bytes", len(data))
	}
	if !strings.Contains(string(data[:16]), "ftyp") {
		t.Fatalf("not an mp4: % x", data[:16])
	}
}
