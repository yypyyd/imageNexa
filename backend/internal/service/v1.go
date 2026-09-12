package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"strconv"

	"backend/internal/config"
	"backend/internal/model"
	"backend/internal/netguard"
	"backend/internal/provider/adobe"
	"backend/internal/provider/byteplus"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/custom"
	"backend/internal/provider/dola"
	"backend/internal/provider/grok"
	"backend/internal/provider/oreate"
	"backend/internal/provider/runway"
	"backend/internal/repo"
	"backend/internal/storage"

	"gorm.io/gorm"
)

var (
	ErrMissingAPIKey     = errors.New("missing api key")
	ErrInvalidAPIKey     = errors.New("invalid api key")
	ErrUnknownModel      = errors.New("unknown model")
	ErrUnsupportedParams = errors.New("unsupported or unpriced parameters for this model")
	ErrBannedPrompt      = errors.New("prompt contains banned content")
	ErrInsufficientFunds = errors.New("insufficient credits")
	ErrGenerationPending = errors.New("generation executor not implemented yet")
	// ErrGenerationAccepted means a synchronous image request has durable state
	// and must be continued through its poll URL. HTTP handlers map the attached
	// task payload to 202 instead of holding the connection past proxy timeouts.
	ErrGenerationAccepted = errors.New("image generation accepted; poll the task URL")
	ErrProviderAuth       = errors.New("provider token invalid or expired")
	ErrNoProviderAccount  = errors.New("no provider account available, please ask an admin to configure one")
	// ErrProviderDisabled means every provider capable of serving the request
	// has been switched off by an administrator. It is distinct from
	// ErrUnsupportedParams: the parameters are valid, the pools are just closed.
	ErrProviderDisabled       = errors.New("all providers for this model are disabled by the administrator")
	ErrProviderQuota          = errors.New("provider quota exhausted")
	ErrProviderTemporary      = errors.New("provider temporary unavailable")
	ErrProviderExecution      = errors.New("provider request failed")
	ErrProviderUnsupported    = errors.New("provider not implemented")
	ErrReferenceTooLarge      = errors.New("reference image too large")
	ErrReferenceVideoTooLarge = errors.New("reference video too large")
	ErrReferenceAudioTooLarge = errors.New("reference audio too large")
	// Alias the provider sentinel so HTTP handlers can classify a request-level
	// moderation refusal without importing Adobe internals.
	ErrContentRejected = adobe.ErrContentRejected
	// ErrConcurrencyFull — every eligible account remained at its configured
	// concurrency limit after the bounded provider queue wait.
	ErrConcurrencyFull = errors.New("all accounts are at their concurrency limit, please try again shortly")
	// ErrUserConcurrencyFull — the caller already has their concurrency-group's max
	// generations in flight (画图台 + API key combined). 0 = unlimited.
	ErrUserConcurrencyFull = errors.New("too many generations in progress, please wait for one to finish")
	// ErrVideoJobNotFound / ErrVideoNotReady — /v1/videos async job lookups.
	ErrVideoJobNotFound    = errors.New("video job not found")
	ErrVideoNotReady       = errors.New("video is not ready yet")
	ErrImageTaskNotFound   = errors.New("image task not found")
	ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")
	// A provider account can be unable to fund one expensive request while still
	// remaining valid for cheaper work. It participates in quota failover but
	// must not be moved to the pool-wide quota state.
	errAccountTaskQuota = errors.New("provider account balance below current task cost")
	// Private scheduler markers: the provider's exact terminal beta verdict may
	// switch accounts only after live probes verify that it consumed no points,
	// and the bounded distinct-account retry chain must never fan out to another
	// route.
	errBytePlusRetryVerified        = errors.New("byteplus beta failure verified uncharged")
	errBytePlusRetryBudgetExhausted = errors.New("byteplus beta account retry budget exhausted")
)

type verifiedBytePlusRetryFailure struct {
	cause    error
	baseline float64
}

func (failure *verifiedBytePlusRetryFailure) Error() string { return failure.cause.Error() }

func (failure *verifiedBytePlusRetryFailure) Unwrap() []error {
	return []error{failure.cause, errBytePlusRetryVerified}
}

func verifiedBytePlusRetry(cause error, baseline float64) error {
	return &verifiedBytePlusRetryFailure{cause: cause, baseline: baseline}
}

func bytePlusRetryVerification(err error) (*verifiedBytePlusRetryFailure, bool) {
	var failure *verifiedBytePlusRetryFailure
	if !errors.As(err, &failure) {
		return nil, false
	}
	return failure, true
}

func finalizeBytePlusRetryVerification(err error, remaining *float64) error {
	verification, ok := bytePlusRetryVerification(err)
	if !ok {
		return err
	}
	if remaining == nil || math.Abs(*remaining-verification.baseline) > 1e-9 {
		return verification.cause
	}
	return err
}

// maxReferenceImageBytes bounds a single decoded reference image. 20 MB
// comfortably covers real photos/screenshots; anything larger is almost
// certainly abuse or a mistake. Mirrors Python core/refs.py.
const (
	maxReferenceImageBytes = 20 * 1024 * 1024
	maxReferenceVideoBytes = 200 * 1024 * 1024
	maxReferenceAudioBytes = 50 * 1024 * 1024
	videoGenerationTimeout = 35 * time.Minute
)

type V1Service struct {
	cfg      *config.Config
	models   *repo.ModelRepository
	events   *repo.EventRepository
	tokens   *repo.TokenRepository
	settings *repo.SiteSettingRepository
	apiKeys  *APICredentialService
	adobe    *adobe.Client
	byteplus *byteplus.Client
	chatgpt  *chatgpt.Client
	runway   *runway.Client
	grok     *grok.Client
	oreate   *oreate.Client
	dola     *dola.Client
	custom   *custom.Client
	store    *storage.Client
	proxyMu  sync.RWMutex
	proxies  providerProxySnapshot
	// refresh re-mints an Adobe access token from its cookie when a request hits a
	// 401 mid-flight (set via SetRefresh — wired after construction to avoid an
	// init cycle). nil for deployments without cookie refresh.
	refresh *RefreshProfileService
	// banned is the admin-managed prompt blocklist (set via SetBannedWords).
	// nil disables the check.
	banned *repo.BannedWordRepository

	// tokenCursors holds one strict round-robin cursor per pool (key: pool name,
	// value: *uint64). Each pick advances the pool's cursor by one so accounts
	// are used in a fixed, even rotation (acct1→acct2→acct3→acct1…) independent
	// of fails/last_used. The cursor distributes equivalent candidates, while
	// the atomic Redis gate remains the final authority for admission. This
	// counter is the fallback for missing/unreachable Redis — see nextCursor.
	tokenCursors sync.Map
	// acctCooldowns holds "pool:accountID" → time.Time until which an account that
	// just failed upstream is demoted to the back of its pool's rotation, so the
	// next request prefers a different account instead of immediately retrying the
	// one that just failed. Nothing is ever removed from the rotation.
	acctCooldowns sync.Map
	// oreateSpam holds accountID → time.Time until which an account that Oreate
	// answered with an explicit spam-user verdict is kept out of the rotation.
	// Unlike acctCooldowns this removes the account from the candidate set, so a
	// flagged account is not re-submitted on every request.
	oreateSpam sync.Map
	// grokBuildLocks serializes SSO->Build conversion/refresh per account. OAuth
	// refresh tokens may rotate, so concurrent first-use requests must not persist
	// different generations of the same credential.
	grokBuildLocks sync.Map
	// imageJobs coalesces opt-in async image requests by (user, idempotency key).
	// A short retention window lets polling observe validation/provider failures
	// that happened before an event row could be created.
	imageJobs sync.Map
	// imageRecoveries coalesces post-restart/post-timeout polling of one durable
	// BytePlus task. The recovery path only resumes an accepted task id; it never
	// replays create_task.
	imageRecoveries sync.Map

	// inflight maps an in-progress event ID → the cancel func of its generation
	// work context, so the maintenance sweep can stop a stuck generation the
	// moment it abandons the row (instead of letting an orphaned goroutine run on
	// for minutes and surface a late "success" on an already-abandoned event).
	inflight *InflightRegistry

	// conc is the Redis-backed concurrency limiter for BOTH the per-account
	// upstream gate (1+ jobs per account) and the per-user gate (画图台 + API key,
	// capped by the user's concurrency group). Bounded gates fail closed.
	conc *ConcurrencyService
}

// acctAcquire takes one per-account upstream slot (capped at max; 0/1 = single),
// tagged with the generation's eventID (unique per job; a generation only ever
// holds one slot on a given account at a time, so failover reuses it cleanly).
func (s *V1Service) acctAcquire(ctx context.Context, accountID, eventID string, max int) (bool, error) {
	if max < 1 {
		max = 1
	}
	return s.conc.Acquire(ctx, "conc:a:"+accountID, max, eventID)
}

func (s *V1Service) acctRelease(ctx context.Context, accountID, eventID string) {
	s.conc.Release(ctx, "conc:a:"+accountID, eventID)
}

// credentialAcquire takes one per-key generation slot. A zero limit is
// intentionally unlimited.
func (s *V1Service) credentialAcquire(ctx context.Context, principal *APIPrincipal, token string) (bool, error) {
	if principal == nil || principal.Credential == nil {
		return true, nil
	}
	return s.conc.Acquire(ctx, "conc:k:"+principal.Credential.ID, principal.Credential.ConcurrencyLimit, token)
}

func (s *V1Service) credentialRelease(ctx context.Context, credentialID, token string) {
	s.conc.Release(ctx, "conc:k:"+credentialID, token)
}

// InflightRegistry tracks the cancel func of every in-progress generation by
// event ID. The generation registers on start and removes on finish; the
// maintenance sweep calls Cancel when it gives up on (abandons) an event.
type InflightRegistry struct {
	m sync.Map // eventID -> context.CancelFunc
}

func (r *InflightRegistry) Add(eventID string, cancel context.CancelFunc) {
	if eventID != "" {
		r.m.Store(eventID, cancel)
	}
}

// Done deregisters an event (called on normal completion).
func (r *InflightRegistry) Done(eventID string) { r.m.Delete(eventID) }

// Cancel stops an in-flight generation by event ID. Returns true if one was
// running and got cancelled. No-op (false) if it already finished.
func (r *InflightRegistry) Cancel(eventID string) bool {
	if v, ok := r.m.LoadAndDelete(eventID); ok {
		v.(context.CancelFunc)()
		return true
	}
	return false
}

// Active reports whether a generation is still registered (running) for the
// event, without cancelling it. Lets the maintenance sweep tell a live long
// render apart from a process-restart orphan.
func (r *InflightRegistry) Active(eventID string) bool {
	_, ok := r.m.Load(eventID)
	return ok
}

type APIPrincipal struct {
	Credential *model.APICredential
	TokenType  string
}

func (p *APIPrincipal) ID() string {
	if p == nil || p.Credential == nil {
		return ""
	}
	return p.Credential.ID
}

type V1ImageRequest struct {
	Model     string
	Prompt    string
	RequestID string
	Size      string
	// Quality is OpenAI's image quality (low|medium|high|auto). Providers that
	// expose it independently from image dimensions receive it unchanged.
	Quality string
	// Background and OutputFormat are part of the GPT Image request surface, but
	// this multi-provider gateway cannot currently guarantee them. Explicit
	// values are rejected instead of being silently ignored.
	Background   string
	OutputFormat string
	// ResponseFormat controls the OpenAI-compatible API response. Empty defaults
	// to url for low-copy relay; b64_json is available when explicitly requested.
	ResponseFormat  string
	AspectRatio     string
	Resolution      string
	N               int
	ReferenceImages []string
	// ReferenceGrid is the legacy request flag for local face swapping. Adobe
	// references are processed automatically; the flag enables it for other
	// providers too.
	ReferenceGrid bool
	// DeAI applies 去AI特征 post-processing (crop / noise / tone jitter +
	// re-encode) to the output and charges the per-tier surcharge on top of
	// the model price. Playground-only; the /v1 OpenAI path never sets it.
	DeAI bool
	// BaseURL is the scheme+host of the inbound request (e.g. "https://host"),
	// used to build absolute, directly-downloadable output URLs. Empty falls
	// back to a relative "/images/..." path.
	BaseURL string
	// AccountID pins the generation to one specific provider account (admin
	// account-test). Empty keeps the normal pool selection with failover.
	AccountID string
}

const asyncImageJobRetention = 10 * time.Minute

type asyncImageJob struct {
	done        chan struct{}
	fingerprint string
	response    map[string]any
	err         error
}

func newAsyncImageJob(fingerprint string) *asyncImageJob {
	return &asyncImageJob{done: make(chan struct{}), fingerprint: fingerprint}
}

func (j *asyncImageJob) complete(response map[string]any, err error) {
	j.response = response
	j.err = err
	close(j.done)
}

// V1ChatResponse is a validated upstream chat-completions response. Body is
// intentionally streamed; closing it finalizes accounting and releases both
// the user and custom-account concurrency slots.
type V1ChatResponse struct {
	Header http.Header
	Body   io.ReadCloser
	Stream bool
}

type chatAccountingBody struct {
	inner     io.ReadCloser
	stream    bool
	tail      string
	sawDone   bool
	completed bool
	once      sync.Once
	finish    func(success bool, reason string, upstreamFailure bool)
}

func (b *chatAccountingBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 && b.stream {
		b.tail += string(p[:n])
		if strings.Contains(b.tail, "data: [DONE]") || strings.Contains(b.tail, "data:[DONE]") {
			b.sawDone = true
		}
		if len(b.tail) > 128 {
			b.tail = b.tail[len(b.tail)-128:]
		}
	}
	if errors.Is(err, io.EOF) {
		b.completed = true
		if !b.stream || b.sawDone {
			b.once.Do(func() { b.finish(true, "", false) })
		} else {
			b.once.Do(func() { b.finish(false, "upstream stream ended without [DONE]", true) })
		}
	} else if err != nil {
		b.once.Do(func() { b.finish(false, "upstream stream read failed", true) })
	}
	return n, err
}

func (b *chatAccountingBody) Close() error {
	err := b.inner.Close()
	if !b.completed {
		b.once.Do(func() { b.finish(false, "client disconnected before completion", false) })
	}
	return err
}

func normalizeImageResponseFormat(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "url":
		return "url", nil
	case "b64_json":
		return "b64_json", nil
	default:
		return "", fmt.Errorf("%w: response_format must be b64_json or url", ErrUnsupportedParams)
	}
}

type V1VideoRequest struct {
	Model           string
	Prompt          string
	RequestID       string
	Duration        string
	AspectRatio     string
	Resolution      string
	ReferenceImages []string
	ReferenceGrid   bool
	ReferenceVideos []MediaReference
	ReferenceAudios []MediaReference
	GenerateAudio   bool
	// BaseURL — see V1ImageRequest.BaseURL.
	BaseURL string
	// AccountID — see V1ImageRequest.AccountID.
	AccountID string
}

// MediaReference owns the uploaded bytes so asynchronous /v1/videos jobs do not
// retain multipart file handles after the HTTP request has returned.
type MediaReference struct {
	Data        []byte
	ContentType string
	Filename    string
}

func NewV1Service(cfg *config.Config, models *repo.ModelRepository, apiKeys *APICredentialService, events *repo.EventRepository, tokens *repo.TokenRepository, settings *repo.SiteSettingRepository, conc *ConcurrencyService, adobeClient *adobe.Client, bytePlusClient *byteplus.Client, chatGPTClient *chatgpt.Client, runwayClient *runway.Client, grokClient *grok.Client, oreateClient *oreate.Client, dolaClient *dola.Client, customClient *custom.Client, store *storage.Client) *V1Service {
	service := &V1Service{
		cfg:      cfg,
		models:   models,
		events:   events,
		tokens:   tokens,
		settings: settings,
		apiKeys:  apiKeys,
		conc:     conc,
		adobe:    adobeClient,
		byteplus: bytePlusClient,
		chatgpt:  chatGPTClient,
		runway:   runwayClient,
		grok:     grokClient,
		oreate:   oreateClient,
		dola:     dolaClient,
		custom:   customClient,
		store:    store,
		inflight: &InflightRegistry{},
	}
	if settings != nil {
		if snap, err := loadProviderProxies(context.Background(), settings); err == nil {
			service.proxies = snap
			service.applyProviderProxySnapshot(snap)
		}
	}
	return service
}

// Inflight exposes the registry so the maintenance sweep can cancel a stuck
// generation when it abandons that event.
func (s *V1Service) Inflight() *InflightRegistry { return s.inflight }

// SetRefresh wires the Adobe cookie-refresh service in after construction
// (RefreshProfileService is built later in bootstrap, so it can't be a ctor arg
// without reordering). Enables refresh-then-retry on a mid-request 401.
func (s *V1Service) SetRefresh(r *RefreshProfileService) { s.refresh = r }

// SetBannedWords wires the prompt blocklist in after construction.
func (s *V1Service) SetBannedWords(r *repo.BannedWordRepository) { s.banned = r }

// applyGlobalProxy snapshots each channel's residential route onto its client.
// Extract APIs, when set, batch-lease dedicated exits per account. ChatGPT /
// Grok / Dola / Oreate still fall back to proxy.url when extract fails or is
// empty. Adobe and BytePlus stay direct unless their own extract API or URL is set.
func (s *V1Service) applyGlobalProxy(ctx context.Context) string {
	snap, err := loadProviderProxies(ctx, s.settings)
	if err != nil {
		s.proxyMu.RLock()
		snap = s.proxies
		s.proxyMu.RUnlock()
		s.applyProviderProxySnapshot(snap)
		return snap.Default
	}
	s.proxyMu.Lock()
	s.proxies = snap
	s.proxyMu.Unlock()
	s.applyProviderProxySnapshot(snap)
	return snap.Default
}

func (s *V1Service) applyProviderProxySnapshot(snap providerProxySnapshot) {
	assignProviderProxies(snap, s.chatgpt, s.grok, s.oreate, s.dola, s.adobe, s.byteplus)
}

func globalProxyHTTPClient(proxyRaw string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Explicitly disable ProxyFromEnvironment. Only the persisted administrator
	// setting controls account egress; empty means local direct.
	transport.Proxy = nil
	proxyRaw = strings.TrimSpace(proxyRaw)
	if proxyRaw != "" {
		proxyURL, err := url.Parse(proxyRaw)
		if err != nil || proxyURL.Host == "" {
			return nil, errors.New("invalid global proxy configuration")
		}
		switch strings.ToLower(proxyURL.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, errors.New("unsupported global proxy scheme")
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// checkBannedPrompt rejects the request when the prompt contains any banned
// word (case-insensitive substring). A hit bumps the word's counter and the
// user's 违禁词触发次数 before rejecting.
func (s *V1Service) checkBannedPrompt(ctx context.Context, principal *APIPrincipal, prompt string) error {
	if s.banned == nil || strings.TrimSpace(prompt) == "" {
		return nil
	}
	words, err := s.banned.List(ctx)
	if err != nil || len(words) == 0 {
		return nil
	}
	lower := strings.ToLower(prompt)
	for _, w := range words {
		term := strings.ToLower(strings.TrimSpace(w.Word))
		if term == "" || !strings.Contains(lower, term) {
			continue
		}
		userID, userName := "", ""
		if principal != nil && principal.Credential != nil {
			userID = principal.Credential.ID
			userName = principal.Credential.Name
		}
		s.banned.RecordHit(ctx, w.ID, w.Word, userID, userName, prompt)
		return fmt.Errorf("%w: banned word \"%s\"", ErrBannedPrompt, w.Word)
	}
	return nil
}

// logRejectedEvent records a request rejected BEFORE the pending event exists
// (banned word, concurrency full, unknown model, insufficient credits…) as a
// failed event, so every attempt shows up in the logs.
func (s *V1Service) logRejectedEvent(ctx context.Context, kind, modelID string, principal *APIPrincipal, prompt, source, reason string) {
	event := &model.EventLog{
		ID:        "evt-" + randomUpper(12),
		TS:        time.Now(),
		Kind:      kind,
		Status:    "failed",
		Model:     strings.TrimSpace(modelID),
		Prompt:    prompt,
		Source:    source,
		Error:     reason,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if m, err := s.models.Get(ctx, event.Model); err == nil {
		event.Model = m.ID
	}
	if principal != nil && principal.Credential != nil {
		event.APICredentialID = principal.Credential.ID
	}
	_ = s.events.Create(ctx, event)
}

// refreshAdobeToken re-mints an Adobe account's access token from its cookie
// (RefreshNow) and returns the updated row. Used to retry a 401 with a fresh
// token instead of replaying the stale one. Returns false if refresh is
// unavailable or the cookie can no longer mint a token (genuinely dead).
func (s *V1Service) refreshAdobeToken(ctx context.Context, tokenID string) (model.TokenAccount, bool) {
	if s.refresh == nil {
		return model.TokenAccount{}, false
	}
	if err := s.refresh.RefreshNow(ctx, tokenID); err != nil {
		return model.TokenAccount{}, false
	}
	t, err := s.tokens.Get(ctx, "adobe", tokenID)
	if err != nil || t == nil {
		return model.TokenAccount{}, false
	}
	return *t, true
}

func (s *V1Service) Authenticate(ctx context.Context, authHeader string) (*APIPrincipal, error) {
	if ParseBearer(authHeader) == "" {
		return nil, ErrMissingAPIKey
	}
	credential, err := s.apiKeys.AuthenticateBearer(ctx, authHeader)
	if err != nil {
		if errors.Is(err, ErrInvalidAPICredential) {
			return nil, ErrInvalidAPIKey
		}
		return nil, err
	}
	return &APIPrincipal{
		Credential: credential,
		TokenType:  "user",
	}, nil
}

func (s *V1Service) ListModels(ctx context.Context, extended bool) ([]map[string]any, error) {
	items, err := s.models.Routes().ListLogical(ctx, true)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		routes, routeErr := s.models.Routes().ListRoutes(ctx, item.ID, true)
		if routeErr != nil {
			return nil, routeErr
		}
		if len(routes) == 0 {
			continue
		}
		created := now
		if !item.CreatedAt.IsZero() {
			created = item.CreatedAt.Unix()
		}
		out = append(out, v1LogicalModelEntry(item, routes, created, extended))
	}
	return out, nil
}

func v1LogicalModelEntry(item model.LogicalModel, routes []model.ModelRoute, created int64, extended bool) map[string]any {
	entry := map[string]any{"id": item.ID, "object": "model", "created": created, "owned_by": "2api", "shutdown_date": nil}
	if !extended {
		return entry
	}
	entry["kind"], entry["type"], entry["modality"], entry["name"] = item.Kind, item.Kind, item.Kind, item.Name
	var ratios, resolutions, durations, operations []string
	profiles := make([]map[string]any, 0)
	seenProfiles := map[string]bool{}
	maxImages, maxVideos, maxAudios, maxMedia, audioOutput := 0, 0, 0, 0, false
	for _, route := range routes {
		for _, profile := range model.DecodeCapabilityProfiles(route.Capabilities) {
			ratios = unionStrings(ratios, profile.Ratios)
			resolutions = unionStrings(resolutions, profile.Resolutions)
			durations = unionStrings(durations, profile.Durations)
			operations = unionStrings(operations, profile.Operations)
			maxImages, maxVideos = max(maxImages, profile.MaxReferenceImages), max(maxVideos, profile.MaxReferenceVideos)
			maxAudios, maxMedia = max(maxAudios, profile.MaxReferenceAudios), max(maxMedia, profile.MaxReferenceMedia)
			audioOutput = audioOutput || profile.SupportsAudioOutput
			raw, _ := json.Marshal(profile)
			if !seenProfiles[string(raw)] {
				seenProfiles[string(raw)] = true
				var anonymous map[string]any
				_ = json.Unmarshal(raw, &anonymous)
				profiles = append(profiles, anonymous)
			}
		}
	}
	sort.Strings(ratios)
	sort.Strings(resolutions)
	sort.Strings(durations)
	sort.Strings(operations)
	entry["supported_ratios"], entry["ratios"], entry["aspectRatios"] = ratios, ratios, ratios
	entry["supported_resolutions"], entry["resolutions"], entry["resolutionTiers"] = resolutions, resolutions, resolutions
	entry["supported_durations"], entry["durations"], entry["supportedDurations"], entry["durationTiers"] = durations, durations, durations, durations
	entry["operations"], entry["capability_profiles"] = operations, profiles
	entry["max_reference_images"], entry["max_reference_videos"] = maxImages, maxVideos
	entry["max_reference_audios"], entry["max_reference_media"] = maxAudios, maxMedia
	entry["supports_audio_output"] = audioOutput
	entry["maxReferenceImages"], entry["maxReferenceVideos"] = maxImages, maxVideos
	entry["maxReferenceAudios"], entry["maxReferenceMedia"] = maxAudios, maxMedia
	entry["supportsAudioOutput"] = audioOutput
	return entry
}

func unionStrings(dst, src []string) []string {
	for _, candidate := range src {
		found := false
		for _, existing := range dst {
			if strings.EqualFold(existing, candidate) {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, candidate)
		}
	}
	return dst
}

// PrepareChatCompletion validates and bills an OpenAI-compatible chat request,
// chooses a matching ChatGPT or custom upstream account, and returns JSON/SSE.
// Text pricing is fixed per request at prices.request (agent override:
// prices_agent.request). A response is successful only after a valid non-stream
// completion reaches EOF or an SSE stream reaches data: [DONE].
func (s *V1Service) PrepareChatCompletion(ctx context.Context, principal *APIPrincipal, payload []byte) (*V1ChatResponse, error) {
	return s.prepareChatCompletion(ctx, principal, payload, "", "v1")
}

// PrepareAdminChatTest runs one non-streaming text request on a specifically
// selected provider account. It uses the normal routing/error/accounting path
// but does not debit the administrator's user balance.
func (s *V1Service) PrepareAdminChatTest(ctx context.Context, modelName, prompt, accountID string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"model":    modelName,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
		"stream":   false,
	})
	response, err := s.prepareChatCompletion(ctx, nil, payload, accountID, "admin")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil || len(body) > 4<<20 {
		return "", fmt.Errorf("%w: invalid chat test response", ErrProviderTemporary)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &result) != nil || len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("%w: empty chat test response", ErrProviderTemporary)
	}
	return result.Choices[0].Message.Content, nil
}

