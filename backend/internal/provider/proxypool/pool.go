// Package proxypool leases sticky residential exits from a provider extract API.
//
// The API can return many endpoints in one call. This package fetches a bounded
// batch, then assigns one exit per account so concurrent generations do not share
// an IP and the same account keeps that exit until TTL or a failure rotation.
package proxypool

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Assigner hands out dedicated proxy URLs for one provider.
type Assigner interface {
	Assign(ctx context.Context, accountID, region string) (proxyURL string, ok bool)
	Rotate(accountID string)
}

type Manager struct {
	mu    sync.Mutex
	cfgs  map[string]string
	pools map[string]*pool
}

type pool struct {
	extract string
	ttl     time.Duration
	mu      sync.Mutex
	leases  map[string]lease
	free    []string
	flight  singleflight.Group
}

type lease struct {
	url       string
	expiresAt time.Time
}

type binding struct {
	m        *Manager
	provider string
}

func NewManager() *Manager {
	return &Manager{
		cfgs:  map[string]string{},
		pools: map[string]*pool{},
	}
}

func (m *Manager) Configure(provider, extractAPI string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	extractAPI = strings.TrimSpace(extractAPI)
	if provider == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfgs == nil {
		m.cfgs = map[string]string{}
	}
	prev := m.cfgs[provider]
	m.cfgs[provider] = extractAPI
	if prev == extractAPI {
		return
	}
	for key := range m.pools {
		if key == provider || strings.HasPrefix(key, provider+"|") {
			delete(m.pools, key)
		}
	}
}

func (m *Manager) Bind(provider string) Assigner {
	return &binding{m: m, provider: strings.ToLower(strings.TrimSpace(provider))}
}

func (b *binding) Assign(ctx context.Context, accountID, region string) (string, bool) {
	if b == nil || b.m == nil {
		return "", false
	}
	extract := b.m.extractAPI(b.provider)
	if extract == "" {
		return "", false
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		accountID = "shared"
	}
	region = strings.ToUpper(strings.TrimSpace(region))
	key := b.provider
	endpoint := extract
	if region != "" {
		key = b.provider + "|" + region
		endpoint = rewriteRegion(extract, region)
	}
	return b.m.poolFor(key, endpoint).assign(ctx, accountID)
}

func (b *binding) Rotate(accountID string) {
	if b == nil || b.m == nil {
		return
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		accountID = "shared"
	}
	b.m.rotate(b.provider, accountID)
}

func (m *Manager) extractAPI(provider string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.TrimSpace(m.cfgs[provider])
}

func (m *Manager) poolFor(key, endpoint string) *pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pools == nil {
		m.pools = map[string]*pool{}
	}
	if existing := m.pools[key]; existing != nil && existing.extract == endpoint {
		return existing
	}
	p := &pool{
		extract: endpoint,
		ttl:     leaseTTL(endpoint),
		leases:  map[string]lease{},
	}
	m.pools[key] = p
	return p
}

func (m *Manager) rotate(provider, accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, p := range m.pools {
		if key == provider || strings.HasPrefix(key, provider+"|") {
			p.drop(accountID)
		}
	}
}

func (p *pool) assign(ctx context.Context, accountID string) (string, bool) {
	if url, ok := p.current(accountID); ok {
		return url, true
	}
	if url, ok := p.takeFree(accountID); ok {
		return url, true
	}
	p.refill(ctx, minBatch)
	if url, ok := p.takeFree(accountID); ok {
		return url, true
	}
	return "", false
}

func (p *pool) current(accountID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.leases[accountID]
	if !ok {
		return "", false
	}
	if time.Now().Before(existing.expiresAt) && existing.url != "" {
		return existing.url, true
	}
	delete(p.leases, accountID)
	return "", false
}

func (p *pool) takeFree(accountID string) (string, bool) {
	p.mu.Lock()
	if existing, ok := p.leases[accountID]; ok && time.Now().Before(existing.expiresAt) && existing.url != "" {
		url := existing.url
		p.mu.Unlock()
		return url, true
	}
	if len(p.free) == 0 {
		p.mu.Unlock()
		return "", false
	}
	next := p.free[0]
	p.free = append([]string(nil), p.free[1:]...)
	p.leases[accountID] = lease{url: next, expiresAt: time.Now().Add(p.ttl)}
	low := len(p.free) < 4
	p.mu.Unlock()
	if low {
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			p.refill(bg, minBatch)
		}()
	}
	return next, true
}

func (p *pool) drop(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.leases, accountID)
}

func (p *pool) refill(ctx context.Context, needed int) {
	endpoint := p.extract
	_, _, _ = p.flight.Do(endpoint, func() (any, error) {
		count := batchCount(endpoint, needed)
		urls := fetchEndpoints(ctx, endpoint, count)
		if len(urls) == 0 {
			return nil, nil
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		used := map[string]bool{}
		for _, item := range p.leases {
			used[item.url] = true
		}
		for _, item := range p.free {
			used[item] = true
		}
		for _, next := range urls {
			if next == "" || used[next] {
				continue
			}
			used[next] = true
			p.free = append(p.free, next)
		}
		return nil, nil
	})
}

func fetchEndpoints(ctx context.Context, endpoint string, count int) []string {
	fetchURL := prepareFetchURL(endpoint, count)
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range splitLines(string(body)) {
		parsed := ParseLine(line)
		if parsed == "" || seen[parsed] {
			continue
		}
		seen[parsed] = true
		out = append(out, parsed)
	}
	return out
}

func splitLines(body string) []string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return strings.Split(body, "\n")
}
