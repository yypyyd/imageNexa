package grok

// This file makes x-statsig-id durable across grok web reships by executing grok's
// OWN obfuscated signer (a Turbopack chunk) inside an embedded JS engine (goja),
// under a synthesized DOM + Web-Animations getComputedStyle shim (statsig_shim.js).
// grok's code does all the per-build byte-indexing / curve-selection; we only supply
// the stable browser primitives. Static defaults remain the last-resort fallback.

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/dop251/goja"
)

//go:embed statsig_shim.js
var statsigShimJS string

const sigPoolSize = 4

// errEngineNotReady means the durable signer engine has not been built yet (no
// homepage fetched, or chunk location failed). Callers fall back to the static path.
var errEngineNotReady = errors.New("statsig engine not ready")

// locateConcurrency bounds parallel chunk fetches during signer discovery.
const locateConcurrency = 24

var (
	// chunkPathRe matches chunk URLs in the homepage; allChunkRefRe additionally
	// matches the loader-manifest's lazy refs (which drop the /_next/ prefix).
	// The Turbopack content-hash alphabet includes '~' (e.g. 0~4v1h2zpw7n0.js) -
	// the signer chunk is frequently one of these, so the class MUST list it or
	// the signer is never fetched/located (403 anti-bot, static fallback goes stale).
	chunkPathRe   = regexp.MustCompile(`/_next/static/chunks/[a-zA-Z0-9_.~\-/]+\.js`)
	allChunkRefRe = regexp.MustCompile(`(?:/_next/)?static/chunks/[a-zA-Z0-9_.~\-/]+\.js`)
	// goja's parser tries to fetch //# sourceMappingURL=... from disk and errors.
	sourceMapRe = regexp.MustCompile(`(?m)//[#@]\s*sourceMappingURL=\S*`)

	sigMgrMu    sync.Mutex
	sigBuildKey string          // hash of the homepage chunk list; changes on reship
	sigChunkSrc string          // current signer chunk source
	sigPool     chan *sigEngine // pool of ready engines for sigChunkSrc
)

// sigEngine wraps one goja runtime with grok's signer chunk loaded. A goja runtime
// is not safe for concurrent use; the pool hands each engine to one goroutine at a
// time so no per-engine locking is needed.
type sigEngine struct {
	rt   *goja.Runtime
	fire goja.Callable // __grokSignInto
}

func newSigEngine(chunkSrc string) (*sigEngine, error) {
	rt := goja.New()
	// SHA-256 bridge for crypto.subtle.digest.
	if err := rt.Set("__goSha256", func(call goja.FunctionCall) goja.Value {
		data := jsBytes(rt, call.Argument(0))
		sum := sha256.Sum256(data)
		return rt.ToValue(rt.NewArrayBuffer(sum[:]))
	}); err != nil {
		return nil, err
	}
	if _, err := rt.RunString(statsigShimJS); err != nil {
		return nil, fmt.Errorf("shim: %w", err)
	}
	if _, err := rt.RunString(sourceMapRe.ReplaceAllString(chunkSrc, "")); err != nil {
		return nil, fmt.Errorf("chunk eval: %w", err)
	}
	if _, err := rt.RunString("__grokBootstrap()"); err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	fire, ok := goja.AssertFunction(rt.Get("__grokSignInto"))
	if !ok {
		return nil, errors.New("statsig js: __grokSignInto missing")
	}
	return &sigEngine{rt: rt, fire: fire}, nil
}

// statsigID runs grok's signer for one request. seedB64 is the raw <meta> content;
// curvesJSON is [[{color,deg,bezier}...]...].
func (e *sigEngine) statsigID(seedB64, curvesJSON, path, method string) (string, error) {
	_ = e.rt.Set("__SEED", seedB64)
	_ = e.rt.Set("__CURVES", curvesJSON)
	_ = e.rt.Set("__PATH", path)
	_ = e.rt.Set("__METHOD", method)
	// RunString drains goja's microtask queue, settling the async signer's promise.
	if _, err := e.fire(goja.Undefined()); err != nil {
		return "", err
	}
	if errv := e.rt.Get("__grokErr"); errv != nil && !goja.IsNull(errv) && !goja.IsUndefined(errv) {
		return "", fmt.Errorf("statsig js: %s", errv.String())
	}
	res := e.rt.Get("__grokResult")
	if res == nil || goja.IsNull(res) || goja.IsUndefined(res) {
		return "", errors.New("statsig js: promise did not settle")
	}
	id := res.String()
	if id == "" {
		return "", errors.New("statsig js: empty id")
	}
	return id, nil
}