// AdminTestRequest is the administrator-only account capability probe. The
// account id is mandatory: a test must never silently fall back to another
// account in the provider pool.
type AdminTestRequest struct {
	Model       string
	Prompt      string
	AspectRatio string
	Resolution  string
	Duration    string
	AccountID   string
}

// PrepareAdminTest runs a canonical text, image, or video model through one
// explicitly selected provider account without charging a downstream API key.
func (s *V1Service) PrepareAdminTest(ctx context.Context, in AdminTestRequest) (map[string]any, error) {
	in.Model = strings.TrimSpace(in.Model)
	in.Prompt = strings.TrimSpace(in.Prompt)
	in.AccountID = strings.TrimSpace(in.AccountID)
	if in.Model == "" || in.Prompt == "" || in.AccountID == "" {
		return nil, fmt.Errorf("%w: model, prompt and account_id are required", ErrUnsupportedParams)
	}
	modelItem, err := s.models.Get(ctx, in.Model)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUnknownModel
		}
		return nil, err
	}
	if !modelItem.Enabled {
		return nil, ErrUnknownModel
	}
	startedAt := time.Now()
	switch modelItem.Type {
	case "text":
		content, err := s.PrepareAdminChatTest(ctx, in.Model, in.Prompt, in.AccountID)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"kind": "text", "content": content,
			"elapsed_ms": time.Since(startedAt).Milliseconds(),
		}, nil
	case "image":
		result, err := s.prepareImageExecution(ctx, nil, V1ImageRequest{
			Model: in.Model, Prompt: in.Prompt, AspectRatio: in.AspectRatio,
			Resolution: in.Resolution, AccountID: in.AccountID,
		}, "admin", false)
		if err != nil {
			return nil, err
		}
		if eventID, _ := result["event_id"].(string); eventID != "" {
			result["url"] = "/admin/api/test/artifacts/" + eventID
		}
		return result, nil
	case "video":
		return s.prepareAdminVideoTest(ctx, V1VideoRequest{
			Model: in.Model, Prompt: in.Prompt, AspectRatio: in.AspectRatio,
			Resolution: in.Resolution, Duration: in.Duration, AccountID: in.AccountID,
		})
	default:
		return nil, ErrUnknownModel
	}
}

func (s *V1Service) prepareChatCompletion(ctx context.Context, principal *APIPrincipal, payload []byte, accountID, source string) (*V1ChatResponse, error) {
	s.applyGlobalProxy(ctx)
	if source == "" {
		source = "v1"
	}
	var request map[string]any
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&request); err != nil {
		return nil, fmt.Errorf("%w: invalid request body", ErrUnsupportedParams)
	}
	modelName := strings.TrimSpace(stringValue(request["model"]))
	messages, ok := request["messages"].([]any)
	if modelName == "" || !ok || len(messages) == 0 {
		return nil, fmt.Errorf("%w: model and messages are required", ErrUnsupportedParams)
	}
	stream, _ := request["stream"].(bool)
	prompt := chatPrompt(messages)

	logicalItem, err := s.models.Get(ctx, modelName)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUnknownModel
		}
		return nil, err
	}
	if !logicalItem.Enabled || logicalItem.Type != "text" {
		return nil, ErrUnknownModel
	}
	requirements := model.RouteRequirements{Operation: "completion"}
	routes, err := s.matchingRoutes(ctx, logicalItem.ID, requirements)
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, ErrUnsupportedParams
	}
	if err := s.checkBannedPrompt(ctx, principal, prompt); err != nil {
		s.logRejectedEvent(context.WithoutCancel(ctx), "text", modelName, principal, prompt, source, err.Error())
		return nil, err
	}

	bookCtx := context.WithoutCancel(ctx)
	userSlot := randomUpper(12)
	if principal != nil && principal.Credential != nil {
		admitted, gateErr := s.credentialAcquire(bookCtx, principal, userSlot)
		if gateErr != nil {
			return nil, gateErr
		}
		if !admitted {
			s.logRejectedEvent(bookCtx, "text", modelName, principal, prompt, source, ErrUserConcurrencyFull.Error())
			return nil, ErrUserConcurrencyFull
		}
	}
	userHeld := principal != nil && principal.Credential != nil
	releaseUser := func() {
		if userHeld {
			s.credentialRelease(bookCtx, principal.Credential.ID, userSlot)
			userHeld = false
		}
	}

	price, err := s.chargeForModel(bookCtx, principal, logicalItem, "text", "request", "", 0, true)
	if err != nil {
		releaseUser()
		s.logRejectedEvent(bookCtx, "text", modelName, principal, prompt, source, err.Error())
		return nil, err
	}
	eventID, err := s.logPendingEvent(bookCtx, "text", logicalItem, principal, prompt, "", "", "", 0, price, "", source, nil, false, "", "")
	if err != nil {
		releaseUser()
		return nil, err
	}
	startedAt := time.Now()
	response, routeErr := runTextRouteFailover(routes, func(route model.ModelRoute) (*V1ChatResponse, error) {
		modelItem, configErr := s.models.Routes().RouteConfig(ctx, route)
		if configErr != nil {
			return nil, configErr
		}
		pool := route.Provider
		var active []model.TokenAccount
		switch pool {
		case "custom":
			active, configErr = s.customActive(ctx, route.LogicalModelID)
		case "chatgpt", "grok":
			var items []model.TokenAccount
			items, configErr = s.tokens.ListByPool(ctx, pool)
			if configErr == nil {
				for _, item := range items {
					// Media quota states do not disable text chat. Durable route
					// bindings and text-specific cooldowns are applied below.
					if (item.Status == "active" || item.Status == "quota") && !item.Dead && strings.TrimSpace(item.Value) != "" {
						active = append(active, item)
					}
				}
				s.rotateRoundRobin(pool, active)
			}
		default:
			return nil, ErrProviderUnsupported
		}
		if configErr != nil {
			return nil, configErr
		}
		routeCtx := withDispatchRoute(ctx, route, defaultRouteCost(route, requirements))
		active, configErr = s.routeAccounts(routeCtx, route, active)
		if configErr != nil {
			return nil, configErr
		}
		active = pinTestAccount(active, active, accountID)
		if len(active) == 0 || pool == "custom" && s.custom == nil || pool == "chatgpt" && s.chatgpt == nil || pool == "grok" && s.grok == nil {
			return nil, ErrNoProviderAccount
		}
		recordBookkeepingError("set chat provider", s.events.SetProvider(bookCtx, eventID, pool))

		upstreamModel := strings.TrimSpace(modelItem.UpstreamModel)
		if upstreamModel == "" {
			upstreamModel = modelItem.ID
		}
		var lastErr error
		busy := 0
		for _, token := range active {
			slots := poolAccountConcurrency(pool, token)
			admitted, gateErr := s.acctAcquire(bookCtx, token.ID, eventID, slots)
			if gateErr != nil {
				return nil, gateErr
			}
			if !admitted {
				busy++
				continue
			}
			current, admissionErr := s.revalidateDispatchAccount(routeCtx, pool, token.ID, "text", false)
			if admissionErr != nil {
				s.acctRelease(bookCtx, token.ID, eventID)
				if errors.Is(admissionErr, ErrNoProviderAccount) || routeFailoverSafe(admissionErr) {
					lastErr = admissionErr
					continue
				}
				return nil, admissionErr
			}
			token = current
			recordBookkeepingError("set chat account", s.events.SetAccount(bookCtx, eventID, token.ID, token.AccountEmail))
			recordBookkeepingError("touch chat account", s.tokens.TouchLastUsed(bookCtx, token.ID))
			dispatch, dispatchErr := s.models.Dispatch().Start(bookCtx, eventID, route.ID, token.ID)
			if dispatchErr != nil {
				s.acctRelease(bookCtx, token.ID, eventID)
				return nil, dispatchErr
			}
			var responseHeader http.Header
			var responseBody io.ReadCloser
			responseStream := stream
			var callErr error
			switch pool {
			case "custom":
				response, upstreamErr := s.custom.ChatCompletions(ctx, stringValue(token.Meta["base_url"]), token.Value, upstreamModel, payload, stream)
				callErr = upstreamErr
				if response != nil {
					responseHeader, responseBody, responseStream = response.Header, response.Body, response.Stream
				}
			case "grok":
				var text string
				if grok.IsBuildTextModel(upstreamModel) {
					var accessToken string
					accessToken, callErr = s.ensureGrokBuildCredential(ctx, token)
					if callErr == nil {
						text, callErr = s.grok.GenerateBuildText(ctx, accessToken, prompt, upstreamModel)
					}
				} else {
					text, callErr = s.grok.GenerateText(ctx, token.Value, prompt, grok.ChatModeForModel(upstreamModel))
				}
				if callErr == nil {
					responseHeader, responseBody = openAITextResponse(modelItem.EffectiveName(), text, stream)
				}
			case "chatgpt":
				var text string
				text, callErr = s.chatgpt.GenerateText(ctx, token.Value, prompt, upstreamModel)
				if callErr == nil {
					responseHeader, responseBody = openAITextResponse(modelItem.EffectiveName(), text, stream)
				}
			}
			if callErr == nil && responseBody == nil {
				callErr = ErrProviderExecution
			}
			// Every real text attempt participates in the same quota refresh
			// contract. Text-only balances remain independent from media quota.
			_ = s.refreshDispatchQuota(routeCtx, pool, token, "text")
			if callErr != nil {
				s.acctRelease(bookCtx, token.ID, eventID)
				lastErr = callErr
				state, failureClass := dispatchFailureClass(callErr)
				recordBookkeepingError("finish failed chat dispatch", s.models.Dispatch().Finish(bookCtx, dispatch.ID, state, failureClass, "", publicGenerationError(callErr)))
				recordBookkeepingError("record failed chat route", s.models.Routes().RecordAccountRouteResult(bookCtx, token.ID, route.ID, failureClass, false))
				switch {
				case errors.Is(callErr, grok.ErrChallenge):
					// Statsig is process-wide and account-independent. Rotating a
					// route would repeat the rejected signature.
					return nil, callErr
				case errors.Is(callErr, custom.ErrAuth), errors.Is(callErr, chatgpt.ErrAuth), errors.Is(callErr, grok.ErrAuth):
					s.markTokenFailure(bookCtx, pool, token, "text", true, false)
					continue
				case errors.Is(callErr, grok.ErrQuotaExhausted):
					// Grok chat throttling is separate from its media credits.
					s.markTokenFailure(bookCtx, pool, token, "text", false, false)
					continue
				case errors.Is(callErr, custom.ErrQuotaExhausted), errors.Is(callErr, chatgpt.ErrQuotaExhausted):
					s.markTokenFailure(bookCtx, pool, token, "text", false, true)
					continue
				case errors.Is(callErr, custom.ErrTemporaryUpstream), errors.Is(callErr, chatgpt.ErrTemporaryUpstream), errors.Is(callErr, grok.ErrTemporaryUpstream):
					s.markTokenFailure(bookCtx, pool, token, "text", false, false)
					continue
				default:
					return nil, callErr
				}
			}

			body := &chatAccountingBody{inner: responseBody, stream: responseStream}
			body.finish = func(success bool, reason string, upstreamFailure bool) {
				defer s.acctRelease(bookCtx, token.ID, eventID)
				defer releaseUser()
				elapsed := int(time.Since(startedAt).Milliseconds())
				if success {
					_, updateErr := s.tokens.Update(bookCtx, pool, token.ID, map[string]any{
						"last_used_at": time.Now(), "success_total": gorm.Expr("success_total + 1"), "fails": 0,
					})
					recordBookkeepingError("record chat account success", updateErr)
					recordBookkeepingError("complete chat event", s.events.UpdateStatus(bookCtx, eventID, "success", "", elapsed))
					recordBookkeepingError("complete chat dispatch", s.models.Dispatch().Finish(bookCtx, dispatch.ID, "succeeded", "", "", nil))
					recordBookkeepingError("record chat route success", s.models.Routes().RecordAccountRouteResult(bookCtx, token.ID, route.ID, "", true))
					recordBookkeepingError("increment chat generation", s.models.IncrementGenerationCount(bookCtx, logicalItem.ID))
					return
				}
				failureClass := "request"
				if upstreamFailure {
					s.markTokenFailure(bookCtx, pool, token, "text", false, false)
					failureClass = "temporary"
				}
				recordBookkeepingError("fail chat event", s.events.UpdateStatus(bookCtx, eventID, "failed", safeStoredGenerationError(reason), elapsed))
				recordBookkeepingError("fail chat dispatch", s.models.Dispatch().Finish(bookCtx, dispatch.ID, "failed", failureClass, "", errors.New(reason)))
				recordBookkeepingError("record failed chat stream route", s.models.Routes().RecordAccountRouteResult(bookCtx, token.ID, route.ID, failureClass, false))
				_ = s.refundIfNeeded(bookCtx, principal, eventID, price)
			}
			return &V1ChatResponse{Header: responseHeader, Body: body, Stream: responseStream}, nil
		}
		if lastErr != nil {
			return nil, lastErr
		}
		if busy > 0 {
			return nil, ErrConcurrencyFull
		}
		return nil, ErrNoProviderAccount
	})
	if routeErr != nil {
		return nil, s.failChatCompletion(bookCtx, principal, eventID, price, releaseUser, routeErr)
	}
	return response, nil
}

func (s *V1Service) failChatCompletion(ctx context.Context, principal *APIPrincipal, eventID string, price float64, releaseUser func(), cause error) error {
	releaseUser()
	publicErr := publicGenerationError(cause)
	recordBookkeepingError("update failed chat event", s.events.UpdateStatus(ctx, eventID, "failed", safeGenerationErrorText(cause), 0))
	_ = s.refundIfNeeded(ctx, principal, eventID, price)
	return publicErr
}

