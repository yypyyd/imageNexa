package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	"backend/internal/provider/adobe"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/custom"
	"backend/internal/provider/grok"
	"backend/internal/provider/imagine"
	"backend/internal/provider/krea"
	"backend/internal/provider/leonardo"
	"backend/internal/provider/oreate"
	"backend/internal/provider/runway"
	"backend/internal/repo"
	"backend/internal/storage"

	"gorm.io/gorm"
)

var (
	ErrMissingAPIKey          = errors.New("missing api key")
	ErrInvalidAPIKey          = errors.New("invalid api key")
	ErrUnknownModel           = errors.New("unknown model")
	ErrUnsupportedParams      = errors.New("unsupported or unpriced parameters for this model")
	ErrBannedPrompt           = errors.New("prompt contains banned content")
	ErrInsufficientFunds      = errors.New("insufficient credits")
	ErrGenerationPending      = errors.New("generation executor not implemented yet")
	ErrProviderAuth           = errors.New("provider token invalid or expired")
	ErrNoProviderAccount      = errors.New("no provider account available, please ask an admin to configure one")
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
	ErrVideoJobNotFound  = errors.New("video job not found")
	ErrVideoNotReady     = errors.New("video is not ready yet")
	ErrImageTaskNotFound = errors.New("image task not found")
	// A provider account can be unable to fund one expensive request while still
	// remaining valid for cheaper work. It participates in quota failover but
	// must not be moved to the pool-wide quota state.
	errAccountTaskQuota = errors.New("provider account balance below current task cost")
)

// maxReferenceImageBytes bounds a single decoded reference image. 20 MB
// comfortably covers real photos/screenshots; anything larger is almost
// certainly abuse or a mistake. Mirrors Python core/refs.py.
const (
	maxReferenceImageBytes = 20 * 1024 * 1024
	maxReferenceVideoBytes = 200 * 1024 * 1024
	maxReferenceAudioBytes = 50 * 1024 * 1024
)

type V1Service struct {
	cfg      *config.Config
	models   *repo.ModelRepository
	users    *repo.UserRepository
	events   *repo.EventRepository
	tokens   *repo.TokenRepository
	settings *repo.SiteSettingRepository
	cgroups  *repo.ConcurrencyGroupRepository
	adobe    *adobe.Client
	chatgpt  *chatgpt.Client
	runway   *runway.Client
	leonardo *leonardo.Client
	krea     *krea.Client
	imagine  *imagine.Client
	grok     *grok.Client
	oreate   *oreate.Client
	custom   *custom.Client
	store    *storage.Client
	proxyMu  sync.RWMutex
	proxy    string
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
	// of fails/last_used. The atomic counter also serializes concurrent picks so
	// two simultaneous requests never start on the same account. It is only the
	// fallback for a missing/unreachable Redis — see nextCursor.
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

	// inflight maps an in-progress event ID → the cancel func of its generation
	// work context, so the maintenance sweep can stop a stuck generation the
	// moment it abandons the row (instead of letting an orphaned goroutine run on
	// for minutes and surface a late "success" on an already-abandoned event).
	inflight *InflightRegistry

	// conc is the Redis-backed concurrency limiter for BOTH the per-account
	// upstream gate (1+ jobs per account) and the per-user gate (画图台 + API key,
	// capped by the user's concurrency group). Self-healing + fail-open.
	conc *ConcurrencyService
}

// acctAcquire takes one per-account upstream slot (capped at max; 0/1 = single),
// tagged with the generation's eventID (unique per job; a generation only ever
// holds one slot on a given account at a time, so failover reuses it cleanly).
func (s *V1Service) acctAcquire(ctx context.Context, accountID, eventID string, max int) bool {
	if max < 1 {
		max = 1
	}
	return s.conc.Acquire(ctx, "conc:a:"+accountID, max, eventID)
}

func (s *V1Service) acctRelease(ctx context.Context, accountID, eventID string) {
	s.conc.Release(ctx, "conc:a:"+accountID, eventID)
}

// userAcquire takes one per-user generation slot, capped by the user's
// concurrency group (0 = unlimited). Returns false when the user is already at
// their limit. `token` is a unique per-generation tag passed back to userRelease.
func (s *V1Service) userAcquire(ctx context.Context, user *model.User, token string) bool {
	if user == nil {
		return true
	}
	return s.conc.Acquire(ctx, "conc:u:"+user.ID, s.userConcurrencyLimit(ctx, user), token)
}

func (s *V1Service) userRelease(ctx context.Context, userID, token string) {
	s.conc.Release(ctx, "conc:u:"+userID, token)
}

// userConcurrencyLimit resolves the user's concurrency-group cap (0 = unlimited),
// falling back to the default group when unset/missing.
func (s *V1Service) userConcurrencyLimit(ctx context.Context, user *model.User) int {
	if s.cgroups == nil || user == nil {
		return 0
	}
	var g *model.ConcurrencyGroup
	if user.ConcurrencyGroupID != "" {
		g, _ = s.cgroups.Get(ctx, user.ConcurrencyGroupID)
	}
	if g == nil {
		g, _ = s.cgroups.GetDefault(ctx)
	}
	if g == nil {
		return 0
	}
	return g.MaxConcurrency
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
	User      *model.User
	TokenType string
}

type V1ImageRequest struct {
	Model     string
	Prompt    string
	RequestID string
	Size      string
	// Quality is OpenAI's image quality (low|medium|high|auto). Only the GPT Image
	// 2 family uses it to select a resolution tier; other models keep their own
	// size/resolution behavior. An explicit internal Resolution always remains
	// authoritative.
	Quality string
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
	done     chan struct{}
	response map[string]any
	err      error
}

func newAsyncImageJob() *asyncImageJob {
	return &asyncImageJob{done: make(chan struct{})}
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

func NewV1Service(cfg *config.Config, models *repo.ModelRepository, users *repo.UserRepository, events *repo.EventRepository, tokens *repo.TokenRepository, settings *repo.SiteSettingRepository, cgroups *repo.ConcurrencyGroupRepository, conc *ConcurrencyService, adobeClient *adobe.Client, chatGPTClient *chatgpt.Client, runwayClient *runway.Client, leonardoClient *leonardo.Client, kreaClient *krea.Client, imagineClient *imagine.Client, grokClient *grok.Client, oreateClient *oreate.Client, customClient *custom.Client, store *storage.Client) *V1Service {
	service := &V1Service{
		cfg:      cfg,
		models:   models,
		users:    users,
		events:   events,
		tokens:   tokens,
		settings: settings,
		cgroups:  cgroups,
		conc:     conc,
		adobe:    adobeClient,
		chatgpt:  chatGPTClient,
		runway:   runwayClient,
		leonardo: leonardoClient,
		krea:     kreaClient,
		imagine:  imagineClient,
		grok:     grokClient,
		oreate:   oreateClient,
		custom:   customClient,
		store:    store,
		inflight: &InflightRegistry{},
	}
	if settings != nil {
		if proxy, err := settings.GetValue(context.Background(), "proxy.url"); err == nil {
			service.proxy = strings.TrimSpace(proxy)
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

// applyGlobalProxy snapshots the administrator's residential route onto only
// the providers with a verified protected-control-plane requirement. Other
// providers remain on direct local egress.
func (s *V1Service) applyGlobalProxy(ctx context.Context) string {
	proxy := ""
	if s.settings != nil {
		var err error
		proxy, err = s.settings.GetValue(ctx, "proxy.url")
		if err != nil {
			// A transient settings-store failure is not equivalent to an
			// administrator clearing the proxy. Keep the last known route.
			s.proxyMu.RLock()
			proxy = s.proxy
			s.proxyMu.RUnlock()
			return s.setProviderProxy(proxy)
		}
	}
	proxy = strings.TrimSpace(proxy)
	s.proxyMu.Lock()
	s.proxy = proxy
	s.proxyMu.Unlock()
	return s.setProviderProxy(proxy)
}

func (s *V1Service) setProviderProxy(proxy string) string {
	if s.chatgpt != nil {
		s.chatgpt.SetProxy(proxy)
	}
	if s.grok != nil {
		s.grok.SetProxy(proxy)
	}
	if s.oreate != nil {
		s.oreate.SetProxy(proxy)
	}
	return strings.TrimSpace(proxy)
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
		if principal != nil && principal.User != nil {
			userID = principal.User.ID
			userName = principal.User.Name
			if userName == "" {
				userName = principal.User.Email
			}
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
		event.Provider = m.Provider
	}
	if principal != nil && principal.User != nil {
		event.UserID = principal.User.ID
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
	token := ParseBearer(authHeader)
	if token == "" {
		return nil, ErrMissingAPIKey
	}

	// Only per-user API keys (hashed in the DB) authenticate to /v1. The old
	// global/shared API_KEY backdoor has been removed.
	user, err := s.users.GetByAPIKeyHash(ctx, HashAPIKey(token))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidAPIKey
		}
		return nil, err
	}
	if user.Status != "active" {
		return nil, ErrInvalidAPIKey
	}
	_ = s.users.TouchAPIKeyUsage(ctx, HashAPIKey(token))
	return &APIPrincipal{
		User:      user,
		TokenType: "user",
	}, nil
}

func (s *V1Service) ListModels(ctx context.Context, extended bool) ([]map[string]any, error) {
	items, err := s.models.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if !item.Enabled {
			continue
		}
		out = append(out, v1ModelEntry(item, now, extended))
	}
	return out, nil
}

func v1ModelEntry(item model.ModelConfig, created int64, extended bool) map[string]any {
	entry := map[string]any{
		"id":       item.EffectiveName(),
		"object":   "model",
		"created":  created,
		"owned_by": item.Provider,
	}
	if !extended {
		return entry
	}
	entry["kind"] = item.Type
	entry["type"] = item.Type
	ratios := repo.JSONRatios(item.Ratios)
	resolutions := repo.JSONStrings(item.Resolutions)
	// Different downstream model importers use either the explicit
	// `supported_*` names or the shorter catalog names. Return both aliases so
	// capability UIs do not silently drop ratios/resolutions.
	entry["supported_ratios"] = ratios
	entry["ratios"] = ratios
	// Some downstream catalog importers use the public settings schema's
	// camelCase names instead of the snake_case/OpenAI aliases above. Keep
	// both representations in the extended model response so capabilities are
	// not silently reduced during import (for example, Grok image ratios).
	entry["aspectRatios"] = ratios
	entry["supported_resolutions"] = resolutions
	entry["resolutions"] = resolutions
	entry["resolutionTiers"] = resolutions
	entry["modality"] = item.Type
	entry["name"] = item.EffectiveName()
	upstreamModel := strings.TrimSpace(item.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = item.ID
	}
	entry["upstreamModel"] = upstreamModel
	operations := []string{}
	switch item.Type {
	case "image":
		operations = append(operations, "generation")
		if item.ImageToImage {
			operations = append(operations, "edit")
		}
	case "video":
		operations = append(operations, "generation")
	case "audio":
		operations = append(operations, "speech")
	default:
		operations = append(operations, "completion")
	}
	entry["operations"] = operations
	// Reference-image capabilities are part of model discovery as well as
	// request validation. Clients use these fields to render the correct number
	// of upload slots and to distinguish ordered frames from unordered assets.
	entry["max_reference_images"] = max(0, item.MaxReferenceImages)
	entry["max_reference_videos"] = max(0, item.MaxReferenceVideos)
	entry["max_reference_audios"] = max(0, item.MaxReferenceAudios)
	entry["max_reference_media"] = max(0, item.MaxReferenceMedia)
	entry["supports_audio_output"] = item.SupportsAudioOutput
	entry["reference_mode"] = defaultString(strings.TrimSpace(item.ReferenceMode), "none")
	entry["maxReferenceImages"] = max(0, item.MaxReferenceImages)
	entry["maxReferenceVideos"] = max(0, item.MaxReferenceVideos)
	entry["maxReferenceAudios"] = max(0, item.MaxReferenceAudios)
	entry["maxReferenceMedia"] = max(0, item.MaxReferenceMedia)
	entry["supportsAudioOutput"] = item.SupportsAudioOutput
	entry["referenceMode"] = defaultString(strings.TrimSpace(item.ReferenceMode), "none")
	entry["durations"] = repo.JSONStrings(item.Durations)
	// Video models expose their selectable clip lengths (the /v1/videos
	// `seconds` param) so a key holder can discover them, e.g. ["5s","8s"].
	// Prefer the explicit durations list; fall back to the priced tiers.
	if item.Type == "video" {
		durations := repo.JSONStrings(item.Durations)
		if len(durations) == 0 {
			for d := range item.DurationPrices {
				durations = append(durations, d)
			}
			sort.Strings(durations)
		}
		entry["supported_durations"] = durations
		entry["durations"] = durations
		entry["supportedDurations"] = durations
		entry["durationTiers"] = durations
	}
	return entry
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

	modelItem, err := s.models.Get(ctx, modelName)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUnknownModel
		}
		return nil, err
	}
	if !modelItem.Enabled || modelItem.Type != "text" {
		return nil, ErrUnknownModel
	}
	if err := s.checkBannedPrompt(ctx, principal, prompt); err != nil {
		s.logRejectedEvent(context.WithoutCancel(ctx), "text", modelName, principal, prompt, source, err.Error())
		return nil, err
	}
	pool := "custom"
	active, err := s.customActive(ctx, modelItem.ID)
	if err != nil {
		return nil, err
	}
	if len(active) == 0 && modelItem.Provider == "chatgpt" {
		pool = "chatgpt"
		items, listErr := s.tokens.ListByPool(ctx, pool)
		if listErr != nil {
			return nil, listErr
		}
		for _, item := range items {
			// ChatGPT's quota status represents image_gen allowance and does not
			// prevent ordinary text conversations.
			if (item.Status == "active" || item.Status == "quota") && !item.Dead && strings.TrimSpace(item.Value) != "" {
				active = append(active, item)
			}
		}
		s.rotateRoundRobin(pool, active)
	}
	configuredGrokModel := strings.TrimSpace(modelItem.UpstreamModel)
	if configuredGrokModel == "" {
		configuredGrokModel = modelItem.ID
	}
	if len(active) == 0 && (modelItem.Provider == "grok" || grok.IsBuildTextModel(configuredGrokModel)) {
		pool = "grok"
		items, listErr := s.tokens.ListByPool(ctx, pool)
		if listErr != nil {
			return nil, listErr
		}
		for _, item := range items {
			// grok's quota status tracks media generation credits; text chat runs
			// on a separate rate limit, so quota-paused accounts still chat. The
			// image/video_limited flags are media-only too.
			if (item.Status == "active" || item.Status == "quota") && !item.Dead && strings.TrimSpace(item.Value) != "" {
				active = append(active, item)
			}
		}
		s.rotateRoundRobin(pool, active)
	}
	active = pinTestAccount(active, active, accountID)
	if len(active) == 0 || (pool == "custom" && s.custom == nil) || (pool == "chatgpt" && s.chatgpt == nil) || (pool == "grok" && s.grok == nil) {
		return nil, ErrNoProviderAccount
	}
	s.applyGlobalProxy(ctx)

	bookCtx := context.WithoutCancel(ctx)
	userSlot := randomUpper(12)
	if principal != nil && principal.User != nil && !s.userAcquire(bookCtx, principal.User, userSlot) {
		s.logRejectedEvent(bookCtx, "text", modelName, principal, prompt, source, ErrUserConcurrencyFull.Error())
		return nil, ErrUserConcurrencyFull
	}
	userHeld := principal != nil && principal.User != nil
	releaseUser := func() {
		if userHeld {
			s.userRelease(bookCtx, principal.User.ID, userSlot)
			userHeld = false
		}
	}

	price, err := s.chargeForModel(bookCtx, principal, modelItem, "text", "request", "", 0, true)
	if err != nil {
		releaseUser()
		s.logRejectedEvent(bookCtx, "text", modelName, principal, prompt, source, err.Error())
		return nil, err
	}
	eventID, err := s.logPendingEvent(bookCtx, "text", modelItem, principal, prompt, "", "", "", 0, price, "", source, nil, false, "")
	if err != nil {
		releaseUser()
		if principal != nil && principal.User != nil && price > 0 {
			if updated, adjustErr := s.users.AdjustCredits(bookCtx, principal.User.ID, price); adjustErr == nil {
				principal.User = updated
			}
		}
		return nil, err
	}
	startedAt := time.Now()
	upstreamModel := strings.TrimSpace(modelItem.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = modelItem.ID
	}

	var lastErr error
	busy := 0
	for _, token := range active {
		slots := accountConcurrency(token)
		if pool == "grok" {
			slots = grokConcurrencyPerAccount
		}
		if !s.acctAcquire(bookCtx, token.ID, eventID, slots) {
			busy++
			continue
		}
		_ = s.events.SetAccount(bookCtx, eventID, token.ID, token.AccountEmail)
		_ = s.tokens.TouchLastUsed(bookCtx, token.ID)
		var responseHeader http.Header
		var responseBody io.ReadCloser
		responseStream := stream
		var callErr error
		switch pool {
		case "custom":
			response, err := s.custom.ChatCompletions(ctx, stringValue(token.Meta["base_url"]), token.Value, upstreamModel, payload, stream)
			callErr = err
			if response != nil {
				responseHeader, responseBody, responseStream = response.Header, response.Body, response.Stream
			}
		case "grok":
			var text string
			var err error
			if grok.IsBuildTextModel(upstreamModel) {
				var accessToken string
				accessToken, err = s.ensureGrokBuildCredential(ctx, token)
				if err == nil {
					text, err = s.grok.GenerateBuildText(ctx, accessToken, prompt, upstreamModel)
				}
			} else {
				text, err = s.grok.GenerateText(ctx, token.Value, prompt, grok.ChatModeForModel(upstreamModel))
			}
			callErr = err
			if err == nil {
				responseHeader, responseBody = openAITextResponse(modelItem.EffectiveName(), text, stream)
			}
		default:
			text, err := s.chatgpt.GenerateText(ctx, token.Value, prompt, upstreamModel)
			callErr = err
			if err == nil {
				responseHeader, responseBody = openAITextResponse(modelItem.EffectiveName(), text, stream)
			}
		}
		if callErr != nil {
			s.acctRelease(bookCtx, token.ID, eventID)
			lastErr = callErr
			switch {
			case errors.Is(callErr, grok.ErrChallenge):
				// Statsig is process-wide and account-independent. The provider
				// already refreshed and retried once; rotating the pool would
				// repeat the same rejected signature against every account.
				return nil, s.failChatCompletion(bookCtx, principal, eventID, price, releaseUser, callErr)
			case errors.Is(callErr, custom.ErrAuth), errors.Is(callErr, chatgpt.ErrAuth), errors.Is(callErr, grok.ErrAuth):
				s.markTokenFailure(bookCtx, pool, token, "text", true, false)
				continue
			case errors.Is(callErr, grok.ErrQuotaExhausted):
				// grok's chat rate limit is per-mode and short-lived, and is
				// unrelated to the media credits the "quota" status guards —
				// don't pause the account, just fail over.
				s.markTokenFailure(bookCtx, pool, token, "text", false, false)
				continue
			case errors.Is(callErr, custom.ErrQuotaExhausted), errors.Is(callErr, chatgpt.ErrQuotaExhausted):
				s.markTokenFailure(bookCtx, pool, token, "text", false, true)
				continue
			case errors.Is(callErr, custom.ErrTemporaryUpstream), errors.Is(callErr, chatgpt.ErrTemporaryUpstream), errors.Is(callErr, grok.ErrTemporaryUpstream):
				s.markTokenFailure(bookCtx, pool, token, "text", false, false)
				continue
			default:
				return nil, s.failChatCompletion(bookCtx, principal, eventID, price, releaseUser, callErr)
			}
		}

		body := &chatAccountingBody{inner: responseBody, stream: responseStream}
		body.finish = func(success bool, reason string, upstreamFailure bool) {
			defer s.acctRelease(bookCtx, token.ID, eventID)
			defer releaseUser()
			elapsed := int(time.Since(startedAt).Milliseconds())
			if success {
				_, _ = s.tokens.Update(bookCtx, pool, token.ID, map[string]any{
					"last_used_at": time.Now(), "success_total": gorm.Expr("success_total + 1"), "fails": 0,
				})
				_ = s.events.UpdateStatus(bookCtx, eventID, "success", "", elapsed)
				_ = s.models.IncrementGenerationCount(bookCtx, modelItem.ID)
				if principal != nil && principal.User != nil {
					_ = s.users.IncrementGenerationCount(bookCtx, principal.User.ID)
				}
				_ = s.maybeGrantInviteReward(bookCtx, principal)
				return
			}
			if upstreamFailure {
				s.markTokenFailure(bookCtx, pool, token, "text", false, false)
			}
			_ = s.events.UpdateStatus(bookCtx, eventID, "failed", reason, elapsed)
			_ = s.refundIfNeeded(bookCtx, principal, eventID, price)
		}
		return &V1ChatResponse{Header: responseHeader, Body: body, Stream: responseStream}, nil
	}

	if lastErr == nil {
		if busy > 0 {
			lastErr = ErrConcurrencyFull
		} else {
			lastErr = ErrProviderExecution
		}
	}
	return nil, s.failChatCompletion(bookCtx, principal, eventID, price, releaseUser, lastErr)
}

