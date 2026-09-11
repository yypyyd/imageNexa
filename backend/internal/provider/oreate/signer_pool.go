package oreate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"backend/internal/provider/proxysession"

	"github.com/chromedp/chromedp"
)

// Oreate refuses a generation that was not requested by a real signed page, so
// the browser both mints the token and posts the request. Opening a page for
// every generation cost 14 to 16 seconds of navigation, which is why pages are
// kept warm here: a submit is then just the stream fetch the site makes. A page
// belongs to one account and pins that account's sticky proxy session, including
// when the page is later recycled, because the token is only valid for that
// account from that exit IP.
const (
	proxySessionEnv = "OREATE_PROXY_SESSION"

	signerPagesEnv = "OREATE_SIGNER_PAGES"
	// A warm page costs about half a gigabyte of headroom and bursts one core
	// while it submits, and one page submits every one to two seconds, so a
	// handful of them already start far more generations than a host can render.
	signerPageMemoryCost = 512 << 20
	minSignerPages       = 2
	maxSignerPages       = 32

	signerMintTimeout = bantiResponseTimeout + 15*time.Second
	// A page is recycled once it goes unused, gets old or has minted enough
	// tokens, which keeps a stale document or a leaking renderer from being
	// reused forever.
	signerPageIdleTTL  = 5 * time.Minute
	signerPageMaxAge   = 30 * time.Minute
	signerPageMaxMints = 200
	signerSweepEvery   = time.Minute
)

// hotSignature is a token plus the proxy session that must carry the requests
// using it, because the token is only valid from that exit IP.
type hotSignature struct {
	Signature
	Proxy string
}

// hotSigner is implemented by signers that keep warm pages. Test signers can
// still implement only the plain Signer interface.
type hotSigner interface {
	SignHot(ctx context.Context, account Account) (hotSignature, error)
}

type sessionInvalidator interface {
	InvalidateSession(account Account)
}

type sessionCloser interface {
	Close()
}

type browserCookieObserver func(account Account, browserCookie string) Account

type signerPageKey struct {
	identity [sha256.Size]byte
	version  [sha256.Size]byte
}

// signerPage is one warm signed chat page: the browser stays open between
// generations so a mint costs a Banti report instead of a page load. A page
// carries one account's cookies, so it only ever signs for that account.
type signerPage struct {
	mintMu sync.Mutex
	ctx    context.Context
	close  func()
	key    signerPageKey
	proxy  string
	opened time.Time
	idleAt time.Time
	mints  int
	stale  atomic.Bool

	account Account
	observe browserCookieObserver
}

type signerPool struct {
	proxy    func() string
	observe  browserCookieObserver
	openPage func(Account) (*signerPage, error)
	limit    int

	mu   sync.Mutex
	live int
	// idle holds warm pages per account and normalized cookie version. Keys contain
	// only fixed-size hashes, never a raw session cookie.
	idle map[signerPageKey][]*signerPage
	// active pages stay registered so invalidation can mark them stale without
	// cancelling the generation currently using them.
	active      map[*signerPage]struct{}
	invalidated map[signerPageKey]uint64

	free      chan struct{}
	done      chan struct{}
	sweep     sync.Once
	closeOnce sync.Once
	closed    bool
}

func newSignerPool(proxy func() string, observers ...browserCookieObserver) *signerPool {
	var observe browserCookieObserver
	if len(observers) > 0 {
		observe = observers[0]
	}
	pool := &signerPool{
		proxy:       proxy,
		observe:     observe,
		limit:       signerPages(),
		idle:        make(map[signerPageKey][]*signerPage),
		active:      make(map[*signerPage]struct{}),
		invalidated: make(map[signerPageKey]uint64),
		free:        make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
	pool.openPage = pool.open
	return pool
}

// signerKey identifies both the logical account and the exact normalized cookie
// version a warm page belongs to. Hashes prevent session material from becoming
// a map key while preserving deterministic equality.
func signerKey(account Account) signerPageKey {
	account = account.normalized()
	identity := ""
	switch {
	case account.ID != "":
		identity = "id:" + account.ID
	case account.OUID != "":
		identity = "ouid:" + account.OUID
	case account.Email != "":
		identity = "email:" + strings.ToLower(account.Email)
	default:
		identity = "anonymous:" + fmt.Sprintf("%x", cookieVersion(account.Cookie))
	}
	return signerPageKey{
		identity: sha256.Sum256([]byte(identity)),
		version:  cookieVersion(account.Cookie),
	}
}

// signerPages sizes the warm pool from the resources this process can use, so
// the same build fits a small container and a large host; the env var stays as
// an override for hosts where the derived number is wrong.
func signerPages() int {
	if raw := strings.TrimSpace(os.Getenv(signerPagesEnv)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			return parsed
		}
	}
	limit := availableCPUs() * 2
	if memory := availableMemory(); memory > 0 {
		limit = min(limit, int(memory/signerPageMemoryCost))
	}
	return min(max(limit, minSignerPages), maxSignerPages)
}