func openAITextResponse(modelName, content string, stream bool) (http.Header, io.ReadCloser) {
	id := "chatcmpl-" + randomUpper(16)
	created := time.Now().Unix()
	contentType := "application/json"
	var raw []byte
	if !stream {
		raw, _ = json.Marshal(map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": modelName,
			"choices": []map[string]any{{
				"index": 0, "message": map[string]any{"role": "assistant", "content": content},
				"logprobs": nil, "finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
	} else {
		contentType = "text/event-stream"
		first, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": modelName,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}},
		})
		last, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": modelName,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		})
		raw = []byte("data: " + string(first) + "\n\ndata: " + string(last) + "\n\ndata: [DONE]\n\n")
	}
	return http.Header{"Content-Type": []string{contentType}}, io.NopCloser(bytes.NewReader(raw))
}

func chatPrompt(messages []any) string {
	parts := make([]string, 0, len(messages))
	var collect func(any, *[]string)
	collect = func(v any, out *[]string) {
		switch x := v.(type) {
		case string:
			if strings.TrimSpace(x) != "" {
				*out = append(*out, x)
			}
		case []any:
			for _, item := range x {
				collect(item, out)
			}
		case map[string]any:
			if text, ok := x["text"]; ok {
				collect(text, out)
			} else if content, ok := x["content"]; ok {
				collect(content, out)
			}
		}
	}
	for _, raw := range messages {
		if message, ok := raw.(map[string]any); ok {
			var content []string
			collect(message["content"], &content)
			if len(content) > 0 {
				role := strings.ToUpper(strings.TrimSpace(stringValue(message["role"])))
				if role == "" {
					role = "USER"
				}
				parts = append(parts, role+": "+strings.Join(content, "\n"))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func (s *V1Service) PrepareImageRequest(ctx context.Context, principal *APIPrincipal, in V1ImageRequest) (map[string]any, error) {
	in.RequestID = strings.TrimSpace(in.RequestID)
	if in.RequestID == "" {
		return s.prepareImageExecution(ctx, principal, in, "v1", true)
	}
	if principal == nil || principal.Credential == nil {
		return nil, ErrInvalidAPIKey
	}
	if len(in.RequestID) > 191 {
		return nil, fmt.Errorf("%w: idempotency key is too long", ErrUnsupportedParams)
	}
	canonical, fingerprint, err := canonicalImageIdempotencyInput(in)
	if err != nil {
		return nil, err
	}
	in = canonical
	if existing, err := s.imageTaskFromEvent(ctx, principal, in.RequestID, fingerprint); err == nil {
		return s.waitForImageResult(ctx, principal, in.RequestID, fingerprint, existing)
	} else if !errors.Is(err, ErrImageTaskNotFound) {
		return nil, err
	}

	key := imageJobKey(principal.Credential.ID, in.RequestID)
	candidate := newAsyncImageJob(fingerprint)
	actual, loaded := s.imageJobs.LoadOrStore(key, candidate)
	job := actual.(*asyncImageJob)
	if loaded {
		if job.fingerprint != fingerprint {
			return nil, ErrIdempotencyConflict
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: waiting for idempotent image request: %v", ErrProviderTemporary, ctx.Err())
		case <-job.done:
			if noRouteFailover(job.err) {
				if existing, lookupErr := s.imageTaskFromEvent(ctx, principal, in.RequestID, fingerprint); lookupErr == nil {
					return s.waitForImageResult(ctx, principal, in.RequestID, fingerprint, existing)
				}
			}
			return job.response, job.err
		}
	}

	lockKey := "idem:image:" + key
	lockToken := randomUpper(24)
	lockAcquired, lockErr := s.conc.Acquire(ctx, lockKey, 1, lockToken)
	if lockErr != nil {
		s.imageJobs.CompareAndDelete(key, candidate)
		return nil, lockErr
	}
	if !lockAcquired {
		s.imageJobs.CompareAndDelete(key, candidate)
		return s.waitForImageResult(ctx, principal, in.RequestID, fingerprint, nil)
	}
	response, execErr := s.prepareImageExecution(ctx, principal, in, "v1", true, fingerprint)
	if execErr != nil {
		if existing, lookupErr := s.imageTaskFromEvent(ctx, principal, in.RequestID, fingerprint); lookupErr == nil {
			response, execErr = s.waitForImageResult(ctx, principal, in.RequestID, fingerprint, existing)
		}
	}
	candidate.complete(response, execErr)
	s.conc.Release(context.Background(), lockKey, lockToken)
	time.AfterFunc(asyncImageJobRetention, func() { s.imageJobs.CompareAndDelete(key, candidate) })
	return response, execErr
}

// StartImageRequest starts an opt-in asynchronous OpenAI image request. The
// caller receives a task object immediately and polls ImageTask. Requests with
// the same user + idempotency key share one execution, preventing retry storms
// from charging or rendering the same image more than once.
func (s *V1Service) StartImageRequest(ctx context.Context, principal *APIPrincipal, in V1ImageRequest) (map[string]any, bool, error) {
	in.RequestID = strings.TrimSpace(in.RequestID)
	if principal == nil || principal.Credential == nil {
		return nil, false, ErrInvalidAPIKey
	}
	if in.RequestID == "" {
		return nil, false, fmt.Errorf("%w: idempotency key is required for async image requests", ErrUnsupportedParams)
	}
	if len(in.RequestID) > 191 {
		return nil, false, fmt.Errorf("%w: idempotency key is too long", ErrUnsupportedParams)
	}
	canonical, fingerprint, err := canonicalImageIdempotencyInput(in)
	if err != nil {
		return nil, false, err
	}
	in = canonical

	if existing, err := s.imageTaskFromEvent(ctx, principal, in.RequestID, fingerprint); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrImageTaskNotFound) {
		return nil, false, err
	}

	key := imageJobKey(principal.Credential.ID, in.RequestID)
	candidate := newAsyncImageJob(fingerprint)
	actual, loaded := s.imageJobs.LoadOrStore(key, candidate)
	job := actual.(*asyncImageJob)
	if loaded {
		if job.fingerprint != fingerprint {
			return nil, false, ErrIdempotencyConflict
		}
		response, pending := asyncImageJobResponse(job, in.RequestID, s.imageTaskURL(in.RequestID, in.BaseURL))
		return response, pending, nil
	}

	lockKey := "idem:image:" + key
	lockToken := randomUpper(24)
	lockAcquired, lockErr := s.conc.Acquire(ctx, lockKey, 1, lockToken)
	if lockErr != nil {
		s.imageJobs.CompareAndDelete(key, candidate)
		return nil, false, lockErr
	}
	if !lockAcquired {
		s.imageJobs.CompareAndDelete(key, candidate)
		if existing, err := s.imageTaskFromEvent(ctx, principal, in.RequestID, fingerprint); err == nil {
			return existing, false, nil
		} else if !errors.Is(err, ErrImageTaskNotFound) {
			return nil, false, err
		}
		return asyncImagePendingResponse(in.RequestID, s.imageTaskURL(in.RequestID, in.BaseURL)), true, nil
	}

	executionCtx := context.WithoutCancel(ctx)
	go func() {
		response, err := s.prepareImageExecution(executionCtx, principal, in, "v1", true, fingerprint)
		candidate.complete(response, err)
		s.conc.Release(context.Background(), lockKey, lockToken)
		time.AfterFunc(asyncImageJobRetention, func() {
			s.imageJobs.CompareAndDelete(key, candidate)
		})
	}()

	return asyncImagePendingResponse(in.RequestID, s.imageTaskURL(in.RequestID, in.BaseURL)), true, nil
}

func (s *V1Service) waitForImageResult(ctx context.Context, principal *APIPrincipal, requestID, fingerprint string, first map[string]any) (map[string]any, error) {
	deadline := time.NewTimer(12 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	current := first
	for {
		if current == nil {
			result, err := s.imageTaskFromEvent(ctx, principal, requestID, fingerprint)
			if err != nil && !errors.Is(err, ErrImageTaskNotFound) {
				return nil, err
			}
			current = result
		}
		if current != nil {
			switch current["status"] {
			case "completed":
				return map[string]any{
					"created": current["created"], "data": current["data"], "model": current["model"], "kind": "image",
				}, nil
			case "failed":
				return nil, fmt.Errorf("%w: previous idempotent image request failed", ErrProviderExecution)
			case "queued", "in_progress":
				// The event row is durable and already carries request_id/poll_url.
				// Do not add another 12-minute synchronous wait after BytePlus has
				// reported accepted/unknown; reverse proxies commonly time out first
				// and hide the recovery headers from the caller.
				return current, ErrGenerationAccepted
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: waiting for idempotent image request: %v", ErrProviderTemporary, ctx.Err())
		case <-deadline.C:
			return nil, fmt.Errorf("%w: waiting for idempotent image request timed out", ErrProviderTemporary)
		case <-ticker.C:
			current = nil
		}
	}
}

func imageRequestFingerprint(in V1ImageRequest) string {
	referenceHashes := make([]string, 0, len(in.ReferenceImages))
	for _, reference := range in.ReferenceImages {
		sum := sha256.Sum256([]byte(reference))
		referenceHashes = append(referenceHashes, hex.EncodeToString(sum[:]))
	}
	payload := struct {
		Model, Prompt, Size, Quality, ResponseFormat, Background, OutputFormat, AspectRatio, Resolution string
		N                                                                                               int
		References                                                                                      []string
		ReferenceGrid, DeAI                                                                             bool
	}{strings.TrimSpace(in.Model), in.Prompt, strings.TrimSpace(in.Size), strings.TrimSpace(in.Quality),
		strings.TrimSpace(in.ResponseFormat), strings.TrimSpace(in.Background), strings.TrimSpace(in.OutputFormat), strings.TrimSpace(in.AspectRatio), strings.TrimSpace(in.Resolution), in.N, referenceHashes,
		in.ReferenceGrid, in.DeAI}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// canonicalImageIdempotencyInput normalizes only request fields whose omitted
// and explicit forms execute identically. It intentionally hashes references
// before reference-grid transformation, then the same fingerprint is carried
// all the way to the event row. Recomputing after mutation makes an exact retry
// look like a conflicting request.
func canonicalImageIdempotencyInput(in V1ImageRequest) (V1ImageRequest, string, error) {
	in.RequestID = strings.TrimSpace(in.RequestID)
	if in.N == 0 {
		in.N = 1
	}
	responseFormat, err := normalizeImageResponseFormat(in.ResponseFormat)
	if err != nil {
		return in, "", err
	}
	in.ResponseFormat = responseFormat
	return in, imageRequestFingerprint(in), nil
}

func imageJobKey(credentialID, requestID string) string {
	return strings.TrimSpace(credentialID) + ":" + strings.TrimSpace(requestID)
}

func (s *V1Service) imageTaskURL(requestID, requestBaseURL string) string {
	path := "/v1/images/tasks?request_id=" + url.QueryEscape(strings.TrimSpace(requestID))
	if base := s.outputBaseURL(requestBaseURL); base != "" {
		return base + path
	}
	return path
}

func asyncImagePendingResponse(requestID, pollURL string) map[string]any {
	return map[string]any{
		"id":          requestID,
		"object":      "image.generation.task",
		"status":      "queued",
		"request_id":  requestID,
		"poll_url":    pollURL,
		"retry_after": 3,
		"data":        []any{},
	}
}

func asyncImageJobResponse(job *asyncImageJob, requestID, pollURL string) (map[string]any, bool) {
	select {
	case <-job.done:
		if job.err != nil {
			if noRouteFailover(job.err) {
				return asyncImagePendingResponse(requestID, pollURL), true
			}
			return map[string]any{
				"id":         requestID,
				"object":     "image.generation.task",
				"status":     "failed",
				"request_id": requestID,
				"poll_url":   pollURL,
				"error":      safeGenerationErrorText(job.err),
				"data":       []any{},
			}, false
		}
		response := make(map[string]any, len(job.response)+5)
		for key, value := range job.response {
			response[key] = value
		}
		response["id"] = requestID
		response["object"] = "image.generation.task"
		response["status"] = "completed"
		response["request_id"] = requestID
		response["poll_url"] = pollURL
		return response, false
	default:
		return asyncImagePendingResponse(requestID, pollURL), true
	}
}

func (s *V1Service) prepareImageExecution(ctx context.Context, principal *APIPrincipal, in V1ImageRequest, source string, charge bool, canonicalFingerprint ...string) (map[string]any, error) {
	s.applyGlobalProxy(ctx)
	// Detach the whole execution from the request lifecycle. The frontend tracks
	// progress by polling /jobs/mine, so a client disconnect — or an nginx/CDN
	// gateway timeout on the slow synchronous response — must NOT cancel an
	// in-flight generation. Binding to the request ctx meant a cancelled request
	// (a) spun uselessly in the upstream poll until its 180s timeout and
	// (b) silently dropped the refund + final status write, leaving the row stuck
	// pending until the maintenance sweep mislabeled it "abandoned".
	//
	// `ctx` (WithoutCancel) is durable and used for ALL bookkeeping (status /
	// refund / cleanup) so those always land. `genCtx` is the cancellable WORK
	// context: an 8-min backstop, AND registered in s.inflight so the maintenance
	// sweep can cancel it the instant it abandons the row — stopping a stuck
	// generation from running on for minutes and surfacing a late "success" on an
	// already-abandoned event.
	ctx = context.WithoutCancel(ctx)
	eventFingerprint := ""
	if len(canonicalFingerprint) > 0 {
		eventFingerprint = strings.TrimSpace(canonicalFingerprint[0])
	}
	// Internal callers normally have no idempotency key. If one does, compute
	// the canonical fingerprint before any reference-grid mutation as a safe
	// fallback; public API entry points pass the already-computed value.
	if strings.TrimSpace(in.RequestID) != "" && eventFingerprint == "" {
		canonical, fingerprint, err := canonicalImageIdempotencyInput(in)
		if err != nil {
			return nil, err
		}
		in, eventFingerprint = canonical, fingerprint
	}
	if len(in.ReferenceImages) > 0 && s.shouldApplyReferenceGrid(ctx, in.Model, in.ReferenceGrid) {
		gridded, err := applyReferenceFaceSwap(in.ReferenceImages)
		if err != nil {
			return nil, err
		}
		in.ReferenceImages = gridded
	}
	in.RequestID = strings.TrimSpace(in.RequestID)
	if len(in.RequestID) > 191 {
		return nil, fmt.Errorf("%w: idempotency key is too long", ErrUnsupportedParams)
	}
	if source == "v1" {
		responseFormat, err := normalizeImageResponseFormat(in.ResponseFormat)
		if err != nil {
			return nil, err
		}
		in.ResponseFormat = responseFormat
		in.BaseURL = s.outputBaseURL(in.BaseURL)
	}
	if source != "admin" {
		if err := s.checkBannedPrompt(ctx, principal, in.Prompt); err != nil {
			s.logRejectedEvent(ctx, "image", in.Model, principal, in.Prompt, source, err.Error())
			return nil, err
		}
	}
	// 去AI特征 is gated by a system-settings switch (default off) — drop the
	// flag when disabled so no surcharge is charged and no processing runs.
	if in.DeAI && !s.deaiEnabled(ctx) {
		in.DeAI = false
	}
	genCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()

	// Each service credential can define an independent concurrency cap. Admin
	// model tests are exempt; zero means unlimited.
	if source != "admin" && principal != nil && principal.Credential != nil {
		slot := randomUpper(12)
		admitted, gateErr := s.credentialAcquire(ctx, principal, slot)
		if gateErr != nil {
			return nil, gateErr
		}
		if !admitted {
			s.logRejectedEvent(ctx, "image", in.Model, principal, in.Prompt, source, ErrUserConcurrencyFull.Error())
			return nil, ErrUserConcurrencyFull
		}
		defer s.credentialRelease(ctx, principal.Credential.ID, slot)
	}

	modelItem, resolution, aspectRatio, price, err := s.prepareImage(ctx, principal, in, charge)
	if err != nil {
		s.logRejectedEvent(ctx, "image", in.Model, principal, in.Prompt, source, err.Error())
		return nil, err
	}
	refCount := len(in.ReferenceImages)
	// API-key requests carrying an idempotency key persist their output so a
	// gateway-timed-out synchronous response can be recovered by task lookup.
	// Other API calls keep the original no-store behavior.
	apiRequest := source == "v1"
	storeOutput := !apiRequest || in.RequestID != ""
	// URL is the default API response so the gateway can avoid downloading and
	// base64-encoding upstream media. Explicit b64_json and idempotent recovery
	// requests still need the generated bytes.
	urlOnly := apiRequest && !storeOutput && in.ResponseFormat == "url"
	var fileURL, relativePath string
	if storeOutput {
		fileURL, relativePath = s.allocateOutput(principal, "png", in.BaseURL)
	}
	// upstreamURL is the provider's original artifact URL. API clients always
	// receive a gateway URL; the gateway hides provider CORS and auth differences.
	var upstreamURL string
	eventID, err := s.logPendingEvent(ctx, "image", modelItem, principal, in.Prompt, aspectRatio, resolution, "", refCount, price, relativePath, source, nil, in.DeAI, in.RequestID, in.ResponseFormat, eventFingerprint)
	if err != nil {
		return nil, err
	}
	if apiRequest && storeOutput {
		// API clients receive an opaque bearer URL rather than the internal
		// owner/timestamp object key. The event resolves the private RustFS key.
		fileURL = publicImageContentURL(in.BaseURL, eventID)
	}
	// Register so the maintenance sweep can cancel this generation if it abandons
	// the row; deregister on return.
	s.inflight.Add(eventID, cancel)
	defer s.inflight.Done(eventID)
	startedAt := time.Now()

	imageBytes, upstreamURL, execErr := s.dispatchImageRoutes(genCtx, eventID, modelItem, in, aspectRatio, resolution, urlOnly)
	if execErr != nil {
		if noRouteFailover(execErr) {
			// create_task is non-idempotent. Accepted and submission-unknown
			// outcomes stay pending so task lookup can resume a known task id or
			// conservatively await maintenance without ever resubmitting.
			if errors.Is(execErr, byteplus.ErrRetryableTaskFailed) || errors.Is(execErr, errBytePlusRetryBudgetExhausted) {
				// The beta-instability exception has exhausted its bounded distinct-account
				// chain (or no alternate existed). Every accepted parent represented by
				// this error is terminal, so close the event instead of starting recovery.
				recordBookkeepingError("update failed beta image event", s.events.UpdateStatus(ctx, eventID, "failed", safeGenerationErrorText(execErr), 0))
				return nil, publicGenerationError(execErr)
			}
			if errors.Is(execErr, byteplus.ErrTaskAccepted) {
				if event, lookupErr := s.events.GetByID(ctx, eventID); lookupErr == nil {
					s.startAcceptedImageRecovery(ctx, event)
				} else {
					recordBookkeepingError("load accepted image event", lookupErr)
				}
				return nil, errors.Join(ErrProviderTemporary, byteplus.ErrTaskAccepted)
			}
			return nil, errors.Join(ErrProviderTemporary, byteplus.ErrTaskSubmissionUnknown)
		}
		recordBookkeepingError("update failed image event", s.events.UpdateStatus(ctx, eventID, "failed", safeGenerationErrorText(execErr), 0))
		return nil, publicGenerationError(execErr)
	}
	if apiRequest && in.ResponseFormat == "b64_json" && len(imageBytes) == 0 {
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		recordBookkeepingError("update empty image event", s.events.UpdateStatus(ctx, eventID, "failed", ErrProviderExecution.Error(), 0))
		return nil, ErrProviderExecution
	}
	// 去AI特征: post-process before storing/returning. Best-effort — a decode
	// failure keeps the original bytes rather than failing a paid generation.
	if in.DeAI {
		if processed, derr := applyDeAI(imageBytes); derr == nil {
			imageBytes = processed
		}
	}
	imageMimeType := ""
	imageExtension := ""
	if len(imageBytes) > 0 {
		imageMimeType, imageExtension, err = detectImageArtifact(imageBytes)
		if err != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			recordBookkeepingError("update invalid image artifact", s.events.UpdateStatus(ctx, eventID, "failed", ErrProviderExecution.Error(), 0))
			return nil, ErrProviderExecution
		}
	}
	if storeOutput {
		// The object key and metadata follow payload magic, never a provider URL
		// suffix or stale Content-Type. This matters for BytePlus, whose resource
		// endpoint can label JPEG/WebP/AVIF results as PNG.
		relativePath = replaceMediaExtension(relativePath, imageExtension)
		if !apiRequest {
			fileURL = replaceMediaExtension(fileURL, imageExtension)
		}
		if err := s.events.SetArtifact(ctx, eventID, relativePath, imageMimeType); err != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			recordBookkeepingError("update image artifact metadata", err)
			recordBookkeepingError("update image metadata failure", s.events.UpdateStatus(ctx, eventID, "failed", ErrProviderExecution.Error(), 0))
			return nil, ErrProviderExecution
		}
		// Upload to RustFS. On failure the generation fails and credits are
		// refunded — we never fall back to local disk.
		if err := s.store.Put(genCtx, relativePath, imageBytes, imageMimeType); err != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			recordBookkeepingError("update image storage failure", s.events.UpdateStatus(ctx, eventID, "failed", ErrProviderExecution.Error(), 0))
			return nil, ErrProviderExecution
		}
		// Best-effort thumbnail for list views; the image serving route falls
		// back to the original when the thumb object is missing.
		if thumb, terr := makeThumbnail(imageBytes); terr == nil {
			_ = s.store.Put(genCtx, ThumbKey(relativePath), thumb, "image/jpeg")
		}
	}
	elapsedMS := int(time.Since(startedAt).Milliseconds())
	if err := s.events.UpdateStatus(ctx, eventID, "success", "", elapsedMS); err != nil {
		return nil, err
	}
	_ = s.models.IncrementGenerationCount(ctx, modelItem.ID)
	if apiRequest {
		if in.ResponseFormat == "b64_json" {
			b64 := base64.StdEncoding.EncodeToString(imageBytes)
			return map[string]any{
				"created":    time.Now().Unix(),
				"data":       []map[string]any{{"b64_json": b64}},
				"model":      modelItem.EffectiveName(),
				"kind":       "image",
				"b64_json":   b64,
				"elapsed_ms": elapsedMS,
				"charged":    price,
				"credits":    principalCredits(principal),
			}, nil
		}
		if storeOutput {
			// Persisted results use our RustFS-backed public image proxy.
			return map[string]any{
				"created":    time.Now().Unix(),
				"data":       []map[string]any{{"url": fileURL}},
				"model":      modelItem.EffectiveName(),
				"kind":       "image",
				"url":        fileURL,
				"elapsed_ms": elapsedMS,
				"charged":    price,
				"credits":    principalCredits(principal),
			}, nil
		}
		if strings.TrimSpace(upstreamURL) != "" {
			// No-store results still use our public proxy. The original URL is kept
			// on the event so OpenImageContent can fetch it with provider credentials
			// when necessary.
			_ = s.events.SetFile(ctx, eventID, upstreamURL)
			outURL := publicImageContentURL(in.BaseURL, eventID)
			return map[string]any{
				"created":    time.Now().Unix(),
				"data":       []map[string]any{{"url": outURL}},
				"model":      modelItem.EffectiveName(),
				"kind":       "image",
				"url":        outURL,
				"elapsed_ms": elapsedMS,
				"charged":    price,
				"credits":    principalCredits(principal),
			}, nil
		}
		// A provider without an upstream URL falls back to base64 even when url
		// was requested, so a successful generation never returns an empty asset.
		b64 := base64.StdEncoding.EncodeToString(imageBytes)
		return map[string]any{
			"created":    time.Now().Unix(),
			"data":       []map[string]any{{"b64_json": b64}},
			"model":      modelItem.EffectiveName(),
			"kind":       "image",
			"b64_json":   b64,
			"elapsed_ms": elapsedMS,
			"charged":    price,
			"credits":    principalCredits(principal),
		}, nil
	}
	return map[string]any{
		"created":    time.Now().Unix(),
		"data":       []map[string]any{{"url": fileURL, "b64_json": nil}},
		"model":      modelItem.EffectiveName(),
		"kind":       "image",
		"url":        fileURL,
		"event_id":   eventID,
		"elapsed_ms": elapsedMS,
		"charged":    price,
		"credits":    principalCredits(principal),
	}, nil
}

// ImageTask recovers an API image request by its idempotency key. Results are
// scoped to the authenticated API-key owner and returned in OpenAI image shape.
func (s *V1Service) ImageTask(ctx context.Context, principal *APIPrincipal, requestID string) (map[string]any, error) {
	requestID = strings.TrimSpace(requestID)
	if principal == nil || principal.Credential == nil || requestID == "" {
		return nil, ErrImageTaskNotFound
	}
	if value, ok := s.imageJobs.Load(imageJobKey(principal.Credential.ID, requestID)); ok {
		job := value.(*asyncImageJob)
		response, pending := asyncImageJobResponse(job, requestID, s.imageTaskURL(requestID, ""))
		if !pending {
			return response, nil
		}
		select {
		case <-job.done:
			if job.err == nil || !noRouteFailover(job.err) {
				return response, nil
			}
		default:
			return response, nil
		}
		// A completed local goroutine can still represent a durable accepted task.
		// Fall through to the event-backed recovery instead of pinning polling to
		// the local temporary error until imageJobs retention expires.
	}
	return s.imageTaskFromEvent(ctx, principal, requestID)
}

func (s *V1Service) imageTaskFromEvent(ctx context.Context, principal *APIPrincipal, requestID string, expectedFingerprint ...string) (map[string]any, error) {
	event, err := s.events.GetImageByRequestID(ctx, principal.Credential.ID, requestID)
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, ErrImageTaskNotFound
	}
	if len(expectedFingerprint) > 0 && strings.TrimSpace(event.RequestFingerprint) != "" && event.RequestFingerprint != expectedFingerprint[0] {
		return nil, ErrIdempotencyConflict
	}
	result := map[string]any{
		"id":         requestID,
		"object":     "image.generation.task",
		"request_id": requestID,
		"poll_url":   s.imageTaskURL(requestID, ""),
		"event_id":   event.ID,
		"created":    event.TS.Unix(),
		"model":      event.Model,
		"data":       []any{},
	}
	switch event.Status {
	case "success":
		if strings.TrimSpace(event.File) == "" {
			return nil, fmt.Errorf("%w: recovered image file is missing", ErrProviderTemporary)
		}
		result["status"] = "completed"
		if strings.TrimSpace(event.MimeType) != "" {
			result["mime_type"] = event.MimeType
		}
		if storedImageResponseFormat(event.ResponseFormat) == "url" {
			contentURL := publicImageContentURL(s.outputBaseURL(""), event.ID)
			result["data"] = []map[string]any{{"url": contentURL}}
			result["url"] = contentURL
			break
		}
		response, err := s.store.Get(ctx, event.File, "")
		if err != nil {
			return nil, fmt.Errorf("%w: failed to load recovered image", ErrProviderTemporary)
		}
		defer response.Body.Close()
		if response.StatusCode >= http.StatusBadRequest {
			return nil, fmt.Errorf("%w: recovered image is unavailable", ErrProviderTemporary)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, netguard.MaxImageBytes+1))
		if err != nil || len(body) == 0 || int64(len(body)) > netguard.MaxImageBytes {
			return nil, fmt.Errorf("%w: recovered image is invalid", ErrProviderTemporary)
		}
		mimeType, _, detectErr := detectImageArtifact(body)
		if detectErr != nil {
			return nil, fmt.Errorf("%w: recovered image is invalid", ErrProviderTemporary)
		}
		if mimeType != event.MimeType {
			recordBookkeepingError("repair recovered image MIME", s.events.SetMimeType(ctx, event.ID, mimeType))
			result["mime_type"] = mimeType
		}
		encoded := base64.StdEncoding.EncodeToString(body)
		result["data"] = []map[string]any{{"b64_json": encoded}}
		result["b64_json"] = encoded
	case "failed":
		result["status"] = "failed"
		result["error"] = safeStoredGenerationError(event.Error)
	default:
		result["status"] = "in_progress"
		s.startAcceptedImageRecovery(ctx, event)
	}
	return result, nil
}

func storedImageResponseFormat(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "b64_json") {
		return "b64_json"
	}
	// URL is the OpenAI-compatible default. It is also the safest fallback for
	// legacy rows created before response_format was persisted because it avoids
	// loading and base64-expanding a large object inside every task poll.
	return "url"
}

// startAcceptedImageRecovery launches one background resume for a durable
// BytePlus parent task. Unknown submissions have no task id and deliberately do
// nothing here: replaying create_task could double-charge the account.
func (s *V1Service) startAcceptedImageRecovery(parent context.Context, event *model.EventLog) {
	if event == nil || event.Status != "pending" || event.Provider != "byteplus" || s.byteplus == nil || s.store == nil {
		return
	}
	attempt, err := s.models.Dispatch().LatestAcceptedForEvent(parent, event.ID)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			recordBookkeepingError("load accepted image dispatch", err)
		}
		return
	}
	if strings.TrimSpace(attempt.UpstreamTaskID) == "" {
		return
	}
	if _, loaded := s.imageRecoveries.LoadOrStore(attempt.ID, struct{}{}); loaded {
		return
	}
	go func(event model.EventLog, attempt model.DispatchAttempt) {
		defer s.imageRecoveries.Delete(attempt.ID)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 12*time.Minute)
		defer cancel()
		s.inflight.Add(event.ID, cancel)
		defer s.inflight.Done(event.ID)

		account, accountErr := s.tokens.Get(ctx, "byteplus", attempt.AccountID)
		if accountErr != nil || account == nil || strings.TrimSpace(account.Value) == "" {
			recordBookkeepingError("load accepted image account", accountErr)
			return
		}
		data, _, resumeErr := s.byteplus.ResumeImageTask(ctx, account.Value, attempt.UpstreamTaskID, true)
		if resumeErr != nil {
			// A transient poll/download failure keeps the accepted task recoverable.
			// Explicit terminal provider outcomes can safely close the event.
			if errors.Is(resumeErr, byteplus.ErrQuotaExhausted) || errors.Is(resumeErr, byteplus.ErrRiskControl) || errors.Is(resumeErr, byteplus.ErrInvalidParams) || errors.Is(resumeErr, byteplus.ErrTaskFailed) {
				publicErr := publicGenerationError(resumeErr)
				if errors.Is(resumeErr, byteplus.ErrTaskFailed) {
					publicErr = ErrProviderExecution
				}
				recordBookkeepingError("finish accepted image event", s.events.UpdateStatus(ctx, event.ID, "failed", publicErr.Error(), 0))
				recordBookkeepingError("finish accepted image dispatch", s.models.Dispatch().Finish(ctx, attempt.ID, "failed", dispatchFailureClassName(resumeErr), attempt.UpstreamTaskID, publicErr))
			}
			return
		}
		if len(data) == 0 {
			return
		}
		objectKey, mimeType, detectErr := recoveredImageArtifact(event.File, event.ID, data)
		if detectErr != nil {
			recordBookkeepingError("reject accepted image artifact", detectErr)
			return
		}
		if metadataErr := s.events.SetArtifact(ctx, event.ID, objectKey, mimeType); metadataErr != nil {
			recordBookkeepingError("update accepted image artifact metadata", metadataErr)
			return
		}
		if putErr := s.store.Put(ctx, objectKey, data, mimeType); putErr != nil {
			recordBookkeepingError("store accepted image", putErr)
			return
		}
		elapsed := int(time.Since(event.TS).Milliseconds())
		if updateErr := s.events.UpdateStatus(ctx, event.ID, "success", "", elapsed); updateErr != nil {
			recordBookkeepingError("complete accepted image event", updateErr)
			return
		}
		recordBookkeepingError("complete accepted image dispatch", s.models.Dispatch().Finish(ctx, attempt.ID, "succeeded", "", attempt.UpstreamTaskID, nil))
		recordBookkeepingError("complete accepted image route", s.models.Routes().RecordAccountRouteResult(ctx, attempt.AccountID, attempt.ModelRouteID, "", true))
		_, updateErr := s.tokens.Update(ctx, "byteplus", account.ID, map[string]any{
			"last_used_at": time.Now(), "success_total": gorm.Expr("success_total + 1"), "fails": 0,
		})
		recordBookkeepingError("complete accepted image account", updateErr)
		_ = s.refreshDispatchQuota(withDispatchRoute(ctx, mustDispatchRoute(ctx, s.models, attempt.ModelRouteID), 0), "byteplus", *account, "image")
		recordBookkeepingError("increment accepted image generation", s.models.IncrementGenerationCount(ctx, event.Model))
	}(*event, *attempt)
}

func dispatchFailureClassName(err error) string {
	_, class := dispatchFailureClass(err)
	return class
}

func mustDispatchRoute(ctx context.Context, models *repo.ModelRepository, routeID string) model.ModelRoute {
	route, err := models.Routes().GetRoute(ctx, routeID)
	if err != nil || route == nil {
		return model.ModelRoute{ID: routeID, QuotaBucketKey: "byteplus.computing_points"}
	}
	return *route
}

// ===== /v1/videos — OpenAI Sora-style async jobs =====
// POST /v1/videos charges + creates a pending event and renders in the
// background; the render captures only the UPSTREAM video URL (no download, no
// RustFS). GET /v1/videos/{id} polls status; /content proxies the upstream URL.

// prepareAdminVideoTest executes one synchronous video capability test through
// the selected account. The artifact stays behind the administrator session and
// the upstream URL is never returned to the browser.
func (s *V1Service) prepareAdminVideoTest(ctx context.Context, in V1VideoRequest) (map[string]any, error) {
	s.applyGlobalProxy(ctx)
	ctx = context.WithoutCancel(ctx)
	modelItem, resolution, aspectRatio, duration, price, err := s.prepareVideo(ctx, nil, in, false)
	if err != nil {
		return nil, err
	}
	eventID, err := s.logPendingEvent(ctx, "video", modelItem, nil, in.Prompt, aspectRatio, resolution, duration,
		0, price, "", "admin", nil, false, "", "")
	if err != nil {
		return nil, err
	}
	genCtx, cancel := context.WithTimeout(ctx, videoGenerationTimeout)
	defer cancel()
	s.inflight.Add(eventID, cancel)
	defer s.inflight.Done(eventID)
	startedAt := time.Now()

	_, videoURL, execErr := s.dispatchVideoRoutes(genCtx, eventID, modelItem, in, aspectRatio, resolution, duration, false)
	if execErr != nil {
		recordBookkeepingError("update failed admin video test", s.events.UpdateStatus(ctx, eventID, "failed", safeGenerationErrorText(execErr), 0))
		return nil, publicGenerationError(execErr)
	}
	if strings.TrimSpace(videoURL) == "" {
		recordBookkeepingError("update empty admin video test", s.events.UpdateStatus(ctx, eventID, "failed", ErrProviderExecution.Error(), 0))
		return nil, ErrProviderExecution
	}
	elapsedMS := int(time.Since(startedAt).Milliseconds())
	if err := s.events.MarkVideoReady(ctx, eventID, videoURL, elapsedMS); err != nil {
		return nil, err
	}
	_ = s.models.IncrementGenerationCount(ctx, modelItem.ID)
	return map[string]any{
		"kind": "video", "model": modelItem.EffectiveName(), "event_id": eventID,
		"url": "/admin/api/test/artifacts/" + eventID, "elapsed_ms": elapsedMS,
	}, nil
}

// StartVideoJob validates+charges, creates the job event, kicks the render off in
// the background, and returns the OpenAI video object (status "queued").
func (s *V1Service) StartVideoJob(ctx context.Context, principal *APIPrincipal, in V1VideoRequest) (map[string]any, error) {
	if principal == nil || principal.Credential == nil {
		return nil, ErrInvalidAPIKey
	}
	in.RequestID = strings.TrimSpace(in.RequestID)
	if in.RequestID == "" {
		return nil, fmt.Errorf("%w: idempotency key is required for video requests", ErrUnsupportedParams)
	}
	if len(in.RequestID) > 191 {
		return nil, fmt.Errorf("%w: idempotency key is too long", ErrUnsupportedParams)
	}
	fingerprint := videoRequestFingerprint(in)
	if existing, lookupErr := s.events.GetByRequestID(ctx, principal.Credential.ID, "video", in.RequestID); lookupErr != nil {
		return nil, lookupErr
	} else if existing != nil {
		if strings.TrimSpace(existing.RequestFingerprint) != "" && existing.RequestFingerprint != fingerprint {
			return nil, ErrIdempotencyConflict
		}
		return s.videoJobFromEvent(existing), nil
	}
	ctx = context.WithoutCancel(ctx)
	if err := s.checkBannedPrompt(ctx, principal, in.Prompt); err != nil {
		s.logRejectedEvent(ctx, "video", in.Model, principal, in.Prompt, "v1", err.Error())
		return nil, err
	}
	modelItem, resolution, aspectRatio, duration, price, err := s.prepareVideo(ctx, principal, in, true)
	if err != nil {
		s.logRejectedEvent(ctx, "video", in.Model, principal, in.Prompt, "v1", err.Error())
		return nil, err
	}
	// Source "v1": no output file is allocated — the result is the upstream URL,
	// stored on the event when the render completes.
	eventID, err := s.logPendingEvent(ctx, "video", modelItem, principal, in.Prompt, aspectRatio, resolution, duration, len(in.ReferenceImages)+len(in.ReferenceVideos)+len(in.ReferenceAudios), price, "", "v1", nil, false, in.RequestID, "", fingerprint)
	if err != nil {
		// The database uniqueness boundary is authoritative across processes. A
		// concurrent winner may have inserted after our initial read; re-read and
		// return that job instead of surfacing a transient duplicate-key error.
		if existing, lookupErr := s.events.GetByRequestID(ctx, principal.Credential.ID, "video", in.RequestID); lookupErr == nil && existing != nil {
			if strings.TrimSpace(existing.RequestFingerprint) != "" && existing.RequestFingerprint != fingerprint {
				return nil, ErrIdempotencyConflict
			}
			return s.videoJobFromEvent(existing), nil
		}
		return nil, err
	}
	go s.runVideoJob(ctx, principal, in, modelItem, eventID, aspectRatio, resolution, duration, price)
	return videoJobObject(eventID, modelItem.EffectiveName(), "queued", 0, duration, sizeFromRatioRes(aspectRatio, resolution), time.Now().Unix(), 0, ""), nil
}