func (s *V1Service) failChatCompletion(ctx context.Context, principal *APIPrincipal, eventID string, price float64, releaseUser func(), cause error) error {
	releaseUser()
	_ = s.events.UpdateStatus(ctx, eventID, "failed", cause.Error(), 0)
	_ = s.refundIfNeeded(ctx, principal, eventID, price)
	switch {
	case errors.Is(cause, custom.ErrBadRequest):
		return fmt.Errorf("%w: %v", ErrUnsupportedParams, cause)
	case errors.Is(cause, chatgpt.ErrContentPolicy):
		return fmt.Errorf("%w: %v", ErrUnsupportedParams, cause)
	case errors.Is(cause, custom.ErrAuth), errors.Is(cause, chatgpt.ErrAuth), errors.Is(cause, grok.ErrAuth):
		return fmt.Errorf("%w: %v", ErrProviderAuth, cause)
	case errors.Is(cause, custom.ErrQuotaExhausted), errors.Is(cause, chatgpt.ErrQuotaExhausted), errors.Is(cause, grok.ErrQuotaExhausted):
		return fmt.Errorf("%w: %v", ErrProviderQuota, cause)
	case errors.Is(cause, custom.ErrTemporaryUpstream), errors.Is(cause, chatgpt.ErrTemporaryUpstream), errors.Is(cause, grok.ErrTemporaryUpstream):
		return fmt.Errorf("%w: %v", ErrProviderTemporary, cause)
	case errors.Is(cause, ErrConcurrencyFull):
		return cause
	default:
		return fmt.Errorf("%w: %v", ErrProviderExecution, cause)
	}
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
	return s.prepareImageExecution(ctx, principal, in, "v1", true)
}

// StartImageRequest starts an opt-in asynchronous OpenAI image request. The
// caller receives a task object immediately and polls ImageTask. Requests with
// the same user + idempotency key share one execution, preventing retry storms
// from charging or rendering the same image more than once.
func (s *V1Service) StartImageRequest(ctx context.Context, principal *APIPrincipal, in V1ImageRequest) (map[string]any, bool, error) {
	in.RequestID = strings.TrimSpace(in.RequestID)
	if principal == nil || principal.User == nil {
		return nil, false, ErrInvalidAPIKey
	}
	if in.RequestID == "" {
		return nil, false, fmt.Errorf("%w: idempotency key is required for async image requests", ErrUnsupportedParams)
	}
	if len(in.RequestID) > 191 {
		return nil, false, fmt.Errorf("%w: idempotency key is too long", ErrUnsupportedParams)
	}

	if existing, err := s.imageTaskFromEvent(ctx, principal, in.RequestID); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrImageTaskNotFound) {
		return nil, false, err
	}

	key := imageJobKey(principal.User.ID, in.RequestID)
	candidate := newAsyncImageJob()
	actual, loaded := s.imageJobs.LoadOrStore(key, candidate)
	job := actual.(*asyncImageJob)
	if loaded {
		response, pending := asyncImageJobResponse(job, in.RequestID, s.imageTaskURL(in.RequestID, in.BaseURL))
		return response, pending, nil
	}

	lockKey := "idem:image:" + key
	lockToken := randomUpper(24)
	if !s.conc.Acquire(ctx, lockKey, 1, lockToken) {
		s.imageJobs.CompareAndDelete(key, candidate)
		if existing, err := s.imageTaskFromEvent(ctx, principal, in.RequestID); err == nil {
			return existing, false, nil
		} else if !errors.Is(err, ErrImageTaskNotFound) {
			return nil, false, err
		}
		return asyncImagePendingResponse(in.RequestID, s.imageTaskURL(in.RequestID, in.BaseURL)), true, nil
	}

	executionCtx := context.WithoutCancel(ctx)
	go func() {
		response, err := s.prepareImageExecution(executionCtx, principal, in, "v1", true)
		candidate.complete(response, err)
		s.conc.Release(context.Background(), lockKey, lockToken)
		time.AfterFunc(asyncImageJobRetention, func() {
			s.imageJobs.CompareAndDelete(key, candidate)
		})
	}()

	return asyncImagePendingResponse(in.RequestID, s.imageTaskURL(in.RequestID, in.BaseURL)), true, nil
}