// jsBytes extracts the byte contents of a JS Uint8Array / ArrayBuffer value.
func jsBytes(rt *goja.Runtime, v goja.Value) []byte {
	if ab, ok := v.Export().(goja.ArrayBuffer); ok {
		return ab.Bytes()
	}
	obj := v.ToObject(rt)
	if buf := obj.Get("buffer"); buf != nil {
		if ab, ok := buf.Export().(goja.ArrayBuffer); ok {
			return ab.Bytes()
		}
	}
	n := int(obj.Get("length").ToInteger())
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = byte(obj.Get(strconv.Itoa(i)).ToInteger())
	}
	return out
}

// signWithEngine borrows an engine from the pool (building one on demand), signs,
// and returns it. Returns an error if the engine subsystem is not ready.
func signWithEngine(seedB64, curvesJSON, path, method string) (string, error) {
	sigMgrMu.Lock()
	src, pool := sigChunkSrc, sigPool
	sigMgrMu.Unlock()
	if src == "" || pool == nil {
		return "", errEngineNotReady
	}
	var eng *sigEngine
	select {
	case eng = <-pool:
	default:
		var err error
		if eng, err = newSigEngine(src); err != nil {
			return "", err
		}
	}
	id, err := eng.statsigID(seedB64, curvesJSON, path, method)
	select {
	case pool <- eng:
	default:
	}
	return id, err
}

// ensureEngine refreshes the global engine pool when the homepage's chunk set
// changes (i.e. grok reshipped). It locates the signer chunk build-agnostically and
// rebuilds the pool. Cheap no-op when the build is unchanged.
func ensureEngine(ctx context.Context, client tlsclient.HttpClient, homeHTML string) {
	paths := chunkPathRe.FindAllString(homeHTML, -1)
	if len(paths) == 0 {
		return
	}
	key := hashStrings(paths)

	sigMgrMu.Lock()
	unchanged := key == sigBuildKey && sigPool != nil
	sigMgrMu.Unlock()
	if unchanged {
		return
	}

	// Inputs for build-agnostic behavioral verification of candidate chunks.
	mm := statsigMetaRe.FindStringSubmatch(homeHTML)
	if mm == nil {
		log.Printf("grok statsig: no seed meta in homepage; cannot locate signer")
		return
	}
	seed, err := decodeStatsigSeed(mm[1])
	if err != nil {
		log.Printf("grok statsig: seed decode failed: %v", err)
		return
	}
	curves, err := parseStatsigCurves(homeHTML)
	if err != nil {
		log.Printf("grok statsig: curves parse failed: %v", err)
		return
	}
	cj, err := json.Marshal(curves)
	if err != nil {
		return
	}

	src, err := locateSignerChunk(ctx, client, dedupe(paths), mm[1], string(cj), seed)
	if err != nil {
		log.Printf("grok statsig: locate signer chunk failed (will use static fallback): %v", err)
		return
	}
	// smoke-test: a build must produce a loadable engine before we commit to it.
	eng, err := newSigEngine(src)
	if err != nil {
		log.Printf("grok statsig: signer chunk did not load in goja (static fallback): %v", err)
		return
	}
	pool := make(chan *sigEngine, sigPoolSize)
	pool <- eng // reuse the smoke-test engine instead of discarding it
	sigMgrMu.Lock()
	sigBuildKey = key
	sigChunkSrc = src
	sigPool = pool
	sigMgrMu.Unlock()
	log.Printf("grok statsig: self-heal engine ready (build %s..)", key[:8])
}