func videoRequestFingerprint(in V1VideoRequest) string {
	referenceImages := make([]string, 0, len(in.ReferenceImages))
	for _, reference := range in.ReferenceImages {
		sum := sha256.Sum256([]byte(reference))
		referenceImages = append(referenceImages, hex.EncodeToString(sum[:]))
	}
	hashMedia := func(items []MediaReference) []string {
		out := make([]string, 0, len(items))
		for _, item := range items {
			sum := sha256.Sum256(item.Data)
			out = append(out, strings.ToLower(strings.TrimSpace(item.ContentType))+":"+hex.EncodeToString(sum[:]))
		}
		return out
	}
	payload := struct {
		Model, Prompt, Duration, AspectRatio, Resolution string
		ReferenceImages                                  []string
		ReferenceVideos                                  []string
		ReferenceAudios                                  []string
		GenerateAudio, ReferenceGrid                     bool
	}{
		strings.TrimSpace(in.Model), in.Prompt, strings.TrimSpace(in.Duration), strings.TrimSpace(in.AspectRatio), strings.TrimSpace(in.Resolution),
		referenceImages, hashMedia(in.ReferenceVideos), hashMedia(in.ReferenceAudios), in.GenerateAudio, in.ReferenceGrid,
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *V1Service) videoJobFromEvent(ev *model.EventLog) map[string]any {
	status, progress := videoJobStatus(ev)
	completedAt := int64(0)
	if ev.Status == "success" || ev.Status == "failed" {
		completedAt = ev.UpdatedAt.Unix()
	}
	errMsg := ""
	if ev.Status == "failed" {
		errMsg = safeStoredGenerationError(ev.Error)
	}
	return videoJobObject(ev.ID, ev.Model, status, progress, ev.Duration, sizeFromRatioRes(ev.Ratio, ev.Resolution), ev.TS.Unix(), completedAt, errMsg)
}

// runVideoJob renders the clip in the background, capturing the upstream URL
// (downloadResult=false → no bytes, no RustFS) and storing it on the event.
func (s *V1Service) runVideoJob(ctx context.Context, principal *APIPrincipal, in V1VideoRequest, modelItem *model.ModelConfig, eventID, aspectRatio, resolution, duration string, price float64) {
	s.applyGlobalProxy(ctx)
	if principal != nil && principal.Credential != nil {
		slot := "video:" + eventID
		admitted, gateErr := s.credentialAcquire(ctx, principal, slot)
		if gateErr != nil {
			recordBookkeepingError("update video concurrency failure", s.events.UpdateStatus(ctx, eventID, "failed", safeGenerationErrorText(gateErr), 0))
			return
		}
		if !admitted {
			_ = s.events.UpdateStatus(ctx, eventID, "failed", ErrUserConcurrencyFull.Error(), 0)
			return
		}
		defer s.credentialRelease(context.WithoutCancel(ctx), principal.Credential.ID, slot)
	}
	genCtx, cancel := context.WithTimeout(ctx, videoGenerationTimeout)
	defer cancel()
	s.inflight.Add(eventID, cancel)
	defer s.inflight.Done(eventID)
	startedAt := time.Now()

	// Face-grid preprocessing decodes/re-encodes the reference images and can take
	// tens of seconds — run it here so POST /v1/videos returns the job id at once.
	if len(in.ReferenceImages) > 0 && s.shouldApplyReferenceGrid(ctx, in.Model, in.ReferenceGrid) {
		gridded, gridErr := applyReferenceFaceSwap(in.ReferenceImages)
		if gridErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			recordBookkeepingError("update video reference failure", s.events.UpdateStatus(ctx, eventID, "failed", ErrUnsupportedParams.Error(), 0))
			return
		}
		in.ReferenceImages = gridded
	}

	// No-store: capture only the UPSTREAM video URL. /content streams it on demand
	// (grok URLs are auth-gated → fetched with the generating account's token).
	_, videoURL, execErr := s.dispatchVideoRoutes(genCtx, eventID, modelItem, in, aspectRatio, resolution, duration, false)
	if execErr != nil {
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		recordBookkeepingError("update async video failure", s.events.UpdateStatus(ctx, eventID, "failed", safeGenerationErrorText(execErr), 0))
		return
	}
	if strings.TrimSpace(videoURL) == "" {
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		recordBookkeepingError("update empty video event", s.events.UpdateStatus(ctx, eventID, "failed", ErrProviderExecution.Error(), 0))
		return
	}
	// Store the upstream URL as the event's "file"; /content fetches it on demand.
	if err := s.events.MarkVideoReady(ctx, eventID, videoURL, int(time.Since(startedAt).Milliseconds())); err != nil {
		return
	}
	_ = s.models.IncrementGenerationCount(ctx, modelItem.ID)
}

// VideoJob returns the OpenAI video object for a job, scoped to the caller.
func (s *V1Service) VideoJob(ctx context.Context, principal *APIPrincipal, id string) (map[string]any, error) {
	ev, err := s.videoEventForUser(ctx, principal, id)
	if err != nil {
		return nil, err
	}
	status, progress := videoJobStatus(ev)
	completedAt := int64(0)
	if ev.Status == "success" || ev.Status == "failed" {
		completedAt = ev.UpdatedAt.Unix()
	}
	errMsg := ""
	if ev.Status == "failed" {
		errMsg = safeStoredGenerationError(ev.Error)
	}
	modelName := ev.Model
	if nameByID, nerr := s.models.NameMap(ctx); nerr == nil {
		if name, ok := nameByID[ev.Model]; ok && strings.TrimSpace(name) != "" {
			modelName = name
		}
	}
	return videoJobObject(ev.ID, modelName, status, progress, ev.Duration, sizeFromRatioRes(ev.Ratio, ev.Resolution), ev.TS.Unix(), completedAt, errMsg), nil
}

// OpenVideoContent streams a completed job's video by proxying the stored
// upstream URL (downloaded on demand — never persisted).
func (s *V1Service) OpenVideoContent(ctx context.Context, principal *APIPrincipal, id string) (io.ReadCloser, string, error) {
	_ = s.applyGlobalProxy(ctx)
	ev, err := s.videoEventForUser(ctx, principal, id)
	if err != nil {
		return nil, "", err
	}
	if ev.Status != "success" || strings.TrimSpace(ev.File) == "" {
		return nil, "", ErrVideoNotReady
	}
	// grok asset URLs (assets.grok.com) are auth-gated — a plain GET 403s. Stream
	// them through the SAME account that generated the clip, using its token. If
	// that account is gone (grok pools churn often), the clip is unrecoverable.
	if ev.Provider == "grok" && s.grok != nil {
		acct, _ := s.tokens.Get(ctx, "grok", ev.AccountID)
		if acct == nil || strings.TrimSpace(acct.Value) == "" {
			return nil, "", fmt.Errorf("%w: grok account no longer available for this video", ErrProviderTemporary)
		}
		return s.grok.OpenAsset(ctx, acct.Value, ev.File)
	}
	// Other providers return publicly-fetchable URLs. Validate the initial URL and
	// every redirect before direct local egress so an upstream-provided URL cannot
	// turn this API into an internal-network proxy.
	allowedHosts, err := s.assetAllowedHosts(ctx, ev)
	if err != nil {
		return nil, "", err
	}
	assetURL, err := netguard.ValidateAssetURL(ctx, ev.File, allowedHosts)
	if err != nil {
		return nil, "", fmt.Errorf("%w: rejected upstream video URL", ErrProviderTemporary)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL.String(), nil)
	if err != nil {
		return nil, "", err
	}
	client, err := globalProxyHTTPClient("", 5*time.Minute)
	if err != nil {
		return nil, "", err
	}
	if err := netguard.HardenHTTPClient(client, allowedHosts); err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: fetch upstream video failed", ErrProviderTemporary)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("%w: upstream video status %d", ErrProviderTemporary, resp.StatusCode)
	}
	body, ct, guardErr := netguard.GuardResponse(resp, netguard.MediaVideo, netguard.MaxVideoBytes)
	if guardErr != nil {
		return nil, "", fmt.Errorf("%w: invalid upstream video", ErrProviderTemporary)
	}
	return body, ct, nil
}

// OpenAdminArtifact streams a successful event artifact through the same
// provider-aware, SSRF-hardened path used by the public API. The administrator
// session is checked by the router; this method derives the historical API-key
// principal solely to reuse the event ownership guard without sending that key
// to the browser.
func (s *V1Service) OpenAdminArtifact(ctx context.Context, id string) (io.ReadCloser, string, error) {
	event, err := s.events.GetByID(ctx, strings.TrimSpace(id))
	if err != nil || event == nil || event.Status != "success" || strings.TrimSpace(event.File) == "" {
		if err != nil {
			return nil, "", err
		}
		return nil, "", ErrNotFound
	}
	principal := adminArtifactPrincipal(event)
	switch event.Kind {
	case "image":
		return s.OpenImageContent(ctx, principal, event.ID)
	case "video":
		return s.OpenVideoContent(ctx, principal, event.ID)
	default:
		return nil, "", ErrNotFound
	}
}

func adminArtifactPrincipal(event *model.EventLog) *APIPrincipal {
	if event != nil && strings.TrimSpace(event.APICredentialID) != "" {
		// Ownership checks need only the immutable credential id. Never load or
		// propagate its secret while serving the administrator preview.
		return &APIPrincipal{Credential: &model.APICredential{ID: event.APICredentialID}}
	}
	return &APIPrincipal{TokenType: "admin"}
}

// OpenImageContent streams a no-store image by proxying the stored upstream URL.
// chatgpt URLs are auth-gated (files.oaiusercontent.com — a plain GET 403s), so
// they're fetched through the generating account's token; other providers'
// URLs are public and proxied directly. Access is always scoped to the bearer
// credential that created the event; the opaque event id is not authorization.
func (s *V1Service) OpenImageContent(ctx context.Context, principal *APIPrincipal, id string) (io.ReadCloser, string, error) {
	_ = s.applyGlobalProxy(ctx)
	ev, err := s.events.GetByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, "", err
	}
	if !eventOwnedByPrincipal(ev, principal, "image") {
		// Deliberately collapse ownership mismatches into not-found so event ids
		// cannot be used to enumerate another API credential's artifacts.
		return nil, "", ErrImageTaskNotFound
	}
	file := strings.TrimSpace(ev.File)
	if ev.Status != "success" || file == "" {
		return nil, "", ErrVideoNotReady
	}
	// Persisted API results keep a private RustFS object key on the event. The
	// public event URL reveals neither that key nor its user-owned directory.
	if !strings.Contains(file, "://") {
		resp, getErr := s.store.Get(ctx, file, "")
		if getErr != nil {
			return nil, "", fmt.Errorf("%w: fetch stored image: %v", ErrProviderTemporary, getErr)
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			resp.Body.Close()
			return nil, "", fmt.Errorf("%w: stored image status %d", ErrProviderTemporary, resp.StatusCode)
		}
		contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
		if contentType == "" {
			contentType = strings.TrimSpace(ev.MimeType)
		}
		if contentType == "" {
			contentType = contentTypeForExt(filepath.Ext(file))
		}
		body, guardedType, guardErr := guardMediaStream(resp.Body, contentType, netguard.MediaImage, netguard.MaxImageBytes)
		if guardErr != nil {
			return nil, "", fmt.Errorf("%w: invalid stored image", ErrProviderTemporary)
		}
		return body, guardedType, nil
	}
	if ev.Provider == "chatgpt" && s.chatgpt != nil {
		// Lazy cache: the first /content hit downloads through the residential
		// proxy and stores a copy under a deterministic key; later hits serve the
		// copy and stop re-spending metered proxy traffic on the same image. This
		// also fixes ChatGPT's estuary download URL going stale (signed/expiring)
		// and permanently 404ing after the signature lapses.
		cacheKey := imageCacheKey(ev.ID)
		if cached, ct, ok := s.openCachedImage(ctx, cacheKey); ok {
			return cached, ct, nil
		}
		acct, _ := s.tokens.Get(ctx, "chatgpt", ev.AccountID)
		if acct == nil || strings.TrimSpace(acct.Value) == "" {
			return nil, "", fmt.Errorf("%w: chatgpt account no longer available for this image", ErrProviderTemporary)
		}
		body, ct, err := s.chatgpt.OpenAsset(ctx, acct.Value, ev.File)
		if err != nil {
			return nil, "", err
		}
		return s.cacheImageStream(ctx, cacheKey, body, ct)
	}
	// grok asset URLs (assets.grok.com) are auth-gated too — stream them with the
	// token of the account that generated the image.
	if ev.Provider == "grok" && s.grok != nil {
		acct, _ := s.tokens.Get(ctx, "grok", ev.AccountID)
		if acct == nil || strings.TrimSpace(acct.Value) == "" {
			return nil, "", fmt.Errorf("%w: grok account no longer available for this image", ErrProviderTemporary)
		}
		body, ct, openErr := s.grok.OpenAsset(ctx, acct.Value, ev.File)
		if openErr != nil {
			return nil, "", openErr
		}
		return guardMediaStream(body, ct, netguard.MediaImage, netguard.MaxImageBytes)
	}
	// Lumina resource-utils URLs are session-bound and short-lived. Fetch them
	// with the exact Cookie account that created the task, then expose the bytes
	// only through this opaque event URL.
	if ev.Provider == "byteplus" && s.byteplus != nil {
		cacheKey := imageCacheKey(ev.ID)
		if cached, ct, ok := s.openCachedImage(ctx, cacheKey); ok {
			return cached, ct, nil
		}
		acct, _ := s.tokens.Get(ctx, "byteplus", ev.AccountID)
		if acct == nil || strings.TrimSpace(acct.Value) == "" {
			return nil, "", fmt.Errorf("%w: byteplus account no longer available for this image", ErrProviderTemporary)
		}
		data, ct, openErr := s.byteplus.OpenAsset(ctx, acct.Value, ev.File)
		if openErr != nil {
			return nil, "", openErr
		}
		if ct != ev.MimeType {
			recordBookkeepingError("persist BytePlus image MIME", s.events.SetMimeType(ctx, ev.ID, ct))
		}
		return s.cacheImageStream(ctx, cacheKey, io.NopCloser(bytes.NewReader(data)), ct)
	}
	allowedHosts, err := s.assetAllowedHosts(ctx, ev)
	if err != nil {
		return nil, "", err
	}
	assetURL, err := netguard.ValidateAssetURL(ctx, ev.File, allowedHosts)
	if err != nil {
		return nil, "", fmt.Errorf("%w: rejected upstream image URL", ErrProviderTemporary)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL.String(), nil)
	if err != nil {
		return nil, "", err
	}
	client, err := globalProxyHTTPClient("", 5*time.Minute)
	if err != nil {
		return nil, "", err
	}
	if err := netguard.HardenHTTPClient(client, allowedHosts); err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: fetch upstream image failed", ErrProviderTemporary)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("%w: upstream image status %d", ErrProviderTemporary, resp.StatusCode)
	}
	body, ct, guardErr := netguard.GuardResponse(resp, netguard.MediaImage, netguard.MaxImageBytes)
	if guardErr != nil {
		return nil, "", fmt.Errorf("%w: invalid upstream image", ErrProviderTemporary)
	}
	return body, ct, nil
}

func (s *V1Service) assetAllowedHosts(ctx context.Context, ev *model.EventLog) ([]string, error) {
	if ev == nil {
		return nil, ErrProviderTemporary
	}
	switch strings.ToLower(strings.TrimSpace(ev.Provider)) {
	case "adobe":
		return staticAssetHosts("adobe"), nil
	case "runway":
		return staticAssetHosts("runway"), nil
	case "oreate":
		return staticAssetHosts("oreate"), nil
	case "dola":
		// Dola video artifacts rotate across ByteDance VOD hosts; an empty list
		// keeps netguard's scheme/IP hardening as the only gate.
		return nil, nil
	case "custom":
		account, err := s.tokens.Get(ctx, "custom", ev.AccountID)
		if err != nil || account == nil {
			return nil, fmt.Errorf("%w: custom account unavailable", ErrProviderTemporary)
		}
		base, parseErr := url.Parse(strings.TrimSpace(stringValue(account.Meta["base_url"])))
		if parseErr != nil || base == nil || !strings.EqualFold(base.Scheme, "https") || base.Hostname() == "" {
			return nil, fmt.Errorf("%w: custom account has unsafe base URL", ErrProviderTemporary)
		}
		return []string{base.Hostname()}, nil
	default:
		return nil, fmt.Errorf("%w: provider has no artifact host policy", ErrProviderTemporary)
	}
}

func staticAssetHosts(provider string) []string {
	switch provider {
	case "adobe":
		return []string{"adobe.com", "adobe.io", "amazonaws.com", "cloudfront.net"}
	case "runway":
		return []string{"runwayml.com", "runwayml.cloud", "amazonaws.com", "cloudfront.net"}
	case "oreate":
		return []string{"oreateai.com"}
	default:
		return nil
	}
}

func downloadPublicArtifact(ctx context.Context, rawURL string, allowedHosts []string, bearer string, kind netguard.MediaKind, maxBytes int64) ([]byte, string, error) {
	return downloadPublicArtifactWithProxy(ctx, rawURL, allowedHosts, bearer, kind, maxBytes, "")
}

func downloadPublicArtifactWithProxy(ctx context.Context, rawURL string, allowedHosts []string, bearer string, kind netguard.MediaKind, maxBytes int64, proxyRaw string) ([]byte, string, error) {
	assetURL, err := netguard.ValidateAssetURL(ctx, rawURL, allowedHosts)
	if err != nil {
		return nil, "", err
	}
	client, err := globalProxyHTTPClient(proxyRaw, 5*time.Minute)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(proxyRaw) == "" {
		if err := netguard.HardenHTTPClient(client, allowedHosts); err != nil {
			return nil, "", err
		}
	} else {
		// Forward proxies resolve the destination themselves, so IP pinning cannot
		// be applied locally. Keep the public-URL preflight above and validate every
		// redirect before allowing the proxy to follow it.
		client.CheckRedirect = netguard.CheckRedirect(allowedHosts)
	}
	originalHost := strings.ToLower(assetURL.Hostname())
	baseRedirectCheck := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := baseRedirectCheck(req, via); err != nil {
			return err
		}
		req.Header.Del("Authorization")
		if bearer != "" && strings.EqualFold(req.URL.Hostname(), originalHost) {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL.String(), nil)
	if err != nil {
		return nil, "", err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		resp.Body.Close()
		return nil, "", fmt.Errorf("artifact status %d", resp.StatusCode)
	}
	body, contentType, err := netguard.GuardResponse(resp, kind, maxBytes)
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, "", err
	}
	return data, contentType, nil
}

func readProviderArtifact(body io.ReadCloser, contentType string, kind netguard.MediaKind, maxBytes int64) ([]byte, error) {
	guarded, _, err := guardMediaStream(body, contentType, kind, maxBytes)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(guarded)
	_ = guarded.Close()
	return data, err
}

// imageCacheKey maps an event ID to the RustFS object key holding a cached copy
// of its no-store generated image. Caching is lazy: the first /content hit
// downloads through the residential proxy and stores a copy, subsequent hits
// serve the copy and stop consuming metered proxy traffic.
func imageCacheKey(eventID string) string {
	return "cache/img/" + strings.TrimSpace(eventID)
}

// openCachedImage returns a cached image stream when one exists. A store read
// error or a miss must never fail the request, so it reports ok=false on both.
func (s *V1Service) openCachedImage(ctx context.Context, key string) (io.ReadCloser, string, bool) {
	if s.store == nil || !s.store.Configured() {
		return nil, "", false
	}
	resp, err := s.store.Get(ctx, key, "")
	if err != nil || resp == nil {
		return nil, "", false
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", false
	}
	ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if ct == "" {
		ct = "image/png"
	}
	body, guardedType, guardErr := guardMediaStream(resp.Body, ct, netguard.MediaImage, netguard.MaxImageBytes)
	if guardErr != nil {
		return nil, "", false
	}
	return body, guardedType, true
}

// cacheImageStream reads a freshly downloaded image fully, stores a best-effort
// copy under key, and returns the same bytes as a stream so the caller's
// streaming contract is unchanged. A cache write failure never fails the fetch.
func (s *V1Service) cacheImageStream(ctx context.Context, key string, body io.ReadCloser, ct string) (io.ReadCloser, string, error) {
	guarded, guardedType, err := guardMediaStream(body, ct, netguard.MediaImage, netguard.MaxImageBytes)
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(guarded)
	_ = guarded.Close()
	if err != nil {
		return nil, "", err
	}
	if s.store != nil && s.store.Configured() && len(data) > 0 {
		_ = s.store.Put(ctx, key, data, guardedType)
	}
	return io.NopCloser(bytes.NewReader(data)), guardedType, nil
}

func guardMediaStream(body io.ReadCloser, contentType string, kind netguard.MediaKind, maxBytes int64) (io.ReadCloser, string, error) {
	return netguard.GuardStream(body, contentType, -1, kind, maxBytes)
}

func publicImageContentURL(baseURL, eventID string) string {
	path := "/v1/images/" + strings.TrimSpace(eventID) + "/content"
	if base := strings.TrimRight(strings.TrimSpace(baseURL), "/"); base != "" {
		return base + path
	}
	return path
}

func (s *V1Service) outputBaseURL(requestBaseURL string) string {
	if s != nil && s.settings != nil {
		if value, err := s.settings.GetValue(context.Background(), "public.base_url"); err == nil {
			if configured := strings.TrimRight(strings.TrimSpace(value), "/"); configured != "" {
				return configured
			}
		}
	}
	if s != nil && s.cfg != nil {
		if configured := strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/"); configured != "" {
			return configured
		}
	}
	return strings.TrimRight(strings.TrimSpace(requestBaseURL), "/")
}

func (s *V1Service) videoEventForUser(ctx context.Context, principal *APIPrincipal, id string) (*model.EventLog, error) {
	ev, err := s.events.GetByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if !eventOwnedByPrincipal(ev, principal, "video") {
		return nil, ErrVideoJobNotFound
	}
	return ev, nil
}

func eventOwnedByPrincipal(ev *model.EventLog, principal *APIPrincipal, kind string) bool {
	if ev == nil || ev.Kind != kind || principal == nil {
		return false
	}
	if principal.TokenType == "admin" {
		return ev.Source == "admin" && strings.TrimSpace(ev.APICredentialID) == ""
	}
	return principal.Credential != nil && strings.TrimSpace(ev.APICredentialID) != "" && ev.APICredentialID == principal.Credential.ID
}

// videoJobStatus maps our event status → OpenAI's (queued|in_progress|completed|
// failed) plus a coarse progress.
func videoJobStatus(ev *model.EventLog) (string, int) {
	switch ev.Status {
	case "success":
		return "completed", 100
	case "failed":
		return "failed", 0
	default:
		if strings.TrimSpace(ev.AccountID) != "" {
			return "in_progress", 50
		}
		return "queued", 0
	}
}

func videoJobObject(id, modelID, status string, progress int, seconds, size string, createdAt, completedAt int64, errMsg string) map[string]any {
	obj := map[string]any{
		"id":         id,
		"object":     "video",
		"model":      modelID,
		"status":     status,
		"progress":   progress,
		"created_at": createdAt,
		"size":       size,
		"seconds":    strings.TrimSuffix(strings.TrimSpace(seconds), "s"),
	}
	if completedAt > 0 {
		obj["completed_at"] = completedAt
	} else {
		obj["completed_at"] = nil
	}
	if errMsg != "" {
		obj["error"] = map[string]any{"message": errMsg}
	} else {
		obj["error"] = nil
	}
	return obj
}

// sizeFromRatioRes reconstructs an OpenAI-style "WxH" label from our stored ratio
// + resolution tier (best-effort; only for display in the job object).
func sizeFromRatioRes(ratio, resolution string) string {
	long := 720
	res := strings.ToUpper(resolution)
	switch {
	case strings.Contains(res, "1080") || strings.Contains(res, "2K"):
		long = 1080
	case strings.Contains(res, "4K") || strings.Contains(res, "2160"):
		long = 2160
	}
	w, h := long, long
	switch strings.TrimSpace(ratio) {
	case "21:9":
		w, h = long, long*9/21
	case "16:9":
		w, h = long, long*9/16
	case "9:16":
		w, h = long*9/16, long
	case "4:3":
		w, h = long, long*3/4
	case "3:4":
		w, h = long*3/4, long
	case "1:1":
		w, h = long, long
	default:
		w, h = long, long*9/16
	}
	return fmt.Sprintf("%dx%d", w, h)
}