func imageJobKey(userID, requestID string) string {
	return strings.TrimSpace(userID) + ":" + strings.TrimSpace(requestID)
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
			return map[string]any{
				"id":         requestID,
				"object":     "image.generation.task",
				"status":     "failed",
				"request_id": requestID,
				"poll_url":   pollURL,
				"error":      job.err.Error(),
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

func (s *V1Service) prepareSessionImage(ctx context.Context, principal *APIPrincipal, in V1ImageRequest) (map[string]any, error) {
	return s.prepareImageExecution(ctx, principal, in, "user", true)
}

func (s *V1Service) prepareAdminTestImage(ctx context.Context, principal *APIPrincipal, in V1ImageRequest) (map[string]any, error) {
	return s.prepareImageExecution(ctx, principal, in, "admin", false)
}

func (s *V1Service) prepareImageExecution(ctx context.Context, principal *APIPrincipal, in V1ImageRequest, source string, charge bool) (map[string]any, error) {
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

	// Per-user concurrency gate (画图台 + API key combined). Admin model-tests are
	// exempt. Held for the whole generation; released on return.
	if source != "admin" && principal != nil && principal.User != nil {
		slot := randomUpper(12)
		if !s.userAcquire(ctx, principal.User, slot) {
			s.logRejectedEvent(ctx, "image", in.Model, principal, in.Prompt, source, ErrUserConcurrencyFull.Error())
			return nil, ErrUserConcurrencyFull
		}
		defer s.userRelease(ctx, principal.User.ID, slot)
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
	eventID, err := s.logPendingEvent(ctx, "image", modelItem, principal, in.Prompt, aspectRatio, resolution, "", refCount, price, relativePath, source, nil, in.DeAI, in.RequestID)
	if err != nil {
		// Charging happens before event creation. If persistence fails (including a
		// distributed idempotency race stopped by the unique index), refund this
		// attempt directly because there is no event row to claim via Refunded.
		if price > 0 && principal != nil && principal.User != nil {
			if updated, refundErr := s.users.AdjustCredits(ctx, principal.User.ID, price); refundErr == nil {
				principal.User = updated
			}
		}
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

	var imageBytes []byte
	switch s.effectiveProvider(genCtx, modelItem) {
	case "adobe":
		b, u, execErr := s.generateAdobeImage(genCtx, eventID, modelItem, in, aspectRatio, resolution, urlOnly)
		if execErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
			switch {
			case errors.Is(execErr, adobe.ErrAuth):
				return nil, ErrProviderAuth
			case errors.Is(execErr, adobe.ErrQuotaExhausted):
				return nil, ErrProviderQuota
			case errors.Is(execErr, adobe.ErrTemporaryUpstream):
				return nil, ErrProviderTemporary
			case errors.Is(execErr, adobe.ErrContentRejected):
				return nil, execErr
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
			}
		}
		imageBytes = b
		upstreamURL = u
	case "chatgpt":
		b, u, execErr := s.generateChatGPTImage(genCtx, eventID, modelItem, in, aspectRatio, resolution, urlOnly)
		if execErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
			switch {
			case errors.Is(execErr, chatgpt.ErrAuth):
				return nil, ErrProviderAuth
			case errors.Is(execErr, chatgpt.ErrQuotaExhausted):
				return nil, ErrProviderQuota
			case errors.Is(execErr, chatgpt.ErrTemporaryUpstream):
				return nil, ErrProviderTemporary
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
			}
		}
		imageBytes = b
		upstreamURL = u
	case "leonardo":
		b, u, execErr := s.generateLeonardoImage(genCtx, eventID, modelItem, in, aspectRatio, resolution, urlOnly)
		if execErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
			switch {
			case errors.Is(execErr, leonardo.ErrAuth):
				return nil, ErrProviderAuth
			case errors.Is(execErr, leonardo.ErrQuotaExhausted):
				return nil, ErrProviderQuota
			case errors.Is(execErr, leonardo.ErrTemporaryUpstream):
				return nil, ErrProviderTemporary
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
			}
		}
		imageBytes = b
		upstreamURL = u
	case "krea":
		b, u, execErr := s.generateKreaImage(genCtx, eventID, modelItem, in, aspectRatio, resolution, urlOnly)
		if execErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
			switch {
			case errors.Is(execErr, krea.ErrAuth):
				return nil, ErrProviderAuth
			case errors.Is(execErr, krea.ErrQuotaExhausted):
				return nil, ErrProviderQuota
			case errors.Is(execErr, krea.ErrTemporaryUpstream):
				return nil, ErrProviderTemporary
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
			}
		}
		imageBytes = b
		upstreamURL = u
	case "imagine":
		b, u, execErr := s.generateImagineImage(genCtx, eventID, modelItem, in, aspectRatio, resolution, urlOnly)
		if execErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
			switch {
			case errors.Is(execErr, imagine.ErrAuth):
				return nil, ErrProviderAuth
			case errors.Is(execErr, imagine.ErrQuotaExhausted):
				return nil, ErrProviderQuota
			case errors.Is(execErr, imagine.ErrTemporaryUpstream):
				return nil, ErrProviderTemporary
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
			}
		}
		imageBytes = b
		upstreamURL = u
	case "runway":
		b, u, execErr := s.generateRunwayImage(genCtx, eventID, modelItem, in, aspectRatio, resolution, urlOnly)
		if execErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
			switch {
			case errors.Is(execErr, runway.ErrAuth):
				return nil, ErrProviderAuth
			case errors.Is(execErr, runway.ErrQuotaExhausted):
				return nil, ErrProviderQuota
			case errors.Is(execErr, runway.ErrTemporaryUpstream):
				return nil, ErrProviderTemporary
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
			}
		}
		imageBytes = b
		upstreamURL = u
	case "grok":
		// Lite (fast mode) text-to-image — the only media a free grok account can
		// generate; aspect ratio is mapped to Grok's mediaGenInput format.
		b, u, execErr := s.generateGrokImage(genCtx, eventID, modelItem, in, aspectRatio, urlOnly)
		if execErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
			switch {
			case errors.Is(execErr, grok.ErrAuth):
				return nil, ErrProviderAuth
			case errors.Is(execErr, grok.ErrQuotaExhausted):
				return nil, ErrProviderQuota
			case errors.Is(execErr, grok.ErrTemporaryUpstream):
				return nil, ErrProviderTemporary
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
			}
		}
		imageBytes = b
		upstreamURL = u
	case "custom":
		b, u, execErr := s.generateCustomImage(genCtx, eventID, modelItem, in, aspectRatio, resolution, urlOnly)
		if execErr != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
			switch {
			case errors.Is(execErr, custom.ErrAuth):
				return nil, ErrProviderAuth
			case errors.Is(execErr, custom.ErrQuotaExhausted):
				return nil, ErrProviderQuota
			case errors.Is(execErr, custom.ErrTemporaryUpstream):
				return nil, ErrProviderTemporary
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
			}
		}
		imageBytes = b
		upstreamURL = u
	default:
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		_ = s.events.UpdateStatus(ctx, eventID, "failed", "provider not implemented", 0)
		return nil, fmt.Errorf("%w: %s", ErrProviderUnsupported, modelItem.Provider)
	}
	if apiRequest && in.ResponseFormat == "b64_json" && len(imageBytes) == 0 {
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		_ = s.events.UpdateStatus(ctx, eventID, "failed", "provider returned no image bytes", 0)
		return nil, fmt.Errorf("%w: provider returned no image bytes", ErrProviderExecution)
	}
	// 去AI特征: post-process before storing/returning. Best-effort — a decode
	// failure keeps the original bytes rather than failing a paid generation.
	if in.DeAI {
		if processed, derr := applyDeAI(imageBytes); derr == nil {
			imageBytes = processed
		}
	}
	if storeOutput {
		// Upload to RustFS. On failure the generation fails and credits are
		// refunded — we never fall back to local disk.
		if err := s.store.Put(genCtx, relativePath, imageBytes, "image/png"); err != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", "storage upload failed: "+err.Error(), 0)
			return nil, fmt.Errorf("%w: %v", ErrProviderExecution, err)
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
	if principal != nil && principal.User != nil {
		_ = s.users.IncrementGenerationCount(ctx, principal.User.ID)
	}
	if charge {
		_ = s.maybeGrantInviteReward(ctx, principal)
	}
	if apiRequest {
		if in.ResponseFormat == "b64_json" {
			b64 := base64.StdEncoding.EncodeToString(imageBytes)
			return map[string]any{
				"created":    time.Now().Unix(),
				"data":       []map[string]any{{"b64_json": b64}},
				"model":      modelItem.EffectiveName(),
				"provider":   modelItem.Provider,
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
				"provider":   modelItem.Provider,
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
				"provider":   modelItem.Provider,
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
			"provider":   modelItem.Provider,
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
		"provider":   modelItem.Provider,
		"kind":       "image",
		"url":        fileURL,
		"elapsed_ms": elapsedMS,
		"charged":    price,
		"credits":    principalCredits(principal),
	}, nil
}

// ImageTask recovers an API image request by its idempotency key. Results are
// scoped to the authenticated API-key owner and returned in OpenAI image shape.
func (s *V1Service) ImageTask(ctx context.Context, principal *APIPrincipal, requestID string) (map[string]any, error) {
	requestID = strings.TrimSpace(requestID)
	if principal == nil || principal.User == nil || requestID == "" {
		return nil, ErrImageTaskNotFound
	}
	if value, ok := s.imageJobs.Load(imageJobKey(principal.User.ID, requestID)); ok {
		response, _ := asyncImageJobResponse(value.(*asyncImageJob), requestID, s.imageTaskURL(requestID, ""))
		return response, nil
	}
	return s.imageTaskFromEvent(ctx, principal, requestID)
}

func (s *V1Service) imageTaskFromEvent(ctx context.Context, principal *APIPrincipal, requestID string) (map[string]any, error) {
	event, err := s.events.GetImageByRequestID(ctx, principal.User.ID, requestID)
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, ErrImageTaskNotFound
	}
	result := map[string]any{
		"id":         requestID,
		"object":     "image.generation.task",
		"request_id": requestID,
		"poll_url":   s.imageTaskURL(requestID, ""),
		"event_id":   event.ID,
		"created":    event.TS.Unix(),
		"data":       []any{},
	}
	switch event.Status {
	case "success":
		if strings.TrimSpace(event.File) == "" {
			return nil, fmt.Errorf("%w: recovered image file is missing", ErrProviderTemporary)
		}
		response, err := s.store.Get(ctx, event.File, "")
		if err != nil {
			return nil, fmt.Errorf("%w: failed to load recovered image", ErrProviderTemporary)
		}
		defer response.Body.Close()
		if response.StatusCode >= http.StatusBadRequest {
			return nil, fmt.Errorf("%w: recovered image is unavailable", ErrProviderTemporary)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20+1))
		if err != nil || len(body) == 0 || len(body) > 32<<20 {
			return nil, fmt.Errorf("%w: recovered image is invalid", ErrProviderTemporary)
		}
		result["status"] = "completed"
		result["data"] = []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(body)}}
	case "failed":
		result["status"] = "failed"
		result["error"] = strings.TrimSpace(event.Error)
	default:
		result["status"] = "in_progress"
	}
	return result, nil
}

func (s *V1Service) prepareSessionVideo(ctx context.Context, principal *APIPrincipal, in V1VideoRequest) (map[string]any, error) {
	return s.prepareVideoExecution(ctx, principal, in, "user", true)
}

func (s *V1Service) prepareAdminTestVideo(ctx context.Context, principal *APIPrincipal, in V1VideoRequest) (map[string]any, error) {
	return s.prepareVideoExecution(ctx, principal, in, "admin", false)
}

func (s *V1Service) prepareVideoExecution(ctx context.Context, principal *APIPrincipal, in V1VideoRequest, source string, charge bool) (map[string]any, error) {
	s.applyGlobalProxy(ctx)
	// Detach from the request lifecycle — see prepareImageExecution. `ctx`
	// (WithoutCancel) carries all bookkeeping; `genCtx` is the cancellable work
	// context (12-min backstop — video polls up to 10 min — and registered so the
	// maintenance sweep can cancel a stuck render when it abandons the row).
	ctx = context.WithoutCancel(ctx)
	if source == "v1" {
		in.BaseURL = s.outputBaseURL(in.BaseURL)
	}
	if len(in.ReferenceImages) > 0 && s.shouldApplyReferenceGrid(ctx, in.Model, in.ReferenceGrid) {
		gridded, err := applyReferenceFaceSwap(in.ReferenceImages)
		if err != nil {
			return nil, err
		}
		in.ReferenceImages = gridded
	}
	if source != "admin" {
		if err := s.checkBannedPrompt(ctx, principal, in.Prompt); err != nil {
			s.logRejectedEvent(ctx, "video", in.Model, principal, in.Prompt, source, err.Error())
			return nil, err
		}
	}
	genCtx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()

	// Per-user concurrency gate (画图台 + API key combined); admin tests exempt.
	if source != "admin" && principal != nil && principal.User != nil {
		slot := randomUpper(12)
		if !s.userAcquire(ctx, principal.User, slot) {
			s.logRejectedEvent(ctx, "video", in.Model, principal, in.Prompt, source, ErrUserConcurrencyFull.Error())
			return nil, ErrUserConcurrencyFull
		}
		defer s.userRelease(ctx, principal.User.ID, slot)
	}

	modelItem, resolution, aspectRatio, duration, price, err := s.prepareVideo(ctx, principal, in, charge)
	if err != nil {
		s.logRejectedEvent(ctx, "video", in.Model, principal, in.Prompt, source, err.Error())
		return nil, err
	}
	refCount := len(in.ReferenceImages) + len(in.ReferenceVideos) + len(in.ReferenceAudios)
	// API-key (source "v1") requests return base64 inline and never persist a
	// file — see prepareImageExecution for the rationale.
	noStore := source == "v1"
	var fileURL, relativePath string
	if !noStore {
		fileURL, relativePath = s.allocateOutput(principal, "mp4", in.BaseURL)
	}
	eventID, err := s.logPendingEvent(ctx, "video", modelItem, principal, in.Prompt, aspectRatio, resolution, duration, refCount, price, relativePath, source, nil, false, "")
	if err != nil {
		return nil, err
	}
	// Register so the maintenance sweep can cancel this render if it abandons the
	// row; deregister on return.
	s.inflight.Add(eventID, cancel)
	defer s.inflight.Done(eventID)
	startedAt := time.Now()

	// API-key (noStore) requests return the upstream video URL directly.
	// downloadResult=false skips the download. grok asset URLs are auth-gated
	// (a plain GET 403s) → gatedVideoURL routes them through the /content proxy.
	prov := s.effectiveProvider(genCtx, modelItem)
	urlOnly := noStore
	gatedVideoURL := prov == "grok"
	var videoBytes []byte
	var videoURL string
	var execErr error
	switch prov {
	case "adobe":
		videoBytes, videoURL, execErr = s.generateAdobeVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), !urlOnly)
	case "runway":
		videoBytes, videoURL, execErr = s.generateRunwayVideo(genCtx, eventID, modelItem, in, aspectRatio, parseDurationSeconds(duration), !urlOnly)
	case "leonardo":
		videoBytes, videoURL, execErr = s.generateLeonardoVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), !urlOnly)
	case "grok":
		videoBytes, videoURL, execErr = s.generateGrokVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), !urlOnly)
	case "oreate":
		videoBytes, videoURL, execErr = s.generateOreateVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), !urlOnly)
	case "custom":
		videoBytes, videoURL, execErr = s.generateCustomVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), !urlOnly)
	default:
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		_ = s.events.UpdateStatus(ctx, eventID, "failed", "provider not implemented", 0)
		return nil, fmt.Errorf("%w: %s", ErrProviderUnsupported, modelItem.Provider)
	}
	if execErr != nil {
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
		switch {
		case errors.Is(execErr, ErrNoProviderAccount):
			return nil, ErrNoProviderAccount
		case errors.Is(execErr, adobe.ErrAuth), errors.Is(execErr, runway.ErrAuth), errors.Is(execErr, grok.ErrAuth), errors.Is(execErr, oreate.ErrAuth), errors.Is(execErr, custom.ErrAuth):
			return nil, ErrProviderAuth
		case errors.Is(execErr, adobe.ErrQuotaExhausted), errors.Is(execErr, runway.ErrQuotaExhausted), errors.Is(execErr, grok.ErrQuotaExhausted), errors.Is(execErr, oreate.ErrQuotaExhausted), errors.Is(execErr, custom.ErrQuotaExhausted):
			return nil, ErrProviderQuota
		case errors.Is(execErr, adobe.ErrTemporaryUpstream), errors.Is(execErr, runway.ErrTemporaryUpstream), errors.Is(execErr, grok.ErrTemporaryUpstream), errors.Is(execErr, oreate.ErrTemporaryUpstream), errors.Is(execErr, oreate.ErrRiskControl), errors.Is(execErr, custom.ErrTemporaryUpstream):
			return nil, ErrProviderTemporary
		case errors.Is(execErr, adobe.ErrContentRejected), errors.Is(execErr, oreate.ErrContentRejected):
			return nil, execErr
		default:
			return nil, fmt.Errorf("%w: %v", ErrProviderExecution, execErr)
		}
	}
	if !noStore {
		if err := s.store.Put(genCtx, relativePath, videoBytes, "video/mp4"); err != nil {
			_ = s.refundIfNeeded(ctx, principal, eventID, price)
			_ = s.events.UpdateStatus(ctx, eventID, "failed", "storage upload failed: "+err.Error(), 0)
			return nil, fmt.Errorf("%w: %v", ErrProviderExecution, err)
		}
		// Best-effort stills: first frame (downscaled) for list thumbnails and
		// the full-res last frame for 首尾帧 continuation. Missing objects fall
		// back to the video itself at serve time.
		if thumb, last, terr := extractVideoFrames(genCtx, videoBytes); terr == nil {
			if len(thumb) > 0 {
				_ = s.store.Put(genCtx, ThumbKey(relativePath), thumb, "image/jpeg")
			}
			if len(last) > 0 {
				_ = s.store.Put(genCtx, LastFrameKey(relativePath), last, "image/jpeg")
			}
		}
	}
	elapsedMS := int(time.Since(startedAt).Milliseconds())
	if err := s.events.UpdateStatus(ctx, eventID, "success", "", elapsedMS); err != nil {
		return nil, err
	}
	_ = s.models.IncrementGenerationCount(ctx, modelItem.ID)
	if principal != nil && principal.User != nil {
		_ = s.users.IncrementGenerationCount(ctx, principal.User.ID)
	}
	if charge {
		_ = s.maybeGrantInviteReward(ctx, principal)
	}
	if noStore && strings.TrimSpace(videoURL) != "" {
		// Return the upstream video URL. grok URLs are auth-gated → store on the
		// event and hand back the /content proxy (re-fetches with the account token).
		outURL := videoURL
		if gatedVideoURL {
			_ = s.events.SetFile(ctx, eventID, videoURL)
			if base := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"); base != "" {
				outURL = base + "/v1/videos/" + eventID + "/content"
			}
		}
		return map[string]any{
			"created":    time.Now().Unix(),
			"data":       []map[string]any{{"url": outURL}},
			"model":      modelItem.EffectiveName(),
			"provider":   modelItem.Provider,
			"kind":       "video",
			"url":        outURL,
			"elapsed_ms": elapsedMS,
			"charged":    price,
			"credits":    principalCredits(principal),
		}, nil
	}
	if noStore {
		b64 := base64.StdEncoding.EncodeToString(videoBytes)
		return map[string]any{
			"created":    time.Now().Unix(),
			"data":       []map[string]any{{"b64_json": b64}},
			"model":      modelItem.EffectiveName(),
			"provider":   modelItem.Provider,
			"kind":       "video",
			"b64_json":   b64,
			"elapsed_ms": elapsedMS,
			"charged":    price,
			"credits":    principalCredits(principal),
		}, nil
	}
	return map[string]any{
		"created":    time.Now().Unix(),
		"data":       []map[string]any{{"url": fileURL}},
		"model":      modelItem.EffectiveName(),
		"provider":   modelItem.Provider,
		"kind":       "video",
		"url":        fileURL,
		"elapsed_ms": elapsedMS,
		"charged":    price,
		"credits":    principalCredits(principal),
	}, nil
}