// submit runs one whole in-page generation on a warm page of the account and
// reports the proxy session that page egresses through, so recovery requests use
// the same exit IP. A page that fails is dropped and the submit retried once, for
// the same reason a mint is.
func (p *signerPool) submit(ctx context.Context, account Account, quotedPayload string) (videoSubmitResult, error) {
	result, err := p.submitOnce(ctx, account, quotedPayload)
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrAuth) {
		return result, err
	}
	return p.submitOnce(ctx, account, quotedPayload)
}

func (p *signerPool) submitOnce(ctx context.Context, account Account, quotedPayload string) (videoSubmitResult, error) {
	page, err := p.acquire(ctx, account)
	if err != nil {
		return videoSubmitResult{}, err
	}
	result, err := page.submit(ctx, quotedPayload)
	p.release(page, err)
	if err != nil {
		return videoSubmitResult{}, err
	}
	result.Proxy = page.proxy
	return result, nil
}

// sign mints one token on a warm page. A page that fails is dropped and the
// mint retried once, because a dead renderer or an aborted document is far more
// likely than the account being refused.
func (p *signerPool) sign(ctx context.Context, account Account) (hotSignature, error) {
	sig, err := p.signOnce(ctx, account)
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrAuth) {
		return sig, err
	}
	return p.signOnce(ctx, account)
}

func (p *signerPool) signOnce(ctx context.Context, account Account) (hotSignature, error) {
	page, err := p.acquire(ctx, account)
	if err != nil {
		return hotSignature{}, err
	}
	signature, err := page.mint(ctx)
	p.release(page, err)
	if err != nil {
		return hotSignature{}, err
	}
	return hotSignature{Signature: signature, Proxy: page.proxy}, nil
}