func (s *V1Service) prepareImage(ctx context.Context, principal *APIPrincipal, in V1ImageRequest, charge bool) (*model.ModelConfig, string, string, float64, error) {
	modelID := strings.TrimSpace(in.Model)
	prompt := strings.TrimSpace(in.Prompt)
	if modelID == "" || prompt == "" {
		return nil, "", "", 0, fmt.Errorf("%w: model and prompt are required", ErrUnsupportedParams)
	}
	n := in.N
	if n == 0 {
		n = 1
	}
	if n != 1 {
		return nil, "", "", 0, fmt.Errorf("%w: n must be 1", ErrUnsupportedParams)
	}
	if strings.TrimSpace(in.Background) != "" {
		return nil, "", "", 0, fmt.Errorf("%w: background is not supported", ErrUnsupportedParams)
	}
	if strings.TrimSpace(in.OutputFormat) != "" {
		return nil, "", "", 0, fmt.Errorf("%w: output_format is not supported", ErrUnsupportedParams)
	}
	modelItem, err := s.models.Get(ctx, modelID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", "", 0, ErrUnknownModel
		}
		return nil, "", "", 0, err
	}
	if !modelItem.Enabled || modelItem.Type != "image" {
		return nil, "", "", 0, ErrUnknownModel
	}
	// Reject oversized reference images before any provider/account is selected.
	if err := ensureReferenceSizes(in.ReferenceImages); err != nil {
		return nil, "", "", 0, err
	}
	aspectRatio, resolution := resolveImageSize(modelItem, in)
	operation := "generation"
	if len(in.ReferenceImages) > 0 {
		operation = "edit"
	}
	if _, err := s.firstAvailableRoute(ctx, modelItem.ID, "image", model.RouteRequirements{Operation: operation,
		Ratio: aspectRatio, Resolution: resolution, Quality: in.Quality, ReferenceImages: len(in.ReferenceImages)}); err != nil {
		return nil, "", "", 0, err
	}
	return modelItem, resolution, aspectRatio, 0, nil
}

func (s *V1Service) prepareVideo(ctx context.Context, principal *APIPrincipal, in V1VideoRequest, charge bool) (*model.ModelConfig, string, string, string, float64, error) {
	modelID := strings.TrimSpace(in.Model)
	prompt := strings.TrimSpace(in.Prompt)
	duration := strings.TrimSpace(in.Duration)
	if modelID == "" || prompt == "" {
		return nil, "", "", "", 0, fmt.Errorf("%w: model and prompt are required", ErrUnsupportedParams)
	}
	if duration == "" {
		return nil, "", "", "", 0, fmt.Errorf("%w: duration is required", ErrUnsupportedParams)
	}
	modelItem, err := s.models.Get(ctx, modelID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", "", "", 0, ErrUnknownModel
		}
		return nil, "", "", "", 0, err
	}
	if !modelItem.Enabled || modelItem.Type != "video" {
		return nil, "", "", "", 0, ErrUnknownModel
	}
	// Validate bytes before account selection; route-specific count constraints
	// are matched as one capability profile below.
	if err := ensureReferenceSizes(in.ReferenceImages); err != nil {
		return nil, "", "", "", 0, err
	}
	if err := validateMediaReferences(in.ReferenceVideos, "video"); err != nil {
		return nil, "", "", "", 0, err
	}
	if err := validateMediaReferences(in.ReferenceAudios, "audio"); err != nil {
		return nil, "", "", "", 0, err
	}
	if len(in.ReferenceVideos) > 0 && (modelItem.ID == "kling-3" || modelItem.ID == "kling-o3") && parseDurationSeconds(duration) > 10 {
		return nil, "", "", "", 0, fmt.Errorf("%w: Kling video modification supports 3s to 10s", ErrUnsupportedParams)
	}
	aspectRatio := strings.TrimSpace(strings.ReplaceAll(in.AspectRatio, "x", ":"))
	if aspectRatio == "" {
		aspectRatio = "16:9"
	}
	resolution := strings.TrimSpace(in.Resolution)
	if resolution == "" {
		resolution = "720p"
	}
	if _, err := s.firstAvailableRoute(ctx, modelItem.ID, "video", model.RouteRequirements{Operation: "generation",
		Ratio: aspectRatio, Resolution: resolution, Duration: duration, ReferenceImages: len(in.ReferenceImages),
		ReferenceVideos: len(in.ReferenceVideos), ReferenceAudios: len(in.ReferenceAudios), GenerateAudio: in.GenerateAudio}); err != nil {
		return nil, "", "", "", 0, err
	}
	return modelItem, resolution, aspectRatio, duration, 0, nil
}

func (s *V1Service) chargeForModel(ctx context.Context, principal *APIPrincipal, modelItem *model.ModelConfig, kind, resolution, duration string, surcharge float64, charge bool) (float64, error) {
	// 2API does not meter or debit downstream credits. Provider quota is reserved
	// independently by the account dispatcher.
	return 0, nil
}

func (s *V1Service) userDir(principal *APIPrincipal) string {
	if principal == nil || principal.Credential == nil {
		return "anon"
	}
	return "api-" + sanitizeOwnerName(principal.Credential.ID)
}

// contentTypeForExt maps a file extension to a MIME type for storage uploads.
func contentTypeForExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	case "heic", "heif":
		return "image/heic"
	case "gif":
		return "image/gif"
	case "mp4":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mov":
		return "video/quicktime"
	default:
		return "application/octet-stream"
	}
}

func detectImageArtifact(raw []byte) (string, string, error) {
	contentType := netguard.DetectMediaType(raw)
	if !strings.HasPrefix(contentType, "image/") || contentType == "image/svg+xml" {
		return "", "", netguard.ErrInvalidMedia
	}
	extension, ok := netguard.MediaExtension(contentType)
	if !ok {
		return "", "", netguard.ErrInvalidMedia
	}
	return contentType, extension, nil
}

func replaceMediaExtension(value, extension string) string {
	value = strings.TrimSpace(value)
	extension = strings.TrimSpace(extension)
	if value == "" || extension == "" {
		return value
	}
	if !strings.HasPrefix(extension, ".") {
		extension = "." + extension
	}
	oldExtension := filepath.Ext(value)
	if oldExtension == "" {
		return value + extension
	}
	return strings.TrimSuffix(value, oldExtension) + extension
}

func recoveredImageArtifact(existingKey, eventID string, raw []byte) (string, string, error) {
	contentType, extension, err := detectImageArtifact(raw)
	if err != nil {
		return "", "", err
	}
	objectKey := replaceMediaExtension(existingKey, extension)
	if objectKey == "" {
		objectKey = imageCacheKey(eventID) + extension
	}
	return objectKey, contentType, nil
}

// allocateOutput builds the object key (= relative path, user-scoped) and the
// directly-downloadable URL pointing at this site's /images proxy. Nothing is
// written here — the bytes are uploaded to RustFS by the caller.
func (s *V1Service) allocateOutput(principal *APIPrincipal, ext, baseURL string) (string, string) {
	userDir := s.userDir(principal)
	filename := time.Now().Format("20060102-150405") + "-" + randomUpper(8) + "." + strings.TrimPrefix(ext, ".")
	relativePath := filepath.ToSlash(filepath.Join(userDir, filename))
	// OpenAI-style clients need a directly-downloadable absolute URL. When the
	// inbound request's base URL is known, build "{scheme}://{host}/images/...";
	// otherwise fall back to the relative path for backward compatibility.
	if base := strings.TrimRight(strings.TrimSpace(baseURL), "/"); base != "" {
		return base + "/images/" + relativePath, relativePath
	}
	return "/images/" + relativePath, relativePath
}

func (s *V1Service) logPendingEvent(ctx context.Context, kind string, modelItem *model.ModelConfig, principal *APIPrincipal, prompt, ratio, resolution, duration string, refs int, cost float64, file, source string, refFiles []string, deai bool, requestID, responseFormat string, requestFingerprint ...string) (string, error) {
	event := &model.EventLog{
		ID:             "evt-" + randomUpper(24),
		RequestID:      requestID,
		ResponseFormat: strings.ToLower(strings.TrimSpace(responseFormat)),
		TS:             time.Now(),
		Kind:           kind,
		Status:         "pending",
		Model:          modelItem.EffectiveName(),
		Prompt:         prompt,
		Ratio:          ratio,
		Resolution:     resolution,
		Duration:       duration,
		Refs:           refs,
		DeAI:           deai,
		Source:         source,
		Cost:           cost,
		File:           file,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if len(requestFingerprint) > 0 {
		event.RequestFingerprint = requestFingerprint[0]
	}
	if len(refFiles) > 0 {
		event.RefFiles = jsonArray(refFiles)
	}
	if principal != nil && principal.Credential != nil {
		event.APICredentialID = principal.Credential.ID
	}
	if err := s.events.Create(ctx, event); err != nil {
		return "", err
	}
	return event.ID, nil
}

// grokConcurrencyPerAccount is how many simultaneous generations one grok account
// may run (grok tolerates 10, unlike the 1-per-account default elsewhere).
const grokConcurrencyPerAccount = 10

// Adobe partner/points accounts use a bounded four-way simultaneous-generation
// limit. This is also the default for newly imported accounts when no override
// is stored, while keeping burst traffic below the provider's risk threshold.
const adobePointsConcurrencyPerAccount = 4

// Absorb short bursts above the account pool capacity instead of rejecting them
// immediately. The generation context owns the upper bound for the whole job.
// accountFailureCooldown is how long an account stays demoted after an upstream
// (auth/temporary/fatal) failure. Short on purpose: it only reorders the
// rotation, and a pool where every account is cooling behaves exactly as before.
const accountFailureCooldown = 45 * time.Second

// oreateSpamQuarantine is how long a spam-user verdict keeps an account out of
// the Oreate rotation. The verdict is an upstream state, not a broken
// credential, so it must expire on its own; the window only has to outlive a
// burst of queued requests.
const oreateSpamQuarantine = 30 * time.Minute

const providerAccountQueueWait = 90 * time.Second
const providerAccountQueuePoll = 300 * time.Millisecond

// A real 10-way production burst on 2026-09-05 saw seven first-account beta
// failures and three requests whose second account hit the same verdict. Six
// distinct accounts keeps the retry bounded while making a repeated beta blip
// very unlikely to escape to the caller. Every failed task must independently
// pass the four-snapshot no-charge proof before it consumes another slot in
// this budget.
const maxBytePlusBetaAccountAttempts = 6

type bytePlusBetaRetryBudget struct {
	active              bool
	attempts            int
	lastErr             error
	nextAccountDeadline time.Time
}

// handle consumes one completed account result after the special retry chain
// has started (or starts it for the first verified beta failure). retry=true
// means the caller may select another, not-yet-attempted BytePlus account.
// A non-beta result ends the chain: accepted/ambiguous work keeps its native
// no-resubmit error, while a definite pre-acceptance failure receives the
// private exhausted marker solely to prevent cross-route fan-out.
func (budget *bytePlusBetaRetryBudget) handle(err error, now time.Time) (retry bool, terminal error) {
	verified := bytePlusBetaRetryVerified(err)
	if !budget.active && !verified {
		return false, err
	}
	if !verified {
		if byteplusNoResubmit(err) {
			return false, err
		}
		return false, errors.Join(err, errBytePlusRetryBudgetExhausted)
	}

	budget.active = true
	budget.attempts++
	budget.lastErr = err
	// Provider execution can itself take longer than the ordinary queue window.
	// Start a fresh bounded wait after each verified terminal result so a free
	// alternate is not rejected merely because the previous task ran slowly.
	budget.nextAccountDeadline = now.Add(providerAccountQueueWait)
	if budget.attempts >= maxBytePlusBetaAccountAttempts {
		return false, errors.Join(err, errBytePlusRetryBudgetExhausted)
	}
	return true, nil
}

// Bound temporary failover so a shared-egress outage cannot fan one downstream
// request across the entire account pool. This is retry/failover accounting,
// not a submit rate limiter or circuit breaker.
const maxTempFailoverAccounts = 3

// Temporary upstream errors (including provider-side overload) are absorbed by waiting
// and retrying inside the request instead of failing it: the synchronous
// response heartbeats keep the downstream connection alive, so a queued burst
// degrades into slower responses rather than user-visible errors. The window
// bounds the total wait; the backoff only separates retries after failures.
const (
	tempRetryWindow         = 300 * time.Second
	tempRetryInitialBackoff = 3 * time.Second
	tempRetryMaxBackoff     = 12 * time.Second
)

// runPoolWithFailover drives a generation across a round-robin-ordered account
// list with per-error-class behavior, so a bad request never burns the whole
// pool while genuinely limited accounts still fail over:
//   - 额度耗尽 quota → mark the account and FAIL OVER to the next account
//     immediately (same-account retry can't help). Repeats until one succeeds or
//     the pool is exhausted.
//   - 认证失效 auth → refresh the token from its cookie and retry ONCE with the
//     fresh token; if it still auth-fails (or there's nothing to refresh, e.g.
//     chatgpt's JWT IS the credential), mark the account and fail over.
//   - 上游临时 temporary → record the failure (no disable/dead) and FAIL OVER to
//     the next account immediately, capped by maxTempFailoverAccounts so a
//     pool-wide blip can't fan a single request out across everything.
//   - 参数错 / request-level (anything else) → return immediately, no retry, no
//     account penalty (the account isn't at fault).
//
// Returns the actual upstream error (never a synthetic "retry failed"). On
// success it stamps success_total/fails=0 on the winning account. classify maps
// a provider error to (isAuth, isQuota, isTemporary, isDead). refreshOnAuth
// (nil for providers whose token IS the credential) re-mints the account's token
// so an auth retry uses a FRESH token instead of replaying the stale one.
func (s *V1Service) runPoolWithFailover(ctx context.Context, eventID, pool string, active []model.TokenAccount, kind string,
	attempt func(token model.TokenAccount) ([]byte, error),
	classify func(error) (isAuth, isQuota, isTemporary, isDead bool),
	refreshOnAuth func(tokenID string) (model.TokenAccount, bool),
	tempFailover bool,
) ([]byte, error) {
	return s.runPoolWithFailoverPolicy(ctx, eventID, pool, active, kind, attempt, classify, refreshOnAuth, tempFailover, true)
}

// runPoolWithFailoverPolicy is the policy-bearing implementation. Most pools
// retain the bounded in-request temporary retry. Adobe third-party images pass
// retryTemporary=false because they have an explicit cross-provider fallback;
// waiting five minutes before entering that fallback defeats its purpose.
func (s *V1Service) runPoolWithFailoverPolicy(ctx context.Context, eventID, pool string, active []model.TokenAccount, kind string,
	attempt func(token model.TokenAccount) ([]byte, error),
	classify func(error) (isAuth, isQuota, isTemporary, isDead bool),
	refreshOnAuth func(tokenID string) (model.TokenAccount, bool),
	tempFailover bool,
	retryTemporary bool,
) ([]byte, error) {
	if plan, ok := dispatchPlanFromContext(ctx); ok {
		var routeErr error
		active, routeErr = s.routeAccounts(ctx, plan.Route, active)
		if routeErr != nil {
			return nil, routeErr
		}
		if len(active) == 0 {
			return nil, ErrNoProviderAccount
		}
	}
	policy := poolPolicy(ctx)
	retryTemporary = retryTemporary && !policy.fastFailover
	requestState := policy.accountRequest(ctx, pool)
	excludedAccounts := requestState.excluded
	var refreshedAt time.Time
	tempDeadCount := 0
	queueDeadline := time.Now().Add(providerAccountQueueWait)
	tempRetryDeadline := time.Now().Add(tempRetryWindow)
	tempRetryBackoff := tempRetryInitialBackoff
	// A verified terminal BytePlus beta-instability verdict may walk a bounded
	// set of different accounts. The attempted set prevents cycling back to any
	// account already used by this request; the special budget also bypasses the
	// ordinary 300-second temporary retry loop and never crosses provider routes.
	var bytePlusBetaRetry bytePlusBetaRetryBudget
	attemptedAccounts := requestState.attempted
	// waitTempRetry pauses before re-running the pool after a temporary
	// upstream failure. It reports false once the retry window is spent or the
	// caller has gone away, at which point the error is surfaced.
	waitTempRetry := func() bool {
		if !retryTemporary || time.Now().After(tempRetryDeadline) || ctx.Err() != nil {
			return false
		}
		timer := time.NewTimer(tempRetryBackoff)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
		tempRetryBackoff *= 2
		if tempRetryBackoff > tempRetryMaxBackoff {
			tempRetryBackoff = tempRetryMaxBackoff
		}
		tempDeadCount = 0
		return true
	}
	for {
		var refreshErr error
		if time.Since(refreshedAt) >= time.Second {
			active, refreshErr = s.refreshPoolAccounts(ctx, pool, kind, active, excludedAccounts)
			refreshedAt = time.Now()
		}
		if refreshErr != nil || len(active) == 0 {
			if bytePlusBetaRetry.active {
				return nil, errors.Join(bytePlusBetaRetry.lastErr, refreshErr)
			}
			if refreshErr != nil {
				return nil, refreshErr
			}
			return nil, ErrNoProviderAccount
		}
		counts, observed := s.conc.ActiveCounts(ctx, accountGateKeys(active))
		var lastErr error
		lastTempDead := false
		retrying := false
		busy := 0
		for _, token := range active {
			if excludedAccounts[token.ID] {
				continue
			}
			if bytePlusBetaRetry.active {
				if _, alreadyTried := attemptedAccounts[token.ID]; alreadyTried {
					continue
				}
			}
			slots := poolAccountConcurrency(pool, token)
			if observed && counts["conc:a:"+token.ID] >= int64(slots) {
				busy++
				continue
			}
			admitted, gateErr := s.acctAcquire(ctx, token.ID, eventID, slots)
			if gateErr != nil {
				if bytePlusBetaRetry.active {
					return nil, errors.Join(bytePlusBetaRetry.lastErr, gateErr)
				}
				return nil, gateErr
			}
			if !admitted {
				busy++
				continue
			}
			_, retry := attemptedAccounts[token.ID]
			attemptedAccounts[token.ID] = struct{}{}
			// release via defer so a panic in tryAccount can't leak the job slot.
			data, failover, tempDead, err := func() ([]byte, bool, bool, error) {
				defer s.acctRelease(ctx, token.ID, eventID)
				return s.tryAccount(ctx, eventID, pool, token, kind, attempt, classify, refreshOnAuth, tempFailover, retry)
			}()
			if err == nil {
				return data, nil
			}
			lastErr = err
			lastTempDead = tempDead
			isAuth, isQuota, _, isDead := classify(err)
			if isAuth || isQuota || (isDead && !tempDead) || errors.Is(err, ErrNoProviderAccount) {
				excludedAccounts[token.ID] = true
			}
			// Once the job's deadline is spent, another account can only fail on
			// the expired context and would mask the failure that consumed it.
			if ctx.Err() != nil {
				if bytePlusBetaRetry.active {
					// Preserve the verified-beta marker even when an alternate's final
					// bookkeeping races the generation deadline. Returning only the
					// alternate error here could incorrectly authorize route failover.
					return nil, errors.Join(bytePlusBetaRetry.lastErr, lastErr, ctx.Err())
				}
				return nil, lastErr
			}
			if bytePlusBetaRetry.active || (pool == "byteplus" && bytePlusBetaRetryVerified(err)) {
				retry, terminal := bytePlusBetaRetry.handle(err, time.Now())
				if terminal != nil {
					return nil, terminal
				}
				if retry {
					continue
				}
			}
			if tempDead {
				// temp-failover policy: this account hit a temporary upstream error.
				// Cap how many accounts one burst may burn, then wait and retry
				// instead of failing the request while the retry window lasts.
				tempDeadCount++
				if tempDeadCount >= maxTempFailoverAccounts {
					if waitTempRetry() {
						retrying = true
						break
					}
					return nil, lastErr
				}
			}
			if failover {
				continue
			}
			// temporary exhausted or request-level error → surface it, no fan-out.
			return nil, lastErr
		}

		if retrying {
			continue
		}
		if bytePlusBetaRetry.active && busy == 0 {
			return nil, bytePlusBetaRetry.lastErr
		}
		if lastErr != nil && busy == 0 && !bytePlusBetaRetry.active {
			// The whole pool was tried and the last failure was a temporary
			// upstream error → wait and retry within the window; anything else
			// (auth/quota exhaustion across the pool) is surfaced immediately.
			if lastTempDead && waitTempRetry() {
				continue
			}
			return nil, lastErr
		}
		if busy == 0 {
			return nil, ErrProviderExecution
		}
		if policy.fastFailover && !bytePlusBetaRetry.active {
			return nil, ErrConcurrencyFull
		}
		accountQueueDeadline := queueDeadline
		if bytePlusBetaRetry.active {
			accountQueueDeadline = bytePlusBetaRetry.nextAccountDeadline
		}
		if time.Now().After(accountQueueDeadline) {
			if bytePlusBetaRetry.active {
				return nil, bytePlusBetaRetry.lastErr
			}
			return nil, ErrConcurrencyFull
		}

		if !waitAccountQueue(ctx, accountQueueDeadline) {
			if bytePlusBetaRetry.active {
				return nil, errors.Join(bytePlusBetaRetry.lastErr, ctx.Err())
			}
			return nil, ErrConcurrencyFull
		}
	}
}

// tryAccount runs one account's attempt with the pool's retry policy:
// 额度耗尽/认证失效 → mark + failover; 上游临时 → record failure + failover (capped
// via the tempDead return); 参数错 → fail fast. Returns (data, failover,
// tempDead, err) — failover=true means move on to the next account. The per-account
// concurrency gate is held by the caller.
func (s *V1Service) tryAccount(ctx context.Context, eventID, pool string, token model.TokenAccount, kind string,
	attempt func(token model.TokenAccount) ([]byte, error),
	classify func(error) (isAuth, isQuota, isTemporary, isDead bool),
	refreshOnAuth func(tokenID string) (model.TokenAccount, bool),
	tempFailover bool,
	retry bool,
) ([]byte, bool, bool, error) {
	routeID := dispatchRouteFromContext(ctx)
	if plan, ok := dispatchPlanFromContext(ctx); ok && pool == "dola" {
		plan.QuotaDay = time.Now()
		ctx = context.WithValue(ctx, dispatchRouteKey, plan)
	}
	fresh, validationErr := s.revalidateDispatchAccount(ctx, pool, token.ID, kind, retry)
	if validationErr != nil {
		return nil, errors.Is(validationErr, ErrNoProviderAccount) || routeFailoverSafe(validationErr), false, validationErr
	}
	token = fresh
	recordBookkeepingError("set generation account", s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail))
	recordBookkeepingError("touch generation account", s.tokens.TouchLastUsed(ctx, token.ID))
	authRefreshed := false
	for {
		if authRefreshed {
			fresh, err := s.revalidateDispatchAccount(ctx, pool, token.ID, kind, true)
			if err != nil {
				return nil, errors.Is(err, ErrNoProviderAccount) || routeFailoverSafe(err), false, err
			}
			token = fresh
		}
		var dispatchID string
		var reservation *model.QuotaReservation
		if routeID != "" {
			attemptRow, startErr := s.models.Dispatch().Start(context.WithoutCancel(ctx), eventID, routeID, token.ID)
			if startErr != nil {
				return nil, false, false, startErr
			}
			dispatchID = attemptRow.ID
			plan, _ := dispatchPlanFromContext(ctx)
			bucketKey := strings.TrimSpace(plan.Route.QuotaBucketKey)
			if binding, bindingErr := s.models.Routes().AccountRoute(ctx, token.ID, routeID); bindingErr == nil && strings.TrimSpace(binding.QuotaBucketKey) != "" {
				bucketKey = strings.TrimSpace(binding.QuotaBucketKey)
			}
			bucketKey = dispatchQuotaBucket(plan, bucketKey)
			if bucketKey != "" && plan.Cost > 0 {
				unit := "credits"
				if policy, ok := model.DecodeQuotaCostPolicy(plan.Route.QuotaCosts); ok && strings.TrimSpace(policy.Unit) != "" {
					unit = strings.TrimSpace(policy.Unit)
				}
				reservation, startErr = s.models.Quotas().ReserveWithUnit(context.WithoutCancel(ctx), eventID, dispatchID, token.ID, bucketKey, unit, plan.Cost)
				if startErr != nil {
					if !errors.Is(startErr, repo.ErrQuotaUnavailable) {
						recordBookkeepingError("finish failed quota reservation", s.models.Dispatch().Finish(context.WithoutCancel(ctx), dispatchID, "failed", "temporary", "", errors.New("quota reservation unavailable")))
						return nil, false, false, startErr
					}
					quotaErr := providerQuotaError(pool, startErr)
					recordBookkeepingError("finish rejected quota dispatch", s.models.Dispatch().Finish(context.WithoutCancel(ctx), dispatchID, "failed", "quota", "", publicGenerationError(quotaErr)))
					return nil, true, false, quotaErr
				}
			}
		}
		data, err := attempt(token)
		var upstreamRemaining *float64
		if routeID != "" {
			upstreamRemaining = s.refreshDispatchQuota(ctx, pool, token, kind)
		}
		finalizedErr := finalizeBytePlusRetryVerification(err, upstreamRemaining)
		if finalizedErr != err {
			// The final quota refresh is newer than the closure's two confirmations.
			// A delayed debit or unavailable snapshot revokes retry permission while
			// preserving the provider's accepted terminal error and task ID.
			err = finalizedErr
		}
		state, failureClass := dispatchFailureClass(err)
		if reservation != nil {
			quotaCtx := context.WithoutCancel(ctx)
			if errors.Is(err, dola.ErrTaskAccepted) {
				// A terminal content rejection still consumed an accepted attempt.
				// Closing its dispatch must not refund the daily generation.
				recordBookkeepingError("settle accepted Dola dispatch quota", s.models.Quotas().Settle(quotaCtx, reservation.ID, upstreamRemaining))
			} else if errors.Is(err, byteplus.ErrRetryableTaskFailed) && !bytePlusBetaRetryVerified(err) {
				// The task is definitely terminal, so close its dispatch; without a
				// no-charge proof, settle rather than refund the reservation.
				recordBookkeepingError("settle unverified beta dispatch quota", s.models.Quotas().Settle(quotaCtx, reservation.ID, upstreamRemaining))
			} else {
				switch state {
				case "succeeded", "accepted":
					recordBookkeepingError("settle dispatch quota", s.models.Quotas().Settle(quotaCtx, reservation.ID, upstreamRemaining))
				case "unknown":
					recordBookkeepingError("mark dispatch quota uncertain", s.models.Quotas().MarkUncertain(quotaCtx, reservation.ID))
				default:
					if failureClass == "quota" {
						// An explicit provider quota verdict is authoritative. Releasing the
						// hold after a refreshed zero would add the request cost back and make
						// the exhausted bucket immediately schedulable again.
						if upstreamRemaining == nil {
							zero := 0.0
							upstreamRemaining = &zero
						}
						recordBookkeepingError("settle exhausted dispatch quota", s.models.Quotas().Settle(quotaCtx, reservation.ID, upstreamRemaining))
					} else {
						recordBookkeepingError("release dispatch quota", s.models.Quotas().Release(quotaCtx, reservation.ID))
					}
				}
			}
		}
		if dispatchID != "" {
			recordBookkeepingError("finish dispatch attempt", s.models.Dispatch().Finish(context.WithoutCancel(ctx), dispatchID, state, failureClass, dispatchUpstreamTaskID(err), publicGenerationError(err)))
			recordBookkeepingError("record account route result", s.models.Routes().RecordAccountRouteResult(context.WithoutCancel(ctx), token.ID, routeID, failureClass, err == nil))
		}
		if err == nil {
			_, updateErr := s.tokens.Update(ctx, pool, token.ID, map[string]any{
				"last_used_at":  time.Now(),
				"success_total": gorm.Expr("success_total + 1"),
				"fails":         0,
			})
			recordBookkeepingError("record account success", updateErr)
			return data, false, false, nil
		}
		if failureClass == "entitlement" {
			// Entitlement is scoped to this account+route. The durable binding was
			// disabled above; the credential remains healthy for every other route.
			return nil, true, false, err
		}
		isAuth, isQuota, isTemp, isDead := classify(err)
		if isQuota {
			if shouldMarkAccountQuota(err) {
				s.markTokenFailure(ctx, pool, token, kind, false, true)
			}
			return nil, true, false, err
		}
		if isAuth {
			// Refresh from cookie and retry ONCE; otherwise the credential is dead.
			if refreshOnAuth != nil && !authRefreshed {
				if refreshed, ok := refreshOnAuth(token.ID); ok {
					token = refreshed
					authRefreshed = true
					continue
				}
			}
			s.markTokenFailure(ctx, pool, token, kind, true, false)
			s.coolDownAccountWithToken(pool, token)
			return nil, true, false, err
		}
		// Fatal / temporary-under-failover-policy upstream error.
		if isDead || (isTemp && tempFailover) {
			if tempFailover {
				// Ops policy (adobe): NEVER kill on these upstream errors — a
				// genuinely bad account and a transient Adobe blip (429/5xx/
				// overload) look the same, and killing wipes healthy accounts.
				// Record the failure and fail over to the next account (no
				// disable/dead). The 4th return value caps how many accounts one
				// request may burn this way (maxTempFailoverAccounts) so a pool-wide
				// blip can't fan a single request across the whole pool.
				s.markTokenUpstreamFailure(ctx, pool, token)
				s.coolDownAccountWithToken(pool, token)
				return nil, true, true, err
			}
			s.markTokenDead(ctx, pool, token, kind)
			// Permanent account death must not consume the temporary-failure cap.
			return nil, true, false, err
		}
		if isTemp {
			// Temporary upstream error → record it against the upstream (not the
			// account) and fail over to the NEXT account, capped via the tempDead
			// return so a pool-wide blip can't fan one request across the whole pool.
			s.markTokenUpstreamFailure(ctx, pool, token)
			s.coolDownAccountWithToken(pool, token)
			return nil, true, true, err
		}
		return nil, false, false, err // 参数错 / request-level
	}
}