// locateSignerChunk finds grok's obfuscated anti-bot signer chunk build-agnostically,
// with no dependency on rotating literals (header name, class names, module ids).
// It screens every reachable chunk with a cheap obfuscator.io fingerprint, then
// confirms the true signer by RUNNING it in goja and checking its output embeds the
// homepage seed (signerEmbedsSeed). The signer is lazily loaded (not referenced in
// the HTML directly), so candidates also include the chunk paths named inside the
// homepage's Turbopack loader manifest.
func locateSignerChunk(ctx context.Context, client tlsclient.HttpClient, homeChunks []string, seedB64, curvesJSON string, seed []byte) (string, error) {
	seen := map[string]bool{}
	var lazy []string
	for _, p := range homeChunks {
		seen[p] = true
	}

	// Pass 1 (sequential): fetch the homepage chunks, verify them directly, and
	// harvest every chunk path they reference (the loader manifest lists the signer).
	for _, p := range homeChunks {
		body, err := fetchChunk(ctx, client, p)
		if err != nil {
			continue
		}
		if src, ok := verifySignerChunk(body, seedB64, curvesJSON, seed); ok {
			return src, nil
		}
		for _, ref := range allChunkRefRe.FindAllString(body, -1) {
			np := normalizeChunkPath(ref)
			if !seen[np] {
				seen[np] = true
				lazy = append(lazy, np)
			}
		}
	}

	// Pass 2 (concurrent): fetch the lazily-referenced chunks, cheap-fingerprint each,
	// behaviorally verify the matches, and stop at the first chunk that round-trips seed.
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	found := make(chan string, 1)
	sem := make(chan struct{}, locateConcurrency)
	var wg sync.WaitGroup
	for _, p := range lazy {
		if ctx2.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx2.Err() != nil {
				return
			}
			body, err := fetchChunk(ctx2, client, p)
			if err != nil {
				return
			}
			if src, ok := verifySignerChunk(body, seedB64, curvesJSON, seed); ok {
				select {
				case found <- src:
					cancel()
				default:
				}
			}
		}(p)
	}
	go func() { wg.Wait(); close(found) }()
	if src, ok := <-found; ok {
		return src, nil
	}
	return "", fmt.Errorf("statsig signer chunk not found among %d candidates", len(homeChunks)+len(lazy))
}

// verifySignerChunk returns the goja-ready source if body is grok's signer: it must
// carry the obfuscator.io fingerprint AND, when executed, produce an x-statsig-id
// whose decoded record embeds the homepage seed.
func verifySignerChunk(body, seedB64, curvesJSON string, seed []byte) (string, bool) {
	if !isObfuscatedSigner(body) {
		return "", false
	}
	clean := sourceMapRe.ReplaceAllString(body, "")
	eng, err := newSigEngine(clean)
	if err != nil {
		return "", false
	}
	id, err := eng.statsigID(seedB64, curvesJSON, "/rest/app-chat/conversations/new", "POST")
	if err != nil {
		return "", false
	}
	if !signerEmbedsSeed(id, seed) {
		return "", false
	}
	return clean, true
}

// normalizeChunkPath turns a bare or /_next/-prefixed chunk ref into a fetch path.
func normalizeChunkPath(ref string) string {
	return "/_next/" + strings.TrimPrefix(ref, "/_next/")
}

// isObfuscatedSigner cheaply screens a chunk for the obfuscator.io string-array
// decoder that grok applies ONLY to its anti-bot signer (the rest of the app is
// plain Turbopack output). The RC4-style byte decoder (`...^...%256`) is the stable
// tell; it matches a handful of chunks, which behavioral verification then narrows
// to exactly one. This is deliberately build-agnostic (no rotating class/id/header).
func isObfuscatedSigner(src string) bool {
	return strings.Contains(src, "%256") &&
		strings.Contains(src, "String.fromCharCode") &&
		strings.Contains(src, "charCodeAt")
}

// signerEmbedsSeed decodes a candidate x-statsig-id, strips the per-call XOR mask
// (plaintext[0] is 0x00, so masked[0] is the key), and checks the plaintext record
// begins with 0x00 + the exact homepage seed. Only grok's real signer round-trips
// OUR seed, so this uniquely identifies the signer chunk regardless of obfuscation.
func signerEmbedsSeed(id string, seed []byte) bool {
	raw, err := base64.RawStdEncoding.DecodeString(id)
	if err != nil || len(raw) < 1+len(seed) {
		return false
	}
	key := raw[0] // plaintext[0] is 0x00, so the mask key == masked byte 0
	for i := 0; i < len(seed); i++ {
		if raw[1+i]^key != seed[i] {
			return false
		}
	}
	return true
}

func fetchChunk(ctx context.Context, client tlsclient.HttpClient, path string) (string, error) {
	if !strings.HasPrefix(path, "http") {
		path = apiBase + path
	}
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":            {"*/*"},
		"user-agent":        {userAgent},
		http.HeaderOrderKey: {"accept", "user-agent"},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("chunk http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func hashStrings(ss []string) string {
	uniq := dedupe(ss)
	h := sha512.New()
	for _, s := range uniq {
		_, _ = io.WriteString(h, s)
		_, _ = io.WriteString(h, "\n")
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