// acquire hands out a warm page of this account, opens one while the pool is
// below its limit, and otherwise waits for a page to come back.
func (p *signerPool) acquire(ctx context.Context, account Account) (*signerPage, error) {
	p.sweep.Do(func() { go p.sweepIdle() })
	key := signerKey(account)
	for {
		select {
		case <-p.done:
			return nil, errors.New("oreate signer pool closed")
		default:
		}
		page, slot, invalidation := p.checkout(key)
		if page != nil {
			return page, nil
		}
		if slot {
			page, err := p.openPage(account)
			if err != nil {
				p.mu.Lock()
				if !p.closed {
					p.live--
				}
				p.mu.Unlock()
				p.wake()
				return nil, err
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				page.close()
				return nil, errors.New("oreate signer pool closed")
			}
			openedKey := signerKey(page.account)
			page.key = openedKey
			if p.invalidated[key] != invalidation || p.invalidated[openedKey] > 0 {
				page.stale.Store(true)
			}
			p.active[page] = struct{}{}
			p.mu.Unlock()
			return page, nil
		}
		select {
		case <-p.free:
		case <-p.done:
			return nil, errors.New("oreate signer pool closed")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// checkout takes a warm page of this account, or reports that the caller may
// open one. At the limit it closes the page that has been idle longest on
// another account to free the slot, since that page cannot serve this one.
func (p *signerPool) checkout(key signerPageKey) (*signerPage, bool, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, 0
	}
	invalidation := p.invalidated[key]
	pages := p.idle[key]
	for len(pages) > 0 {
		page := pages[len(pages)-1]
		pages = pages[:len(pages)-1]
		if page.usable() {
			p.store(key, pages)
			p.active[page] = struct{}{}
			return page, false, invalidation
		}
		p.retire(page)
	}
	p.store(key, pages)
	if p.live >= p.limit {
		victim, victimKey := p.oldestIdle()
		if victim == nil {
			return nil, false, invalidation
		}
		p.drop(victimKey, victim)
		p.retire(victim)
	}
	p.live++
	return nil, true, invalidation
}

func (p *signerPool) store(key signerPageKey, pages []*signerPage) {
	if len(pages) == 0 {
		delete(p.idle, key)
		return
	}
	p.idle[key] = pages
}

// oldestIdle picks the least recently used idle page across all accounts.
func (p *signerPool) oldestIdle() (*signerPage, signerPageKey) {
	var oldest *signerPage
	var oldestKey signerPageKey
	for key, pages := range p.idle {
		for _, page := range pages {
			if oldest == nil || page.idleAt.Before(oldest.idleAt) {
				oldest, oldestKey = page, key
			}
		}
	}
	return oldest, oldestKey
}

func (p *signerPool) drop(key signerPageKey, page *signerPage) {
	pages := p.idle[key]
	for i, candidate := range pages {
		if candidate == page {
			p.store(key, append(pages[:i], pages[i+1:]...))
			return
		}
	}
}

// retire closes a page and gives its slot back; the pool lock is held.
func (p *signerPool) retire(page *signerPage) {
	delete(p.active, page)
	page.close()
	p.live--
}

func (p *signerPool) release(page *signerPage, err error) {
	p.mu.Lock()
	if p.closed {
		delete(p.active, page)
		p.mu.Unlock()
		page.close()
		return
	}
	delete(p.active, page)
	if err != nil || !page.usable() {
		p.retire(page)
	} else {
		page.idleAt = time.Now()
		p.idle[page.key] = append(p.idle[page.key], page)
	}
	p.mu.Unlock()
	p.wake()
}

// invalidate closes idle pages for one account/version immediately and marks
// checked-out pages stale. It deliberately does not cancel active work.
func (p *signerPool) invalidate(account Account) {
	key := signerKey(account)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.invalidated[key]++
	pages := p.idle[key]
	delete(p.idle, key)
	for _, page := range pages {
		p.retire(page)
	}
	for page := range p.active {
		if page.key == key {
			page.stale.Store(true)
		}
	}
	p.mu.Unlock()
	p.wake()
}

// wake releases one waiter without blocking when none is waiting.
func (p *signerPool) wake() {
	select {
	case p.free <- struct{}{}:
	default:
	}
}

func (p *signerPool) Close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.mu.Lock()
		p.closed = true
		pages := make(map[*signerPage]struct{}, p.live)
		for _, idle := range p.idle {
			for _, page := range idle {
				pages[page] = struct{}{}
			}
		}
		for page := range p.active {
			page.stale.Store(true)
			pages[page] = struct{}{}
		}
		p.idle = make(map[signerPageKey][]*signerPage)
		p.active = make(map[*signerPage]struct{})
		p.live = 0
		p.mu.Unlock()
		for page := range pages {
			page.close()
		}
	})
}

// sweepIdle closes pages nothing has needed for a while so a quiet instance
// stops paying for browsers it is not using.
func (p *signerPool) sweepIdle() {
	ticker := time.NewTicker(signerSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.mu.Lock()
			for key, pages := range p.idle {
				kept := pages[:0]
				for _, page := range pages {
					if page.usable() && time.Since(page.idleAt) < signerPageIdleTTL {
						kept = append(kept, page)
						continue
					}
					p.retire(page)
				}
				p.store(key, kept)
			}
			p.mu.Unlock()
			p.wake()
		}
	}
}