func dispatchUpstreamTaskID(err error) string {
	var dolaErr *dola.VideoSubmissionError
	if errors.As(err, &dolaErr) {
		return dolaErr.ConversationID
	}
	if err == nil {
		return ""
	}
	return byteplus.AcceptedTaskID(err)
}

func providerQuotaError(pool string, cause error) error {
	switch pool {
	case "adobe":
		return fmt.Errorf("%w: %w", adobe.ErrQuotaExhausted, cause)
	case "byteplus":
		return fmt.Errorf("%w: %w", byteplus.ErrQuotaExhausted, cause)
	case "chatgpt":
		return fmt.Errorf("%w: %w", chatgpt.ErrQuotaExhausted, cause)
	case "runway":
		return fmt.Errorf("%w: %w", runway.ErrQuotaExhausted, cause)
	case "grok":
		return fmt.Errorf("%w: %w", grok.ErrQuotaExhausted, cause)
	case "oreate":
		return fmt.Errorf("%w: %w", oreate.ErrQuotaExhausted, cause)
	case "dola":
		return fmt.Errorf("%w: %w", dola.ErrQuotaExhausted, cause)
	case "custom":
		return fmt.Errorf("%w: %w", custom.ErrQuotaExhausted, cause)
	default:
		return fmt.Errorf("%w: %v", ErrProviderQuota, cause)
	}
}

// refreshDispatchQuota is deliberately called after every real provider
// attempt, regardless of outcome. The snapshot is written while this attempt's
// reservation is still held, so concurrent completions cannot erase one
// another's allowance.
func (s *V1Service) refreshDispatchQuota(parent context.Context, pool string, token model.TokenAccount, kind string) *float64 {
	plan, ok := dispatchPlanFromContext(parent)
	if !ok || strings.TrimSpace(plan.Route.QuotaBucketKey) == "" || pool == "custom" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer cancel()
	var data map[string]any
	var err error
	switch pool {
	case "adobe":
		if s.adobe != nil {
			data, err = s.adobe.FetchCreditsBalance(probeCtx, token.Value)
		}
	case "byteplus":
		if s.byteplus != nil {
			data, err = s.byteplus.FetchCreditsBalance(probeCtx, token.Value)
		}
	case "chatgpt":
		if s.chatgpt != nil && kind == "image" {
			data, err = s.chatgpt.FetchImageQuota(probeCtx, token.Value)
		}
	case "runway":
		if s.runway != nil {
			data, err = s.runway.FetchCreditsBalance(probeCtx, token.Value)
		}
	case "grok":
		if s.grok != nil && kind != "text" {
			data, err = s.grok.FetchCreditsBalance(probeCtx, token.Value)
		}
	case "oreate":
		if s.oreate != nil {
			data, err = s.oreate.FetchCreditsBalance(probeCtx, oreateAccountFromToken(token))
		}
	}
	if err != nil || data == nil || boolValueWithDefault(data["unknown"], false) || boolValueWithDefault(data["auth_failed"], false) {
		return nil
	}
	remaining, hasRemaining := anyFloat(data["remaining"])
	if !hasRemaining {
		return nil
	}
	var total *float64
	if value, exists := anyFloat(data["total"]); exists {
		total = &value
	}
	bucketKey := strings.TrimSpace(plan.Route.QuotaBucketKey)
	if binding, bindingErr := s.models.Routes().AccountRoute(probeCtx, token.ID, plan.Route.ID); bindingErr == nil && strings.TrimSpace(binding.QuotaBucketKey) != "" {
		bucketKey = strings.TrimSpace(binding.QuotaBucketKey)
	}
	resetAt := parseResetTime(data["reset_after"])
	if resetAt == nil {
		resetAt = parseResetTime(data["available_until"])
	}
	unit := "credits"
	if policy, ok := model.DecodeQuotaCostPolicy(plan.Route.QuotaCosts); ok && strings.TrimSpace(policy.Unit) != "" {
		unit = strings.TrimSpace(policy.Unit)
	}
	_, snapshotErr := s.models.Quotas().UpsertSnapshot(context.WithoutCancel(parent), token.ID, bucketKey, unit, total, &remaining, resetAt)
	recordBookkeepingError("refresh quota snapshot", snapshotErr)
	metaPatch := map[string]any{
		"cached_quota_remaining": canonicalQuotaNumber(remaining),
		"cached_quota_at":        int(time.Now().Unix()),
	}
	if total != nil {
		metaPatch["cached_quota_total"] = canonicalQuotaNumber(*total)
	}
	if used, exists := anyFloat(data["used"]); exists {
		metaPatch["cached_quota_used"] = canonicalQuotaNumber(used)
	}
	patch := map[string]any{}
	if rawReset := strings.TrimSpace(stringValue(data["reset_after"])); rawReset != "" {
		patch["cached_quota_reset_after"] = rawReset
	} else if rawReset := strings.TrimSpace(stringValue(data["available_until"])); rawReset != "" {
		patch["cached_quota_reset_after"] = rawReset
	}
	tokenErr := s.tokens.UpdateMergingMeta(context.WithoutCancel(parent), pool, token.ID, metaPatch, patch)
	recordBookkeepingError("refresh quota account metadata", tokenErr)
	return &remaining
}

func shouldMarkAccountQuota(err error) bool {
	return !errors.Is(err, errAccountTaskQuota)
}

func adobeErrClass(e error) (bool, bool, bool, bool) {
	return errors.Is(e, adobe.ErrAuth), errors.Is(e, adobe.ErrQuotaExhausted), errors.Is(e, adobe.ErrTemporaryUpstream), errors.Is(e, adobe.ErrDeadUpstream)
}

// noStore url-only mode: adobe returns a presigned image URL (meta["image_url"]);
// skip the download and return it directly.
func (s *V1Service) generateAdobeImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	urlOnly := noStore
	if s.adobe == nil {
		return nil, "", errors.New("adobe client not configured")
	}
	s.applyGlobalProxy(ctx)

	items, err := s.tokens.ListByPool(ctx, "adobe")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		// Image quota is tracked separately from video — an account whose video
		// quota is exhausted (VideoLimited) is still usable for image as long as
		// its image quota remains. status=="quota" means BOTH kinds are limited
		// (or a legacy/full quota mark), so it's excluded for either kind.
		if item.Status == "active" && !item.Dead && !item.ImageLimited && strings.TrimSpace(item.Value) != "" && !adobePointsAccount(item) && adobeAccountSupportsModel(item, modelItem.ID, "image") {
			active = append(active, item)
		}
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("adobe", active)

	refs, err := decodeReferenceImages(in.ReferenceImages, max(1, modelItem.MaxReferenceImages))
	if err != nil {
		return nil, "", err
	}

	// Round-robin order. Adobe uses tempFailover=true: a temporary upstream error
	// ("system under load") fails over to the next account without penalizing the
	// current one, capped at maxTempFailoverAccounts; auth/quota also fail over
	// (see runPoolWithFailover). imageURL is captured from the successful attempt.
	var imageURL string
	data, err := s.runPoolWithFailoverPolicy(ctx, eventID, "adobe", active, "image", func(token model.TokenAccount) ([]byte, error) {
		var blobIDs []string
		for _, ref := range refs {
			id, upErr := s.adobe.UploadImage(ctx, token.Value, ref, "image/png", "")
			if upErr != nil {
				return nil, upErr
			}
			blobIDs = append(blobIDs, id)
		}
		submitARP := adobe.SubmissionARPSessionToken(token.ARPSessionToken)
		d, meta, genErr := s.adobe.GenerateImage(ctx, token.Value, submitARP, modelItem.ID, in.Prompt, aspectRatio, resolution, blobIDs, false)
		if genErr == nil {
			imageURL = strings.TrimSpace(stringValue(meta["image_url"]))

		}
		return d, genErr
	}, adobeErrClass, func(id string) (model.TokenAccount, bool) {
		return s.refreshAdobeToken(ctx, id)
	}, true, strings.TrimSpace(in.AccountID) == "")
	if err == nil && !urlOnly {
		// Generation and quota settlement have already completed. Keep the URL
		// for diagnostics/recovery and retry only this download, never submission.
		recordBookkeepingError("retain generated image URL", s.events.SetFile(context.WithoutCancel(ctx), eventID, imageURL))
		data, err = downloadGeneratedArtifact(ctx, imageURL, func(ctx context.Context, url string) ([]byte, error) {
			data, _, err := downloadPublicArtifact(ctx, url, staticAssetHosts("adobe"), "", netguard.MediaImage, netguard.MaxImageBytes)
			return data, err
		})
	}
	return data, imageURL, err
}

func (s *V1Service) generateAdobeVideo(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1VideoRequest, aspectRatio, resolution string, durationSeconds int, downloadResult bool) ([]byte, string, error) {
	if s.adobe == nil {
		return nil, "", errors.New("adobe client not configured")
	}
	s.applyGlobalProxy(ctx)

	items, err := s.tokens.ListByPool(ctx, "adobe")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		// Video quota is tracked separately from image — skip accounts whose
		// video quota is exhausted (VideoLimited), but an image-only limit
		// (ImageLimited) leaves the account usable for video. status=="quota"
		// means BOTH kinds are limited (or a legacy/full quota mark), so it's
		// excluded for either kind.
		if item.Status == "active" && !item.Dead && !item.VideoLimited && strings.TrimSpace(item.Value) != "" && adobeAccountSupportsModel(item, modelItem.ID, "video") {
			active = append(active, item)
		}
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("adobe", active)

	refs, err := decodeReferenceImages(in.ReferenceImages, max(1, modelItem.MaxReferenceImages))
	if err != nil {
		return nil, "", err
	}

	engine, upstreamModel := resolveAdobeVideoEngine(modelItem.ID)
	referenceMode := defaultString(strings.TrimSpace(modelItem.ReferenceMode), "frame")

	// Round-robin order; fail over to the next account on auth/quota; temporary
	// upstream errors fail over too without penalizing the account (tempFailover,
	// capped at maxTempFailoverAccounts). videoURL is
	// captured from the successful attempt's meta (the upstream presigned URL).
	var videoURL string
	data, err := s.runPoolWithFailoverPolicy(ctx, eventID, "adobe", active, "video", func(token model.TokenAccount) ([]byte, error) {
		inputs := adobe.VideoInputs{GenerateAudio: in.GenerateAudio}
		for _, ref := range refs {
			id, upErr := s.adobe.UploadImage(ctx, token.Value, ref, "image/png", engine)
			if upErr != nil {
				return nil, upErr
			}
			inputs.ImageBlobIDs = append(inputs.ImageBlobIDs, id)
		}
		for _, ref := range in.ReferenceVideos {
			id, upErr := s.adobe.UploadMedia(ctx, token.Value, ref.Data, ref.ContentType, engine, "video")
			if upErr != nil {
				return nil, upErr
			}
			inputs.VideoBlobIDs = append(inputs.VideoBlobIDs, id)
		}
		for _, ref := range in.ReferenceAudios {
			id, upErr := s.adobe.UploadMedia(ctx, token.Value, ref.Data, ref.ContentType, engine, "audio")
			if upErr != nil {
				return nil, upErr
			}
			inputs.AudioBlobIDs = append(inputs.AudioBlobIDs, id)
		}
		submitARP := adobe.SubmissionARPSessionToken(token.ARPSessionToken)
		bytes, meta, genErr := s.adobe.GenerateVideo(ctx, token.Value, submitARP, engine, in.Prompt, aspectRatio, durationSeconds, resolution, referenceMode, upstreamModel, inputs, false)
		if genErr == nil {
			videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
			if downloadResult {
				bytes, _, genErr = downloadPublicArtifact(ctx, videoURL, staticAssetHosts("adobe"), "", netguard.MediaVideo, netguard.MaxVideoBytes)
				if genErr != nil {
					genErr = fmt.Errorf("%w: invalid video artifact", adobe.ErrTemporaryUpstream)
				}
			}
		}
		return bytes, genErr
	}, adobeErrClass, func(id string) (model.TokenAccount, bool) {
		return s.refreshAdobeToken(ctx, id)
	}, true, strings.TrimSpace(in.AccountID) == "")
	return data, videoURL, err
}

func (s *V1Service) generateRunwayVideo(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1VideoRequest, aspectRatio string, durationSeconds int, downloadResult bool) ([]byte, string, error) {
	if s.runway == nil {
		return nil, "", errors.New("runway client not configured")
	}
	// Runway i2v strictly requires exactly one first-frame image.
	refs, err := decodeReferenceImages(in.ReferenceImages, 1)
	if err != nil {
		return nil, "", err
	}
	if len(refs) != 1 {
		return nil, "", errors.New("runway 图生视频需要且仅需 1 张首帧图")
	}
	frame := refs[0]

	items, err := s.tokens.ListByPool(ctx, "runway")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		if item.Status != "active" || item.Dead || strings.TrimSpace(item.Value) == "" {
			continue
		}
		// No pre-deduct (same policy as the image flow): skip only accounts we KNOW
		// are out of credits (cached remaining <= 0) — those are treated as dead.
		// Unknown balance gets the benefit of the doubt.
		if rem, ok := jsonMapInt(item.Meta, "cached_quota_remaining"); ok && rem <= 0 {
			continue
		}
		active = append(active, item)
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("runway", active)

	var videoURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "runway", active, "video", func(token model.TokenAccount) ([]byte, error) {
		teamID := ""
		if token.Meta != nil {
			teamID = strings.TrimSpace(stringValue(token.Meta["team_id"]))
		}
		blob, meta, genErr := s.runway.GenerateVideo(ctx, token.Value, teamID, in.Prompt, aspectRatio, durationSeconds, frame, false)
		if genErr == nil {
			videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
			if downloadResult {
				blob, _, genErr = downloadPublicArtifact(ctx, videoURL, staticAssetHosts("runway"), "", netguard.MediaVideo, netguard.MaxVideoBytes)
				if genErr != nil {
					genErr = fmt.Errorf("%w: invalid video artifact", runway.ErrTemporaryUpstream)
				}
			}
		}
		return blob, genErr
	}, runwayErrClass, nil, true)
	return data, videoURL, err
}

func runwayErrClass(err error) (isAuth, isQuota, isTemporary, isDead bool) {
	return errors.Is(err, runway.ErrAuth), errors.Is(err, runway.ErrQuotaExhausted), errors.Is(err, runway.ErrTemporaryUpstream), false
}

// customAccountServes reports whether a custom (upstream) account is usable for a
// given model id: active, not dead, has a base_url, and its meta.models list (csv
// of model ids it serves) contains the id. An empty models list serves ALL ids.
func customAccountServes(item model.TokenAccount, modelID string) bool {
	if item.Status != "active" || item.Dead || strings.TrimSpace(item.Value) == "" {
		return false
	}
	if item.Meta == nil || strings.TrimSpace(stringValue(item.Meta["base_url"])) == "" {
		return false
	}
	list := strings.TrimSpace(stringValue(item.Meta["models"]))
	if list == "" {
		return true
	}
	for _, m := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(m), modelID) {
			return true
		}
	}
	return false
}

// customActive returns the custom accounts that serve modelID, ordered by weight
// (higher first; ties by id) so heavier upstreams are preferred.
func (s *V1Service) customActive(ctx context.Context, modelID string) ([]model.TokenAccount, error) {
	items, err := s.tokens.ListByPool(ctx, "custom")
	if err != nil {
		return nil, err
	}
	var active []model.TokenAccount
	for _, item := range items {
		if customAccountServes(item, modelID) {
			active = append(active, item)
		}
	}
	s.rotateRoundRobin("custom", active) // weight priority + round-robin within ties
	return active, nil
}

func poolAccountConcurrency(pool string, item model.TokenAccount) int {
	if item.Concurrency > 0 {
		return min(item.Concurrency, 20)
	}
	if pool == "grok" {
		return grokConcurrencyPerAccount
	}
	if pool == "adobe" {
		if total, ok := jsonMapInt(item.Meta, "cached_quota_total"); ok && total >= 10000 {
			return adobePointsConcurrencyPerAccount
		}
	}
	return 1
}

// generateCustomImage forwards an image generation to an OpenAI-compatible
// upstream. Custom upstreams use direct server egress. A multipart request
// carrying references is itself the generation submit.
func (s *V1Service) generateCustomImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	urlOnly := noStore
	if s.custom == nil {
		return nil, "", errors.New("custom client not configured")
	}
	s.applyGlobalProxy(ctx)
	refs, err := decodeReferenceImages(in.ReferenceImages, max(1, modelItem.MaxReferenceImages))
	if err != nil {
		return nil, "", err
	}
	active, err := s.customActive(ctx, modelItem.ID)
	if err != nil {
		return nil, "", err
	}
	active = pinTestAccount(active, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	size := upstreamSize(aspectRatio, resolution)
	quality := upstreamQualityForModel(modelItem.ID, in.Quality, resolution)
	var imageURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "custom", active, "image", func(token model.TokenAccount) ([]byte, error) {
		baseURL := stringValue(token.Meta["base_url"])
		d, assetURL, genErr := s.custom.GenerateImage(ctx, baseURL, token.Value, modelItem.ID, in.Prompt, size, quality, refs, false)
		if genErr != nil {
			return nil, genErr
		}
		imageURL = strings.TrimSpace(assetURL)
		if !urlOnly {
			parsedBase, parseErr := url.Parse(strings.TrimSpace(baseURL))
			if parseErr != nil || parsedBase.Hostname() == "" || !strings.EqualFold(parsedBase.Scheme, "https") {
				return nil, fmt.Errorf("%w: unsafe custom base URL", custom.ErrTemporaryUpstream)
			}
			d, _, genErr = downloadPublicArtifact(ctx, imageURL, []string{parsedBase.Hostname()}, token.Value, netguard.MediaImage, netguard.MaxImageBytes)
			if genErr != nil {
				return nil, fmt.Errorf("%w: invalid image artifact", custom.ErrTemporaryUpstream)
			}
		}
		return d, nil
	}, customErrClass, nil, true)
	return data, imageURL, err
}

// generateCustomVideo forwards a video generation to an OpenAI-compatible
// (Sora-style) upstream. Custom upstream traffic uses direct server egress.
// Billing uses the local model price.
func (s *V1Service) generateCustomVideo(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1VideoRequest, aspectRatio, resolution string, durationSeconds int, downloadResult bool) ([]byte, string, error) {
	if s.custom == nil {
		return nil, "", errors.New("custom client not configured")
	}
	s.applyGlobalProxy(ctx)
	active, err := s.customActive(ctx, modelItem.ID)
	if err != nil {
		return nil, "", err
	}
	active = pinTestAccount(active, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	size := upstreamVideoSize(aspectRatio, resolution)
	// Optional reference frames (image-to-video / first-last frames) — forwarded
	// to the upstream as multipart input_reference[] files.
	frames, err := decodeReferenceImages(in.ReferenceImages, max(1, modelItem.MaxReferenceImages))
	if err != nil {
		return nil, "", err
	}
	var videoURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "custom", active, "video", func(token model.TokenAccount) ([]byte, error) {
		baseURL := stringValue(token.Meta["base_url"])
		d, assetURL, genErr := s.custom.GenerateVideo(ctx, baseURL, token.Value, modelItem.ID, in.Prompt, size, durationSeconds, frames, false)
		if genErr != nil {
			return nil, genErr
		}
		videoURL = strings.TrimSpace(assetURL)
		if downloadResult {
			parsedBase, parseErr := url.Parse(strings.TrimSpace(baseURL))
			if parseErr != nil || parsedBase.Hostname() == "" || !strings.EqualFold(parsedBase.Scheme, "https") {
				return nil, fmt.Errorf("%w: unsafe custom base URL", custom.ErrTemporaryUpstream)
			}
			d, _, genErr = downloadPublicArtifact(ctx, videoURL, []string{parsedBase.Hostname()}, token.Value, netguard.MediaVideo, netguard.MaxVideoBytes)
			if genErr != nil {
				return nil, fmt.Errorf("%w: invalid video artifact", custom.ErrTemporaryUpstream)
			}
		}
		return d, nil
	}, customErrClass, nil, true)
	return data, videoURL, err
}

func customErrClass(err error) (isAuth, isQuota, isTemporary, isDead bool) {
	return errors.Is(err, custom.ErrAuth), errors.Is(err, custom.ErrQuotaExhausted), errors.Is(err, custom.ErrTemporaryUpstream), false
}

// upstreamSize maps our (ratio, resolution) to an OpenAI-style "WxH" size string
// for the upstream. The pixel base scales with the tier (1K/2K/4K); the ratio
// sets the shape. Upstreams that key off ratio (our own /v1) read it fine.
func upstreamSize(aspectRatio, resolution string) string {
	base := 1024
	switch strings.ToUpper(strings.TrimSpace(resolution)) {
	case "2K":
		base = 2048
	case "4K":
		base = 4096
	}
	w, h := 1, 1
	parts := strings.Split(strings.ReplaceAll(strings.TrimSpace(aspectRatio), "x", ":"), ":")
	if len(parts) == 2 {
		if a, e1 := strconv.Atoi(strings.TrimSpace(parts[0])); e1 == nil && a > 0 {
			if b, e2 := strconv.Atoi(strings.TrimSpace(parts[1])); e2 == nil && b > 0 {
				w, h = a, b
			}
		}
	}
	if w >= h {
		return fmt.Sprintf("%dx%d", base, base*h/w)
	}
	return fmt.Sprintf("%dx%d", base*w/h, base)
}

// upstreamVideoSize maps our (ratio, resolution) to a "WxH" size for video
// upstreams. Video "Np" tiers set the SHORT edge in pixels (like grok:
// 720p 1:1 → 720x720, 720p 16:9 → 1280x720); 2K/4K fall back to the
// long-edge mapping shared with images.
func upstreamVideoSize(aspectRatio, resolution string) string {
	short := 0
	switch res := strings.ToLower(strings.TrimSpace(resolution)); res {
	case "540p":
		short = 540
	case "720p", "":
		short = 720
	case "1080p":
		short = 1080
	}
	if short == 0 {
		return upstreamSize(aspectRatio, resolution)
	}
	w, h := 1, 1
	parts := strings.Split(strings.ReplaceAll(strings.TrimSpace(aspectRatio), "x", ":"), ":")
	if len(parts) == 2 {
		if a, e1 := strconv.Atoi(strings.TrimSpace(parts[0])); e1 == nil && a > 0 {
			if b, e2 := strconv.Atoi(strings.TrimSpace(parts[1])); e2 == nil && b > 0 {
				w, h = a, b
			}
		}
	}
	if w >= h {
		return fmt.Sprintf("%dx%d", short*w/h, short)
	}
	return fmt.Sprintf("%dx%d", short, short*h/w)
}

