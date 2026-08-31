package grok

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestStatsigDiscoveryDiagnostics(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("GROK_TOK"))
	proxy := strings.TrimSpace(os.Getenv("GROK_TEST_PROXY"))
	if token == "" || proxy == "" {
		t.Skip("set GROK_TOK and GROK_TEST_PROXY to run discovery diagnostics")
	}
	c := NewClient(proxy)
	client, err := c.newSubmitTLSClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	home, err := fetchStatsigHomepage(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	mm := statsigMetaRe.FindStringSubmatch(home)
	if mm == nil {
		t.Fatal("seed meta missing")
	}
	seed, err := decodeStatsigSeed(mm[1])
	if err != nil {
		t.Fatal(err)
	}
	curves, err := parseStatsigCurves(home)
	if err != nil {
		t.Fatal(err)
	}
	curvesRaw, err := json.Marshal(curves)
	if err != nil {
		t.Fatal(err)
	}
	queue := dedupe(chunkPathRe.FindAllString(home, -1))
	seen := make(map[string]bool, len(queue))
	depth := make(map[string]int, len(queue))
	for _, path := range queue {
		seen[path] = true
	}

	type hit struct {
		path     string
		depth    int
		flags    []string
		engine   string
		verified bool
	}
	var hits []hit
	const maxInspect = 200
	for pos := 0; pos < len(queue) && pos < maxInspect; pos++ {
		path := queue[pos]
		body, ferr := fetchChunk(ctx, client, path)
		if ferr != nil {
			continue
		}
		for _, ref := range allChunkRefRe.FindAllString(body, -1) {
			next := normalizeChunkPath(ref)
			if !seen[next] {
				seen[next] = true
				depth[next] = depth[path] + 1
				queue = append(queue, next)
			}
		}
		checks := []struct {
			name string
			text string
		}{
			{"pct256", "%256"},
			{"hex100", "%0x100"},
			{"fromCharCode", "fromCharCode"},
			{"charCodeAt", "charCodeAt"},
			{"subtle", "subtle"},
			{"digest", "digest"},
			{"getComputedStyle", "getComputedStyle"},
			{"querySelector", "querySelector"},
			{"btoa", "btoa"},
			{"atob", "atob"},
		}
		var flags []string
		for _, check := range checks {
			if strings.Contains(body, check.text) {
				flags = append(flags, check.name)
			}
		}
		if len(flags) >= 4 || isObfuscatedSigner(body) {
			item := hit{path: path, depth: depth[path], flags: flags}
			eng, engineErr := newSigEngine(body)
			if engineErr != nil {
				item.engine = engineErr.Error()
			} else {
				id, signErr := eng.statsigID(mm[1], string(curvesRaw), "/rest/app-chat/conversations/new", "POST")
				if signErr != nil {
					item.engine = signErr.Error()
				} else {
					item.engine = "signed"
					item.verified = signerEmbedsSeed(id, seed)
				}
			}
			if len(item.engine) > 180 {
				item.engine = item.engine[:180]
			}
			hits = append(hits, item)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].path < hits[j].path })
	t.Logf("homepage=%d discovered=%d inspected=%d hits=%d", len(chunkPathRe.FindAllString(home, -1)), len(seen), min(len(queue), maxInspect), len(hits))
	for _, item := range hits {
		t.Logf("candidate depth=%d path=%s flags=%s engine=%q verified=%v", item.depth, item.path, strings.Join(item.flags, ","), item.engine, item.verified)
	}
}