// open launches one browser pinned to this account's sticky proxy session,
// installs the account cookies and waits for the page runtime that mints tokens.
func (p *signerPool) open(account Account) (*signerPage, error) {
	path := chromiumExecPath()
	if path == "" {
		return nil, errors.New("oreate signer: no Chrome/Chromium binary (set OREATE_CHROME)")
	}
	proxyURL := ""
	if p.proxy != nil {
		proxyURL = stickyProxyURL(p.proxy(), accountProxySession(account))
	}
	// The page outlives the generation that opened it, so its browser hangs off
	// the background context and is only closed by the pool.
	pageCtx, cancelPage := context.WithCancel(context.Background())
	opts, bridge, err := browserExecOptions(pageCtx, path, proxyURL)
	if err != nil {
		cancelPage()
		return nil, err
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(pageCtx, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	page := &signerPage{
		ctx:     browserCtx,
		proxy:   proxyURL,
		opened:  time.Now(),
		idleAt:  time.Now(),
		account: account.normalized(),
		observe: p.observe,
	}
	page.close = sync.OnceFunc(func() {
		cancelBrowser()
		cancelAlloc()
		cancelPage()
		if bridge != nil {
			bridge.Close()
		}
	})
	// chromedp ties the browser process to whichever context first runs an
	// action, so the browser is started on the page's own context: starting it
	// from a bounded one would take the browser down with that bound.
	started := make(chan error, 1)
	go func() { started <- chromedp.Run(browserCtx) }()
	select {
	case err := <-started:
		if err != nil {
			page.close()
			return nil, fmt.Errorf("oreate signer: browser start: %w", err)
		}
	case <-time.After(signerTimeout):
		page.close()
		return nil, errors.New("oreate signer: browser start timeout")
	}
	if err := page.prepare(account); err != nil {
		page.close()
		return nil, err
	}
	return page, nil
}

// prepare loads the signed chat page the same way the one-shot signer does, and
// bounds itself so a hanging proxy cannot block the pool.
func (page *signerPage) prepare(account Account) error {
	ctx, cancel := context.WithTimeout(page.ctx, signerTimeout)
	defer cancel()
	page.account = account.normalized()
	if err := chromedp.Run(ctx, signerCookieActions(page.account)...); err != nil {
		return fmt.Errorf("oreate signer: cookie setup: %w", err)
	}
	navErr := navigateSignerPage(ctx)
	if err := chromedp.Run(ctx, chromedp.Poll(parisReadyJS, nil,
		chromedp.WithPollingInterval(100*time.Millisecond), chromedp.WithPollingTimeout(parisReadyTimeout))); err != nil {
		diagnostics := browserRuntimeDiagnostics(ctx)
		log.Printf("oreate signer: Paris runtime not ready (%s)", diagnostics)
		if navErr != nil {
			return fmt.Errorf("%w: oreate signer: page navigation: %v (%s)", ErrTemporaryUpstream, navErr, diagnostics)
		}
		return fmt.Errorf("%w: oreate signer: Paris runtime (%s)", ErrTemporaryUpstream, diagnostics)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`true`, nil)); err != nil {
		return fmt.Errorf("%w: oreate signer: page context lost: %v", ErrTemporaryUpstream, err)
	}
	if err := chromedp.Run(ctx, signerCookieActions(page.account)...); err != nil {
		log.Printf("oreate signer: cookie refresh skipped: %s", clipSignerLog(err))
	}
	browserCookie, err := browserOreateCookieHeader(ctx)
	if err != nil {
		log.Printf("oreate signer: cookie capture skipped: %s", clipSignerLog(err))
		return nil
	}
	page.captureBrowserCookies(browserCookie)
	return nil
}

func (page *signerPage) captureBrowserCookies(browserCookie string) {
	if page.observe != nil {
		page.account = page.observe(page.account, browserCookie)
	} else {
		page.account = applyBrowserCookies(page.account, browserCookie, nil)
	}
	page.key.version = cookieVersion(page.account.Cookie)
}

// mint asks the page runtime for one token. Only one mint runs on a page at a
// time because the report state lives on the document.
func (page *signerPage) mint(ctx context.Context) (Signature, error) {
	page.mintMu.Lock()
	defer page.mintMu.Unlock()
	runCtx, cancel, err := page.runContext(ctx, signerMintTimeout)
	if err != nil {
		return Signature{}, err
	}
	defer cancel()
	if err := chromedp.Run(runCtx, chromedp.Evaluate(mintBantiEvaluateJS(), nil)); err != nil {
		return Signature{}, fmt.Errorf("%w: oreate signer: Banti dispatch: %v", ErrTemporaryUpstream, err)
	}
	var jt string
	if err := chromedp.Run(runCtx,
		chromedp.Poll(`window.__oreateSignerJT`, &jt,
			chromedp.WithPollingInterval(25*time.Millisecond), chromedp.WithPollingTimeout(bantiResponseTimeout)),
	); err != nil {
		return Signature{}, fmt.Errorf("%w: oreate signer: Banti response: %v", ErrTemporaryUpstream, err)
	}
	page.mints++
	jt = strings.TrimSpace(jt)
	if jt == "" || len(jt) > maxBantiJTLength {
		return Signature{}, ErrRiskControl
	}
	if browserCookie, err := browserOreateCookieHeader(runCtx); err == nil {
		page.captureBrowserCookies(browserCookie)
	}
	return Signature{JT: jt, BID: page.account.BID, Cookie: page.account.Cookie}, nil
}

// submit mints a token and posts the generation from the page itself, which is
// what risk control accepts. Only one submit runs on a page at a time because
// the request state lives on the document.
func (page *signerPage) submit(ctx context.Context, quotedPayload string) (videoSubmitResult, error) {
	page.mintMu.Lock()
	defer page.mintMu.Unlock()
	runCtx, cancel, err := page.runContext(ctx, inPageSubmitTimeout)
	if err != nil {
		return videoSubmitResult{}, err
	}
	defer cancel()
	started := time.Now()
	if err := chromedp.Run(runCtx, chromedp.Evaluate(inPageSubmitScript(quotedPayload), nil)); err != nil {
		return videoSubmitResult{}, fmt.Errorf("%w: oreate signer: submit dispatch: %v", ErrTemporaryUpstream, err)
	}
	pollErr := chromedp.Run(runCtx, chromedp.Poll(`window.__oreateSubmit && window.__oreateSubmit.done === true`, nil,
		chromedp.WithPollingInterval(250*time.Millisecond), chromedp.WithPollingTimeout(inPageRenderWait)))
	var result videoSubmitResult
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`window.__oreateSubmit || {}`, &result)); err != nil {
		if pollErr != nil {
			return videoSubmitResult{}, fmt.Errorf("%w: oreate signer: submit stream: %v", ErrTemporaryUpstream, pollErr)
		}
		return videoSubmitResult{}, fmt.Errorf("%w: oreate signer: submit state: %v", ErrTemporaryUpstream, err)
	}
	if pollErr != nil && result.Failure == "" {
		result.Failure = pollErr.Error()
	}
	if browserCookie, err := browserOreateCookieHeader(runCtx); err == nil {
		page.captureBrowserCookies(browserCookie)
	}
	result.Cookie = page.account.Cookie
	page.mints++
	log.Printf("oreate inpage submit: submits=%d elapsed=%s status=%d failure=%s", page.mints,
		time.Since(started).Round(time.Millisecond), result.Status, clipSignerLog(fmt.Errorf("%s", result.Failure)))
	return result, nil
}