// ===== /v1/videos — OpenAI Sora-style async jobs =====
// POST /v1/videos charges + creates a pending event and renders in the
// background; the render captures only the UPSTREAM video URL (no download, no
// RustFS). GET /v1/videos/{id} polls status; /content proxies the upstream URL.

// StartVideoJob validates+charges, creates the job event, kicks the render off in
// the background, and returns the OpenAI video object (status "queued").
func (s *V1Service) StartVideoJob(ctx context.Context, principal *APIPrincipal, in V1VideoRequest) (map[string]any, error) {
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
	eventID, err := s.logPendingEvent(ctx, "video", modelItem, principal, in.Prompt, aspectRatio, resolution, duration, len(in.ReferenceImages)+len(in.ReferenceVideos)+len(in.ReferenceAudios), price, "", "v1", nil, false, "")
	if err != nil {
		return nil, err
	}
	go s.runVideoJob(ctx, principal, in, modelItem, eventID, aspectRatio, resolution, duration, price)
	return videoJobObject(eventID, modelItem.EffectiveName(), "queued", 0, duration, sizeFromRatioRes(aspectRatio, resolution), time.Now().Unix(), 0, ""), nil
}

// runVideoJob renders the clip in the background, capturing the upstream URL
// (downloadResult=false → no bytes, no RustFS) and storing it on the event.
func (s *V1Service) runVideoJob(ctx context.Context, principal *APIPrincipal, in V1VideoRequest, modelItem *model.ModelConfig, eventID, aspectRatio, resolution, duration string, price float64) {
	s.applyGlobalProxy(ctx)
	genCtx, cancel := context.WithTimeout(ctx, 12*time.Minute)
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
			_ = s.events.UpdateStatus(ctx, eventID, "failed", gridErr.Error(), 0)
			return
		}
		in.ReferenceImages = gridded
	}

	// No-store: capture only the UPSTREAM video URL. /content streams it on demand
	// (grok URLs are auth-gated → fetched with the generating account's token).
	var videoURL string
	var execErr error
	switch s.effectiveProvider(genCtx, modelItem) {
	case "adobe":
		_, videoURL, execErr = s.generateAdobeVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), false)
	case "runway":
		_, videoURL, execErr = s.generateRunwayVideo(genCtx, eventID, modelItem, in, aspectRatio, parseDurationSeconds(duration), false)
	case "leonardo":
		_, videoURL, execErr = s.generateLeonardoVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), false)
	case "grok":
		_, videoURL, execErr = s.generateGrokVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), false)
	case "oreate":
		_, videoURL, execErr = s.generateOreateVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), false)
	case "custom":
		_, videoURL, execErr = s.generateCustomVideo(genCtx, eventID, modelItem, in, aspectRatio, resolution, parseDurationSeconds(duration), false)
	default:
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		_ = s.events.UpdateStatus(ctx, eventID, "failed", "provider not implemented", 0)
		return
	}
	if execErr != nil {
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		_ = s.events.UpdateStatus(ctx, eventID, "failed", execErr.Error(), 0)
		return
	}
	if strings.TrimSpace(videoURL) == "" {
		_ = s.refundIfNeeded(ctx, principal, eventID, price)
		_ = s.events.UpdateStatus(ctx, eventID, "failed", "upstream returned no video url", 0)
		return
	}
	// Store the upstream URL as the event's "file"; /content fetches it on demand.
	if err := s.events.MarkVideoReady(ctx, eventID, videoURL, int(time.Since(startedAt).Milliseconds())); err != nil {
		return
	}
	_ = s.models.IncrementGenerationCount(ctx, modelItem.ID)
	if principal != nil && principal.User != nil {
		_ = s.users.IncrementGenerationCount(ctx, principal.User.ID)
	}
	_ = s.maybeGrantInviteReward(ctx, principal)
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
		errMsg = ev.Error
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
		if s.settings != nil {
			if proxy, perr := s.settings.GetValue(ctx, "proxy.url"); perr == nil {
				s.grok.SetProxy(proxy)
			}
		}
		acct, _ := s.tokens.Get(ctx, "grok", ev.AccountID)
		if acct == nil || strings.TrimSpace(acct.Value) == "" {
			return nil, "", fmt.Errorf("%w: grok account no longer available for this video", ErrProviderTemporary)
		}
		return s.grok.OpenAsset(ctx, acct.Value, ev.File)
	}
	// Other providers return publicly-fetchable URLs. Artifact bytes are the data
	// plane and always use direct local egress to preserve the proxy allowance.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ev.File, nil)
	if err != nil {
		return nil, "", err
	}
	client, err := globalProxyHTTPClient("", 5*time.Minute)
	if err != nil {
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
	ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if ct == "" {
		ct = "video/mp4"
	}
	return resp.Body, ct, nil
}

// OpenImageContent streams a no-store image by proxying the stored upstream URL.
// chatgpt URLs are auth-gated (files.oaiusercontent.com — a plain GET 403s), so
// they're fetched through the generating account's token; other providers'
// URLs are public and proxied directly. The caller intentionally does not need
// an API key or web session: this is the directly-downloadable URL returned by
// the image API, and the event ID is a random opaque identifier.
func (s *V1Service) OpenImageContent(ctx context.Context, principal *APIPrincipal, id string) (io.ReadCloser, string, error) {
	_ = s.applyGlobalProxy(ctx)
	ev, err := s.events.GetByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, "", err
	}
	if ev == nil || ev.Kind != "image" {
		return nil, "", ErrVideoJobNotFound
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
			contentType = contentTypeForExt(filepath.Ext(file))
		}
		return resp.Body, contentType, nil
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
		if s.settings != nil {
			if proxy, perr := s.settings.GetValue(ctx, "proxy.url"); perr == nil {
				s.chatgpt.SetProxy(proxy)
			}
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
		if s.settings != nil {
			if proxy, perr := s.settings.GetValue(ctx, "proxy.url"); perr == nil {
				s.grok.SetProxy(proxy)
			}
		}
		acct, _ := s.tokens.Get(ctx, "grok", ev.AccountID)
		if acct == nil || strings.TrimSpace(acct.Value) == "" {
			return nil, "", fmt.Errorf("%w: grok account no longer available for this image", ErrProviderTemporary)
		}
		return s.grok.OpenAsset(ctx, acct.Value, ev.File)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ev.File, nil)
	if err != nil {
		return nil, "", err
	}
	client, err := globalProxyHTTPClient("", 5*time.Minute)
	if err != nil {
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
	ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if ct == "" {
		ct = "image/png"
	}
	return resp.Body, ct, nil
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
	return resp.Body, ct, true
}

// cacheImageStream reads a freshly downloaded image fully, stores a best-effort
// copy under key, and returns the same bytes as a stream so the caller's
// streaming contract is unchanged. A cache write failure never fails the fetch.
func (s *V1Service) cacheImageStream(ctx context.Context, key string, body io.ReadCloser, ct string) (io.ReadCloser, string, error) {
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, "", err
	}
	if s.store != nil && s.store.Configured() && len(data) > 0 {
		cct := strings.TrimSpace(ct)
		if cct == "" {
			cct = "image/png"
		}
		_ = s.store.Put(ctx, key, data, cct)
	}
	return io.NopCloser(bytes.NewReader(data)), ct, nil
}

func publicImageContentURL(baseURL, eventID string) string {
	path := "/v1/images/" + strings.TrimSpace(eventID) + "/content"
	if base := strings.TrimRight(strings.TrimSpace(baseURL), "/"); base != "" {
		return base + path
	}
	return path
}