func upstreamImageQuality(quality, _ string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(quality)); normalized {
	case "low", "medium", "high":
		return normalized
	default:
		return "low"
	}
}

func upstreamQualityForModel(modelID, quality, resolution string) string {
	if !supportsQualityResolutionModel(modelID) {
		return ""
	}
	return upstreamImageQuality(quality, resolution)
}

// generateOreateVideo runs OreateAI's Seedance video flow. Oreate's
// website requires a fresh browser-generated Banti token for every attempt, so
// risk-control failures are temporary and participate in the bounded retry loop.
func (s *V1Service) generateOreateVideo(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1VideoRequest, aspectRatio, resolution string, durationSeconds int, downloadResult bool) ([]byte, string, error) {
	if s.oreate == nil {
		return nil, "", errors.New("oreate client not configured")
	}
	if len(in.ReferenceAudios) > 0 {
		return nil, "", fmt.Errorf("%w: OreateAI Seedance does not currently expose reference-audio slots", ErrUnsupportedParams)
	}
	decodedImages, err := decodeReferenceImages(in.ReferenceImages, max(1, modelItem.MaxReferenceImages))
	if err != nil {
		return nil, "", err
	}
	imageRefs := make([]oreate.MediaReference, 0, len(decodedImages))
	for i, data := range decodedImages {
		contentType := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
		switch contentType {
		case "image/jpeg", "image/png", "image/webp":
		default:
			return nil, "", fmt.Errorf("%w: OreateAI reference images must be JPEG, PNG, or WebP", ErrUnsupportedParams)
		}
		imageRefs = append(imageRefs, oreate.MediaReference{Data: data, ContentType: contentType, Filename: fmt.Sprintf("reference-image-%d", i+1)})
	}
	videoRefs := make([]oreate.MediaReference, 0, len(in.ReferenceVideos))
	totalReferenceDuration := 0.0
	for _, ref := range in.ReferenceVideos {
		seconds, parseErr := oreate.MP4DurationSeconds(ref.Data)
		if parseErr != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrUnsupportedParams, parseErr)
		}
		totalReferenceDuration += seconds
		videoRefs = append(videoRefs, oreate.MediaReference{
			Data: ref.Data, ContentType: ref.ContentType, Filename: ref.Filename, DurationSec: seconds,
		})
	}
	upstreamModel := strings.TrimSpace(modelItem.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(modelItem.ID)
	}
	referenceDuration := 0
	if len(videoRefs) > 0 {
		referenceDuration = int(math.Ceil(totalReferenceDuration))
	}
	requiredCredits, err := oreate.SeedanceRequiredCredits(upstreamModel, resolution, durationSeconds, in.GenerateAudio, referenceDuration)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnsupportedParams, err)
	}
	ctx = withDispatchCost(ctx, float64(requiredCredits))
	s.applyGlobalProxy(ctx)
	items, err := s.tokens.ListByPool(ctx, "oreate")
	if err != nil {
		return nil, "", err
	}
	active := make([]model.TokenAccount, 0, len(items))
	for _, item := range items {
		if item.Status != "active" || item.Dead || item.VideoLimited || strings.TrimSpace(item.Value) == "" {
			continue
		}
		if remaining, ok := jsonMapInt(item.Meta, "cached_quota_remaining"); ok && remaining < oreateMinUsableCredits {
			continue
		}
		active = append(active, item)
	}
	active = pinTestAccount(items, active, in.AccountID)
	active = s.dropOreateSpamQuarantined(active)
	active, knownInsufficient := filterOreateAccountsByCredits(active, requiredCredits)
	if len(active) == 0 {
		if knownInsufficient {
			return nil, "", oreate.ErrQuotaExhausted
		}
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("oreate", active)
	s.prioritizeOreate80CreditAccounts(active, requiredCredits)
	var videoURL string
	data, err := s.runPoolWithFailoverPolicy(ctx, eventID, "oreate", active, "video", func(token model.TokenAccount) ([]byte, error) {
		blob, meta, genErr := s.oreate.GenerateVideo(ctx, oreateAccountFromToken(token), oreate.VideoOptions{
			ModelID: upstreamModel, Prompt: in.Prompt, Ratio: aspectRatio, Resolution: resolution,
			Duration: durationSeconds, Audio: in.GenerateAudio, DownloadResult: false,
			ReferenceImages: imageRefs, ReferenceVideos: videoRefs,
		})
		if genErr != nil {
			if errors.Is(genErr, oreate.ErrSpamUser) || errors.Is(genErr, oreate.ErrAccountChallenge) {
				s.quarantineOreateSpamAccount(token.ID)
				genErr = fmt.Errorf("%w (上游要求该账号完成视频风控验证，已隔离并换号重试)", genErr)
			}
			return nil, genErr
		}
		s.oreateSpam.Delete(token.ID)
		videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
		if downloadResult {
			blob, _, genErr = downloadPublicArtifact(ctx, videoURL, staticAssetHosts("oreate"), "", netguard.MediaVideo, netguard.MaxVideoBytes)
			if genErr != nil {
				return nil, fmt.Errorf("%w: invalid video artifact", oreate.ErrTemporaryUpstream)
			}
		}
		return blob, nil
	}, oreateErrClass, nil, false, false)
	return data, videoURL, err
}

// A spam-user verdict is scoped to the account and to video: the same cookie on
// the same exit route still generates images through Oreate's web UI, every
// video model is refused, and other accounts kept producing video inside the
// same window. So it stays failover-eligible — rotate onto the next account
// instead of failing the request — while quarantineOreateSpamAccount parks the
// flagged one and dropOreateSpamQuarantined hides it from later candidate sets
// until the window expires.
func (s *V1Service) quarantineOreateSpamAccount(accountID string) {
	if accountID == "" {
		return
	}
	s.oreateSpam.Store(accountID, time.Now().Add(oreateSpamQuarantine))
}

func (s *V1Service) oreateSpamQuarantined(accountID string) bool {
	value, ok := s.oreateSpam.Load(accountID)
	if !ok {
		return false
	}
	until, _ := value.(time.Time)
	if time.Now().Before(until) {
		return true
	}
	s.oreateSpam.Delete(accountID)
	return false
}

// dropOreateSpamQuarantined hides quarantined accounts, but keeps the full set
// when every candidate is quarantined — an upstream-wide block must still let
// one request through to re-probe, which is what clears the quarantine once
// Oreate accepts the account again.
func (s *V1Service) dropOreateSpamQuarantined(items []model.TokenAccount) []model.TokenAccount {
	kept := make([]model.TokenAccount, 0, len(items))
	for _, item := range items {
		if !s.oreateSpamQuarantined(item.ID) {
			kept = append(kept, item)
		}
	}
	if len(kept) == 0 {
		return items
	}
	return kept
}

func isOreateTemporaryFailover(err error) bool {
	return errors.Is(err, oreate.ErrTemporaryUpstream) || errors.Is(err, oreate.ErrRiskControl)
}

// generateDolaVideo runs Dola's chat-transport Seedance video flow. Dola
// exposes daily free video allowance per account with no balance endpoint, so
// quota is observed through refusal copy and failover moves to the next
// cookie immediately.
func (s *V1Service) generateDolaVideo(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1VideoRequest, aspectRatio string, durationSeconds int, downloadResult bool) ([]byte, string, error) {
	if s.dola == nil {
		return nil, "", errors.New("dola client not configured")
	}
	if len(in.ReferenceVideos) > 0 || len(in.ReferenceAudios) > 0 {
		return nil, "", fmt.Errorf("%w: Dola video does not accept reference videos or audio", ErrUnsupportedParams)
	}
	decodedImages, err := decodeReferenceImages(in.ReferenceImages, max(1, modelItem.MaxReferenceImages))
	if err != nil {
		return nil, "", err
	}
	imageRefs := make([]dola.MediaReference, 0, len(decodedImages))
	for i, data := range decodedImages {
		contentType := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
		switch contentType {
		case "image/jpeg", "image/png", "image/webp":
		default:
			return nil, "", fmt.Errorf("%w: Dola reference images must be JPEG, PNG, or WebP", ErrUnsupportedParams)
		}
		imageRefs = append(imageRefs, dola.MediaReference{Data: data, ContentType: contentType, Filename: fmt.Sprintf("reference-image-%d", i+1)})
	}
	upstreamModel := strings.TrimSpace(modelItem.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = dola.DefaultVideoModel
	}
	requiredCredits, err := dola.RequiredCredits(durationSeconds)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnsupportedParams, err)
	}
	ctx = withDispatchCost(ctx, float64(requiredCredits))
	if plan, ok := dispatchPlanFromContext(ctx); !ok || !plan.QuotaTracked {
		return nil, "", errors.New("dola: metered dispatch route required")
	}
	s.applyGlobalProxy(ctx)
	items, err := s.tokens.ListByPool(ctx, "dola")
	if err != nil {
		return nil, "", err
	}
	active := make([]model.TokenAccount, 0, len(items))
	for _, item := range items {
		if !model.DolaAccountReady(item) || item.Status != "active" || item.Dead || item.VideoLimited || strings.TrimSpace(item.Value) == "" {
			continue
		}
		active = append(active, item)
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("dola", active)
	var videoURL string
	data, err := s.runPoolWithFailoverPolicy(ctx, eventID, "dola", active, "video", func(token model.TokenAccount) ([]byte, error) {
		dolaAccount := dolaAccountFromToken(token)
		proxyURL, proxyErr := s.dola.AccountProxy(ctx, dolaAccount)
		if proxyErr != nil {
			return nil, fmt.Errorf("%w: %v", dola.ErrTemporaryUpstream, proxyErr)
		}
		blob, meta, genErr := s.dola.GenerateVideo(ctx, dolaAccount, dola.VideoOptions{
			ModelID:         upstreamModel,
			Prompt:          in.Prompt,
			Ratio:           aspectRatio,
			Duration:        durationSeconds,
			ReferenceImages: imageRefs,
		})
		if genErr != nil {
			if !errors.Is(genErr, dola.ErrTaskAccepted) && !errors.Is(genErr, dola.ErrTaskSubmissionUnknown) &&
				(errors.Is(genErr, dola.ErrAuth) || errors.Is(genErr, dola.ErrChallenge) || errors.Is(genErr, dola.ErrVideoNotReady)) {
				state, detail := dolaReadinessVerdict(genErr)
				bookkeeping, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				recordBookkeepingError("invalidate Dola readiness", s.tokens.InvalidateDolaReadiness(bookkeeping, token, state, detail))
				cancel()
			}
			return nil, genErr
		}
		videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
		if downloadResult {
			// Dola artifacts land on rotating ByteDance VOD hosts, so the host
			// policy stays open to any public HTTPS endpoint and netguard's
			// IP/scheme hardening carries the SSRF defence.
			blob, _, genErr = downloadPublicArtifactWithProxy(ctx, videoURL, nil, "", netguard.MediaVideo, netguard.MaxVideoBytes, proxyURL)
			if genErr != nil {
				return nil, dola.AcceptedFailure(stringValue(meta["conversation_id"]), errGeneratedArtifactUnavailable)
			}
		}
		return blob, nil
	}, dolaErrClass, nil, false, false)
	return data, videoURL, err
}

func dolaAccountFromToken(token model.TokenAccount) dola.Account {
	account := dola.Account{ID: token.ID, Cookie: strings.TrimSpace(token.Value)}
	if token.Meta != nil {
		account.UserAgent = strings.TrimSpace(stringValue(token.Meta["user_agent"]))
	}
	return account
}

func dolaErrClass(err error) (isAuth, isQuota, isTemporary, isDead bool) {
	if errors.Is(err, dola.ErrChallenge) || errors.Is(err, dola.ErrTaskAccepted) || errors.Is(err, dola.ErrTaskSubmissionUnknown) {
		// A slide challenge needs interactive verification on this exact session.
		// Do not rotate through other accounts or resubmit the generation.
		return false, false, false, false
	}
	return errors.Is(err, dola.ErrAuth),
		errors.Is(err, dola.ErrQuotaExhausted),
		errors.Is(err, dola.ErrTemporaryUpstream),
		false
}

// isDead is never reported: a spam-user verdict is a reversible upstream state,
// not a broken credential — the same cookie serves again once the account or its
// route stops being flagged. Killing the account instead wiped ~100 usable
// accounts during a single blocked window.
func oreateErrClass(err error) (isAuth, isQuota, isTemporary, isDead bool) {
	return errors.Is(err, oreate.ErrAuth),
		errors.Is(err, oreate.ErrQuotaExhausted),
		isOreateTemporaryFailover(err),
		false
}

// filterOreateAccountsByCredits keeps unknown balances eligible for backwards
// compatibility, but never schedules an account below either the operating
// floor or this request's exact cost. Insufficiency above the floor remains
// task-specific: the same account may still serve a cheaper Seedance request.
func filterOreateAccountsByCredits(items []model.TokenAccount, required int) ([]model.TokenAccount, bool) {
	eligible := make([]model.TokenAccount, 0, len(items))
	knownInsufficient := false
	minimum := max(required, oreateMinUsableCredits)
	for _, item := range items {
		if remaining, ok := jsonMapInt(item.Meta, "cached_quota_remaining"); ok && remaining < minimum {
			knownInsufficient = true
			continue
		}
		eligible = append(eligible, item)
	}
	return eligible, knownInsufficient
}

// prioritizeOreate80CreditAccounts drains the 80-point tier with requests it
// can afford, preserving higher balances for expensive Seedance combinations.
// Cooling remains the primary ordering rule; within each cooling group this is
// a stable partition, so weight and round-robin order are preserved.
func (s *V1Service) prioritizeOreate80CreditAccounts(items []model.TokenAccount, required int) {
	if required > 80 || len(items) < 2 {
		return
	}
	cooling := make(map[string]bool, len(items))
	for _, item := range items {
		cooling[item.ID] = s.accountCooling("oreate", item.ID)
	}
	isPreferred := func(item model.TokenAccount) bool {
		remaining, ok := jsonMapInt(item.Meta, "cached_quota_remaining")
		return ok && remaining == 80
	}
	sort.SliceStable(items, func(i, j int) bool {
		if cooling[items[i].ID] != cooling[items[j].ID] {
			return !cooling[items[i].ID]
		}
		return isPreferred(items[i]) && !isPreferred(items[j])
	})
}

// generateGrokVideo runs grok's imagine video pipeline across the grok pool.
// Mirrors the runway policy: no pre-deduct, skip accounts known out of credits
// (cached remaining <= 0), and treat an out-of-credits / auth failure as a dead
// account (the grok sso can't be renewed — 失效就失效). Reference images are
// decoded and uploaded by the provider client, so both text-to-video and
// image-to-video requests preserve the caller's media inputs.
func (s *V1Service) generateGrokVideo(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1VideoRequest, aspectRatio, resolution string, durationSeconds int, downloadResult bool) ([]byte, string, error) {
	if s.grok == nil {
		return nil, "", errors.New("grok client not configured")
	}

	// Optional reference frames (image-to-video), up to the model's max.
	frames, err := decodeReferenceImages(in.ReferenceImages, max(1, modelItem.MaxReferenceImages))
	if err != nil {
		return nil, "", err
	}

	items, err := s.tokens.ListByPool(ctx, "grok")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		if item.Status != "active" || item.Dead || strings.TrimSpace(item.Value) == "" {
			continue
		}
		// Keep the explicit per-account flag as a fail-safe for an upstream quota
		// response, but do not derive it from the subscription tier.
		if item.VideoLimited {
			continue
		}
		if rem, ok := jsonMapInt(item.Meta, "cached_quota_remaining"); ok && rem <= 0 {
			continue
		}
		active = append(active, item)
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("grok", active)

	res := strings.TrimSpace(resolution)
	if res == "" {
		res = "720p"
	}
	var videoURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "grok", active, "video", func(token model.TokenAccount) ([]byte, error) {
		blob, meta, genErr := s.grok.GenerateVideo(ctx, token.Value, in.Prompt, aspectRatio, res, durationSeconds, frames, false)
		if genErr == nil {
			videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
			if downloadResult {
				body, contentType, openErr := s.grok.OpenAsset(ctx, token.Value, videoURL)
				if openErr != nil {
					return nil, openErr
				}
				blob, genErr = readProviderArtifact(body, contentType, netguard.MediaVideo, netguard.MaxVideoBytes)
				if genErr != nil {
					genErr = fmt.Errorf("%w: invalid video artifact", grok.ErrTemporaryUpstream)
				}
			}
		}
		return blob, genErr
	}, grokErrClass, nil, true)
	return data, videoURL, err
}

func grokErrClass(err error) (isAuth, isQuota, isTemporary, isDead bool) {
	return errors.Is(err, grok.ErrAuth), errors.Is(err, grok.ErrQuotaExhausted), errors.Is(err, grok.ErrTemporaryUpstream), false
}

// generateGrokImage runs grok's Lite (fast mode) text-to-image pipeline across
// the grok pool. Same policy as generateGrokVideo: no pre-deduct, skip accounts
// whose cached credits are gone, and treat an out-of-credits / auth failure as a
// dead account (the grok sso can't be renewed). Lite is text-to-image only.
// noStore url-only mode: skip the download and return the grok asset URL, which
// is auth-gated and therefore served through the /content proxy.
func (s *V1Service) generateGrokImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio string, noStore bool) ([]byte, string, error) {
	urlOnly := noStore
	if s.grok == nil {
		return nil, "", errors.New("grok client not configured")
	}

	items, err := s.tokens.ListByPool(ctx, "grok")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		if item.Status != "active" || item.Dead || item.ImageLimited || strings.TrimSpace(item.Value) == "" {
			continue
		}
		if rem, ok := jsonMapInt(item.Meta, "cached_quota_remaining"); ok && rem <= 0 {
			continue
		}
		active = append(active, item)
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("grok", active)

	var imageURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "grok", active, "image", func(token model.TokenAccount) ([]byte, error) {
		blob, meta, genErr := s.grok.GenerateImage(ctx, token.Value, in.Prompt, aspectRatio, false)
		if genErr == nil {
			imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
			if !urlOnly {
				body, contentType, openErr := s.grok.OpenAsset(ctx, token.Value, imageURL)
				if openErr != nil {
					return nil, openErr
				}
				blob, genErr = readProviderArtifact(body, contentType, netguard.MediaImage, netguard.MaxImageBytes)
				if genErr != nil {
					genErr = fmt.Errorf("%w: invalid image artifact", grok.ErrTemporaryUpstream)
				}
			}
		}
		return blob, genErr
	}, grokErrClass, nil, true)
	return data, imageURL, err
}

// generateRunwayImage runs the Runway gemini image pipeline (Nano Banana Pro or
// Nano Banana 2, selected by the model id) across the runway pool. Unlike the
// video path it does NOT pre-deduct credits: it simply round-robins the pool and
// generates. Per ops decision an out-of-credits account is treated like a dead
// 401 — marked dead (status=disabled) and skipped — because Runway credits don't
// refill daily, so a "quota" mark (which the maintenance loop would revive) is
// wrong. Reference images (up to the model's max) are uploaded per attempt.
// noStore url-only mode (API-key requests without DeAI): skip the artifact
// download and return the upstream image URL directly, no bytes.
func (s *V1Service) generateRunwayImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	// API-key (noStore) requests don't support DeAI (only the web drawing board
	// does), so url-only mode == noStore — skip the download, return the URL.
	urlOnly := noStore
	if s.runway == nil {
		return nil, "", errors.New("runway client not configured")
	}
	refs, err := decodeReferenceImages(in.ReferenceImages, max(1, modelItem.MaxReferenceImages))
	if err != nil {
		return nil, "", err
	}

	items, err := s.tokens.ListByPool(ctx, "runway")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		if item.Status != "active" || item.Dead || strings.TrimSpace(item.Value) == "" {
			continue
		}
		// No pre-deduct: skip only accounts we KNOW are out of credits
		// (cached remaining <= 0); they're treated as dead. Unknown balance gets
		// the benefit of the doubt — upstream rejects if it's truly empty.
		if rem, ok := jsonMapInt(item.Meta, "cached_quota_remaining"); ok && rem <= 0 {
			continue
		}
		active = append(active, item)
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("runway", active)

	imageSize := strings.TrimSpace(resolution)
	if imageSize == "" {
		imageSize = "1K"
	}
	var artURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "runway", active, "image", func(token model.TokenAccount) ([]byte, error) {
		teamID := ""
		if token.Meta != nil {
			teamID = strings.TrimSpace(stringValue(token.Meta["team_id"]))
		}
		blob, meta, genErr := s.runway.GenerateImage(ctx, token.Value, teamID, modelItem.ID, in.Prompt, aspectRatio, imageSize, refs, false)
		if genErr == nil {
			artURL = strings.TrimSpace(stringValue(meta["image_url"]))
			if !urlOnly {
				blob, _, genErr = downloadPublicArtifact(ctx, artURL, staticAssetHosts("runway"), "", netguard.MediaImage, netguard.MaxImageBytes)
				if genErr != nil {
					genErr = fmt.Errorf("%w: invalid image artifact", runway.ErrTemporaryUpstream)
				}
			}
		}
		return blob, genErr
	}, runwayErrClass, nil, true)
	return data, artURL, err
}

// reconcileChatGPTQuota re-reads OpenAI's image_gen remaining right after a
// successful generation and writes it back (negative / unknown clamp to 0),
// flipping the account to 限额 when it hits 0 — so accounts limit one-by-one as
// they're used, not all at once on a later batch probe. Runs while the
// per-account concurrency gate is still held. Best-effort (never fails the render).
func (s *V1Service) reconcileChatGPTQuota(ctx context.Context, tokenID, accessToken string) {
	if s.chatgpt == nil {
		return
	}
	data, err := s.chatgpt.FetchImageQuota(ctx, accessToken)
	if err != nil || boolValueWithDefault(data["auth_failed"], false) {
		return
	}
	// An unknown read (403/429/timeout) right after a successful render must not
	// clobber the balance with a bogus 0 or 限额 an account that just worked.
	if boolValueWithDefault(data["unknown"], false) {
		return
	}
	rem, exhausted := chatgptRemaining(data)
	item, err := s.tokens.Get(ctx, "chatgpt", tokenID)
	if err != nil {
		return
	}
	metaPatch := map[string]any{
		"cached_quota_remaining": rem,
		"cached_quota_at":        int(time.Now().Unix()),
	}
	patch := map[string]any{}
	if reset := strings.TrimSpace(stringValue(data["reset_after"])); reset != "" {
		patch["cached_quota_reset_after"] = reset
	} else if strings.TrimSpace(item.CachedQuotaResetAfter) == "" {
		patch["cached_quota_reset_after"] = time.Unix((time.Now().Unix()/86400+1)*86400, 0).UTC().Format(time.RFC3339)
	}
	if exhausted && item.Status == "active" {
		patch["status"] = "quota"
	}
	updateErr := s.tokens.UpdateMergingMeta(ctx, "chatgpt", tokenID, metaPatch, patch)
	recordBookkeepingError("reconcile chatgpt quota metadata", updateErr)
}

// chatgpt image URLs are auth-gated (files.oaiusercontent.com — a plain GET
// 403s), so url-only mode returns the URL for the caller to proxy via
// OpenImageContent using the generating account's token.
func (s *V1Service) generateChatGPTImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	urlOnly := noStore
	if s.chatgpt == nil {
		return nil, "", errors.New("chatgpt client not configured")
	}

	items, err := s.tokens.ListByPool(ctx, "chatgpt")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		if item.Status == "active" && !item.Dead && strings.TrimSpace(item.Value) != "" {
			active = append(active, item)
		}
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("chatgpt", active)

	refLimit := modelItem.MaxReferenceImages
	if refLimit <= 0 {
		refLimit = 1
	}
	refs, err := decodeReferenceImages(in.ReferenceImages, refLimit)
	if err != nil {
		return nil, "", err
	}

	// Round-robin order; on a transient upstream error FAIL OVER to the next
	// account (tempFailover=true, capped at maxTempFailoverAccounts) — never mark the
	// account dead. Auth/quota fail over immediately (see runPoolWithFailover).
	var imageURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "chatgpt", active, "image", func(token model.TokenAccount) ([]byte, error) {
		d, meta, genErr := s.chatgpt.GenerateImage(ctx, token.Value, in.Prompt, modelItem.ID, aspectRatio, resolution, refs, false)
		if genErr == nil {
			imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
			if !urlOnly {
				body, contentType, openErr := s.chatgpt.OpenAsset(ctx, token.Value, imageURL)
				if openErr != nil {
					return nil, openErr
				}
				d, genErr = readProviderArtifact(body, contentType, netguard.MediaImage, netguard.MaxImageBytes)
				if genErr != nil {
					genErr = fmt.Errorf("%w: invalid image artifact", chatgpt.ErrTemporaryUpstream)
				}
			}
		}
		if genErr == nil {
			// Sync the real OpenAI quota BEFORE the concurrency gate releases, so the
			// freshly-decremented remaining (and 限额 flip at 0) gates the next pick.
			s.reconcileChatGPTQuota(ctx, token.ID, token.Value)
		}
		return d, genErr
	}, func(e error) (bool, bool, bool, bool) {
		return errors.Is(e, chatgpt.ErrAuth), errors.Is(e, chatgpt.ErrQuotaExhausted), errors.Is(e, chatgpt.ErrTemporaryUpstream), false
	}, nil, true) // chatgpt token IS the credential — no cookie to refresh; switch accounts on transient errors
	return data, imageURL, err
}