// runContext bounds one page operation by the shorter of the caller's deadline
// and the operation's own budget, while keeping the page's own lifetime.
func (page *signerPage) runContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < budget {
			budget = left
		}
	}
	if budget <= 0 {
		return nil, nil, context.DeadlineExceeded
	}
	runCtx, cancel := context.WithTimeout(page.ctx, budget)
	return runCtx, cancel, nil
}

func (page *signerPage) usable() bool {
	return !page.stale.Load() && page.ctx.Err() == nil && page.mints < signerPageMaxMints && time.Since(page.opened) < signerPageMaxAge
}

// stickyProxyURL pins the proxy to one exit IP by labelling the session in the
// user name, which is how rotating residential pools expose sticky sessions.
// Pools that do not support the label, or a proxy without credentials, are left
// alone. OREATE_PROXY_SESSION=false turns the rewrite off for this provider;
// PROXY_STICKY_SESSION=false is the process-wide kill switch.
func stickyProxyURL(raw, session string) string {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(strings.TrimSpace(os.Getenv(proxySessionEnv)), "false") {
		return raw
	}
	return proxysession.URL(raw, session)
}

// accountProxySession is stable for one Oreate account so a recycled signer
// page reconnects through the same residential exit instead of hopping IPs.
func accountProxySession(account Account) string {
	account = account.normalized()
	if key := proxysession.Key(account.ID); key != "" {
		return key
	}
	if key := proxysession.Key(account.Cookie); key != "" {
		return key
	}
	return newProxySession()
}

func newProxySession() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}