func (s *V1Service) outputBaseURL(requestBaseURL string) string {
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
	if ev == nil || ev.Kind != "video" {
		return nil, ErrVideoJobNotFound
	}
	if principal != nil && principal.User != nil && ev.UserID != principal.User.ID {
		return nil, ErrVideoJobNotFound
	}
	return ev, nil
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

// hasActiveProviderToken reports whether the provider pool holds at least one
// usable token for this kind of generation — mirrors the selection filter in
// the generate* paths. Used to fail fast (before charging / creating a job)
// with a clear "no account" error instead of dialing upstream with no token.
func (s *V1Service) hasActiveProviderToken(ctx context.Context, provider, kind string) (bool, error) {
	items, err := s.tokens.ListByPool(ctx, provider)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Status != "active" || item.Dead || strings.TrimSpace(item.Value) == "" {
			continue
		}
		// Adobe tracks the two quotas separately. Grok's subscription tier is not
		// an entitlement signal for media anymore; video/image availability is
		// decided by the upstream generation response and cached credit balance.
		if provider == "adobe" || provider == "grok" {
			if kind == "video" && item.VideoLimited {
				continue
			}
			if kind == "image" && item.ImageLimited {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}

func (s *V1Service) prepareImage(ctx context.Context, principal *APIPrincipal, in V1ImageRequest, charge bool) (*model.ModelConfig, string, string, float64, error) {
	modelID := strings.TrimSpace(in.Model)
	prompt := strings.TrimSpace(in.Prompt)
	if modelID == "" || prompt == "" {
		return nil, "", "", 0, errors.New("model and prompt required")
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
	// Fail fast before charging if the provider has no usable account. Use the
	// effective provider: a custom upstream serving this model id routes to
	// "custom" (effectiveProvider only returns it when such an account exists, so
	// the precheck is satisfied); otherwise check the native provider pool.
	effectiveProvider := s.effectiveProvider(ctx, modelItem)
	if effectiveProvider != "custom" {
		if ok, err := s.hasActiveProviderToken(ctx, effectiveProvider, "image"); err != nil {
			return nil, "", "", 0, err
		} else if !ok {
			return nil, "", "", 0, ErrNoProviderAccount
		}
	}
	refLimit := 0
	if modelItem.ImageToImage {
		refLimit = modelItem.MaxReferenceImages
		if refLimit <= 0 {
			refLimit = 1
		}
	}
	if len(in.ReferenceImages) > refLimit {
		return nil, "", "", 0, errors.New("too many reference images")
	}
	// Reject oversized reference images before charging (all providers, all paths).
	if err := ensureReferenceSizes(in.ReferenceImages); err != nil {
		return nil, "", "", 0, err
	}
	// Native providers use their own resolution parameter, derived from `size` on
	// /v1 requests or supplied directly by the web UI. Only the GPT Image 2 family
	// interprets `quality` as a resolution tier adapter.
	aspectRatio, resolution := resolveImageSize(modelItem, in)
	// Snap to the nearest ratio the model actually supports — a `size`-derived
	// ratio (e.g. 1:3) must never be passed through to an upstream that rejects
	// it (Runway 400s on ratios outside its list).
	aspectRatio = snapRatio(aspectRatio, repo.JSONStrings(modelItem.Ratios))
	// parseImageSize defaults a blank resolution to "2K" (OpenAI-size parity).
	// For a model that doesn't price that tier — e.g. gpt-image-2 is 1K-only —
	// fall back to its first supported tier so a missing/stale resolution from
	// the client doesn't get rejected as "unsupported or unpriced".
	if _, ok := modelPrice(modelItem, "image", resolution, "", false); !ok {
		if fb := firstPricedResolution(modelItem); fb != "" {
			resolution = fb
		}
	}
	var surcharge float64
	if in.DeAI {
		surcharge = s.deaiSurcharge(ctx, resolution)
	}
	price, err := s.chargeForModel(ctx, principal, modelItem, "image", resolution, "", surcharge, charge)
	if err != nil {
		return nil, "", "", 0, err
	}
	return modelItem, resolution, aspectRatio, price, nil
}

func (s *V1Service) prepareVideo(ctx context.Context, principal *APIPrincipal, in V1VideoRequest, charge bool) (*model.ModelConfig, string, string, string, float64, error) {
	modelID := strings.TrimSpace(in.Model)
	prompt := strings.TrimSpace(in.Prompt)
	duration := strings.TrimSpace(in.Duration)
	if modelID == "" || prompt == "" {
		return nil, "", "", "", 0, errors.New("model and prompt required")
	}
	if duration == "" {
		return nil, "", "", "", 0, errors.New("duration required")
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
	// Fail fast before charging — effective provider (custom upstream by id, else native).
	effectiveProvider := s.effectiveProvider(ctx, modelItem)
	if effectiveProvider == "custom" {
		// custom serves this id (effectiveProvider guaranteed it) — precheck ok
	} else if ok, err := s.hasActiveProviderToken(ctx, effectiveProvider, "video"); err != nil {
		return nil, "", "", "", 0, err
	} else if !ok {
		return nil, "", "", "", 0, ErrNoProviderAccount
	}
	if err := validateVideoReferenceLimits(modelItem, in); err != nil {
		return nil, "", "", "", 0, err
	}
	// Reject oversized reference images before charging (all providers, all paths).
	if err := ensureReferenceSizes(in.ReferenceImages); err != nil {
		return nil, "", "", "", 0, err
	}
	if err := validateMediaReferences(in.ReferenceVideos, "video"); err != nil {
		return nil, "", "", "", 0, err
	}
	if err := validateMediaReferences(in.ReferenceAudios, "audio"); err != nil {
		return nil, "", "", "", 0, err
	}
	if len(in.ReferenceVideos) > 0 && (modelItem.ID == "firefly-kling-3" || modelItem.ID == "firefly-kling-o3") && parseDurationSeconds(duration) > 10 {
		return nil, "", "", "", 0, fmt.Errorf("%w: Kling video modification supports 3s to 10s", ErrUnsupportedParams)
	}
	// Runway i2v strictly requires exactly one first-frame image. Enforce it here,
	// BEFORE charging, so a missing/extra frame fails fast instead of charge →
	// upstream reject → refund. generateRunwayVideo keeps its own guard too.
	if modelItem.Provider == "runway" {
		n := 0
		for _, r := range in.ReferenceImages {
			if strings.TrimSpace(r) != "" {
				n++
			}
		}
		if n != 1 {
			return nil, "", "", "", 0, errors.New("runway 图生视频需要且仅需 1 张首帧图")
		}
	}
	aspectRatio := strings.TrimSpace(strings.ReplaceAll(in.AspectRatio, "x", ":"))
	if aspectRatio == "" {
		aspectRatio = "16:9"
	}
	resolution := strings.TrimSpace(in.Resolution)
	if resolution == "" {
		resolution = "720p"
	}
	if effectiveProvider == "adobe" {
		engine, _ := resolveAdobeVideoEngine(modelItem.ID)
		if !adobe.SupportsVideoDuration(engine, parseDurationSeconds(duration)) || !adobe.SupportsVideoResolution(engine, resolution) {
			return nil, "", "", "", 0, ErrUnsupportedParams
		}
	}
	price, err := s.chargeForModel(ctx, principal, modelItem, "video", resolution, duration, 0, charge)
	if err != nil {
		return nil, "", "", "", 0, err
	}
	return modelItem, resolution, aspectRatio, duration, price, nil
}

func (s *V1Service) chargeForModel(ctx context.Context, principal *APIPrincipal, modelItem *model.ModelConfig, kind, resolution, duration string, surcharge float64, charge bool) (float64, error) {
	// 代理用户走代理价(某档未设代理价则回退普通价)。principal.User 即将被扣费的
	// 用户,无论画图台还是 key 调用都从这里取,所以一处即覆盖所有路径。
	agent := principal != nil && principal.User != nil && principal.User.Role == "agent"
	price, ok := modelPrice(modelItem, kind, resolution, duration, agent)
	if !ok {
		return 0, ErrUnsupportedParams
	}
	price += surcharge
	if !charge || principal == nil || principal.User == nil {
		return 0, nil
	}
	updated, debited, err := s.users.TryDebitCredits(ctx, principal.User.ID, price)
	if err != nil {
		return 0, err
	}
	if !debited {
		if updated != nil {
			principal.User = updated
		}
		return 0, ErrInsufficientFunds
	}
	principal.User = updated
	return price, nil
}

func (s *V1Service) userDir(principal *APIPrincipal) string {
	if principal == nil {
		return "anon"
	}
	return OwnerDir(principal.User)
}

// OwnerDir is the storage directory (= /images/<owner>/ segment) a user's outputs
// live under: sanitized name → sanitized email-local → id → "anon".
func OwnerDir(user *model.User) string {
	if user != nil {
		if d := sanitizeOwnerName(user.Name); d != "" {
			return d
		}
		if d := sanitizeOwnerName(strings.Split(user.Email, "@")[0]); d != "" {
			return d
		}
		if user.ID != "" {
			return user.ID
		}
	}
	return "anon"
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

func (s *V1Service) logPendingEvent(ctx context.Context, kind string, modelItem *model.ModelConfig, principal *APIPrincipal, prompt, ratio, resolution, duration string, refs int, cost float64, file, source string, refFiles []string, deai bool, requestID string) (string, error) {
	event := &model.EventLog{
		ID:         "evt-" + randomUpper(24),
		RequestID:  requestID,
		TS:         time.Now(),
		Kind:       kind,
		Status:     "pending",
		Model:      modelItem.ID,
		Provider:   modelItem.Provider,
		Prompt:     prompt,
		Ratio:      ratio,
		Resolution: resolution,
		Duration:   duration,
		Refs:       refs,
		DeAI:       deai,
		Source:     source,
		Cost:       cost,
		File:       file,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if len(refFiles) > 0 {
		event.RefFiles = jsonArray(refFiles)
	}
	if principal != nil && principal.User != nil {
		event.UserID = principal.User.ID
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
	tempDeadCount := 0
	queueDeadline := time.Now().Add(providerAccountQueueWait)
	tempRetryDeadline := time.Now().Add(tempRetryWindow)
	tempRetryBackoff := tempRetryInitialBackoff
	// waitTempRetry pauses before re-running the pool after a temporary
	// upstream failure. It reports false once the retry window is spent or the
	// caller has gone away, at which point the error is surfaced.
	waitTempRetry := func() bool {
		if time.Now().After(tempRetryDeadline) || ctx.Err() != nil {
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
		var lastErr error
		lastTempDead := false
		retrying := false
		busy := 0
		for _, token := range active {
			slots := poolAccountConcurrency(pool, token)
			if !s.acctAcquire(ctx, token.ID, eventID, slots) {
				busy++
				continue
			}
			// release via defer so a panic in tryAccount can't leak the job slot.
			data, failover, tempDead, err := func() ([]byte, bool, bool, error) {
				defer s.acctRelease(ctx, token.ID, eventID)
				return s.tryAccount(ctx, eventID, pool, token, kind, attempt, classify, refreshOnAuth, tempFailover)
			}()
			if err == nil {
				return data, nil
			}
			lastErr = err
			lastTempDead = tempDead
			// Once the job's deadline is spent, another account can only fail on
			// the expired context and would mask the failure that consumed it.
			if ctx.Err() != nil {
				return nil, lastErr
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
		if lastErr != nil {
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
		if time.Now().After(queueDeadline) {
			return nil, ErrConcurrencyFull
		}

		timer := time.NewTimer(providerAccountQueuePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%w: %v", ErrConcurrencyFull, ctx.Err())
		case <-timer.C:
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
) ([]byte, bool, bool, error) {
	_ = s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail)
	_ = s.tokens.TouchLastUsed(ctx, token.ID)
	authRefreshed := false
	for {
		data, err := attempt(token)
		if err == nil {
			_, _ = s.tokens.Update(ctx, pool, token.ID, map[string]any{
				"last_used_at":  time.Now(),
				"success_total": gorm.Expr("success_total + 1"),
				"fails":         0,
			})
			return data, false, false, nil
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
			s.coolDownAccount(pool, token.ID)
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
				s.coolDownAccount(pool, token.ID)
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
			s.coolDownAccount(pool, token.ID)
			return nil, true, true, err
		}
		return nil, false, false, err // 参数错 / request-level
	}
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
	data, err := s.runPoolWithFailover(ctx, eventID, "adobe", active, "image", func(token model.TokenAccount) ([]byte, error) {
		var blobIDs []string
		for _, ref := range refs {
			id, upErr := s.adobe.UploadImage(ctx, token.Value, ref, "image/png", "")
			if upErr != nil {
				return nil, upErr
			}
			blobIDs = append(blobIDs, id)
		}
		d, meta, genErr := s.adobe.GenerateImage(ctx, token.Value, modelItem.ID, in.Prompt, aspectRatio, resolution, blobIDs, !urlOnly)
		if genErr == nil {
			imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
		}
		return d, genErr
	}, adobeErrClass, func(id string) (model.TokenAccount, bool) {
		return s.refreshAdobeToken(ctx, id)
	}, true)
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
	data, err := s.runPoolWithFailover(ctx, eventID, "adobe", active, "video", func(token model.TokenAccount) ([]byte, error) {
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
		bytes, meta, genErr := s.adobe.GenerateVideo(ctx, token.Value, engine, in.Prompt, aspectRatio, durationSeconds, resolution, referenceMode, upstreamModel, inputs, downloadResult)
		if genErr == nil {
			videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
		}
		return bytes, genErr
	}, adobeErrClass, func(id string) (model.TokenAccount, bool) {
		return s.refreshAdobeToken(ctx, id)
	}, true)
	return data, videoURL, err
}

// leonardoMinCredits is the per-generation token cost (one Leonardo image = 30
// tokens). An account with fewer is treated as 限额 and skipped — it can't afford
// a generation. Daily renewal (tokenRenewalDate) drives auto-recovery.
const leonardoMinCredits = 30

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

	var lastErr error
	var videoURL string
	busy := 0
	for _, token := range active {
		// 1 concurrent job per account: skip any account already generating.
		if !s.acctAcquire(ctx, token.ID, eventID, 1) {
			busy++
			continue
		}
		var data []byte
		done, failover := func() (bool, bool) {
			defer s.acctRelease(ctx, token.ID, eventID)
			_ = s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail)
			_ = s.tokens.TouchLastUsed(ctx, token.ID)
			teamID := ""
			if token.Meta != nil {
				teamID = strings.TrimSpace(stringValue(token.Meta["team_id"]))
			}
			d, meta, genErr := s.runway.GenerateVideo(ctx, token.Value, teamID, in.Prompt, aspectRatio, durationSeconds, frame, downloadResult)
			if genErr == nil {
				_, _ = s.tokens.Update(ctx, "runway", token.ID, map[string]any{
					"last_used_at":  time.Now(),
					"success_total": gorm.Expr("success_total + 1"),
					"fails":         0,
				})
				data = d
				videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
				return true, false
			}
			lastErr = genErr
			switch {
			case errors.Is(genErr, runway.ErrAuth), errors.Is(genErr, runway.ErrQuotaExhausted):
				// 额度没了 / token 失效 → 当 401 判死(status=disabled, dead),换号。
				s.markTokenFailure(ctx, "runway", token, "video", true, false)
				return false, true
			case errors.Is(genErr, runway.ErrTemporaryUpstream):
				// 上游临时错误 → 直接换下一个号。
				return false, true
			default:
				// 参数级错误(如 prompt 未过审)→ 直接失败,不换号。
				return false, false
			}
		}()
		if done {
			return data, videoURL, nil
		}
		if failover {
			continue
		}
		return nil, "", lastErr
	}
	if lastErr == nil {
		if busy > 0 {
			return nil, "", ErrConcurrencyFull
		}
		lastErr = ErrProviderExecution
	}
	return nil, "", lastErr
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

// accountConcurrency is the configured per-account simultaneous-job cap, with a
// default of one. Pool-specific defaults are applied by poolAccountConcurrency.
func accountConcurrency(item model.TokenAccount) int {
	if item.Concurrency > 0 {
		return item.Concurrency
	}
	return 1
}

func poolAccountConcurrency(pool string, item model.TokenAccount) int {
	if item.Concurrency > 0 {
		return min(item.Concurrency, 20)
	}
	if pool == "adobe" {
		if total, ok := jsonMapInt(item.Meta, "cached_quota_total"); ok && total >= 10000 {
			return adobePointsConcurrencyPerAccount
		}
	}
	return 1
}

// effectiveProvider routes a model to the "custom" upstream whenever a custom
// account declares it serves that model id (id-based override of the model's
// native provider) — so an upstream can take over any model by matching its id.
// Otherwise the model's own provider is used.
func (s *V1Service) effectiveProvider(ctx context.Context, modelItem *model.ModelConfig) string {
	if s.custom != nil {
		if active, err := s.customActive(ctx, modelItem.ID); err == nil && len(active) > 0 {
			return "custom"
		}
	}
	return modelItem.Provider
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
	quality := upstreamQualityForModel(modelItem.ID, resolution)
	var lastErr error
	busy := 0
	for _, token := range active {
		if !s.acctAcquire(ctx, token.ID, eventID, accountConcurrency(token)) {
			busy++
			continue
		}
		var data []byte
		var imgURL string
		done, failover := func() (bool, bool) {
			defer s.acctRelease(ctx, token.ID, eventID)
			_ = s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail)
			_ = s.tokens.TouchLastUsed(ctx, token.ID)
			baseURL := stringValue(token.Meta["base_url"])
			d, u, genErr := s.custom.GenerateImage(ctx, baseURL, token.Value, modelItem.ID, in.Prompt, size, quality, refs, !urlOnly)
			if genErr == nil {
				_, _ = s.tokens.Update(ctx, "custom", token.ID, map[string]any{
					"last_used_at": time.Now(), "success_total": gorm.Expr("success_total + 1"), "fails": 0,
				})
				data = d
				imgURL = u
				return true, false
			}
			lastErr = genErr
			switch {
			case errors.Is(genErr, custom.ErrAuth):
				s.markTokenFailure(ctx, "custom", token, "image", true, false)
				return false, true
			case errors.Is(genErr, custom.ErrQuotaExhausted):
				s.markTokenFailure(ctx, "custom", token, "image", false, true)
				return false, true
			case errors.Is(genErr, custom.ErrTemporaryUpstream):
				return false, true
			default:
				return false, false
			}
		}()
		if done {
			return data, imgURL, nil
		}
		if failover {
			continue
		}
		return nil, "", lastErr
	}
	if lastErr == nil {
		if busy > 0 {
			return nil, "", ErrConcurrencyFull
		}
		lastErr = ErrProviderExecution
	}
	return nil, "", lastErr
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
	var lastErr error
	var videoURL string
	busy := 0
	for _, token := range active {
		if !s.acctAcquire(ctx, token.ID, eventID, accountConcurrency(token)) {
			busy++
			continue
		}
		var data []byte
		done, failover := func() (bool, bool) {
			defer s.acctRelease(ctx, token.ID, eventID)
			_ = s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail)
			_ = s.tokens.TouchLastUsed(ctx, token.ID)
			baseURL := stringValue(token.Meta["base_url"])
			d, url, genErr := s.custom.GenerateVideo(ctx, baseURL, token.Value, modelItem.ID, in.Prompt, size, durationSeconds, frames, downloadResult)
			if genErr == nil {
				_, _ = s.tokens.Update(ctx, "custom", token.ID, map[string]any{
					"last_used_at": time.Now(), "success_total": gorm.Expr("success_total + 1"), "fails": 0,
				})
				data = d
				videoURL = url
				return true, false
			}
			lastErr = genErr
			switch {
			case errors.Is(genErr, custom.ErrAuth):
				s.markTokenFailure(ctx, "custom", token, "video", true, false)
				return false, true
			case errors.Is(genErr, custom.ErrQuotaExhausted):
				s.markTokenFailure(ctx, "custom", token, "video", false, true)
				return false, true
			case errors.Is(genErr, custom.ErrTemporaryUpstream):
				return false, true
			default:
				return false, false
			}
		}()
		if done {
			return data, videoURL, nil
		}
		if failover {
			continue
		}
		return nil, "", lastErr
	}
	if lastErr == nil {
		if busy > 0 {
			return nil, "", ErrConcurrencyFull
		}
		lastErr = ErrProviderExecution
	}
	return nil, "", lastErr
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

// upstreamQuality maps a resolution tier to the OpenAI quality enum.
func upstreamQuality(resolution string) string {
	switch strings.ToUpper(strings.TrimSpace(resolution)) {
	case "2K":
		return "medium"
	case "4K":
		return "high"
	case "1K":
		return "low"
	}
	return ""
}

func upstreamQualityForModel(modelID, resolution string) string {
	if !supportsQualityResolutionModel(modelID) {
		return ""
	}
	return upstreamQuality(resolution)
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
	data, err := s.runPoolWithFailover(ctx, eventID, "oreate", active, "video", func(token model.TokenAccount) ([]byte, error) {
		// Reserve the exact request cost under the account row lock. This closes the
		// gap between pool selection and submit when a preceding queued job has just
		// consumed the same account's balance.
		allowed, deducted, reserveErr := s.tokens.ReserveQuota(ctx, "oreate", token.ID, requiredCredits)
		if reserveErr != nil {
			return nil, fmt.Errorf("%w: reserve credits: %v", oreate.ErrTemporaryUpstream, reserveErr)
		}
		if !allowed {
			return nil, fmt.Errorf("%w: %w", oreate.ErrQuotaExhausted, errAccountTaskQuota)
		}
		blob, meta, genErr := s.oreate.GenerateVideo(ctx, oreateAccountFromToken(token), oreate.VideoOptions{
			ModelID: upstreamModel, Prompt: in.Prompt, Ratio: aspectRatio, Resolution: resolution,
			Duration: durationSeconds, Audio: in.GenerateAudio, DownloadResult: downloadResult,
			ReferenceImages: imageRefs, ReferenceVideos: videoRefs,
		})
		if genErr != nil {
			if deducted {
				_ = s.tokens.RefundQuota(ctx, "oreate", token.ID, requiredCredits)
			}
			if errors.Is(genErr, oreate.ErrQuotaExhausted) {
				if remaining, known := s.reconcileOreateCredits(ctx, token); known && remaining >= oreateMinUsableCredits {
					genErr = fmt.Errorf("%w: %w", genErr, errAccountTaskQuota)
				}
			}
			if errors.Is(genErr, oreate.ErrSpamUser) {
				s.quarantineOreateSpamAccount(token.ID)
				genErr = fmt.Errorf("%w (上游只对该账号的视频风控：同一 cookie 在 Oreate 官网仍能出图，已换号重试)", genErr)
			}
			return nil, genErr
		}
		s.oreateSpam.Delete(token.ID)
		videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
		_, _ = s.reconcileOreateCredits(ctx, token)
		return blob, nil
	}, oreateErrClass, nil, false)
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

func (s *V1Service) reconcileOreateCredits(ctx context.Context, token model.TokenAccount) (int, bool) {
	data, err := s.oreate.FetchCreditsBalance(ctx, oreateAccountFromToken(token))
	if err != nil {
		return 0, false
	}
	remaining, hasRemaining := data["remaining"].(int)
	if _, updateErr := persistOreateQuotaSnapshot(ctx, s.tokens, token.ID, data, time.Now()); updateErr != nil {
		log.Printf("oreate reconciliation: could not persist refreshed quota for %s: %v", token.ID, updateErr)
	}
	return remaining, hasRemaining
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
	if s.settings != nil {
		if proxy, err := s.settings.GetValue(ctx, "proxy.url"); err == nil {
			s.grok.SetProxy(proxy)
		}
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
	var lastErr error
	var videoURL string
	busy := 0
	for _, token := range active {
		// grok allows 10 concurrent jobs per account (unlike the 1-per-account
		// default of the other pools).
		if !s.acctAcquire(ctx, token.ID, eventID, grokConcurrencyPerAccount) {
			busy++
			continue
		}
		var data []byte
		done, failover := func() (bool, bool) {
			defer s.acctRelease(ctx, token.ID, eventID)
			_ = s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail)
			_ = s.tokens.TouchLastUsed(ctx, token.ID)
			d, meta, genErr := s.grok.GenerateVideo(ctx, token.Value, in.Prompt, aspectRatio, res, durationSeconds, frames, downloadResult)
			if genErr == nil {
				_, _ = s.tokens.Update(ctx, "grok", token.ID, map[string]any{
					"last_used_at":  time.Now(),
					"success_total": gorm.Expr("success_total + 1"),
					"fails":         0,
				})
				data = d
				videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
				return true, false
			}
			lastErr = genErr
			switch {
			case errors.Is(genErr, grok.ErrChallenge):
				return false, false
			case errors.Is(genErr, grok.ErrAuth), errors.Is(genErr, grok.ErrQuotaExhausted):
				// 失效 / 额度没了 → 当 401 判死(不续期),换号。
				s.markTokenFailure(ctx, "grok", token, "video", true, false)
				return false, true
			case errors.Is(genErr, grok.ErrTemporaryUpstream):
				return false, true
			default:
				return false, false
			}
		}()
		if done {
			return data, videoURL, nil
		}
		if failover {
			continue
		}
		return nil, "", lastErr
	}
	if lastErr == nil {
		if busy > 0 {
			return nil, "", ErrConcurrencyFull
		}
		lastErr = ErrProviderExecution
	}
	return nil, "", lastErr
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
	if s.settings != nil {
		if proxy, err := s.settings.GetValue(ctx, "proxy.url"); err == nil {
			s.grok.SetProxy(proxy)
		}
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

	var lastErr error
	var imageURL string
	busy := 0
	for _, token := range active {
		if !s.acctAcquire(ctx, token.ID, eventID, grokConcurrencyPerAccount) {
			busy++
			continue
		}
		var data []byte
		done, failover := func() (bool, bool) {
			defer s.acctRelease(ctx, token.ID, eventID)
			_ = s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail)
			_ = s.tokens.TouchLastUsed(ctx, token.ID)
			d, meta, genErr := s.grok.GenerateImage(ctx, token.Value, in.Prompt, aspectRatio, !urlOnly)
			if genErr == nil {
				_, _ = s.tokens.Update(ctx, "grok", token.ID, map[string]any{
					"last_used_at":  time.Now(),
					"success_total": gorm.Expr("success_total + 1"),
					"fails":         0,
				})
				data = d
				imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
				return true, false
			}
			lastErr = genErr
			switch {
			case errors.Is(genErr, grok.ErrChallenge):
				return false, false
			case errors.Is(genErr, grok.ErrAuth), errors.Is(genErr, grok.ErrQuotaExhausted):
				s.markTokenFailure(ctx, "grok", token, "image", true, false)
				return false, true
			case errors.Is(genErr, grok.ErrTemporaryUpstream):
				return false, true
			default:
				return false, false
			}
		}()
		if done {
			return data, imageURL, nil
		}
		if failover {
			continue
		}
		return nil, "", lastErr
	}
	if lastErr == nil {
		if busy > 0 {
			return nil, "", ErrConcurrencyFull
		}
		lastErr = ErrProviderExecution
	}
	return nil, "", lastErr
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
	var lastErr error
	busy := 0
	for _, token := range active {
		// 1 concurrent job per account: skip any account already generating.
		if !s.acctAcquire(ctx, token.ID, eventID, 1) {
			busy++
			continue
		}
		var data []byte
		var artURL string
		done, failover := func() (bool, bool) {
			defer s.acctRelease(ctx, token.ID, eventID)
			_ = s.events.SetAccount(ctx, eventID, token.ID, token.AccountEmail)
			_ = s.tokens.TouchLastUsed(ctx, token.ID)
			teamID := ""
			if token.Meta != nil {
				teamID = strings.TrimSpace(stringValue(token.Meta["team_id"]))
			}
			// downloadResult=false in url-only mode → skip the artifact download and
			// just return meta["image_url"].
			d, meta, genErr := s.runway.GenerateImage(ctx, token.Value, teamID, modelItem.ID, in.Prompt, aspectRatio, imageSize, refs, !urlOnly)
			if genErr == nil {
				_, _ = s.tokens.Update(ctx, "runway", token.ID, map[string]any{
					"last_used_at":  time.Now(),
					"success_total": gorm.Expr("success_total + 1"),
					"fails":         0,
				})
				data = d
				artURL = strings.TrimSpace(stringValue(meta["image_url"]))
				return true, false
			}
			lastErr = genErr
			switch {
			case errors.Is(genErr, runway.ErrAuth), errors.Is(genErr, runway.ErrQuotaExhausted):
				// 额度没了 / token 失效 → 当 401 判死(status=disabled, dead),换号。
				s.markTokenFailure(ctx, "runway", token, "image", true, false)
				return false, true
			case errors.Is(genErr, runway.ErrTemporaryUpstream):
				// 上游临时错误 → 直接换下一个号。
				return false, true
			default:
				// 参数级错误(如 prompt 未过审)→ 直接失败,不换号。
				return false, false
			}
		}()
		if done {
			return data, artURL, nil
		}
		if failover {
			continue
		}
		return nil, "", lastErr
	}
	if lastErr == nil {
		if busy > 0 {
			return nil, "", ErrConcurrencyFull
		}
		lastErr = ErrProviderExecution
	}
	return nil, "", lastErr
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
	meta := cloneJSONMap(item.Meta)
	meta["cached_quota_remaining"] = rem
	meta["cached_quota_at"] = int(time.Now().Unix())
	patch := map[string]any{"meta": meta}
	if reset := strings.TrimSpace(stringValue(data["reset_after"])); reset != "" {
		patch["cached_quota_reset_after"] = reset
	} else if strings.TrimSpace(item.CachedQuotaResetAfter) == "" {
		patch["cached_quota_reset_after"] = leonardoResetAfter("")
	}
	if exhausted && item.Status == "active" {
		patch["status"] = "quota"
	}
	_, _ = s.tokens.Update(ctx, "chatgpt", tokenID, patch)
}

// chatgpt image URLs are auth-gated (files.oaiusercontent.com — a plain GET
// 403s), so url-only mode returns the URL for the caller to proxy via
// OpenImageContent using the generating account's token.
func (s *V1Service) generateChatGPTImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	urlOnly := noStore
	if s.chatgpt == nil {
		return nil, "", errors.New("chatgpt client not configured")
	}
	if s.settings != nil {
		if proxy, err := s.settings.GetValue(ctx, "proxy.url"); err == nil {
			s.chatgpt.SetProxy(proxy)
		}
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
		d, meta, genErr := s.chatgpt.GenerateImage(ctx, token.Value, in.Prompt, modelItem.ID, aspectRatio, resolution, refs, !urlOnly)
		if genErr == nil {
			imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
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

// leonardoResetAfter returns when a Leonardo account's daily free tokens renew.
// Leonardo resets at 08:00 Beijing == 00:00 UTC, so when the upstream gives no
// explicit renewal time we deterministically use the next UTC midnight — this is
// filled at import so 恢复时间 is always populated, not left blank.
func leonardoResetAfter(availableUntil string) string {
	if v := strings.TrimSpace(availableUntil); v != "" {
		return v
	}
	return time.Unix((time.Now().Unix()/86400+1)*86400, 0).UTC().Format(time.RFC3339)
}

// applyLeonardoPlanMeta 记下账号是普通号还是积分号:积分号(paid)的余额是买来的点数 /
// 订阅点数,按上游 tokenRenewalDate 月度续期,不能套用普通号的每日重置。余额读取失败
// (unknown,没有 paid 字段)时不写,以免把已知的积分号标记清掉。
func applyLeonardoPlanMeta(meta map[string]any, data map[string]any) {
	paid, ok := data["paid"].(bool)
	if !ok {
		return
	}
	meta["paid_account"] = paid
	if plan := strings.TrimSpace(stringValue(data["plan"])); plan != "" {
		meta["plan"] = plan
	}
}

// leonardoDimensions maps the catalog's resolution+ratio to Leonardo pixel sizes.
func leonardoDimensions(resolution, aspectRatio string) (int, int) {
	res := strings.ToUpper(strings.TrimSpace(resolution))
	ar := strings.TrimSpace(aspectRatio)
	if res == "4K" {
		switch ar {
		case "2:3":
			return 2000, 3000
		case "16:9":
			return 4096, 2304
		case "4:3":
			return 4096, 3072
		case "4:5":
			return 3264, 4080
		case "9:16":
			return 2160, 3840
		case "2:1":
			return 4096, 2048
		default: // 1:1
			return 4096, 4096
		}
	}
	switch ar { // 2K (default)
	case "2:3":
		return 1664, 2496
	case "16:9":
		return 2560, 1440
	case "4:3":
		return 2304, 1728
	case "4:5":
		return 2432, 3040
	case "9:16":
		return 1440, 2560
	case "2:1":
		return 3232, 1616
	default: // 1:1
		return 2048, 2048
	}
}

func (s *V1Service) generateLeonardoImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	urlOnly := noStore
	if s.leonardo == nil {
		return nil, "", errors.New("leonardo client not configured")
	}
	items, err := s.tokens.ListByPool(ctx, "leonardo")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		if item.Status != "active" || item.Dead || strings.TrimSpace(item.Value) == "" {
			continue
		}
		// Skip accounts under the per-generation floor (treated as 限额). Unknown
		// balance gets the benefit of the doubt (upstream rejects if truly empty).
		if rem, ok := jsonMapInt(item.Meta, "cached_quota_remaining"); ok && rem < leonardoMinCredits {
			continue
		}
		active = append(active, item)
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("leonardo", active)

	width, height := leonardoDimensions(resolution, aspectRatio)
	// The upstream Leonardo model name (e.g. seedream-4.5): upstream_model when set
	// (catalog ids must be unique across providers), else the catalog id.
	upstreamModel := strings.TrimSpace(modelItem.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(modelItem.ID)
	}

	// Optional image-to-image: decode the reference image once up front (Leonardo
	// seedream takes at most one).
	refLimit := modelItem.MaxReferenceImages
	if refLimit <= 0 {
		refLimit = 1
	}
	refs, err := decodeReferenceImages(in.ReferenceImages, refLimit)
	if err != nil {
		return nil, "", err
	}

	// token.Value is the cookie; GenerateImage mints a fresh JWT each attempt, so an
	// auth failure means the cookie itself is dead — no refresher (nil).
	var imageURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "leonardo", active, "image", func(token model.TokenAccount) ([]byte, error) {
		// Atomically pre-deduct the per-generation cost so concurrent picks of the
		// same near-empty account can't over-commit it. A known-insufficient
		// balance surfaces as quota → the driver fails over to the next account.
		allowed, deducted, rerr := s.tokens.ReserveQuota(ctx, "leonardo", token.ID, leonardoMinCredits)
		if rerr != nil {
			return nil, fmt.Errorf("%w: reserve: %v", leonardo.ErrTemporaryUpstream, rerr)
		}
		if !allowed {
			return nil, leonardo.ErrQuotaExhausted
		}
		data, meta, genErr := s.leonardo.GenerateImage(ctx, token.Value, upstreamModel, in.Prompt, width, height, nil, refs, !urlOnly)
		if genErr != nil {
			// Release the hold so a failed render doesn't burn credits.
			if deducted {
				_ = s.tokens.RefundQuota(ctx, "leonardo", token.ID, leonardoMinCredits)
			}
			return nil, genErr
		}
		imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
		// Success → overwrite the held value with the REAL upstream balance and
		// sink to 限额 if below the floor (best-effort; never fails a done render).
		s.reconcileLeonardoCredits(ctx, token.ID, token.Value)
		return data, nil
	}, func(e error) (bool, bool, bool, bool) {
		return errors.Is(e, leonardo.ErrAuth), errors.Is(e, leonardo.ErrQuotaExhausted), errors.Is(e, leonardo.ErrTemporaryUpstream), false
	}, nil, true)
	return data, imageURL, err
}

// leonardoVideoDimensions maps the catalog resolution+ratio to the pixel sizes
// Leonardo's video models accept (16:9 / 9:16 landscape-portrait pairs).
func leonardoVideoDimensions(resolution, aspectRatio string) (int, int) {
	res := strings.ToLower(strings.TrimSpace(resolution))
	portrait := strings.TrimSpace(aspectRatio) == "9:16"
	if res == "1080p" {
		if portrait {
			return 1080, 1920
		}
		return 1920, 1080
	}
	// 720p (default)
	if portrait {
		return 720, 1280
	}
	return 1280, 720
}

// leonardoVideoMinCredits is the cheapest per-clip token cost across Leonardo's
// video models (LTX Fast = 40 tokens); accounts under it can't afford a clip.
const leonardoVideoMinCredits = 40

// generateLeonardoVideo runs the Leonardo text-to-video pipeline. Same account
// policy as generateLeonardoImage: atomically pre-reserve the floor cost, refund
// on failure, reconcile the real upstream balance after success.
func (s *V1Service) generateLeonardoVideo(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1VideoRequest, aspectRatio, resolution string, durationSeconds int, downloadResult bool) ([]byte, string, error) {
	if s.leonardo == nil {
		return nil, "", errors.New("leonardo client not configured")
	}
	items, err := s.tokens.ListByPool(ctx, "leonardo")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		if item.Status != "active" || item.Dead || strings.TrimSpace(item.Value) == "" {
			continue
		}
		if item.VideoLimited {
			continue
		}
		if rem, ok := jsonMapInt(item.Meta, "cached_quota_remaining"); ok && rem < leonardoVideoMinCredits {
			continue
		}
		active = append(active, item)
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("leonardo", active)

	width, height := leonardoVideoDimensions(resolution, aspectRatio)
	// The upstream Leonardo model name: upstream_model when set (catalog ids must
	// be unique across providers), else the catalog id.
	upstreamModel := strings.TrimSpace(modelItem.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(modelItem.ID)
	}

	// token.Value is the cookie; GenerateVideo mints a fresh JWT each attempt, so
	// an auth failure means the cookie itself is dead — no refresher (nil).
	var videoURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "leonardo", active, "video", func(token model.TokenAccount) ([]byte, error) {
		allowed, deducted, rerr := s.tokens.ReserveQuota(ctx, "leonardo", token.ID, leonardoVideoMinCredits)
		if rerr != nil {
			return nil, fmt.Errorf("%w: reserve: %v", leonardo.ErrTemporaryUpstream, rerr)
		}
		if !allowed {
			return nil, leonardo.ErrQuotaExhausted
		}
		data, meta, genErr := s.leonardo.GenerateVideo(ctx, token.Value, upstreamModel, in.Prompt, width, height, durationSeconds, downloadResult)
		if genErr != nil {
			if deducted {
				_ = s.tokens.RefundQuota(ctx, "leonardo", token.ID, leonardoVideoMinCredits)
			}
			return nil, genErr
		}
		videoURL = strings.TrimSpace(stringValue(meta["video_url"]))
		s.reconcileLeonardoCredits(ctx, token.ID, token.Value)
		return data, nil
	}, func(e error) (bool, bool, bool, bool) {
		return errors.Is(e, leonardo.ErrAuth), errors.Is(e, leonardo.ErrQuotaExhausted), errors.Is(e, leonardo.ErrTemporaryUpstream), false
	}, nil, true)
	return data, videoURL, err
}

// reconcileLeonardoCredits re-fetches an account's real token balance after a
// render and writes it back, flipping the account to 限额 when below the per-gen
// floor. Stores the daily renewal time so RecoverQuota can auto-recover it.
func (s *V1Service) reconcileLeonardoCredits(ctx context.Context, tokenID, cookie string) {
	if s.leonardo == nil {
		return
	}
	data, err := s.leonardo.FetchCreditsBalance(ctx, cookie)
	if err != nil {
		return
	}
	rem, ok := data["remaining"].(int)
	if !ok {
		return
	}
	item, err := s.tokens.Get(ctx, "leonardo", tokenID)
	if err != nil {
		return
	}
	meta := cloneJSONMap(item.Meta)
	meta["cached_quota_remaining"] = rem
	meta["cached_quota_at"] = int(time.Now().Unix())
	applyLeonardoPlanMeta(meta, data)
	patch := map[string]any{"meta": meta}
	patch["cached_quota_reset_after"] = leonardoResetAfter(stringValue(data["available_until"]))
	if rem < leonardoMinCredits && item.Status == "active" {
		patch["status"] = "quota"
	}
	_, _ = s.tokens.Update(ctx, "leonardo", tokenID, patch)
}

// kreaRefreshAndPersist ensures the account's Krea cookie has a valid access token
// (refreshing via the rotating refresh_token when expired) and persists the new
// cookie — the refresh_token is single-use, so the rotated value MUST be saved.
func kreaRefreshAndPersist(ctx context.Context, client *krea.Client, tokens *repo.TokenRepository, tokenID, cookie string) (string, error) {
	if client == nil {
		return cookie, nil
	}
	fresh, changed, err := client.RefreshIfNeeded(ctx, cookie)
	if err != nil {
		return "", err
	}
	if changed && tokenID != "" {
		_, _ = tokens.Update(ctx, "krea", tokenID, map[string]any{"value": fresh})
	}
	return fresh, nil
}

// kreaDimensions maps the catalog's resolution+ratio to Krea pixel sizes.
func kreaDimensions(resolution, aspectRatio string) (int, int) {
	res := strings.ToUpper(strings.TrimSpace(resolution))
	ar := strings.TrimSpace(aspectRatio)
	if res == "2K" {
		switch ar {
		case "4:3":
			return 2048, 1536
		case "3:4":
			return 1536, 2048
		case "16:9":
			return 2048, 1152
		case "9:16":
			return 1152, 2048
		default: // 1:1
			return 2048, 2048
		}
	}
	switch ar { // 1K (default)
	case "4:3":
		return 1024, 768
	case "3:4":
		return 768, 1024
	case "16:9":
		return 1024, 576
	case "9:16":
		return 576, 1024
	default: // 1:1
		return 1024, 1024
	}
}

func (s *V1Service) generateKreaImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	urlOnly := noStore
	if s.krea == nil {
		return nil, "", errors.New("krea client not configured")
	}
	items, err := s.tokens.ListByPool(ctx, "krea")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		// No numeric floor — Krea signals 限额 with a 402 at generation time, which
		// the failover driver turns into mark-quota + next account.
		if item.Status == "active" && !item.Dead && strings.TrimSpace(item.Value) != "" {
			active = append(active, item)
		}
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("krea", active)

	width, height := kreaDimensions(resolution, aspectRatio)
	refLimit := modelItem.MaxReferenceImages
	if refLimit <= 0 {
		refLimit = 1
	}
	refs, err := decodeReferenceImages(in.ReferenceImages, refLimit)
	if err != nil {
		return nil, "", err
	}

	var imageURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "krea", active, "image", func(token model.TokenAccount) ([]byte, error) {
		// Refresh the (rotating) Supabase token if expired and persist the new
		// cookie, then generate with the fresh cookie.
		cookie, rerr := kreaRefreshAndPersist(ctx, s.krea, s.tokens, token.ID, token.Value)
		if rerr != nil {
			return nil, rerr
		}
		data, meta, genErr := s.krea.GenerateImage(ctx, cookie, in.Prompt, width, height, refs, !urlOnly)
		if genErr == nil {
			imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
		}
		return data, genErr
	}, func(e error) (bool, bool, bool, bool) {
		return errors.Is(e, krea.ErrAuth), errors.Is(e, krea.ErrQuotaExhausted), errors.Is(e, krea.ErrTemporaryUpstream), false
	}, nil, true)
	return data, imageURL, err
}

// imagineRefreshAndPersist ensures the account's Imagine credential has a valid
// access token (refreshing via the rotating refreshToken when expired) and
// persists the new credential — both tokens rotate, so the value MUST be saved.
func imagineRefreshAndPersist(ctx context.Context, client *imagine.Client, tokens *repo.TokenRepository, tokenID, cred string) (string, error) {
	if client == nil {
		return cred, nil
	}
	fresh, changed, err := client.RefreshIfNeeded(ctx, cred)
	if err != nil {
		return "", err
	}
	if changed && tokenID != "" {
		_, _ = tokens.Update(ctx, "imagine", tokenID, map[string]any{"value": fresh})
	}
	return fresh, nil
}

// imagineStyle maps the catalog model id to its upstream style_id + resolution.
func imagineStyle(modelID string) (int, string) {
	if strings.TrimSpace(modelID) == "imagine-1.5pro" {
		return 41004, "4K"
	}
	return 41001, "2K"
}

func (s *V1Service) generateImagineImage(ctx context.Context, eventID string, modelItem *model.ModelConfig, in V1ImageRequest, aspectRatio, resolution string, noStore bool) ([]byte, string, error) {
	urlOnly := noStore
	if s.imagine == nil {
		return nil, "", errors.New("imagine client not configured")
	}
	items, err := s.tokens.ListByPool(ctx, "imagine")
	if err != nil {
		return nil, "", err
	}
	var active []model.TokenAccount
	for _, item := range items {
		// No numeric floor — Imagine signals 限额 with a 402 at generation time,
		// which the failover driver turns into mark-quota + next account.
		if item.Status == "active" && !item.Dead && strings.TrimSpace(item.Value) != "" {
			active = append(active, item)
		}
	}
	active = pinTestAccount(items, active, in.AccountID)
	if len(active) == 0 {
		return nil, "", ErrNoProviderAccount
	}
	s.rotateRoundRobin("imagine", active)

	// Each model supports exactly one resolution (2K / 4K) — force it per model.
	styleID, res := imagineStyle(modelItem.ID)

	var imageURL string
	data, err := s.runPoolWithFailover(ctx, eventID, "imagine", active, "image", func(token model.TokenAccount) ([]byte, error) {
		// Refresh the (rotating) access token if expired and persist the new
		// credential, then generate with the fresh token.
		cred, rerr := imagineRefreshAndPersist(ctx, s.imagine, s.tokens, token.ID, token.Value)
		if rerr != nil {
			return nil, rerr
		}
		data, meta, genErr := s.imagine.GenerateImage(ctx, cred, styleID, res, aspectRatio, in.Prompt, !urlOnly)
		if genErr != nil {
			return nil, genErr
		}
		imageURL = strings.TrimSpace(stringValue(meta["image_url"]))
		return data, nil
	}, func(e error) (bool, bool, bool, bool) {
		return errors.Is(e, imagine.ErrAuth), errors.Is(e, imagine.ErrQuotaExhausted), errors.Is(e, imagine.ErrTemporaryUpstream), false
	}, nil, true)
	return data, imageURL, err
}

func (s *V1Service) refundIfNeeded(ctx context.Context, principal *APIPrincipal, eventID string, price float64) error {
	if principal == nil || principal.User == nil || price <= 0 {
		return nil
	}
	// Exactly-once: claim the refund via the event's `refunded` flag. If another
	// path (e.g. the abandoned-purge sweep) already refunded, MarkRefunded
	// returns false and we skip — no double refund.
	claimed, err := s.events.MarkRefunded(ctx, eventID)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	updated, err := s.users.AdjustCredits(ctx, principal.User.ID, price)
	if err == nil {
		principal.User = updated
	}
	return err
}

func (s *V1Service) maybeGrantInviteReward(ctx context.Context, principal *APIPrincipal) error {
	if principal == nil || principal.User == nil || s.settings == nil {
		return nil
	}
	enabledRaw, err := s.settings.GetValue(ctx, "credits.invite_enabled")
	if err != nil {
		return err
	}
	if !parseBoolSetting(enabledRaw, true) {
		return nil
	}
	rewardRaw, err := s.settings.GetValue(ctx, "credits.invite_reward")
	if err != nil {
		return err
	}
	_, err = s.users.GrantInviteReward(ctx, principal.User.ID, parseIntSetting(rewardRaw, 3))
	return err
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

func validateVideoReferenceLimits(modelItem *model.ModelConfig, in V1VideoRequest) error {
	imageLimit := max(0, modelItem.MaxReferenceImages)
	videoLimit := max(0, modelItem.MaxReferenceVideos)
	audioLimit := max(0, modelItem.MaxReferenceAudios)
	if len(in.ReferenceImages) > imageLimit {
		return fmt.Errorf("%w: this model accepts at most %d reference images", ErrUnsupportedParams, imageLimit)
	}
	if len(in.ReferenceVideos) > videoLimit {
		return fmt.Errorf("%w: this model accepts at most %d reference videos", ErrUnsupportedParams, videoLimit)
	}
	if len(in.ReferenceAudios) > audioLimit {
		return fmt.Errorf("%w: this model accepts at most %d reference audios", ErrUnsupportedParams, audioLimit)
	}
	if totalLimit := max(0, modelItem.MaxReferenceMedia); totalLimit > 0 {
		total := len(in.ReferenceImages) + len(in.ReferenceVideos) + len(in.ReferenceAudios)
		if total > totalLimit {
			return fmt.Errorf("%w: this model accepts at most %d reference media items in total", ErrUnsupportedParams, totalLimit)
		}
	}
	if in.GenerateAudio && !modelItem.SupportsAudioOutput {
		return fmt.Errorf("%w: this model does not support generated audio", ErrUnsupportedParams)
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
		// Only raw base64 is accepted (no "data:...;base64," URL prefix). A data
		// URL now fails to decode rather than being silently stripped.
		// decoded size ≈ len(b64) * 3 / 4 — reject oversized payloads up front,
		// before allocating the decoded buffer.
		if (len(v)*3)/4 > maxReferenceImageBytes {
			return nil, ErrReferenceTooLarge
		}
		data, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(v)
			if err != nil {
				return nil, errors.New("invalid reference image encoding")
			}
		}
		if len(data) == 0 {
			return nil, errors.New("empty reference image")
		}
		if len(data) > maxReferenceImageBytes {
			return nil, ErrReferenceTooLarge
		}
		out = append(out, data)
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
	ar, resolution := parseImageSize(in.Size, in.AspectRatio, in.Resolution)
	if supportsQualityResolutionModel(item.ID) && strings.TrimSpace(in.Resolution) == "" && strings.TrimSpace(in.Quality) != "" {
		resolution = resolutionForQuality(item, in.Quality)
	}
	return ar, resolution
}

func supportsQualityResolutionModel(modelID string) bool {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "gpt-image-2", "firefly-gpt-image-2":
		return true
	default:
		return false
	}
}

// snapRatio returns the entry in supported closest in value to ar ("W:H").
// ar is returned as-is when it's already supported, unparsable, or the model
// has no ratio list.
func snapRatio(ar string, supported []string) string {
	parse := func(s string) (float64, bool) {
		var w, h int
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &w, &h); err != nil || w <= 0 || h <= 0 {
			return 0, false
		}
		return float64(w) / float64(h), true
	}
	v, ok := parse(ar)
	if !ok || len(supported) == 0 {
		return ar
	}
	best, bestDelta := "", 0.0
	for _, s := range supported {
		if strings.TrimSpace(strings.ReplaceAll(s, "x", ":")) == ar {
			return ar
		}
		sv, sok := parse(strings.ReplaceAll(s, "x", ":"))
		if !sok {
			continue
		}
		if d := absFloat(v - sv); best == "" || d < bestDelta {
			best, bestDelta = strings.TrimSpace(strings.ReplaceAll(s, "x", ":")), d
		}
	}
	if best == "" {
		return ar
	}
	return best
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

// firstPricedResolution returns the model's lowest priced image tier (1K/2K/4K
// order), or "" if none is priced. Used to rescue a request whose resolution
// the model doesn't support.
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

// deaiSurcharge returns the 去AI特征 surcharge (积分) for an image resolution
// tier, from site settings (defaults: 1K=1, 2K=2, 4K=3).
func (s *V1Service) deaiSurcharge(ctx context.Context, resolution string) float64 {
	key, def := "deai.price_1k", 1
	switch strings.ToUpper(strings.TrimSpace(resolution)) {
	case "2K":
		key, def = "deai.price_2k", 2
	case "4K":
		key, def = "deai.price_4k", 3
	}
	if s.settings == nil {
		return float64(def)
	}
	raw, err := s.settings.GetValue(ctx, key)
	if err != nil {
		return float64(def)
	}
	n := parseIntSetting(raw, def)
	if n < 0 {
		n = 0
	}
	return float64(n)
}

func firstPricedResolution(item *model.ModelConfig) string {
	if item == nil {
		return ""
	}
	for _, r := range []string{"1K", "2K", "4K"} {
		if _, ok := jsonMapFloat(item.Prices, r); ok {
			return r
		}
	}
	return ""
}

// resolutionForQuality maps GPT Image 2's `quality` to one of its priced
// resolution tiers: low→1K, medium→2K, high→4K, auto/blank→the model's lowest
// priced tier. Other models must not call this helper.
func resolutionForQuality(item *model.ModelConfig, quality string) string {
	order := []string{"1K", "2K", "4K"}
	var priced []string
	for _, r := range order {
		if _, ok := jsonMapFloat(item.Prices, r); ok {
			priced = append(priced, r)
		}
	}
	if len(priced) == 0 {
		return firstPricedResolution(item)
	}
	rank := map[string]int{"low": 0, "medium": 1, "high": 2}
	want, ok := rank[strings.ToLower(strings.TrimSpace(quality))]
	if !ok {
		return priced[0] // auto / unknown → model default (lowest priced)
	}
	idxOf := func(r string) int {
		for i, v := range order {
			if v == r {
				return i
			}
		}
		return 0
	}
	best, bestDist := priced[0], 99
	for _, r := range priced {
		d := idxOf(r) - want
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			best, bestDist = r, d
		}
	}
	return best
}

// modelPrice returns the charge for (kind, resolution, duration). The set of
// supported tiers is always driven by the NORMAL prices; `agent` only overrides
// the amount with the agent price when one is set for that tier (else it falls
// back to the normal price).
func modelPrice(item *model.ModelConfig, kind, resolution, duration string, agent bool) (float64, bool) {
	if item == nil {
		return 0, false
	}
	// tierPrice: normal price gates support; agent price (if present) overrides.
	tierPrice := func(normal, agentMap map[string]any, key string) (float64, bool) {
		nv, ok := jsonMapFloat(normal, key)
		if !ok {
			return 0, false
		}
		if agent {
			if av, aok := jsonMapFloat(agentMap, key); aok {
				return av, true
			}
		}
		return nv, true
	}
	if kind == "video" {
		rv, rok := tierPrice(item.Prices, item.PricesAgent, resolution)
		dv, dok := tierPrice(item.DurationPrices, item.DurationPricesAgent, duration)
		if !rok || !dok {
			return 0, false
		}
		return rv + dv, true
	}
	if kind == "text" {
		return tierPrice(item.Prices, item.PricesAgent, "request")
	}
	return tierPrice(item.Prices, item.PricesAgent, resolution)
}

func jsonMapFloat(m map[string]any, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
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
	if principal == nil || principal.User == nil {
		return 0
	}
	return principal.User.Credits
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
		// the cookie. chatgpt/runway/leonardo auth means the stored credential is
		// dead — a raw JWT (chatgpt/runway) or a cookie whose session no longer
		// authenticates (leonardo) — there's nothing left to refresh from.
		// grok is intentionally excluded: a grok sso can momentarily 401 while
		// still valid (upstream blip / proxy / anti-bot), so an auth failure just
		// fails over for this request without permanently killing the account.
		if pool == "chatgpt" || pool == "runway" || pool == "leonardo" || pool == "krea" || pool == "imagine" {
			patch["status"] = "disabled"
			patch["dead"] = true
		}
	default:
		// Neither pool is auto-disabled on generic (non-auth / non-quota) failures
		// — the account usually still works, so it stays active (green). fails is
		// only tracked for rotation ordering. (A chatgpt *auth* failure still marks
		// the token dead in the isAuth case above; that is a genuinely dead token.)
	}
	_, _ = s.tokens.Update(ctx, pool, token.ID, patch)
}

// markTokenUpstreamFailure records a failure caused by the provider itself
// (overload / 5xx / circuit open) WITHOUT touching the account's own health
// counters: during an upstream outage every account fails identically, so
// charging it to fails/fail_total makes a perfectly healthy pool look dead in
// the admin UI (and drowns real per-account problems in noise).
func (s *V1Service) markTokenUpstreamFailure(ctx context.Context, pool string, token model.TokenAccount) {
	_, _ = s.tokens.Update(ctx, pool, token.ID, map[string]any{
		"last_used_at":   time.Now(),
		"upstream_fails": gorm.Expr("upstream_fails + 1"),
	})
}

// markTokenDead disables an account and marks it dead on a fatal upstream error
// (a non-overload temporary Adobe failure that ops policy treats as account death).
func (s *V1Service) markTokenDead(ctx context.Context, pool string, token model.TokenAccount, kind string) {
	_, _ = s.tokens.Update(ctx, pool, token.ID, map[string]any{
		"last_used_at": time.Now(),
		"fail_total":   gorm.Expr("fail_total + 1"),
		"fails":        gorm.Expr("fails + 1"),
		"status":       "disabled",
		"dead":         true,
	})
}

// nextCursor returns the pool's current round-robin position and advances it by
// one. Concurrent callers each get a distinct value, so parallel picks land on
// different accounts instead of racing onto the same one.
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

// Ordinary 10-credit Adobe accounts can execute partner image models (verified
// against GPT Image 2, Nano Banana 2, and Flux Kontext Max). Video entitlement
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