// generateBytePlusImage runs one of the five Lumina image models through the
// normal account-pool scheduler. The provider client owns the closed model
// mapping, request-specific defaults, ImageX reference uploads, polling and
// authenticated artifact download.
func (s *V1Service) generateBytePlusImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	if s.byteplus == nil {
		return nil, "", errors.New("byteplus client not configured")
	}
	refLimit := modelItem.MaxReferenceImages
	if refLimit <= 0 {
		refLimit = 14
	}
	refs, err := decodeReferenceImages(in.ReferenceImages, refLimit)
	if err != nil {
		return nil, "", err
	}

	modelKey := strings.TrimSpace(modelItem.ID)
	if strings.TrimSpace(modelItem.UpstreamModel) != "" {
		modelKey = strings.TrimSpace(modelItem.UpstreamModel)
	}
	request := byteplus.ImageRequest{
		Model:       modelKey,
		Prompt:      in.Prompt,
		Size:        resolution,
		AspectRatio: aspectRatio,
		Quality:     upstreamQualityForModel(modelItem.ID, in.Quality, resolution),
		References:  refs,
	}
	requiredCredits, err := byteplus.RequiredCredits(request)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnsupportedParams, err)
	}
	ctx = withDispatchCost(ctx, requiredCredits)
	items, err := s.tokens.ListByPool(ctx, "byteplus")
	if err != nil {
		return nil, "", err
	}
	active := make([]model.TokenAccount, 0, len(items))
	now := time.Now()
	for _, item := range items {
		if item.Status == "active" && !item.Dead && strings.TrimSpace(item.Value) != "" && bytePlusAccountSessionUsable(item, now) {
			active = append(active, item)
		}
	}
	active = pinTestAccount(items, active, in.AccountID)
	active, knownInsufficient := filterBytePlusAccountsByCredits(active, requiredCredits)
	if len(active) == 0 {
		if knownInsufficient {
			return nil, "", byteplus.ErrQuotaExhausted
		}
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("byteplus", active)
	s.prioritizeBytePlusAccounts(active)
	providerModel, providerModelOK := byteplus.LookupModel(request.Model)
	verifyBetaNoCharge := providerModelOK && providerModel.Key == byteplus.ModelGPTImage2

	var imageURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "byteplus", active, "image", func(token model.TokenAccount) ([]byte, error) {
		beforeBalance, beforeBalanceKnown := 0.0, false
		if verifyBetaNoCharge {
			beforeBalance, beforeBalanceKnown = s.probeBytePlusRemaining(ctx, token.Value)
		}
		request.DownloadResult = !noStore
		result, meta, genErr := s.byteplus.GenerateImage(ctx, token.Value, request)
		if genErr == nil {
			imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
			return result, nil
		}
		if errors.Is(genErr, byteplus.ErrRetryableTaskFailed) && beforeBalanceKnown &&
			s.bytePlusBetaFailureUncharged(ctx, token.Value, beforeBalance) {
			genErr = verifiedBytePlusRetry(genErr, beforeBalance)
		}

		// Accepted or submission-ambiguous requests remain non-replayable unless the
		// exact GPT Image 2 beta verdict also carries the private no-charge proof.
		if byteplusNoResubmit(genErr) {
			return nil, genErr
		}
		return result, genErr
	}, byteplusErrClass, nil, true)
	return data, imageURL, err
}

const bytePlusBillingConfirmationDelay = 2 * time.Second

func (s *V1Service) probeBytePlusRemaining(parent context.Context, cookie string) (float64, bool) {
	if s.byteplus == nil || strings.TrimSpace(cookie) == "" || parent.Err() != nil {
		return 0, false
	}
	probeCtx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	data, err := s.byteplus.FetchCreditsBalance(probeCtx, cookie)
	if err != nil || data == nil || boolValueWithDefault(data["unknown"], false) || boolValueWithDefault(data["auth_failed"], false) {
		return 0, false
	}
	remaining, ok := anyFloat(data["remaining"])
	return remaining, ok && remaining >= 0
}

// bytePlusBetaFailureUncharged requires two fresh post-failure snapshots. A
// delayed or unknown billing result fails closed and keeps ErrTaskAccepted's
// no-resubmit behavior.
func (s *V1Service) bytePlusBetaFailureUncharged(ctx context.Context, cookie string, before float64) bool {
	after, ok := s.probeBytePlusRemaining(ctx, cookie)
	if !ok || math.Abs(after-before) > 1e-9 {
		return false
	}
	timer := time.NewTimer(bytePlusBillingConfirmationDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	confirmed, ok := s.probeBytePlusRemaining(ctx, cookie)
	return ok && math.Abs(confirmed-before) <= 1e-9
}

// filterBytePlusAccountsByCredits keeps legacy accounts with an unknown balance
// eligible, but never submits a request to an account whose known available
// balance is below this model/size/reference combination's exact upstream cost.
func filterBytePlusAccountsByCredits(items []model.TokenAccount, required float64) ([]model.TokenAccount, bool) {
	eligible := make([]model.TokenAccount, 0, len(items))
	knownInsufficient := false
	for _, item := range items {
		if remaining, ok := jsonMapFloat(item.Meta, "cached_quota_remaining"); ok && remaining+0.000001 < required {
			knownInsufficient = true
			continue
		}
		eligible = append(eligible, item)
	}
	return eligible, knownInsufficient
}

// prioritizeBytePlusAccounts uses best-fit balance ordering within the existing
// cooling/weight groups. Cheap models drain smaller balances first, preserving
// high-credit accounts for GPT Image 2's 96/230-point requests; unknown balances
// remain a compatibility fallback behind known-sufficient accounts.
func (s *V1Service) prioritizeBytePlusAccounts(items []model.TokenAccount) {
	if len(items) < 2 {
		return
	}
	cooling := make(map[string]bool, len(items))
	for _, item := range items {
		cooling[item.ID] = s.accountCooling("byteplus", item.ID)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if cooling[items[i].ID] != cooling[items[j].ID] {
			return !cooling[items[i].ID]
		}
		if items[i].Weight != items[j].Weight {
			return items[i].Weight > items[j].Weight
		}
		left, leftKnown := jsonMapFloat(items[i].Meta, "cached_quota_remaining")
		right, rightKnown := jsonMapFloat(items[j].Meta, "cached_quota_remaining")
		if leftKnown != rightKnown {
			return leftKnown
		}
		return leftKnown && left < right
	})
}

func byteplusErrClass(err error) (isAuth, isQuota, isTemporary, isDead bool) {
	// The provider still exposes ErrTaskAccepted for audit and conservative
	// callers. This one exact terminal failure is the account-level exception.
	if bytePlusBetaRetryVerified(err) {
		return false, false, true, false
	}
	// An accepted task or an ambiguous create response may both already represent
	// a paid task. Neither may move to another account or replay create_task.
	if byteplusNoResubmit(err) {
		return false, false, false, false
	}
	return errors.Is(err, byteplus.ErrAuth), errors.Is(err, byteplus.ErrQuotaExhausted), errors.Is(err, byteplus.ErrTemporaryUpstream), false
}

func bytePlusBetaRetryVerified(err error) bool {
	return errors.Is(err, byteplus.ErrRetryableTaskFailed) && errors.Is(err, errBytePlusRetryVerified)
}

func byteplusNoResubmit(err error) bool {
	return errors.Is(err, byteplus.ErrTaskAccepted) || errors.Is(err, byteplus.ErrTaskSubmissionUnknown)
}

func (s *V1Service) refundIfNeeded(ctx context.Context, principal *APIPrincipal, eventID string, price float64) error {
	return nil
}

// ensureReferenceSizes rejects any reference image over the byte cap BEFORE
// charging, so an oversized image fails fast (no charge, no pending-log churn)
// across every entry path — session /generate, API-key /v1, and admin /test.
// decodeReferenceImages re-checks at decode time as a backstop; this mirrors its
// base64 length pre-check (decoded ≈ len(b64)*3/4).
func ensureReferenceSizes(inputs []string) error {
	for _, raw := range inputs {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if (len(v)*3)/4 > maxReferenceImageBytes {
			return ErrReferenceTooLarge
		}
	}
	return nil
}

func validateMediaReferences(inputs []MediaReference, kind string) error {
	for _, ref := range inputs {
		if len(ref.Data) == 0 {
			return fmt.Errorf("empty reference %s", kind)
		}
		contentType := strings.ToLower(strings.TrimSpace(strings.Split(ref.ContentType, ";")[0]))
		switch kind {
		case "video":
			if len(ref.Data) > maxReferenceVideoBytes {
				return ErrReferenceVideoTooLarge
			}
			if contentType != "video/mp4" && contentType != "video/quicktime" && contentType != "video/mov" {
				return fmt.Errorf("%w: reference video must be MP4 or MOV", ErrUnsupportedParams)
			}
		case "audio":
			if len(ref.Data) > maxReferenceAudioBytes {
				return ErrReferenceAudioTooLarge
			}
			switch contentType {
			case "audio/mpeg", "audio/wav", "audio/x-wav", "audio/mp4", "audio/aac", "audio/x-m4a":
			default:
				return fmt.Errorf("%w: unsupported reference audio format", ErrUnsupportedParams)
			}
		}
	}
	return nil
}

func decodeReferenceImages(inputs []string, limit int) ([][]byte, error) {
	if limit <= 0 {
		limit = 1
	}
	if len(inputs) > limit {
		return nil, errors.New("too many reference images")
	}
	out := make([][]byte, 0, len(inputs))
	for _, raw := range inputs {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		// JSON requests may use either raw base64 or a data URL. The data URL's
		// declared MIME is intentionally ignored; payload magic and a real decoder
		// are authoritative.
		if strings.HasPrefix(strings.ToLower(v), "data:") {
			comma := strings.IndexByte(v, ',')
			if comma <= 5 || !strings.Contains(strings.ToLower(v[:comma]), ";base64") {
				return nil, fmt.Errorf("%w: invalid reference image encoding", ErrUnsupportedParams)
			}
			v = strings.TrimSpace(v[comma+1:])
		}
		// decoded size ≈ len(b64) * 3 / 4 — reject oversized payloads up front,
		// before allocating the decoded buffer.
		if (len(v)*3)/4 > maxReferenceImageBytes {
			return nil, ErrReferenceTooLarge
		}
		data, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(v)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid reference image encoding", ErrUnsupportedParams)
			}
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%w: empty reference image", ErrUnsupportedParams)
		}
		if len(data) > maxReferenceImageBytes {
			return nil, ErrReferenceTooLarge
		}
		actualType := netguard.DetectMediaType(data)
		switch actualType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			// These formats are registered with image.Decode by the service package.
			// Normalize every provider input to PNG so Adobe/Runway/Custom and other
			// fixed image/png upload paths never lie about JPEG/GIF/WebP bytes.
		default:
			return nil, fmt.Errorf("%w: unsupported reference image format", ErrUnsupportedParams)
		}
		config, _, configErr := image.DecodeConfig(bytes.NewReader(data))
		if configErr != nil || config.Width <= 0 || config.Height <= 0 {
			return nil, fmt.Errorf("%w: reference image dimensions are invalid", ErrUnsupportedParams)
		}
		if int64(config.Width)*int64(config.Height) > maxReferencePixels {
			return nil, ErrReferenceTooLarge
		}
		decoded, _, decodeErr := image.Decode(bytes.NewReader(data))
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: reference image is not decodable", ErrUnsupportedParams)
		}
		var encoded bytes.Buffer
		if encodeErr := png.Encode(&encoded, decoded); encodeErr != nil {
			return nil, fmt.Errorf("%w: reference image normalization failed", ErrUnsupportedParams)
		}
		if encoded.Len() > maxReferenceImageBytes {
			return nil, ErrReferenceTooLarge
		}
		out = append(out, encoded.Bytes())
	}
	return out, nil
}

func parseImageSize(size, aspectRatio, resolution string) (string, string) {
	ar := strings.TrimSpace(strings.ReplaceAll(aspectRatio, "x", ":"))
	rs := strings.TrimSpace(resolution)
	if size != "" && strings.Contains(strings.ToLower(size), "x") {
		var w, h int
		_, _ = fmt.Sscanf(strings.ToLower(size), "%dx%d", &w, &h)
		if w > 0 && h > 0 {
			if ar == "" {
				ar = guessRatio(w, h)
			}
			if rs == "" {
				maxEdge := w
				if h > maxEdge {
					maxEdge = h
				}
				switch {
				case maxEdge >= 3500:
					rs = "4K"
				case maxEdge >= 1800:
					rs = "2K"
				default:
					rs = "1K"
				}
			}
		}
	}
	if ar == "" {
		ar = "1:1"
	}
	if rs == "" {
		rs = "2K"
	}
	return ar, rs
}

func resolveImageSize(item *model.ModelConfig, in V1ImageRequest) (string, string) {
	return parseImageSize(in.Size, in.AspectRatio, in.Resolution)
}

func supportsQualityResolutionModel(modelID string) bool {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "gpt-image-2", "firefly-gpt-image-2", "lumina-gpt-image-2",
		"chatgpt-gpt-image-2", "byteplus-gpt-image-2", "adobe-gpt-image-2":
		return true
	default:
		return false
	}
}

func guessRatio(w, h int) string {
	type candidate struct {
		W int
		H int
	}
	// The 17 ratios actually used across our models. Must stay in sync with the
	// custom-model picker (CustomModelModal RATIO_OPTS) and the docs 对照表, so a
	// /v1 `size` maps to exactly one of them. 9:21 is intentionally absent —
	// no image provider accepts it (Runway 400s on it). snapRatio then clamps
	// the guess to the target model's own supported list.
	candidates := []candidate{
		{1, 1},
		{5, 4}, {4, 3}, {3, 2}, {16, 9}, {2, 1}, {21, 9}, {3, 1}, {4, 1}, {8, 1}, // 横
		{4, 5}, {3, 4}, {2, 3}, {9, 16}, {1, 3}, {1, 4}, {1, 8}, // 竖
	}
	best := candidates[0]
	bestDelta := absFloat(float64(w)/float64(h) - float64(best.W)/float64(best.H))
	for _, item := range candidates[1:] {
		delta := absFloat(float64(w)/float64(h) - float64(item.W)/float64(item.H))
		if delta < bestDelta {
			best = item
			bestDelta = delta
		}
	}
	return fmt.Sprintf("%d:%d", best.W, best.H)
}

// deaiEnabled reports whether the 去AI特征 feature is switched on in system
// settings (default off). When off, an incoming deai flag is ignored entirely.
func (s *V1Service) deaiEnabled(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	raw, err := s.settings.GetValue(ctx, "deai.enabled")
	if err != nil {
		return false
	}
	return parseBoolSetting(raw, false)
}

func jsonMapFloat(m map[string]any, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	return anyFloat(v)
}

func anyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		// datatypes.JSONMap.Scan decodes with UseNumber(), so values loaded from
		// the DB arrive as json.Number — NOT float64. Without this case every
		// price read back from Postgres looked "unpriced".
		if f, err := x.Float64(); err == nil {
			return f, true
		}
	case string:
		var out float64
		if _, err := fmt.Sscanf(strings.TrimSpace(x), "%f", &out); err == nil {
			return out, true
		}
	}
	return 0, false
}

func canonicalQuotaNumber(value float64) any {
	if value == float64(int(value)) {
		return int(value)
	}
	return value
}

func sanitizeOwnerName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func parseDurationSeconds(raw string) int {
	raw = strings.ToLower(strings.TrimSpace(raw))
	raw = strings.TrimSuffix(raw, "s")
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		return 5
	}
	return n
}

func resolveAdobeVideoEngine(modelID string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "gemini-veo31", "firefly-veo31":
		// Use the fast tier — it's the only Veo 3.1 version this account is
		// entitled to (standard "3.1-generate" returns 403 user_not_entitled).
		// "firefly-veo31" is the legacy id, kept for back-compat with historical
		// rows/logs; the model is branded "gemini-veo31" now.
		return "veo31-fast", ""
	case "gemini-veo31-lite":
		return "veo31-lite", ""
	case "firefly-kling-3":
		return "kling-v3", ""
	case "firefly-kling-o3":
		return "kling-o3", ""
	case "firefly-runway-4.5":
		return "runway45", ""
	case "firefly-seedance-2":
		return "seedance20", ""
	case "firefly-seedance-2-fast":
		return "seedance20-fast", ""
	case "firefly-ray":
		return "luma", ""
	case "firefly-video":
		return "firefly-video", ""
	default:
		return "sora2", ""
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func principalCredits(principal *APIPrincipal) float64 {
	return 0
}

// markTokenFailure applies Python mark_bad semantics for a failed generation
// attempt against a pool token. It always bumps fail counters; the status side
// effects depend on the failure reason and the provider/pool.
//
//   - quota:  status="quota"; when no cached_quota_reset_after is present, set
//     quota_recover_at to next UTC midnight so the maintenance loop can revive it.
//   - auth on chatgpt: status="disabled" + dead=true (the access token IS the
//     credential; a 401 means it's dead).
//   - auth on adobe: NOT disabled/dead — the access token auto-refreshes from the
//     cookie, so rotate for this request and let the refresh loop mint a new one.
//   - other (non-auth/non-quota): NEITHER pool is auto-disabled — accounts stay
//     active/green and fails is tracked only for rotation ordering.
func (s *V1Service) markTokenFailure(ctx context.Context, pool string, token model.TokenAccount, kind string, isAuth, isQuota bool) {
	patch := map[string]any{
		"last_used_at": time.Now(),
		"fail_total":   gorm.Expr("fail_total + 1"),
		"fails":        gorm.Expr("fails + 1"),
	}
	switch {
	case isQuota:
		// Adobe quota is per-kind: a video-quota error must not block image
		// requests (and vice-versa). Flag only the failing kind, and only sink
		// the account into the shared "quota" waiting status once BOTH kinds are
		// limited. Other pools (chatgpt) are single-kind, so they go straight to
		// "quota" as before.
		if pool == "adobe" {
			imageLimited := token.ImageLimited
			videoLimited := token.VideoLimited
			if kind == "video" {
				videoLimited = true
				patch["video_limited"] = true
			} else {
				imageLimited = true
				patch["image_limited"] = true
			}
			if imageLimited && videoLimited {
				patch["status"] = "quota"
			}
		} else {
			patch["status"] = "quota"
		}
		if strings.TrimSpace(token.CachedQuotaResetAfter) == "" {
			recoverAt := time.Unix((time.Now().Unix()/86400+1)*86400, 0).UTC()
			patch["quota_recover_at"] = &recoverAt
		}
	case isAuth:
		// Adobe auth failures are NOT disabling: the access token refreshes from
		// the cookie. ChatGPT/BytePlus/Runway credentials cannot be refreshed by
		// this request path, so a definitive auth failure disables the account.
		// grok is intentionally excluded: a grok sso can momentarily 401 while
		// still valid (upstream blip / proxy / anti-bot), so an auth failure just
		// fails over for this request without permanently killing the account.
		if pool == "chatgpt" || pool == "byteplus" || pool == "runway" {
			patch["status"] = "disabled"
			patch["dead"] = true
		}
	default:
		// Neither pool is auto-disabled on generic (non-auth / non-quota) failures
		// — the account usually still works, so it stays active (green). fails is
		// only tracked for rotation ordering. (A chatgpt *auth* failure still marks
		// the token dead in the isAuth case above; that is a genuinely dead token.)
	}
	_, updateErr := s.tokens.Update(ctx, pool, token.ID, patch)
	recordBookkeepingError("record account failure", updateErr)
}

// markTokenUpstreamFailure records a failure caused by the provider itself
// (overload / 5xx / circuit open) WITHOUT touching the account's own health
// counters: during an upstream outage every account fails identically, so
// charging it to fails/fail_total makes a perfectly healthy pool look dead in
// the admin UI (and drowns real per-account problems in noise).
func (s *V1Service) markTokenUpstreamFailure(ctx context.Context, pool string, token model.TokenAccount) {
	_, updateErr := s.tokens.Update(ctx, pool, token.ID, map[string]any{
		"last_used_at":   time.Now(),
		"upstream_fails": gorm.Expr("upstream_fails + 1"),
	})
	recordBookkeepingError("record upstream failure", updateErr)
}

// markTokenDead disables an account and marks it dead on a fatal upstream error
// (a non-overload temporary Adobe failure that ops policy treats as account death).
func (s *V1Service) markTokenDead(ctx context.Context, pool string, token model.TokenAccount, kind string) {
	_, updateErr := s.tokens.Update(ctx, pool, token.ID, map[string]any{
		"last_used_at": time.Now(),
		"fail_total":   gorm.Expr("fail_total + 1"),
		"fails":        gorm.Expr("fails + 1"),
		"status":       "disabled",
		"dead":         true,
	})
	recordBookkeepingError("mark account dead", updateErr)
}

// nextCursor returns the pool's current round-robin position and advances it by
// one. Concurrent callers get different cursor values; changing candidate
// groups can still overlap, so the atomic account gate enforces capacity.
//
// The counter lives in Redis so the rotation survives restarts: with a
// per-process counter every deploy reset it to 0 and the scheduler kept
// re-picking the head of the account list, leaving most of a large pool idle.
// The in-memory fallback (Redis down/unset) starts at a random offset for the
// same reason.
func (s *V1Service) nextCursor(pool string) uint64 {
	if n, ok := s.conc.NextCursor(context.Background(), pool); ok {
		return n
	}
	v, _ := s.tokenCursors.LoadOrStore(pool, randomCursor())
	return atomic.AddUint64(v.(*uint64), 1) - 1
}

func randomCursor() *uint64 {
	start := rand.Uint64()
	return &start
}

// coolDownAccount demotes an account in its pool's rotation for a short window
// after an upstream failure (see acctCooldowns).
func (s *V1Service) coolDownAccount(pool, accountID string) {
	if accountID == "" {
		return
	}
	s.acctCooldowns.Store(pool+":"+accountID, time.Now().Add(accountFailureCooldown))
}

func (s *V1Service) coolDownAccountWithToken(pool string, token model.TokenAccount) {
	s.coolDownAccount(pool, token.ID)
	s.rotateAccountProxy(pool, token)
}

func (s *V1Service) rotateAccountProxy(pool string, token model.TokenAccount) {
	switch pool {
	case "chatgpt":
		if s.chatgpt != nil {
			s.chatgpt.RotateProxySession(token.Value)
		}
	case "grok":
		if s.grok != nil {
			s.grok.RotateProxySession(token.Value)
		}
	case "dola":
		if s.dola != nil {
			s.dola.RotateProxySession(token.ID)
		}
	case "adobe":
		if s.adobe != nil {
			s.adobe.RotateProxySession(token.Value)
		}
	case "byteplus":
		if s.byteplus != nil {
			s.byteplus.RotateProxySession(token.Value)
		}
	}
}

func (s *V1Service) accountCooling(pool, accountID string) bool {
	key := pool + ":" + accountID
	v, ok := s.acctCooldowns.Load(key)
	if !ok {
		return false
	}
	until, _ := v.(time.Time)
	if time.Now().Before(until) {
		return true
	}
	s.acctCooldowns.Delete(key)
	return false
}

// rotateRoundRobin orders the active accounts by a stable key (ID) and rotates
// the slice in place so iteration begins at the pool's current cursor position,
// then advances the cursor. This is strict round-robin: account selection
// cycles in fixed order regardless of fails or last_used, except that accounts
// inside their post-failure cooldown are moved to the back. The fall-through
// retry chain is preserved — on failure the caller's loop simply continues to
// the next account in rotation order.
// pinTestAccount narrows account selection to the single account requested by
// an admin 账号生图测试. The pinned account is taken from the pool's full list
// (bypassing active/dead/limited filters) so a limited or disabled account can
// still be probed. Returns nil when the account isn't in this pool.
func pinTestAccount(items, active []model.TokenAccount, accountID string) []model.TokenAccount {
	id := strings.TrimSpace(accountID)
	if id == "" {
		return active
	}
	for _, item := range items {
		if item.ID == id && strings.TrimSpace(item.Value) != "" {
			return []model.TokenAccount{item}
		}
	}
	return nil
}

// Ordinary 10-credit Adobe accounts can execute the retained partner image
// models (GPT Image 2 and Nano Banana 2). Video entitlement
// is model-specific: standard Veo 3.1, Luma Ray, and native Firefly Video work
// on ordinary accounts; Lite/Kling/Runway/Seedance return user_not_entitled.
// Admin-pinned tests bypass the normal active filters but still call this helper
// only while constructing the non-pinned scheduler pool.
func adobeAccountSupportsModel(item model.TokenAccount, modelID, kind string) bool {
	if strings.EqualFold(strings.TrimSpace(kind), "image") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "firefly-video", "gemini-veo31", "firefly-veo31", "firefly-ray":
		return true
	}
	total, ok := jsonMapInt(item.Meta, "cached_quota_total")
	return ok && total >= 10000
}

func adobePointsAccount(item model.TokenAccount) bool {
	total, ok := jsonMapInt(item.Meta, "cached_quota_total")
	return ok && total >= 10000
}

func (s *V1Service) rotateRoundRobin(pool string, items []model.TokenAccount) {
	if len(items) <= 1 {
		return
	}
	// Accounts that just failed upstream are demoted behind the healthy ones (but
	// never removed — they're still tried if everything else is busy/cooling).
	// Weight = priority: higher-weight accounts come first, so the scheduler tries
	// them before lower-weight ones (and only falls through when they're at their
	// concurrency cap). Within the SAME cooling state and weight all accounts are
	// equal, so they're rotated by the pool cursor for even distribution.
	cooling := make(map[string]bool, len(items))
	for _, item := range items {
		cooling[item.ID] = s.accountCooling(pool, item.ID)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if cooling[items[i].ID] != cooling[items[j].ID] {
			return !cooling[items[i].ID]
		}
		if items[i].Weight != items[j].Weight {
			return items[i].Weight > items[j].Weight
		}
		return items[i].ID < items[j].ID
	})
	start := s.nextCursor(pool)
	for i := 0; i < len(items); {
		j := i + 1
		for j < len(items) && items[j].Weight == items[i].Weight && cooling[items[j].ID] == cooling[items[i].ID] {
			j++
		}
		if g := j - i; g > 1 {
			off := int(start % uint64(g))
			if off != 0 {
				grp := items[i:j]
				rot := make([]model.TokenAccount, 0, g)
				rot = append(rot, grp[off:]...)
				rot = append(rot, grp[:off]...)
				copy(grp, rot)
			}
		}
		i = j
	}
}
