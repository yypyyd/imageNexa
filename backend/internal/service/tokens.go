package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var validTokenPools = map[string]string{
	"chatgpt":  "openai",
	"adobe":    "adobe",
	"byteplus": "byteplus",
	"runway":   "runway",
	"grok":     "grok",
	"oreate":   "oreate",
	"dola":     "dola",
	"custom":   "custom",
}

// ErrNotFound is retained as the control-plane service sentinel after the
// legacy public-site service that originally declared it was removed.
var ErrNotFound = gorm.ErrRecordNotFound

const (
	oreateCredentialGenerationMetaKey    = "oreate_credential_generation"
	dolaCredentialGenerationMetaKey      = "dola_credential_generation"
	bytePlusCredentialFingerprintMetaKey = "byteplus_credential_fingerprint"
	bytePlusIdentityFingerprintMetaKey   = "byteplus_identity_fingerprint"
	bytePlusProbeVersionMetaKey          = "byteplus_probe_version"
	bytePlusSessionExpiresAtMetaKey      = "byteplus_session_expires_at"
)

// bytePlusAccountClient is the narrow provider surface needed by account
// imports. Keeping the probe behind this interface lets the import lifecycle be
// exercised with local fakes without ever contacting BytePlus.
type bytePlusAccountClient interface {
	FetchProfile(context.Context, string) (map[string]any, error)
	FetchCreditsBalance(context.Context, string) (map[string]any, error)
}

// bytePlusPendingStore owns the credential-generation compare-and-set used by
// asynchronous import probes. The production implementation below performs the
// comparison inside the SQL UPDATE, so a worker for an older import can never
// overwrite a newer credential generation.
type bytePlusPendingStore interface {
	Create(context.Context, *model.TokenAccount) error
	Get(context.Context, string) (*model.TokenAccount, error)
	RefreshCredential(context.Context, string, string, string, string) (*model.TokenAccount, error)
	CompleteProbe(context.Context, string, string, string, string, bytePlusProbeResult) (bool, error)
}

type bytePlusProbeResult struct {
	Status      string
	Dead        bool
	Email       string
	DisplayName string
	QuotaMeta   map[string]any
}

type repoBytePlusPendingStore struct {
	tokens *repo.TokenRepository
}

func (s repoBytePlusPendingStore) Create(ctx context.Context, item *model.TokenAccount) error {
	return s.tokens.Create(ctx, item)
}

func (s repoBytePlusPendingStore) Get(ctx context.Context, id string) (*model.TokenAccount, error) {
	return s.tokens.Get(ctx, "byteplus", id)
}

func (s repoBytePlusPendingStore) RefreshCredential(ctx context.Context, id, cookie, fingerprint, version string) (*model.TokenAccount, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	metaPatch := map[string]any{
		"pending_check":                      true,
		bytePlusCredentialFingerprintMetaKey: fingerprint,
		bytePlusIdentityFingerprintMetaKey:   bytePlusIdentityFingerprint(cookie),
		bytePlusProbeVersionMetaKey:          version,
		bytePlusSessionExpiresAtMetaKey:      bytePlusSessionExpiryText(cookie),
	}
	patch := map[string]any{
		"value":  cookie,
		"status": "pending",
		"dead":   false,
		"fails":  0,
	}
	// This branch is only expected for a cryptographic ID collision or a legacy
	// row. Do not carry a different account's discovered identity across it.
	if stored := strings.TrimSpace(stringValue(item.Meta[bytePlusCredentialFingerprintMetaKey])); stored != "" && stored != fingerprint {
		patch["account_email"] = ""
		patch["account_display_name"] = ""
	}
	if err := s.tokens.UpdateMergingMeta(ctx, "byteplus", id, metaPatch, patch); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s repoBytePlusPendingStore) CompleteProbe(ctx context.Context, id, cookie, fingerprint, version string, result bytePlusProbeResult) (bool, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if !bytePlusProbeStillCurrent(current, cookie, fingerprint, version) {
		return false, nil
	}

	condition := "value = ? AND meta ->> '" + bytePlusCredentialFingerprintMetaKey + "' = ? AND meta ->> '" + bytePlusProbeVersionMetaKey + "' = ?"
	args := []any{cookie, fingerprint, version}
	conditionalValue := func(column string, value any) clause.Expr {
		exprArgs := append(append([]any{}, args...), value)
		return gorm.Expr("CASE WHEN "+condition+" THEN ? ELSE "+column+" END", exprArgs...)
	}

	metaPatch := cloneJSONMap(datatypes.JSONMap(result.QuotaMeta))
	metaPatch["pending_check"] = false
	encodedMeta, err := json.Marshal(metaPatch)
	if err != nil {
		return false, err
	}
	metaArgs := append(append([]any{}, args...), string(encodedMeta))
	patch := map[string]any{
		"status": conditionalValue("status", result.Status),
		"dead":   conditionalValue("dead", result.Dead),
		"meta": gorm.Expr(
			"CASE WHEN "+condition+" THEN COALESCE(meta, '{}'::jsonb) || CAST(? AS jsonb) ELSE meta END",
			metaArgs...,
		),
	}
	if result.Email != "" {
		patch["account_email"] = conditionalValue("account_email", result.Email)
	}
	if result.DisplayName != "" {
		patch["account_display_name"] = conditionalValue("account_display_name", result.DisplayName)
	}
	item, err := s.tokens.Update(ctx, "byteplus", id, patch)
	if err != nil {
		return false, err
	}
	return bytePlusProbeMatches(item.Meta, fingerprint, version) && !boolValueWithDefault(item.Meta["pending_check"], false), nil
}

type TokenService struct {
	quotas   quotaSnapshotWriter
	tokens   *repo.TokenRepository
	refresh  *repo.RefreshProfileRepository
	events   *repo.EventRepository
	settings *repo.SiteSettingRepository
	adobe    *adobe.Client
	byteplus bytePlusAccountClient
	// byteplusPending is separate from the general token repository because its
	// asynchronous probe completion requires a credential-generation CAS.
	byteplusPending bytePlusPendingStore
	chatgpt         *chatgpt.Client
	runway          *runway.Client
	grok            *grok.Client
	oreate          *oreate.Client
	dola            *dola.Client
	custom          *custom.Client
	// sem caps concurrent background pending-probe goroutines (mirrors Python's
	// 10-worker _quota_check_pool) so a big paste doesn't fire hundreds of
	// simultaneous upstream requests.
	sem chan struct{}
	// Separate guards keep quota maintenance and no-cost session keepalive from
	// overlapping with themselves while allowing either job to make progress.
	oreateRefreshing        atomic.Bool
	oreateSessionRefreshing atomic.Bool
	dolaVerifying           atomic.Bool
}

func NewTokenService(tokens *repo.TokenRepository, refresh *repo.RefreshProfileRepository, events *repo.EventRepository, settings *repo.SiteSettingRepository, adobeClient *adobe.Client, bytePlusClient *byteplus.Client, chatGPTClient *chatgpt.Client, runwayClient *runway.Client, grokClient *grok.Client, oreateClient *oreate.Client, dolaClient *dola.Client, customClient *custom.Client, quotas *repo.QuotaRepository) *TokenService {
	service := &TokenService{
		tokens:   tokens,
		refresh:  refresh,
		events:   events,
		settings: settings,
		adobe:    adobeClient,
		byteplusPending: repoBytePlusPendingStore{
			tokens: tokens,
		},
		chatgpt: chatGPTClient,
		runway:  runwayClient,
		grok:    grokClient,
		oreate:  oreateClient,
		dola:    dolaClient,
		custom:  customClient,
		sem:     make(chan struct{}, 10),
	}
	// Avoid storing a typed nil pointer in the interface: pending imports should
	// take the explicit no-client path rather than attempting a network probe.
	if quotas != nil {
		service.quotas = quotas
	}
	if bytePlusClient != nil {
		service.byteplus = bytePlusClient
	}
	if oreateClient != nil {
		oreateClient.SetSessionRotationCallback(service.persistOreateBrowserSession)
	}
	return service
}

// applyProxy snapshots each channel's residential route onto its client.
func (s *TokenService) applyProxy(ctx context.Context) {
	snap, err := loadProviderProxies(ctx, s.settings)
	if err != nil {
		// Preserve the last applied route on a transient settings-store error.
		return
	}
	var byteplusClient *byteplus.Client
	if client, ok := s.byteplus.(*byteplus.Client); ok {
		byteplusClient = client
	}
	assignProviderProxies(snap, s.chatgpt, s.grok, s.oreate, s.dola, s.adobe, byteplusClient)
}

func (s *TokenService) List(ctx context.Context) (map[string][]ginToken, error) {
	items, err := s.tokens.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]ginToken{}
	for _, item := range items {
		out[item.Pool] = append(out[item.Pool], ginToken{
			ID:           item.ID,
			ValuePreview: previewSecret(item.Value),
			Status:       item.Status,
			Fails:        item.Fails,
			AddedAt:      item.AddedAt,
		})
	}
	return out, nil
}

func (s *TokenService) Add(ctx context.Context, pool, value, tokenID string) (*model.TokenAccount, error) {
	pool = normalizePool(pool)
	if pool == "" {
		return nil, errors.New("unknown pool")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("pool and value required")
	}
	if tokenID == "" {
		tokenID = newTokenID(pool)
	}
	return s.createToken(ctx, pool, tokenID, value, "active", nil)
}

func (s *TokenService) ImportChatGPTToken(ctx context.Context, accessToken, tokenID string) (*model.TokenAccount, error) {
	s.applyProxy(ctx)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, errors.New("access_token required")
	}
	if grok.IsGrokToken(accessToken) || grok.IsGrokOAuthToken(accessToken) {
		return nil, errors.New("this token is grok, not chatgpt")
	}
	// Land as pending and return instantly; a background worker probes quota and
	// flips the row active/dead (Python import_chatgpt_token). pending tokens are
	// not schedulable — the pool only hands out status=="active".
	meta := datatypes.JSONMap{"pending_check": true}
	info := chatgpt.ExtractAccountInfo(accessToken)
	_, exp := parseJWTEmailExpiry(accessToken)
	// Identity is (pool, email): reuse the existing row for this email, else mint a
	// fresh id — never trust the caller's id (it can collide with an unrelated row
	// → a spurious 23505/400 for a brand-new account).
	email := strings.TrimSpace(stringValue(info["email"]))
	if existing, _ := s.tokens.GetByPoolEmail(ctx, "chatgpt", email); existing != nil {
		tokenID = existing.ID
	} else if email != "" || tokenID == "" {
		tokenID = newTokenID("chatgpt")
	}
	item, err := s.createToken(ctx, "chatgpt", tokenID, accessToken, "pending", meta)
	if err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			if item, err = s.tokens.Update(ctx, "chatgpt", tokenID, map[string]any{
				"value": accessToken, "status": "pending", "dead": false, "fails": 0, "meta": meta,
			}); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	// JWT-derived fields are free (no network) — hydrate them up front. OpenAI
	// tokens carry the email under the nested "https://api.openai.com/profile"
	// claim, so read it from ExtractAccountInfo — parseJWTEmailExpiry only sees
	// top-level claims and returns "" for ChatGPT tokens.
	patch := map[string]any{}
	if email := strings.TrimSpace(stringValue(info["email"])); email != "" {
		patch["account_email"] = email
	}
	if display := strings.TrimSpace(stringValue(info["plan_type"])); display != "" {
		patch["account_display_name"] = display
	}
	if exp != nil {
		patch["cached_quota_reset_after"] = exp.Format(time.RFC3339)
	}
	if len(patch) > 0 {
		if updated, uerr := s.tokens.Update(ctx, "chatgpt", tokenID, patch); uerr == nil {
			item = updated
		}
	}
	go s.checkPendingChatGPT(tokenID, accessToken)
	return item, nil
}

// ImportRunwayToken lands a Runway JWT as a pending account and probes its
// credit balance off-thread (mirrors ImportChatGPTToken). The workspace/team id
// (= the JWT "id" claim) is stashed in meta["team_id"] so generation can send it
// as x-runway-workspace later. Recovery time == the JWT expiry.
func (s *TokenService) ImportRunwayToken(ctx context.Context, accessToken, tokenID string) (*model.TokenAccount, error) {
	s.applyProxy(ctx)
	accessToken = strings.TrimSpace(strings.TrimPrefix(accessToken, "Bearer "))
	if accessToken == "" {
		return nil, errors.New("access_token required")
	}
	if !runway.IsRunwayToken(accessToken) {
		return nil, errors.New("not a runway token")
	}
	teamID := runway.TeamIDFromToken(accessToken)
	// JWT-derived fields are free (no network); email + exp are top-level claims.
	email, exp := parseJWTEmailExpiry(accessToken)
	// Identity is (pool, email): reuse the existing row for this email, else mint a
	// fresh id — never trust the caller's id (it can collide → spurious 400).
	if existing, _ := s.tokens.GetByPoolEmail(ctx, "runway", email); existing != nil {
		tokenID = existing.ID
	} else if email != "" || tokenID == "" {
		tokenID = newTokenID("runway")
	}
	meta := datatypes.JSONMap{"pending_check": true}
	if teamID != "" {
		meta["team_id"] = teamID
	}
	item, err := s.createToken(ctx, "runway", tokenID, accessToken, "pending", meta)
	if err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			if item, err = s.tokens.Update(ctx, "runway", tokenID, map[string]any{
				"value": accessToken, "status": "pending", "meta": meta,
			}); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	patch := map[string]any{}
	if email != "" {
		patch["account_email"] = email
	}
	if exp != nil {
		patch["cached_quota_reset_after"] = exp.Format(time.RFC3339)
	}
	if len(patch) > 0 {
		if updated, uerr := s.tokens.Update(ctx, "runway", tokenID, patch); uerr == nil {
			item = updated
		}
	}
	go s.checkPendingRunway(tokenID, accessToken)
	return item, nil
}

// ImportBytePlusCookie imports the complete browser Cookie header used by
// BytePlus Lumina. The CSRF value is required in both the Cookie header and
// X-Csrf-Token; storing only csrfToken is therefore insufficient.
func (s *TokenService) ImportBytePlusCookie(ctx context.Context, cookie string) (*model.TokenAccount, error) {
	cookie = normalizeBytePlusCookie(cookie)
	if cookie == "" {
		return nil, errors.New("cookie required")
	}
	if !byteplus.IsBytePlusCookie(cookie) {
		return nil, errors.New("not a byteplus lumina cookie")
	}
	if s.byteplusPending == nil {
		return nil, errors.New("byteplus token repository unavailable")
	}
	fingerprint := bytePlusCredentialFingerprint(cookie)
	tokenID := bytePlusTokenIDFromFingerprint(fingerprint)
	identityFingerprint := bytePlusIdentityFingerprint(cookie)
	if identityFingerprint != "" {
		tokenID = bytePlusTokenIDFromFingerprint(identityFingerprint)
		// Accounts imported before stable identity support use a hash of the old
		// session Cookie as their row id. Reuse that row when AccountID matches so
		// a 48-hour login rotation replaces the credential instead of creating a
		// duplicate account every two days.
		if s.tokens != nil {
			existing, listErr := s.tokens.ListByPool(ctx, "byteplus")
			if listErr != nil {
				return nil, listErr
			}
			tokenID = bytePlusExistingTokenID(existing, identityFingerprint, tokenID)
		}
	}
	version := randomUpper(20)
	meta := datatypes.JSONMap{
		"pending_check":                      true,
		bytePlusCredentialFingerprintMetaKey: fingerprint,
		bytePlusIdentityFingerprintMetaKey:   identityFingerprint,
		bytePlusProbeVersionMetaKey:          version,
		bytePlusSessionExpiresAtMetaKey:      bytePlusSessionExpiryText(cookie),
	}
	now := time.Now()
	item := &model.TokenAccount{
		ID: tokenID, Pool: "byteplus", Value: cookie, Status: "pending", Meta: meta,
		AddedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	err := s.byteplusPending.Create(ctx, item)
	if err != nil {
		if !errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, err
		}
		// Re-importing the same complete Cookie is idempotent at the account level:
		// it refreshes the existing row and advances only the probe generation. The
		// merge preserves quota fields written by a worker finishing concurrently.
		item, err = s.byteplusPending.RefreshCredential(ctx, tokenID, cookie, fingerprint, version)
		if err != nil {
			return nil, err
		}
	}
	go s.checkPendingBytePlus(tokenID, cookie, fingerprint, version)
	return item, nil
}

// ConfigureBytePlusLogin attaches password-login renewal material to an
// already-imported account. The secret stays in refresh_profiles and is never
// returned by the control plane.
func (s *TokenService) ConfigureBytePlusLogin(ctx context.Context, accountID, identity, secret, cookie string) error {
	identity, secret = strings.TrimSpace(identity), strings.TrimSpace(secret)
	if identity == "" || secret == "" {
		return nil
	}
	if s.refresh == nil {
		return errors.New("refresh profile repository unavailable")
	}
	next := time.Now()
	if expiry := bytePlusSessionExpiry(cookie); !expiry.IsZero() {
		next = expiry.Add(-6 * time.Hour)
	}
	return s.refresh.UpsertBytePlusLogin(ctx, accountID, identity, secret, next)
}

// RotateBytePlusCookie replaces one existing account's session after a local
// password login. Stable AccountID matching prevents a bad credential mapping
// from overwriting a different account.
func (s *TokenService) RotateBytePlusCookie(ctx context.Context, accountID, cookie string) (*model.TokenAccount, error) {
	cookie = normalizeBytePlusCookie(cookie)
	if cookie == "" || !byteplus.IsBytePlusCookie(cookie) {
		return nil, errors.New("not a byteplus lumina cookie")
	}
	if s.tokens == nil || s.byteplusPending == nil {
		return nil, errors.New("byteplus token repository unavailable")
	}
	current, err := s.tokens.Get(ctx, "byteplus", accountID)
	if err != nil {
		return nil, err
	}
	expected := strings.TrimSpace(stringValue(current.Meta[bytePlusIdentityFingerprintMetaKey]))
	if expected == "" {
		expected = bytePlusIdentityFingerprint(current.Value)
	}
	if actual := bytePlusIdentityFingerprint(cookie); expected == "" || actual == "" || actual != expected {
		return nil, errors.New("byteplus login returned a different AccountID")
	}
	fingerprint := bytePlusCredentialFingerprint(cookie)
	version := randomUpper(20)
	item, err := s.byteplusPending.RefreshCredential(ctx, accountID, cookie, fingerprint, version)
	if err != nil {
		return nil, err
	}
	go s.checkPendingBytePlus(accountID, cookie, fingerprint, version)
	return item, nil
}

func (s *TokenService) checkPendingBytePlus(tokenID, cookie, fingerprint, version string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("token import: byteplus pending check panicked for %s", tokenID)
		}
	}()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if s.byteplus == nil {
		s.completePendingBytePlus(ctx, tokenID, cookie, fingerprint, version, bytePlusProbeResult{Status: "active"})
		return
	}
	profile, err := s.byteplus.FetchProfile(ctx, cookie)
	if err != nil {
		if errors.Is(err, byteplus.ErrAuth) {
			s.completePendingBytePlus(ctx, tokenID, cookie, fingerprint, version, bytePlusProbeResult{Status: "disabled", Dead: true})
			return
		}
		// A transient profile failure is not proof that the durable browser session
		// is invalid; the generation path remains the final authority.
		s.completePendingBytePlus(ctx, tokenID, cookie, fingerprint, version, bytePlusProbeResult{Status: "active"})
		return
	}
	result := bytePlusProbeResult{
		Status:      "active",
		Email:       firstString(profile, "email", "user_email"),
		DisplayName: firstString(profile, "display_name", "user_name", "name"),
		QuotaMeta:   map[string]any{},
	}
	if balance, balanceErr := s.byteplus.FetchCreditsBalance(ctx, cookie); balanceErr == nil {
		for source, target := range map[string]string{
			"remaining": "cached_quota_remaining",
			"used":      "cached_quota_used",
			"total":     "cached_quota_total",
		} {
			if value, ok := anyFloat(balance[source]); ok {
				result.QuotaMeta[target] = canonicalQuotaNumber(value)
			}
		}
		result.QuotaMeta["cached_quota_at"] = int(time.Now().Unix())
	} else if errors.Is(balanceErr, byteplus.ErrAuth) {
		result.Status = "disabled"
		result.Dead = true
		s.completePendingBytePlus(ctx, tokenID, cookie, fingerprint, version, result)
		return
	}
	s.completePendingBytePlus(ctx, tokenID, cookie, fingerprint, version, result)
}

func (s *TokenService) completePendingBytePlus(ctx context.Context, tokenID, cookie, fingerprint, version string, result bytePlusProbeResult) {
	if s.byteplusPending == nil {
		return
	}
	_, _ = s.byteplusPending.CompleteProbe(ctx, tokenID, cookie, fingerprint, version, result)
}

func normalizeBytePlusCookie(cookie string) string {
	cookie = cleanAdobeCookie(cookie)
	parts := strings.Split(cookie, ";")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			cleaned = append(cleaned, part)
		}
	}
	return strings.Join(cleaned, "; ")
}

func bytePlusCredentialFingerprint(cookie string) string {
	digest := sha256.Sum256([]byte(normalizeBytePlusCookie(cookie)))
	return fmt.Sprintf("%x", digest[:])
}

// bytePlusIdentityFingerprint is stable across 48-hour login rotations. The
// AccountID cookie is an opaque account identifier rather than a bearer token;
// only its SHA-256 digest is persisted in metadata or used for row identity.
func bytePlusIdentityFingerprint(cookie string) string {
	accountID := bytePlusCookieValue(cookie, "AccountID")
	if accountID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("byteplus-account\x00" + accountID))
	return fmt.Sprintf("%x", digest[:])
}

func bytePlusExistingTokenID(items []model.TokenAccount, identityFingerprint, fallback string) string {
	for _, item := range items {
		if item.Pool != "byteplus" {
			continue
		}
		stored := strings.TrimSpace(stringValue(item.Meta[bytePlusIdentityFingerprintMetaKey]))
		if stored == "" {
			stored = bytePlusIdentityFingerprint(item.Value)
		}
		if stored == identityFingerprint {
			return item.ID
		}
	}
	return fallback
}

func bytePlusCookieValue(cookie, wanted string) string {
	for _, part := range strings.Split(normalizeBytePlusCookie(cookie), ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), wanted) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// bytePlusSessionExpiry reads the unverified exp claim from Lumina's digest
// cookie. It is used only as a local scheduling deadline, never as proof of
// identity or authorization.
func bytePlusSessionExpiry(cookie string) time.Time {
	token := bytePlusCookieValue(cookie, "digest")
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0).UTC()
}

func bytePlusSessionExpiryText(cookie string) string {
	if expiry := bytePlusSessionExpiry(cookie); !expiry.IsZero() {
		return expiry.Format(time.RFC3339)
	}
	return ""
}

func bytePlusAccountSessionExpiry(item model.TokenAccount) time.Time {
	if item.Pool != "byteplus" {
		return time.Time{}
	}
	if raw := strings.TrimSpace(stringValue(item.Meta[bytePlusSessionExpiresAtMetaKey])); raw != "" {
		if expiry, err := time.Parse(time.RFC3339, raw); err == nil {
			return expiry.UTC()
		}
	}
	return bytePlusSessionExpiry(item.Value)
}

// Unknown legacy sessions remain eligible and are still validated by the
// provider. A known-expired digest is never scheduled for ordinary traffic.
func bytePlusAccountSessionUsable(item model.TokenAccount, now time.Time) bool {
	expiry := bytePlusAccountSessionExpiry(item)
	return expiry.IsZero() || expiry.After(now)
}

func bytePlusTokenIDFromFingerprint(fingerprint string) string {
	// Forty hex characters retain 160 bits of the complete credential digest,
	// comfortably within TokenAccount.ID's 64-character database limit.
	const idFingerprintLength = 40
	fingerprint = strings.ToUpper(strings.TrimSpace(fingerprint))
	if len(fingerprint) > idFingerprintLength {
		fingerprint = fingerprint[:idFingerprintLength]
	}
	return "BP" + fingerprint
}

func bytePlusProbeMatches(meta datatypes.JSONMap, fingerprint, version string) bool {
	return strings.TrimSpace(stringValue(meta[bytePlusCredentialFingerprintMetaKey])) == fingerprint &&
		strings.TrimSpace(stringValue(meta[bytePlusProbeVersionMetaKey])) == version
}

func bytePlusProbeStillCurrent(item *model.TokenAccount, cookie, fingerprint, version string) bool {
	return item != nil && item.Value == cookie && bytePlusProbeMatches(item.Meta, fingerprint, version)
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringValue(values[key])); value != "" {
			return value
		}
	}
	return ""
}

func (s *TokenService) ImportAdobeCookie(ctx context.Context, cookie, arpSessionToken, tokenID string) (*model.TokenAccount, *model.RefreshProfile, error) {
	s.applyProxy(ctx)
	cookie = cleanAdobeCookie(cookie)
	arpSessionToken = strings.TrimSpace(arpSessionToken)
	if cookie == "" {
		return nil, nil, errors.New("cookie required")
	}
	if tokenID == "" {
		tokenID = newTokenID("adobe")
	}
	now := time.Now()
	// Register the cookie refresh profile up front. Push next_retry_at out a full
	// interval so the maintenance loop doesn't race the import worker on the first
	// exchange — the worker below owns the initial hydrate.
	nextRetry := now.Add(54000 * time.Second)
	profile := &model.RefreshProfile{
		ID:              tokenID,
		Name:            tokenID,
		Pool:            "adobe",
		Kind:            "adobe_cookie",
		Cookie:          cookie,
		ARPSessionToken: arpSessionToken,
		Enabled:         true,
		IntervalSeconds: 54000,
		ImportedAt:      &now,
		NextRetryAt:     &nextRetry,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.refresh.Create(ctx, profile); err != nil {
		if !errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, nil, err
		}
		profilePatch := map[string]any{
			"cookie":        cookie,
			"enabled":       true,
			"imported_at":   now,
			"next_retry_at": nextRetry,
		}
		// A plain-Cookie re-import must not rotate an existing browser-compatible
		// ARP session or erase a richer token captured from a structured export.
		if arpSessionToken != "" {
			profilePatch["arp_session_token"] = arpSessionToken
		}
		profile, err = s.refresh.Update(ctx, tokenID, profilePatch)
		if err != nil {
			return nil, nil, err
		}
	}
	// Adobe's SherlockSdk emits this base sid token immediately, before its
	// optional Forter/BFP vendors complete. Generate the same local session shape
	// when the import did not provide one; this performs no network request and
	// preserves an existing stored value on plain-Cookie re-imports.
	arpSessionToken = strings.TrimSpace(profile.ARPSessionToken)
	if arpSessionToken == "" {
		arpSessionToken = adobe.NewARPSessionToken()
		updatedProfile, updateErr := s.refresh.Update(ctx, tokenID, map[string]any{"arp_session_token": arpSessionToken})
		if updateErr != nil {
			return nil, nil, updateErr
		}
		profile = updatedProfile
	}
	// Land a placeholder pending token (value filled in by the worker). NOT
	// schedulable — the pool only hands out status=="active". The import returns
	// instantly; the row flips active/dead once the worker finishes the three
	// Adobe round-trips. Mirrors Python import_adobe_cookie.
	meta := datatypes.JSONMap{"pending_check": true}
	item, err := s.createToken(ctx, "adobe", tokenID, "", "pending", meta)
	if err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			accountPatch := map[string]any{
				"status":            "pending",
				"meta":              meta,
				"arp_session_token": arpSessionToken,
			}
			item, err = s.tokens.Update(ctx, "adobe", tokenID, accountPatch)
			if err != nil {
				return nil, nil, err
			}
		} else {
			return nil, nil, err
		}
	}
	if arpSessionToken != "" && item.ARPSessionToken != arpSessionToken {
		item, err = s.tokens.Update(ctx, "adobe", tokenID, map[string]any{"arp_session_token": arpSessionToken})
		if err != nil {
			return nil, nil, err
		}
	}
	go s.checkPendingAdobe(tokenID, cookie)
	return item, profile, nil
}

// checkPendingAdobe runs the three Adobe round-trips off-thread for a freshly
// imported cookie so the import request returns instantly (Python
// _check_pending_adobe). Step 1 (exchange) is authoritative — a bad/expired
// cookie can't mint a token, so failure marks the row dead. Steps 2-3 (credits /
// profile) are best-effort hydration.
func (s *TokenService) checkPendingAdobe(tokenID, cookie string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("token import: adobe pending check panicked for %s", tokenID)
		}
	}()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s.applyProxy(ctx)

	if s.adobe == nil {
		s.finishPending(ctx, "adobe", tokenID, "disabled", true, nil)
		return
	}
	result, err := s.adobe.ExchangeCookie(ctx, cookie)
	if err != nil {
		s.finishPending(ctx, "adobe", tokenID, "disabled", true, nil)
		_, _ = s.refresh.Update(ctx, tokenID, map[string]any{
			"last_attempt_at":      time.Now(),
			"last_error":           safeGenerationErrorText(err),
			"consecutive_failures": 1,
		})
		return
	}
	// Seed the real access token, then activate so the pool can schedule it.
	seed := map[string]any{"value": result.AccessToken}
	email, exp := parseJWTEmailExpiry(result.AccessToken)
	if email != "" {
		seed["account_email"] = email
	}
	if exp != nil {
		seed["cached_quota_reset_after"] = exp.Format(time.RFC3339)
	}
	_, _ = s.tokens.Update(ctx, "adobe", tokenID, seed)

	quotaMeta := map[string]any{}
	if cb, e := s.adobe.FetchCreditsBalance(ctx, result.AccessToken); e == nil {
		if ra := strings.TrimSpace(stringValue(cb["available_until"])); ra != "" {
			_, _ = s.tokens.Update(ctx, "adobe", tokenID, map[string]any{"cached_quota_reset_after": ra})
		}
		quotaMeta["cached_quota_at"] = int(time.Now().Unix())
		if rem, ok := cb["remaining"].(int); ok {
			quotaMeta["cached_quota_remaining"] = rem
		}
	}
	if prof, e := s.adobe.FetchAccountProfile(ctx, result.AccessToken); e == nil {
		p := map[string]any{}
		if em := strings.TrimSpace(stringValue(prof["email"])); em != "" {
			p["account_email"] = em
		}
		if dn := strings.TrimSpace(stringValue(prof["display_name"])); dn != "" {
			p["account_display_name"] = dn
		}
		if len(p) > 0 {
			_, _ = s.tokens.Update(ctx, "adobe", tokenID, p)
		}
	}

	s.finishPending(ctx, "adobe", tokenID, "active", false, quotaMeta)
	_, _ = s.refresh.Update(ctx, tokenID, map[string]any{
		"last_attempt_at":      time.Now(),
		"last_success_at":      time.Now(),
		"last_error":           "",
		"consecutive_failures": 0,
		"next_retry_at":        time.Now().Add(54000 * time.Second),
	})
}

// checkPendingChatGPT probes a freshly imported ChatGPT token's quota off-thread
// (Python _check_pending_chatgpt). 401 → dead; a non-auth error gets the benefit
// of the doubt and activates so a transient blip can't sideline a good account.
func (s *TokenService) checkPendingChatGPT(tokenID, accessToken string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("token import: chatgpt pending check panicked for %s", tokenID)
		}
	}()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s.applyProxy(ctx)
	s.probeAndFinishChatGPT(ctx, tokenID, accessToken)
}

// probeAndFinishChatGPT probes a ChatGPT token's image_gen quota and writes the
// terminal status. Shared by the import worker and the stale-pending reaper.
//
// Crucially it distinguishes a definitive "remaining==0" from an *unknown* read
// (Cloudflare 403 / 429 / timeout — common when a big batch fires many probes
// from one IP at once): only a definitive 0 sinks the account to 限额, while an
// unknown read gets the benefit of the doubt and activates. Treating unknown as
// exhausted was sidelining perfectly good freshly-registered accounts.
func (s *TokenService) probeAndFinishChatGPT(ctx context.Context, tokenID, accessToken string) {
	if s.chatgpt == nil {
		s.finishPending(ctx, "chatgpt", tokenID, "active", false, nil)
		return
	}
	// Retry on unknown/transient reads with backoff so a rate-limited blip during
	// a bulk import doesn't misjudge quota.
	var data map[string]any
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		// This diagnostic read follows the same global egress as generation. A
		// challenge/401 is still not treated as definitive proof that the JWT died.
		data, err = s.chatgpt.FetchImageQuotaDirect(ctx, accessToken)
		if err != nil {
			break
		}
		if boolValueWithDefault(data["auth_failed"], false) {
			break
		}
		if !boolValueWithDefault(data["unknown"], false) {
			break
		}
		select {
		case <-ctx.Done():
			s.finishPending(ctx, "chatgpt", tokenID, "active", false, nil)
			return
		case <-time.After(time.Duration(2*(attempt+1)) * time.Second):
		}
	}
	if err != nil {
		// network/proxy error — benefit of the doubt, activate.
		s.finishPending(ctx, "chatgpt", tokenID, "active", false, nil)
		return
	}
	if boolValueWithDefault(data["auth_failed"], false) {
		// A quota probe is not a liveness proof. OpenAI can return 401/403 here
		// when the shared egress is challenged or region-bound, while the JWT is
		// still usable for the actual Web conversation. Keep the account active;
		// generation-time auth handling is the authority for permanently dead JWTs.
		s.finishPending(ctx, "chatgpt", tokenID, "active", false, map[string]any{
			"quota_probe_unknown": true,
			"quota_probe_error":   safeQuotaProbeError(data),
		})
		return
	}
	// Still unknown after retries — don't trust it as 0; activate so the account
	// isn't stranded. A real per-generation probe will re-judge quota on use.
	if boolValueWithDefault(data["unknown"], false) {
		s.finishPending(ctx, "chatgpt", tokenID, "active", false, nil)
		return
	}
	rem, exhausted := chatgptRemaining(data)
	quotaMeta := map[string]any{
		"cached_quota_remaining": rem,
		"cached_quota_at":        int(time.Now().Unix()),
	}
	// reset 时间:优先用 OpenAI 的 reset_after,缺失则默认次日重置,保证限额号能被
	// RecoverQuota 到点自动复活、重新探测,而不会永久搁置。
	reset := strings.TrimSpace(stringValue(data["reset_after"]))
	if reset == "" {
		reset = nextUTCResetAfter()
	}
	_, _ = s.tokens.Update(ctx, "chatgpt", tokenID, map[string]any{"cached_quota_reset_after": reset})
	// remaining<=0(0 / 负数)→ 置「限额」,池子不再调度,到点自动恢复。
	status := "active"
	if exhausted {
		status = "quota"
	}
	s.finishPending(ctx, "chatgpt", tokenID, status, false, quotaMeta)
}

// RecheckPendingChatGPT re-probes a ChatGPT account that is still stuck in
// pending (e.g. an import probe that never completed before a restart). Exported
// for the maintenance reaper.
func (s *TokenService) RecheckPendingChatGPT(ctx context.Context, tokenID, accessToken string) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	s.probeAndFinishChatGPT(ctx, tokenID, accessToken)
}

// ReprobeStalePendingChatGPT re-probes ChatGPT accounts stuck in pending for
// longer than `older` — e.g. an import whose off-thread probe was interrupted by
// a process restart, which would otherwise leave a good account pending forever
// (RecoverQuota only revives 限额, never pending). Each re-probe goes through the
// shared semaphore so the sweep can't stampede OpenAI.
func (s *TokenService) ReprobeStalePendingChatGPT(ctx context.Context, older time.Duration) {
	if s.chatgpt == nil {
		return
	}
	cutoff := time.Now().Add(-older)
	items, err := s.tokens.ListStalePending(ctx, "chatgpt", cutoff)
	if err != nil {
		return
	}
	for _, it := range items {
		if strings.TrimSpace(it.Value) == "" {
			continue
		}
		id, token := it.ID, it.Value
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			s.RecheckPendingChatGPT(rctx, id, token)
		}()
	}
}

// ReprobeStalePendingAdobe re-runs the Adobe import worker for cookies stuck in
// pending past `older` — a cookie→token exchange interrupted mid-flight (process
// restart or a redis/network blip during import) otherwise strands the row as an
// empty-email, unschedulable pending zombie forever (RecoverQuota only revives
// 限额, never pending). The stored cookie lives on the refresh profile; each
// re-probe self-limits through checkPendingAdobe's semaphore.
func (s *TokenService) ReprobeStalePendingAdobe(ctx context.Context, older time.Duration) {
	if s.adobe == nil {
		return
	}
	cutoff := time.Now().Add(-older)
	items, err := s.tokens.ListStalePending(ctx, "adobe", cutoff)
	if err != nil {
		return
	}
	for _, it := range items {
		prof, err := s.refresh.Get(ctx, it.ID)
		if err != nil || prof == nil || strings.TrimSpace(prof.Cookie) == "" {
			continue
		}
		go s.checkPendingAdobe(it.ID, prof.Cookie)
	}
}

// chatgptRemaining normalizes OpenAI's image_gen remaining: the raw rate-limit
// counter can go NEGATIVE on over-used accounts, and "—"(absent)means unknown —
// both clamp to 0, and 0 counts as exhausted (→ 限额). Returns (remaining≥0,
// exhausted).
func nextUTCResetAfter() string {
	return time.Unix((time.Now().Unix()/86400+1)*86400, 0).UTC().Format(time.RFC3339)
}

func chatgptRemaining(data map[string]any) (int, bool) {
	raw, ok := data["remaining"]
	if !ok || raw == nil {
		return 0, true
	}
	rem := intValue(raw)
	if rem < 0 {
		rem = 0
	}
	return rem, rem <= 0
}

// checkPendingRunway probes a freshly imported Runway token's credit balance
// off-thread (mirrors checkPendingChatGPT). ErrAuth → dead; any other error
// gets the benefit of the doubt and activates so a transient blip can't sideline
// a good account.
func (s *TokenService) checkPendingRunway(tokenID, accessToken string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("token import: runway pending check panicked for %s", tokenID)
		}
	}()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s.applyProxy(ctx)

	if s.runway == nil {
		s.finishPending(ctx, "runway", tokenID, "active", false, nil)
		return
	}
	data, err := s.runway.FetchCreditsBalance(ctx, accessToken)
	if err != nil {
		if errors.Is(err, runway.ErrAuth) {
			s.finishPending(ctx, "runway", tokenID, "disabled", true, nil)
			return
		}
		// network/proxy error — benefit of the doubt, activate.
		s.finishPending(ctx, "runway", tokenID, "active", false, nil)
		return
	}
	quotaMeta := map[string]any{}
	if rem, ok := data["remaining"].(int); ok {
		quotaMeta["cached_quota_remaining"] = rem
		quotaMeta["cached_quota_at"] = int(time.Now().Unix())
	}
	if used, ok := data["used"].(int); ok {
		quotaMeta["cached_quota_used"] = used
	}
	if total, ok := data["total"].(int); ok {
		quotaMeta["cached_quota_total"] = total
	}
	s.finishPending(ctx, "runway", tokenID, "active", false, quotaMeta)
}

// ImportGrokToken lands a Grok website "sso" cookie (a JWT carrying only a
// session_id) as a pending account and probes its credit balance off-thread.
// Identity is the session id (grok sso has no email/exp claim). No refresh: a
// dead session just dies (失效就失效).
func (s *TokenService) ImportGrokToken(ctx context.Context, ssoToken, tokenID string) (*model.TokenAccount, error) {
	s.applyProxy(ctx)
	ssoToken = strings.TrimSpace(strings.TrimPrefix(ssoToken, "Bearer "))
	ssoToken = strings.TrimPrefix(ssoToken, "sso=")
	if ssoToken == "" {
		return nil, errors.New("sso token required")
	}
	if grok.IsGrokOAuthToken(ssoToken) {
		return nil, errors.New("grok 需要网站 sso Cookie，不能导入 Sub2API/CPA 的 OAuth access_token")
	}
	if !grok.IsGrokToken(ssoToken) {
		return nil, errors.New("not a grok sso token")
	}
	sid := grok.SessionIDFromToken(ssoToken)
	// Fully async, no dedup: every import mints a fresh row (a passed-in tokenID is
	// an explicit edit → update). We do NOT look up an existing account by
	// email/session_id, and we leave account_email empty — email, quota and
	// recovery time are all filled off-thread by checkPendingGrok, which also
	// disables the account if the sso session is dead.
	if strings.TrimSpace(tokenID) == "" {
		tokenID = newTokenID("grok")
	}
	meta := datatypes.JSONMap{"pending_check": true}
	if sid != "" {
		meta["session_id"] = sid
	}
	item, err := s.createToken(ctx, "grok", tokenID, ssoToken, "pending", meta)
	if err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			if item, err = s.tokens.Update(ctx, "grok", tokenID, map[string]any{
				"value": ssoToken, "status": "pending", "meta": meta,
			}); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	go s.checkPendingGrok(tokenID, ssoToken)
	return item, nil
}

func (s *TokenService) checkPendingGrok(tokenID, ssoToken string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("token import: grok pending check panicked for %s", tokenID)
		}
	}()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if s.grok == nil {
		s.finishPending(ctx, "grok", tokenID, "active", false, nil)
		return
	}
	s.applyProxy(ctx)
	// Validate the session first: a dead sso answers 200 {"status":"unauthenticated"},
	// which FetchSession now maps to ErrAuth → disable. Also backfill the email if
	// import couldn't resolve it (empty account_email currently shows the session id).
	if email, _, serr := s.grok.FetchSession(ctx, ssoToken); serr != nil {
		if errors.Is(serr, grok.ErrAuth) {
			s.finishPending(ctx, "grok", tokenID, "disabled", true, nil)
			return
		}
	} else if strings.TrimSpace(email) != "" {
		_, _ = s.tokens.Update(ctx, "grok", tokenID, map[string]any{"account_email": strings.TrimSpace(email)})
	}
	data, err := s.grok.FetchCreditsBalance(ctx, ssoToken)
	if err != nil {
		if errors.Is(err, grok.ErrAuth) {
			s.finishPending(ctx, "grok", tokenID, "disabled", true, nil)
			return
		}
		s.finishPending(ctx, "grok", tokenID, "active", false, nil)
		return
	}
	quotaMeta := map[string]any{}
	if rem, ok := data["remaining"].(int); ok {
		quotaMeta["cached_quota_remaining"] = rem
		quotaMeta["cached_quota_at"] = int(time.Now().Unix())
	}
	if used, ok := data["used"].(int); ok {
		quotaMeta["cached_quota_used"] = used
	}
	if total, ok := data["total"].(int); ok {
		quotaMeta["cached_quota_total"] = total
	}
	if reset := strings.TrimSpace(stringValue(data["reset_after"])); reset != "" {
		_, _ = s.tokens.Update(ctx, "grok", tokenID, map[string]any{"cached_quota_reset_after": reset})
	}
	s.finishPending(ctx, "grok", tokenID, "active", false, quotaMeta)
}

const (
	grokLivenessInterval  = 6 * time.Hour
	grokLivenessBatchSize = 4
)

func selectGrokLivenessCandidates(items []model.TokenAccount, now time.Time) []model.TokenAccount {
	type candidate struct {
		account   model.TokenAccount
		checkedAt int64
	}
	cutoff := now.Add(-grokLivenessInterval).Unix()
	due := make([]candidate, 0, grokLivenessBatchSize)
	for i := range items {
		it := items[i]
		if it.Pool != "grok" || it.Dead || it.Status == "disabled" {
			continue
		}
		if !it.SchedulingStub && strings.TrimSpace(it.Value) == "" {
			continue
		}
		checkedAt, ok := jsonMapInt(it.Meta, "grok_liveness_checked_at")
		if !ok || checkedAt <= 0 {
			checkedAt, _ = jsonMapInt(it.Meta, "cached_quota_at")
		}
		if int64(checkedAt) > cutoff {
			continue
		}
		due = append(due, candidate{account: it, checkedAt: int64(checkedAt)})
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].checkedAt != due[j].checkedAt {
			return due[i].checkedAt < due[j].checkedAt
		}
		if !due[i].account.CreatedAt.Equal(due[j].account.CreatedAt) {
			return due[i].account.CreatedAt.Before(due[j].account.CreatedAt)
		}
		return due[i].account.ID < due[j].account.ID
	})
	if len(due) > grokLivenessBatchSize {
		due = due[:grokLivenessBatchSize]
	}
	out := make([]model.TokenAccount, len(due))
	for i := range due {
		out[i] = due[i].account
	}
	return out
}

// RefreshGrokLiveness re-validates a bounded batch of due Grok accounts.
// Grok sso can't be renewed and has no reset-based death deadline (billingPeriodEnd
// is only a credits-renewal date — the sso keeps working past it), so liveness is
// probed directly through the authenticated credits endpoint. A missing
// subscription is not an entitlement signal: current Grok accounts can expose
// video without an ACTIVE subscription, so it must never become video_limited.
// The credits balance is re-synced and 恢复时间 refreshed from its weekly reset.
// Both successful and failed attempts record a timestamp so a bad account cannot
// monopolize every maintenance tick or continuously consume residential traffic.
func (s *TokenService) RefreshGrokLiveness(ctx context.Context) {
	if s.grok == nil {
		return
	}
	stubs, err := s.tokens.ListSchedulingByPool(ctx, "grok")
	if err != nil {
		return
	}
	picked := selectGrokLivenessCandidates(stubs, time.Now())
	if len(picked) == 0 {
		return
	}
	ids := make([]string, 0, len(picked))
	for _, account := range picked {
		ids = append(ids, account.ID)
	}
	loaded, err := s.tokens.ListByIDs(ctx, "grok", ids)
	if err != nil {
		return
	}
	byID := make(map[string]model.TokenAccount, len(loaded))
	for _, account := range loaded {
		byID[account.ID] = account
	}
	items := make([]model.TokenAccount, 0, len(picked))
	for _, account := range picked {
		full, ok := byID[account.ID]
		if !ok || strings.TrimSpace(full.Value) == "" {
			continue
		}
		items = append(items, full)
	}
	if len(items) == 0 {
		return
	}
	s.applyProxy(ctx)
	for i := range items {
		it := items[i]
		// The credits endpoint is both the useful media balance and an authenticated
		// liveness check. Avoid a second subscription request per account: apart from
		// no longer being an entitlement signal, probing both endpoints for a large
		// pool exhausts the clearance proxy's connection budget.
		data, derr := s.grok.FetchCreditsBalance(ctx, it.Value)
		metaPatch := map[string]any{"grok_liveness_checked_at": int(time.Now().Unix())}
		if derr != nil && errors.Is(derr, grok.ErrAuth) {
			// A single 401/403 can be an upstream blip or anti-bot response; leave the
			// account alive until a later scheduled check. Generation-time auth
			// handling remains the final authority for disabling a dead SSO.
			_ = s.tokens.UpdateMergingMeta(ctx, "grok", it.ID, metaPatch, nil)
			continue
		}
		if derr != nil {
			// Other transient/network errors: keep the cached balance and retry later.
			_ = s.tokens.UpdateMergingMeta(ctx, "grok", it.ID, metaPatch, nil)
			continue
		}
		metaPatch["cached_quota_at"] = int(time.Now().Unix())
		if rem, ok := data["remaining"].(int); ok {
			metaPatch["cached_quota_remaining"] = rem
		}
		if used, ok := data["used"].(int); ok {
			metaPatch["cached_quota_used"] = used
		}
		if total, ok := data["total"].(int); ok {
			metaPatch["cached_quota_total"] = total
		}
		patch := map[string]any{}
		if reset := strings.TrimSpace(stringValue(data["reset_after"])); reset != "" {
			patch["cached_quota_reset_after"] = reset
		}
		_ = s.tokens.UpdateMergingMeta(ctx, "grok", it.ID, metaPatch, patch)
		recordBookkeepingError("refresh grok quota bucket", s.storeQuotaSnapshot(ctx, "grok", it.ID, data))
	}
	log.Printf("grok liveness: checked %d due account(s)", len(items))
}

func oreateCookieValue(cookie, name string) string {
	for _, part := range strings.Split(cookie, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && key == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func oreateIdentityHash(ouid string) string {
	sum := sha256.Sum256([]byte("oreate:" + strings.TrimSpace(ouid)))
	return hex.EncodeToString(sum[:])
}

func (s *TokenService) persistOreateBrowserSession(accountID, expectedOldCookie, newCookie string) {
	if s.tokens == nil || s.oreate == nil || strings.TrimSpace(accountID) == "" || expectedOldCookie == newCookie || !oreate.IsOreateCookie(newCookie) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	metaPatch := map[string]any{"oreate_cookie_observed_at": time.Now().Unix()}
	if bid := oreateCookieValue(newCookie, "__bid_n"); bid != "" {
		metaPatch["bid"] = bid
	}
	if ouid := oreateCookieValue(newCookie, "OUID"); ouid != "" {
		metaPatch["ouid"] = ouid
	}
	if _, err := s.tokens.RotateOreateSession(ctx, accountID, expectedOldCookie, newCookie, metaPatch); err != nil {
		log.Printf("persist oreate browser session failed for %s", accountID)
	}
}

// ImportOreateAccount stores only the website cookie and non-secret profile
// metadata. Passwords from account-export files are deliberately never accepted
// by this boundary. A background probe validates the session and hydrates quota.
func (s *TokenService) ImportOreateAccount(ctx context.Context, cookie, email, _, userAgent string, regTS int64, vip, tokenID string) (*model.TokenAccount, error) {
	s.applyProxy(ctx)
	cookie = strings.TrimSpace(cookie)
	if !oreate.IsOreateCookie(cookie) {
		return nil, errors.New("not an OreateAI cookie")
	}
	email = strings.TrimSpace(email)
	// The cookie is authoritative: export metadata can be stale, and two rows for
	// one upstream OUID would share the same browser session and first-use bonus.
	ouid := oreateCookieValue(cookie, "OUID")
	if ouid == "" {
		return nil, errors.New("OreateAI cookie is missing OUID")
	}
	if strings.TrimSpace(tokenID) == "" {
		tokenID = newTokenID("oreate")
	}
	credentialGeneration := newTokenID("oreate-generation")
	meta := datatypes.JSONMap{
		"pending_check": true, "ouid": ouid,
		oreateCredentialGenerationMetaKey: credentialGeneration,
	}
	metaPatch := map[string]any{
		"pending_check": true, "ouid": ouid,
		oreateCredentialGenerationMetaKey: credentialGeneration,
	}
	if userAgent = strings.TrimSpace(userAgent); userAgent != "" {
		meta["user_agent"] = userAgent
		metaPatch["user_agent"] = userAgent
	}
	if regTS != 0 {
		meta["reg_ts"] = regTS
		metaPatch["reg_ts"] = regTS
	}
	if vip = strings.TrimSpace(vip); vip != "" {
		meta["vip"] = vip
		metaPatch["vip"] = vip
	}
	identityHash := oreateIdentityHash(ouid)
	previous, _ := s.tokens.GetByIdentityHash(ctx, "oreate", identityHash)
	now := time.Now()
	candidate := &model.TokenAccount{
		ID: tokenID, Pool: "oreate", Value: cookie, IdentityHash: identityHash,
		Status: "pending", Meta: meta, AccountEmail: email, AddedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	item, fresh, err := s.tokens.UpsertOreateByIdentity(ctx, candidate, metaPatch)
	if err != nil {
		return nil, err
	}
	if previous != nil && previous.Value != cookie && s.oreate != nil {
		s.oreate.InvalidateSession(previous.ID, previous.Value)
	}
	account := oreate.Account{ID: item.ID, Cookie: cookie, Email: email, OUID: ouid, UserAgent: userAgent, RegTS: regTS, VIP: vip}
	go s.checkPendingOreate(item.ID, credentialGeneration, account, fresh)
	return item, nil
}

func (s *TokenService) checkPendingOreate(tokenID, credentialGeneration string, account oreate.Account, claimFirstImage bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("token import: oreate pending check panicked for %s", tokenID)
		}
	}()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if s.oreate == nil {
		s.finishPendingOreate(ctx, tokenID, credentialGeneration, "active", false, nil)
		return
	}
	s.applyProxy(ctx)
	profile, profileErr := s.oreate.FetchProfile(ctx, account)
	if errors.Is(profileErr, oreate.ErrAuth) {
		s.finishPendingOreate(ctx, tokenID, credentialGeneration, "disabled", true, nil)
		return
	}
	if profileErr == nil {
		patch := map[string]any{}
		metaPatch := map[string]any{}
		if profile.Email != "" {
			patch["account_email"] = profile.Email
			account.Email = profile.Email
		}
		if profile.OUID != "" {
			metaPatch["ouid"] = profile.OUID
			account.OUID = profile.OUID
		}
		if profile.VIP != "" {
			metaPatch["vip"] = profile.VIP
			account.VIP = profile.VIP
		}
		if profile.RegTS != 0 {
			metaPatch["reg_ts"] = profile.RegTS
			account.RegTS = profile.RegTS
		}
		if len(patch) > 0 || len(metaPatch) > 0 {
			if applied, _ := s.tokens.UpdateMergingMetaIfGeneration(ctx, "oreate", tokenID, oreateCredentialGenerationMetaKey, credentialGeneration, metaPatch, patch); !applied {
				return
			}
		}
	}
	data, quotaErr := s.oreate.FetchCreditsBalance(ctx, account)
	if errors.Is(quotaErr, oreate.ErrAuth) {
		s.finishPendingOreate(ctx, tokenID, credentialGeneration, "disabled", true, nil)
		return
	}
	if quotaErr != nil {
		// Network and upstream errors are inconclusive; keep the account usable and
		// let the live quota path retry later.
		if s.finishPendingOreate(ctx, tokenID, credentialGeneration, "active", false, nil) && claimFirstImage {
			go s.claimOreateFirstImage(tokenID, credentialGeneration, account)
		}
		return
	}
	quotaMeta := map[string]any{"cached_quota_at": int(time.Now().Unix())}
	remaining, hasRemaining := data["remaining"].(int)
	if hasRemaining {
		quotaMeta["cached_quota_remaining"] = remaining
	}
	if total, ok := data["total"].(int); ok {
		quotaMeta["cached_quota_total"] = total
	}
	terminalPatch := map[string]any{}
	if reset := strings.TrimSpace(stringValue(data["reset_after"])); reset != "" {
		terminalPatch["cached_quota_reset_after"] = reset
	}
	status := "active"
	if hasRemaining && remaining < oreateMinUsableCredits {
		status = "quota"
	}
	quotaMeta["pending_check"] = false
	terminalPatch["status"] = status
	terminalPatch["dead"] = false
	applied, _ := s.tokens.UpdateMergingMetaIfGeneration(ctx, "oreate", tokenID, oreateCredentialGenerationMetaKey, credentialGeneration, quotaMeta, terminalPatch)
	if applied && claimFirstImage {
		go s.claimOreateFirstImage(tokenID, credentialGeneration, account)
	}
}

// ImportDolaAccount stores a cookie and verifies the protocol session
// before admitting it to scheduling. Verification never submits video;
// importing an existing session preserves its two-use daily quota.
func (s *TokenService) ImportDolaAccount(ctx context.Context, cookie, userAgent string) (*model.TokenAccount, error) {
	s.applyProxy(ctx)
	cookie = strings.TrimSpace(cookie)
	if !dola.IsDolaCookie(cookie) {
		return nil, errors.New("not a Dola cookie (need sessionid with a browser fingerprint or Passport session bundle)")
	}
	sessionID := dola.CookieValue(cookie, "sessionid")
	credentialGeneration := newTokenID("dola-generation")
	meta := datatypes.JSONMap{
		"pending_check":  true,
		"dola_readiness": "pending", "dola_readiness_version": "", "dola_verified_generation": "", "dola_probe_id": "", "dola_probe_until": "", "dola_retry_at": "", "dola_probe_attempts": 0, "dola_readiness_detail": "",
		dolaCredentialGenerationMetaKey: credentialGeneration,
	}
	metaPatch := map[string]any{
		"pending_check":  true,
		"dola_readiness": "pending", "dola_readiness_version": "", "dola_verified_generation": "", "dola_probe_id": "", "dola_probe_until": "", "dola_retry_at": "", "dola_probe_attempts": 0, "dola_readiness_detail": "",
		dolaCredentialGenerationMetaKey: credentialGeneration,
	}
	if userAgent = strings.TrimSpace(userAgent); userAgent != "" {
		meta["user_agent"] = userAgent
		metaPatch["user_agent"] = userAgent
	}
	identityHash := dola.IdentityHash(sessionID)
	now := time.Now()
	candidate := &model.TokenAccount{
		ID: newTokenID("dola"), Pool: "dola", Value: cookie, IdentityHash: identityHash,
		Status: "pending", Meta: meta, AddedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	item, _, err := s.tokens.UpsertDolaByIdentity(ctx, candidate, metaPatch)
	if err != nil {
		return nil, err
	}
	go s.verifyDolaReadiness(item.ID, false)
	return item, nil
}

// claimOreateFirstImage generates the account's first image so Oreate releases
// the 50-credit first-use bonus, then refreshes the cached balance. The image
// itself is discarded: only the grant matters, and it is a one-time award the
// account cannot collect later without generating.
func (s *TokenService) claimOreateFirstImage(tokenID, credentialGeneration string, account oreate.Account) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("token import: oreate first-image claim panicked for %s", tokenID)
		}
	}()
	if s.oreate == nil {
		return
	}
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), oreateFirstImageTimeout)
	defer cancel()
	item, err := s.tokens.Get(ctx, "oreate", tokenID)
	if err != nil || item == nil || item.Dead || strings.TrimSpace(stringValue(item.Meta[oreateCredentialGenerationMetaKey])) != credentialGeneration {
		return
	}
	if _, claimed := jsonMapInt(item.Meta, oreateFirstImageAtMetaKey); claimed {
		return
	}
	s.applyProxy(ctx)
	// Record the attempt before spending credits: a crash or timeout must not let
	// a later import pay for a second image.
	marked, markErr := s.tokens.UpdateMergingMetaIfGeneration(ctx, "oreate", tokenID, oreateCredentialGenerationMetaKey, credentialGeneration, map[string]any{
		oreateFirstImageAtMetaKey:    int(time.Now().Unix()),
		oreateFirstImageStateMetaKey: oreateFirstImageRunning,
		oreateFirstImageErrMetaKey:   "",
	}, nil)
	if markErr != nil || !marked {
		return
	}
	imageURL, claimErr := s.oreate.ClaimFirstImageBonus(ctx, account)
	if claimErr != nil {
		log.Printf("token import: oreate first-use bonus not claimed for %s (%s)", tokenID, safeGenerationErrorText(claimErr))
	}
	s.recordOreateFirstImage(ctx, tokenID, credentialGeneration, imageURL, claimErr)
	// The grant lands asynchronously, so read the balance until it grows past the
	// pre-claim reading or the bounded window elapses.
	before, _ := jsonMapInt(item.Meta, "cached_quota_remaining")
	for deadline := time.Now().Add(oreateFirstImageBonusWait); ; {
		data, balanceErr := s.oreate.FetchCreditsBalance(ctx, account)
		if balanceErr == nil {
			applied, persistErr := persistOreateQuotaSnapshotGeneration(ctx, s.tokens, tokenID, credentialGeneration, data, time.Now())
			if persistErr != nil || !applied {
				log.Printf("token import: could not persist oreate balance for %s", tokenID)
				return
			}
			if remaining, ok := data["remaining"].(int); ok && remaining > before {
				return
			}
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(oreateFirstImageBonusPoll):
		}
	}
}

// recordOreateFirstImage persists the outcome of the import-time image so the
// accounts UI can tell a spam-blocked account from one whose stream merely broke
// (the site charged for that image and released the bonus all the same).
func (s *TokenService) recordOreateFirstImage(ctx context.Context, tokenID, credentialGeneration, imageURL string, claimErr error) {
	state, detail := oreateFirstImageOK, ""
	if claimErr != nil {
		detail = safeGenerationErrorText(claimErr)
		switch {
		case errors.Is(claimErr, oreate.ErrSpamUser):
			state = oreateFirstImageSpam
		case strings.TrimSpace(imageURL) != "":
			// The image rendered before the stream ended badly.
			state = oreateFirstImagePartial
		default:
			state = oreateFirstImageFailed
		}
	}
	if applied, err := s.tokens.UpdateMergingMetaIfGeneration(ctx, "oreate", tokenID, oreateCredentialGenerationMetaKey, credentialGeneration, map[string]any{
		oreateFirstImageStateMetaKey: state,
		oreateFirstImageErrMetaKey:   detail,
	}, nil); err != nil || !applied {
		log.Printf("token import: could not persist oreate first-image state for %s", tokenID)
	}
}

func oreateAccountFromToken(item model.TokenAccount) oreate.Account {
	account := oreate.Account{ID: item.ID, Cookie: item.Value, Email: item.AccountEmail}
	if item.Meta == nil {
		return account
	}
	account.UserAgent = strings.TrimSpace(stringValue(item.Meta["user_agent"]))
	account.OUID = strings.TrimSpace(stringValue(item.Meta["ouid"]))
	account.BID = strings.TrimSpace(stringValue(item.Meta["bid"]))
	account.VIP = strings.TrimSpace(stringValue(item.Meta["vip"]))
	account.RegTS = int64(intValue(item.Meta["reg_ts"]))
	return account
}

const (
	// oreateFirstImageAtMetaKey records when an account claimed the one-time
	// first-use image bonus, whether or not the generation itself succeeded.
	oreateFirstImageAtMetaKey = "oreate_first_image_at"
	// oreateFirstImageStateMetaKey / oreateFirstImageErrMetaKey surface that
	// attempt's outcome (and its upstream message) in the accounts list.
	oreateFirstImageStateMetaKey = "oreate_first_image_state"
	oreateFirstImageErrMetaKey   = "oreate_first_image_error"
	oreateFirstImageRunning      = "running"
	oreateFirstImageOK           = "ok"
	oreateFirstImageSpam         = "spam"
	oreateFirstImagePartial      = "partial"
	oreateFirstImageFailed       = "failed"
	oreateFirstImageTimeout      = 6 * time.Minute
	oreateFirstImageBonusWait    = 60 * time.Second
	oreateFirstImageBonusPoll    = 5 * time.Second

	oreateMinUsableCredits             = 60
	oreateQuotaRefreshBatchSize        = 4
	oreateQuotaRefreshInterval         = 30 * time.Minute
	oreateSessionRefreshBatchSize      = 2
	oreateSessionRefreshInterval       = 6 * time.Hour
	oreateQuotaCheckedAtMetaKey        = "oreate_quota_checked_at"
	oreateLegacyRetirementCheckedAtKey = "oreate_retirement_checked_at"
)

func oreateStatusForBalance(item model.TokenAccount, remaining int, known bool) (string, bool) {
	if !known || item.Dead || (item.Status != "active" && item.Status != "quota") {
		return "", false
	}
	if remaining < oreateMinUsableCredits {
		return "quota", item.Status != "quota"
	}
	if item.Status == "quota" {
		return "active", true
	}
	return "", false
}

// oreateQuotaPatches converts a successful upstream balance response into one
// atomic metadata merge plus the lifecycle transition it authorizes. Low-credit
// rows are retained in quota state; only a later successful balance at or above
// the operating floor may reactivate them.
// oreateVideoQuotaMetaKey caches the part of the balance Oreate accepts for
// video generation (see Client.FetchCreditsBalance).
const oreateVideoQuotaMetaKey = "cached_video_quota_remaining"

func oreateQuotaPatches(item model.TokenAccount, data map[string]any, now time.Time) (map[string]any, map[string]any) {
	metaPatch := map[string]any{
		"cached_quota_at":           int(now.Unix()),
		oreateQuotaCheckedAtMetaKey: int(now.Unix()),
	}
	remaining, hasRemaining := data["remaining"].(int)
	if hasRemaining {
		metaPatch["cached_quota_remaining"] = remaining
	}
	if total, ok := data["total"].(int); ok {
		metaPatch["cached_quota_total"] = total
	}
	if videoRemaining, ok := data["video_remaining"].(int); ok {
		metaPatch[oreateVideoQuotaMetaKey] = videoRemaining
	}
	patch := map[string]any{
		"cached_quota_reset_after": strings.TrimSpace(stringValue(data["reset_after"])),
	}
	if status, change := oreateStatusForBalance(item, remaining, hasRemaining); change {
		patch["status"] = status
		if status == "active" {
			patch["fails"] = 0
			patch["quota_recover_at"] = nil
		}
	}
	return metaPatch, patch
}

func persistOreateQuotaSnapshot(ctx context.Context, tokens *repo.TokenRepository, tokenID string, data map[string]any, now time.Time) (*model.TokenAccount, error) {
	item, err := tokens.Get(ctx, "oreate", tokenID)
	if err != nil {
		return nil, err
	}
	metaPatch, patch := oreateQuotaPatches(*item, data, now)
	if err := tokens.UpdateMergingMeta(ctx, "oreate", tokenID, metaPatch, patch); err != nil {
		return nil, err
	}
	return tokens.Get(ctx, "oreate", tokenID)
}

func persistOreateQuotaSnapshotGeneration(ctx context.Context, tokens *repo.TokenRepository, tokenID, credentialGeneration string, data map[string]any, now time.Time) (bool, error) {
	item, err := tokens.Get(ctx, "oreate", tokenID)
	if err != nil {
		return false, err
	}
	metaPatch, patch := oreateQuotaPatches(*item, data, now)
	return tokens.UpdateMergingMetaIfGeneration(ctx, "oreate", tokenID, oreateCredentialGenerationMetaKey, credentialGeneration, metaPatch, patch)
}

func selectOreateQuotaRefreshCandidates(items []model.TokenAccount, now time.Time) []model.TokenAccount {
	cutoff := now.Add(-oreateQuotaRefreshInterval).Unix()
	type candidate struct {
		account   model.TokenAccount
		checkedAt int64
	}
	due := make([]candidate, 0, oreateQuotaRefreshBatchSize)
	for i := range items {
		item := items[i]
		remaining, known := jsonMapInt(item.Meta, "cached_quota_remaining")
		needsRefresh := item.Status == "quota" || (item.Status == "active" && known && remaining < oreateMinUsableCredits)
		if item.Dead || !needsRefresh || strings.TrimSpace(item.Value) == "" {
			continue
		}
		checkedAt, ok := jsonMapInt(item.Meta, oreateQuotaCheckedAtMetaKey)
		if !ok {
			checkedAt, _ = jsonMapInt(item.Meta, oreateLegacyRetirementCheckedAtKey)
		}
		if int64(checkedAt) > cutoff {
			continue
		}
		due = append(due, candidate{account: item, checkedAt: int64(checkedAt)})
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].checkedAt != due[j].checkedAt {
			return due[i].checkedAt < due[j].checkedAt
		}
		return due[i].account.ID < due[j].account.ID
	})
	if len(due) > oreateQuotaRefreshBatchSize {
		due = due[:oreateQuotaRefreshBatchSize]
	}
	out := make([]model.TokenAccount, len(due))
	for i := range due {
		out[i] = due[i].account
	}
	return out
}

// RefreshLowCreditOreateAccounts periodically rechecks quota-state rows and
// active legacy rows cached below the operating floor. A successful reading
// keeps low balances parked and reactivates replenished accounts; failed probes
// leave the last known state untouched and are throttled for 30 minutes.
func (s *TokenService) RefreshLowCreditOreateAccounts(ctx context.Context) {
	if s.oreate == nil || !s.oreateRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.oreateRefreshing.Store(false)
		items, err := s.tokens.ListByPool(ctx, "oreate")
		if err != nil {
			return
		}
		items = selectOreateQuotaRefreshCandidates(items, time.Now())
		if len(items) == 0 {
			return
		}
		s.applyProxy(ctx)
		var wg sync.WaitGroup
		var refreshed atomic.Int32
		for i := range items {
			item := items[i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				nowUnix := int(time.Now().Unix())
				if markerErr := s.tokens.UpdateMergingMeta(probeCtx, "oreate", item.ID, map[string]any{
					oreateQuotaCheckedAtMetaKey: nowUnix,
				}, nil); markerErr != nil {
					return
				}
				data, fetchErr := s.oreate.FetchCreditsBalance(probeCtx, oreateAccountFromToken(item))
				if fetchErr != nil {
					return
				}
				if _, updateErr := persistOreateQuotaSnapshot(probeCtx, s.tokens, item.ID, data, time.Unix(int64(nowUnix), 0)); updateErr != nil {
					log.Printf("oreate maintenance: could not persist refreshed quota for %s", item.ID)
					return
				}
				recordBookkeepingError("refresh oreate quota bucket", s.storeQuotaSnapshot(probeCtx, "oreate", item.ID, data))
				refreshed.Add(1)
			}()
		}
		wg.Wait()
		if n := refreshed.Load(); n > 0 {
			log.Printf("oreate maintenance: refreshed %d low-credit account(s)", n)
		}
	}()
}

func selectOreateSessionRefreshCandidates(items []model.TokenAccount, now time.Time) []model.TokenAccount {
	cutoff := now.Add(-oreateSessionRefreshInterval).Unix()
	type candidate struct {
		account   model.TokenAccount
		checkedAt int64
	}
	due := make([]candidate, 0, oreateSessionRefreshBatchSize)
	for _, item := range items {
		if item.Dead || (item.Status != "active" && item.Status != "quota") || strings.TrimSpace(item.Value) == "" {
			continue
		}
		checkedAt, _ := jsonMapInt(item.Meta, "oreate_session_checked_at")
		if observedAt, ok := jsonMapInt(item.Meta, "oreate_cookie_observed_at"); ok && observedAt > checkedAt {
			checkedAt = observedAt
		}
		if int64(checkedAt) > cutoff {
			continue
		}
		due = append(due, candidate{account: item, checkedAt: int64(checkedAt)})
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].checkedAt != due[j].checkedAt {
			return due[i].checkedAt < due[j].checkedAt
		}
		return due[i].account.ID < due[j].account.ID
	})
	if len(due) > oreateSessionRefreshBatchSize {
		due = due[:oreateSessionRefreshBatchSize]
	}
	out := make([]model.TokenAccount, len(due))
	for i := range due {
		out[i] = due[i].account
	}
	return out
}

// RefreshOreateSessions proactively visits a small number of signed pages and
// mints a Banti token without submitting a generation. Set-Cookie rotations are
// persisted through the client's CAS callback; the check marker prevents bursts.
func (s *TokenService) RefreshOreateSessions(ctx context.Context) {
	if s.oreate == nil || !s.oreateSessionRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.oreateSessionRefreshing.Store(false)
		items, err := s.tokens.ListByPool(ctx, "oreate")
		if err != nil {
			return
		}
		items = selectOreateSessionRefreshCandidates(items, time.Now())
		if len(items) == 0 {
			return
		}
		s.applyProxy(ctx)
		var refreshed int
		for _, item := range items {
			probeCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
			err := s.oreate.RefreshSession(probeCtx, oreateAccountFromToken(item))
			cancel()
			nowUnix := time.Now().Unix()
			markerCtx, cancelMarker := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, _ = s.tokens.RotateOreateSession(markerCtx, item.ID, item.Value, item.Value, map[string]any{
				"oreate_session_checked_at": nowUnix,
			})
			cancelMarker()
			if err == nil {
				refreshed++
			}
		}
		if refreshed > 0 {
			log.Printf("oreate maintenance: refreshed %d browser session(s)", refreshed)
		}
	}()
}

// ImportCustomAccount adds an upstream as a custom account: base_url + key, the
// csv list of model ids it serves (empty = all), plus optional weight and
// per-account concurrency. No probe — the account goes active immediately and is
// matched to custom models by id at generation time. Custom upstream calls use
// direct server egress and do not inherit the residential proxy setting.
func (s *TokenService) ImportCustomAccount(ctx context.Context, baseURL, apiKey, models, name string, weight, concurrency int, tokenID string) (*model.TokenAccount, error) {
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)
	// Edit mode: tokenID points at an existing custom account. base_url required;
	// a blank key keeps the stored one.
	if strings.TrimSpace(tokenID) != "" {
		existing, gerr := s.tokens.Get(ctx, "custom", tokenID)
		if gerr != nil {
			return nil, gerr
		}
		if baseURL == "" {
			return nil, errors.New("base_url required")
		}
		baseURL, gerr = normalizeCustomBaseURL(ctx, baseURL)
		if gerr != nil {
			return nil, gerr
		}
		meta := datatypes.JSONMap{"base_url": baseURL}
		if m := strings.TrimSpace(models); m != "" {
			meta["models"] = m
		}
		patch := map[string]any{"meta": meta, "weight": weight, "concurrency": concurrency, "account_email": strings.TrimSpace(name)}
		if apiKey != "" {
			patch["value"] = apiKey
		}
		item, uerr := s.tokens.Update(ctx, "custom", tokenID, patch)
		if uerr != nil {
			return nil, uerr
		}
		_ = existing
		return item, nil
	}
	if baseURL == "" || apiKey == "" {
		return nil, errors.New("base_url and key required")
	}
	baseURL, err := normalizeCustomBaseURL(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	meta := datatypes.JSONMap{"base_url": baseURL}
	if m := strings.TrimSpace(models); m != "" {
		meta["models"] = m
	}
	tokenID = newTokenID("custom")
	item, err := s.createToken(ctx, "custom", tokenID, apiKey, "active", meta)
	if err != nil {
		return nil, err
	}
	patch := map[string]any{}
	if strings.TrimSpace(name) != "" {
		patch["account_email"] = strings.TrimSpace(name)
	}
	if weight != 0 {
		patch["weight"] = weight
	}
	if concurrency > 0 {
		patch["concurrency"] = concurrency
	}
	if len(patch) > 0 {
		if updated, uerr := s.tokens.Update(ctx, "custom", tokenID, patch); uerr == nil {
			item = updated
		}
	}
	return item, nil
}

func normalizeCustomBaseURL(ctx context.Context, raw string) (string, error) {
	parsed, err := netguard.ValidateAssetURL(ctx, strings.TrimSpace(raw), nil)
	if err != nil {
		return "", fmt.Errorf("invalid base_url: public HTTPS on port 443 is required: %w", err)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid base_url: query and fragment are not allowed: %w", netguard.ErrUnsafeAssetURL)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

// finishPending writes the terminal status/dead flag and clears the pending_check
// marker (merging any cached quota) for a background import probe.
func (s *TokenService) finishPendingOreate(ctx context.Context, id, credentialGeneration, status string, dead bool, quotaMeta map[string]any) bool {
	metaPatch := map[string]any{"pending_check": false}
	for k, v := range quotaMeta {
		metaPatch[k] = v
	}
	patch := map[string]any{"status": status, "dead": dead}
	applied, _ := s.tokens.UpdateMergingMetaIfGeneration(ctx, "oreate", id, oreateCredentialGenerationMetaKey, credentialGeneration, metaPatch, patch)
	return applied
}

func (s *TokenService) finishPending(ctx context.Context, pool, id, status string, dead bool, quotaMeta map[string]any) {
	metaPatch := map[string]any{"pending_check": false}
	for k, v := range quotaMeta {
		metaPatch[k] = v
	}
	patch := map[string]any{"status": status}
	if dead {
		patch["dead"] = true
	} else if status == "active" || status == "quota" {
		// Re-importing an email can reuse a previously dead row. An active
		// or quota terminal probe must clear that stale marker, otherwise the UI
		// continues treating the freshly supplied, authenticated token as dead.
		patch["dead"] = false
	}
	_ = s.tokens.UpdateMergingMeta(ctx, pool, id, metaPatch, patch)
}

func (s *TokenService) Update(ctx context.Context, pool, id string, body map[string]any) (*model.TokenAccount, error) {
	pool = normalizePool(pool)
	if pool == "" {
		return nil, errors.New("unknown pool")
	}
	patch := map[string]any{}
	if raw, ok := body["status"]; ok {
		status := normalizeTokenStatus(stringValue(raw))
		if status == "" {
			return nil, errors.New("invalid status")
		}
		patch["status"] = status
		if status == "active" {
			patch["dead"] = false
			if _, hasFails := body["fails"]; !hasFails {
				patch["fails"] = 0
			}
		}
	}
	if raw, ok := body["value"]; ok {
		value := strings.TrimSpace(stringValue(raw))
		if value == "" {
			return nil, errors.New("value cannot be empty")
		}
		patch["value"] = value
		patch["dead"] = false
	}
	if raw, ok := body["fails"]; ok {
		patch["fails"] = intValue(raw)
	}
	if raw, ok := body["weight"]; ok {
		weight := intValue(raw)
		if weight < -1000 || weight > 1000 {
			return nil, errors.New("weight must be between -1000 and 1000")
		}
		patch["weight"] = weight
	}
	if raw, ok := body["concurrency"]; ok {
		if pool != "custom" && pool != "adobe" {
			return nil, errors.New("concurrency is only configurable for custom or adobe accounts")
		}
		concurrency := intValue(raw)
		if concurrency < 1 || concurrency > 20 {
			return nil, errors.New("concurrency must be between 1 and 20")
		}
		patch["concurrency"] = concurrency
	}
	if len(patch) == 0 {
		return s.tokens.Get(ctx, pool, id)
	}
	return s.tokens.Update(ctx, pool, id, patch)
}

func (s *TokenService) Delete(ctx context.Context, pool, id string) error {
	pool = normalizePool(pool)
	if pool == "" {
		return errors.New("unknown pool")
	}
	var deletedOreate *model.TokenAccount
	if pool == "oreate" {
		deletedOreate, _ = s.tokens.Get(ctx, pool, id)
	}
	rows, err := s.tokens.Delete(ctx, pool, id)
	if err != nil {
		return err
	}
	// Also drop the matching cookie refresh profile (token id == profile id),
	// otherwise the background refresher re-creates the token. Track whether a
	// profile existed so we can mirror Python's 404-when-nothing-removed.
	profileRemoved := false
	if _, getErr := s.refresh.Get(ctx, id); getErr == nil {
		profileRemoved = true
	}
	_ = s.refresh.Delete(ctx, id)
	if rows == 0 && !profileRemoved {
		return ErrNotFound
	}
	if deletedOreate != nil && s.oreate != nil {
		s.oreate.InvalidateSession(deletedOreate.ID, deletedOreate.Value)
	}
	return nil
}

// DeleteBulk removes many accounts by id (across pools) plus their cookie
// refresh profiles. Returns how many account rows were removed.
func (s *TokenService) DeleteBulk(ctx context.Context, ids []string) (int, error) {
	seen := make(map[string]struct{}, len(ids))
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
	}
	if len(clean) == 0 {
		return 0, nil
	}
	var deletedOreate []model.TokenAccount
	if s.oreate != nil {
		all, listErr := s.tokens.ListByIDs(ctx, "oreate", clean)
		if listErr != nil {
			return 0, listErr
		}
		deletedOreate = all
	}
	rows, err := s.tokens.DeleteByIDs(ctx, clean)
	if err != nil {
		return 0, err
	}
	// Drop matching cookie refresh profiles (id == token id) so the background
	// refresher doesn't re-create the tokens.
	_ = s.refresh.DeleteByIDs(ctx, clean)
	for _, item := range deletedOreate {
		s.oreate.InvalidateSession(item.ID, item.Value)
	}
	return int(rows), nil
}

func (s *TokenService) Accounts(ctx context.Context) ([]map[string]any, error) {
	items, err := s.tokens.List(ctx)
	if err != nil {
		return nil, err
	}
	inFlight, err := s.events.InFlightByAccount(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, accountRow(item, inFlight[item.ID]))
	}
	return out, nil
}

// Quota publishes every trusted provider probe to the same bucket used by
// dispatch. Unknown/error readings preserve the previous schedulable balance.
func (s *TokenService) Quota(ctx context.Context, pool, id string) (map[string]any, error) {
	snapshot, err := s.probeQuota(ctx, pool, id)
	if err != nil {
		return nil, err
	}
	if err := s.storeQuotaSnapshot(ctx, normalizePool(pool), id, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *TokenService) probeQuota(ctx context.Context, pool, id string) (map[string]any, error) {
	s.applyProxy(ctx)
	item, err := s.tokens.Get(ctx, normalizePool(pool), id)
	if err != nil {
		return nil, err
	}
	if poolToType(item.Pool) == "openai" && s.chatgpt != nil {
		data, err := s.chatgpt.FetchImageQuota(ctx, item.Value)
		if err != nil {
			return nil, err
		}
		authFailed := boolValueWithDefault(data["auth_failed"], false)
		// The quota endpoint is best-effort and may be challenged independently of
		// the conversation endpoint. Never permanently kill an account from this
		// admin refresh path; a real generation 401 remains the final authority.
		patch := map[string]any{}
		metaPatch := map[string]any{"cached_quota_at": int(time.Now().Unix())}
		unknown := boolValueWithDefault(data["unknown"], false)
		rem, exhausted := chatgptRemaining(data)
		// Only trust a definitive reading. An unknown read (403/429/timeout) must
		// not clobber the cached balance with a bogus 0 nor sink the account.
		if !unknown {
			metaPatch["cached_quota_remaining"] = rem
		}
		resetAfter := strings.TrimSpace(stringValue(data["reset_after"]))
		if resetAfter == "" {
			resetAfter = nextUTCResetAfter()
		}
		patch["cached_quota_reset_after"] = resetAfter
		item.CachedQuotaResetAfter = resetAfter
		// remaining<=0(负数/0)→ 限额;>0 且当前是 quota → 恢复 active。unknown 时
		// 不动状态(疑罪从无)。auth_failed 与其它 unknown 一样只表示本次读数
		// 不可信，不能据此把账号置死。
		if !authFailed && !unknown {
			if exhausted {
				patch["status"] = "quota"
			} else if item.Status == "quota" {
				patch["status"] = "active"
			}
		}
		_ = s.tokens.UpdateMergingMeta(ctx, item.Pool, item.ID, metaPatch, patch)
		return map[string]any{
			"supported":       true,
			"remaining":       rem,
			"total":           nil,
			"reset_after":     emptyToNil(item.CachedQuotaResetAfter),
			"quota_cached_at": metaPatch["cached_quota_at"],
			"unchanged":       false,
			"auth_failed":     authFailed,
			"unknown":         boolValueWithDefault(data["unknown"], false),
			"error":           safeQuotaProbeError(data),
		}, nil
	}
	if poolToType(item.Pool) == "adobe" && s.adobe != nil {
		data, err := s.adobe.FetchCreditsBalance(ctx, item.Value)
		if err != nil {
			// A balance endpoint can be independently challenged (notably generic
			// 403s). Only a real generation verdict may disable an account/route.
			return nil, err
		}
		if quotaProbeUntrusted(data) {
			return unknownQuotaSnapshot(data), nil
		}
		patch := map[string]any{}
		metaPatch := map[string]any{"cached_quota_at": int(time.Now().Unix())}
		if remaining, ok := data["remaining"].(int); ok {
			metaPatch["cached_quota_remaining"] = remaining
		}
		if used, ok := data["used"].(int); ok {
			metaPatch["cached_quota_used"] = used
		}
		if total, ok := data["total"].(int); ok {
			metaPatch["cached_quota_total"] = total
		}
		if resetAfter := strings.TrimSpace(stringValue(data["available_until"])); resetAfter != "" {
			patch["cached_quota_reset_after"] = resetAfter
			item.CachedQuotaResetAfter = resetAfter
		}
		_ = s.tokens.UpdateMergingMeta(ctx, item.Pool, item.ID, metaPatch, patch)
		return map[string]any{
			"supported":       true,
			"remaining":       data["remaining"],
			"used":            data["used"],
			"total":           data["total"],
			"reset_after":     emptyToNil(item.CachedQuotaResetAfter),
			"quota_cached_at": metaPatch["cached_quota_at"],
			"unchanged":       false,
			"unknown":         boolValueWithDefault(data["unknown"], false),
			"error":           safeQuotaProbeError(data),
		}, nil
	}
	if poolToType(item.Pool) == "byteplus" && s.byteplus != nil {
		data, err := s.byteplus.FetchCreditsBalance(ctx, item.Value)
		if err != nil {
			return nil, err
		}
		if quotaProbeUntrusted(data) {
			return unknownQuotaSnapshot(data), nil
		}
		metaPatch := map[string]any{}
		var upstreamRemaining *float64
		for source, target := range map[string]string{
			"remaining": "cached_quota_remaining",
			"used":      "cached_quota_used",
			"total":     "cached_quota_total",
		} {
			if value, ok := anyFloat(data[source]); ok {
				if source == "remaining" {
					upstreamRemaining = &value
				} else {
					metaPatch[target] = canonicalQuotaNumber(value)
				}
			}
		}
		if upstreamRemaining != nil {
			metaPatch["cached_quota_at"] = int(time.Now().Unix())
		}
		patch := map[string]any{}
		if reset := strings.TrimSpace(stringValue(data["reset_after"])); reset != "" {
			patch["cached_quota_reset_after"] = reset
			item.CachedQuotaResetAfter = reset
		}
		if upstreamRemaining != nil && *upstreamRemaining > 0 && item.Status == "quota" {
			patch["status"] = "active"
		}
		if err := s.tokens.SettleQuotaTracked(ctx, item.Pool, item.ID, 0, upstreamRemaining, metaPatch); err != nil {
			return nil, err
		}
		if len(patch) > 0 {
			_, _ = s.tokens.Update(ctx, item.Pool, item.ID, patch)
		}
		quotaCachedAt := metaPatch["cached_quota_at"]
		if quotaCachedAt == nil {
			quotaCachedAt = item.Meta["cached_quota_at"]
		}
		return map[string]any{
			"supported": true, "remaining": data["remaining"], "used": data["used"], "total": data["total"],
			"reset_after": emptyToNil(item.CachedQuotaResetAfter), "quota_cached_at": quotaCachedAt,
			"unchanged": false, "unknown": boolValueWithDefault(data["unknown"], false), "error": safeQuotaProbeError(data),
		}, nil
	}
	if poolToType(item.Pool) == "runway" && s.runway != nil {
		data, err := s.runway.FetchCreditsBalance(ctx, item.Value)
		if err != nil {
			return nil, err
		}
		if quotaProbeUntrusted(data) {
			return unknownQuotaSnapshot(data), nil
		}
		patch := map[string]any{}
		metaPatch := map[string]any{"cached_quota_at": int(time.Now().Unix())}
		if remaining, ok := data["remaining"].(int); ok {
			// Refresh only updates the displayed balance number — it never flips
			// status. Out-of-credits is judged at generation time (dead/401), so a
			// refresh can't sink a runway account into a revivable "quota" state.
			metaPatch["cached_quota_remaining"] = remaining
		}
		if used, ok := data["used"].(int); ok {
			metaPatch["cached_quota_used"] = used
		}
		if total, ok := data["total"].(int); ok {
			metaPatch["cached_quota_total"] = total
		}
		_ = s.tokens.UpdateMergingMeta(ctx, item.Pool, item.ID, metaPatch, patch)
		// Recovery time stays the JWT expiry (cached at import) — Runway credits
		// reset monthly, so the credits endpoint carries no reset timestamp.
		return map[string]any{
			"supported":       true,
			"remaining":       data["remaining"],
			"used":            data["used"],
			"total":           data["total"],
			"reset_after":     emptyToNil(item.CachedQuotaResetAfter),
			"quota_cached_at": metaPatch["cached_quota_at"],
			"unchanged":       false,
			"unknown":         boolValueWithDefault(data["unknown"], false),
			"error":           safeQuotaProbeError(data),
		}, nil
	}
	if poolToType(item.Pool) == "grok" && s.grok != nil {
		data, err := s.grok.FetchCreditsBalance(ctx, item.Value)
		if err != nil {
			return nil, err
		}
		if quotaProbeUntrusted(data) {
			return unknownQuotaSnapshot(data), nil
		}
		patch := map[string]any{}
		metaPatch := map[string]any{"cached_quota_at": int(time.Now().Unix())}
		if remaining, ok := data["remaining"].(int); ok {
			// Refresh only updates the displayed credit number; never flips status.
			// Out-of-credits is judged at generation time (dead/401, no renewal).
			metaPatch["cached_quota_remaining"] = remaining
		}
		if used, ok := data["used"].(int); ok {
			metaPatch["cached_quota_used"] = used
		}
		if total, ok := data["total"].(int); ok {
			metaPatch["cached_quota_total"] = total
		}
		// Recovery time is the credits' weekly reset (when the grant refills) —
		// purely informational, NOT a death deadline (liveness is judged by the
		// subscriptions sweep / real 401s), so it's safe to refresh every time.
		if reset := strings.TrimSpace(stringValue(data["reset_after"])); reset != "" {
			patch["cached_quota_reset_after"] = reset
			item.CachedQuotaResetAfter = reset
		}
		_ = s.tokens.UpdateMergingMeta(ctx, item.Pool, item.ID, metaPatch, patch)
		return map[string]any{
			"supported":       true,
			"remaining":       data["remaining"],
			"used":            data["used"],
			"total":           data["total"],
			"reset_after":     emptyToNil(item.CachedQuotaResetAfter),
			"quota_cached_at": metaPatch["cached_quota_at"],
			"unchanged":       false,
			"unknown":         boolValueWithDefault(data["unknown"], false),
			"error":           safeQuotaProbeError(data),
		}, nil
	}
	if poolToType(item.Pool) == "oreate" && s.oreate != nil {
		data, err := s.oreate.FetchCreditsBalance(ctx, oreateAccountFromToken(*item))
		if err != nil {
			return nil, err
		}
		if quotaProbeUntrusted(data) {
			return unknownQuotaSnapshot(data), nil
		}
		now := time.Now()
		item, err = persistOreateQuotaSnapshot(ctx, s.tokens, item.ID, data, now)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"supported": true, "remaining": data["remaining"], "used": data["used"], "total": data["total"],
			"reset_after": emptyToNil(item.CachedQuotaResetAfter), "quota_cached_at": int(now.Unix()),
			"unchanged": false, "unknown": false, "status": item.Status, "dead": item.Dead, "error": safeQuotaProbeError(data),
		}, nil
	}
	remaining, hasRemaining := jsonMapFloat(item.Meta, "cached_quota_remaining")
	quotaAt, _ := jsonMapInt(item.Meta, "cached_quota_at")
	typeLabel := poolToType(item.Pool)
	return map[string]any{
		"supported":       typeLabel == "openai" || typeLabel == "adobe" || typeLabel == "byteplus" || typeLabel == "runway" || typeLabel == "grok" || typeLabel == "oreate",
		"remaining":       valueOrNil((typeLabel == "openai" || typeLabel == "byteplus" || typeLabel == "runway" || typeLabel == "grok" || typeLabel == "oreate") && hasRemaining, remaining),
		"total":           nil,
		"reset_after":     emptyToNil(item.CachedQuotaResetAfter),
		"quota_cached_at": valueOrNil(quotaAt != 0, quotaAt),
		"unchanged":       true,
		"unknown":         false,
		"error":           nil,
	}, nil
}

func (s *TokenService) Email(ctx context.Context, pool, id string) (map[string]any, error) {
	s.applyProxy(ctx)
	item, err := s.tokens.Get(ctx, normalizePool(pool), id)
	if err != nil {
		return nil, err
	}
	if poolToType(item.Pool) == "openai" {
		email := strings.TrimSpace(item.AccountEmail)
		if email != "" {
			return map[string]any{"email": email, "cached": true}, nil
		}
		info := chatgpt.ExtractAccountInfo(item.Value)
		if extracted := strings.TrimSpace(stringValue(info["email"])); extracted != "" {
			_, _ = s.tokens.Update(ctx, item.Pool, item.ID, map[string]any{"account_email": extracted})
			return map[string]any{"email": extracted, "cached": false}, nil
		}
		return map[string]any{"email": nil, "cached": false}, nil
	}
	if poolToType(item.Pool) == "runway" {
		email := strings.TrimSpace(item.AccountEmail)
		if email != "" {
			return map[string]any{"email": email, "cached": true}, nil
		}
		// Runway email is a top-level JWT claim — decode it (no network).
		if extracted, _ := parseJWTEmailExpiry(item.Value); extracted != "" {
			_, _ = s.tokens.Update(ctx, item.Pool, item.ID, map[string]any{"account_email": extracted})
			return map[string]any{"email": extracted, "cached": false}, nil
		}
		return map[string]any{"email": nil, "cached": false}, nil
	}
	if poolToType(item.Pool) == "byteplus" {
		email := strings.TrimSpace(item.AccountEmail)
		if email != "" {
			return map[string]any{"email": email, "cached": true}, nil
		}
		if s.byteplus == nil {
			return map[string]any{"email": nil, "cached": false}, nil
		}
		profile, err := s.byteplus.FetchProfile(ctx, item.Value)
		if err != nil {
			if errors.Is(err, byteplus.ErrAuth) {
				_, _ = s.tokens.Update(ctx, item.Pool, item.ID, map[string]any{
					"status": "disabled", "dead": true, "fails": gorm.Expr("fails + 1"),
				})
			}
			return nil, err
		}
		patch := map[string]any{}
		email = firstString(profile, "email", "user_email")
		if email != "" {
			patch["account_email"] = email
		}
		if name := firstString(profile, "display_name", "user_name", "name"); name != "" {
			patch["account_display_name"] = name
		}
		if len(patch) > 0 {
			_, _ = s.tokens.Update(ctx, item.Pool, item.ID, patch)
		}
		return map[string]any{"email": emptyToNil(email), "cached": false}, nil
	}
	if poolToType(item.Pool) == "oreate" {
		email := strings.TrimSpace(item.AccountEmail)
		if email != "" {
			return map[string]any{"email": email, "cached": true}, nil
		}
		if s.oreate == nil {
			return map[string]any{"email": nil, "cached": false}, nil
		}
		profile, err := s.oreate.FetchProfile(ctx, oreateAccountFromToken(*item))
		if err != nil {
			if errors.Is(err, oreate.ErrAuth) {
				_, _ = s.tokens.Update(ctx, item.Pool, item.ID, map[string]any{
					"status": "disabled", "dead": true, "fails": gorm.Expr("fails + 1"),
				})
			}
			return nil, err
		}
		email = strings.TrimSpace(profile.Email)
		if email != "" {
			_, _ = s.tokens.Update(ctx, item.Pool, item.ID, map[string]any{"account_email": email})
		}
		return map[string]any{"email": emptyToNil(email), "cached": false}, nil
	}
	if poolToType(item.Pool) != "adobe" {
		return map[string]any{"email": nil}, nil
	}
	email := strings.TrimSpace(item.AccountEmail)
	if email == "" {
		if s.adobe == nil {
			return map[string]any{"email": nil, "cached": false}, nil
		}
		profile, err := s.adobe.FetchAccountProfile(ctx, item.Value)
		if err != nil {
			if errors.Is(err, adobe.ErrAuth) {
				_, _ = s.tokens.Update(ctx, item.Pool, item.ID, map[string]any{
					"status": "disabled",
					"dead":   true,
					"fails":  gorm.Expr("fails + 1"),
				})
			}
			return nil, err
		}
		patch := map[string]any{}
		if profileEmail := strings.TrimSpace(stringValue(profile["email"])); profileEmail != "" {
			patch["account_email"] = profileEmail
			email = profileEmail
		}
		if displayName := strings.TrimSpace(stringValue(profile["display_name"])); displayName != "" {
			patch["account_display_name"] = displayName
		}
		if len(patch) > 0 {
			_, _ = s.tokens.Update(ctx, item.Pool, item.ID, patch)
		}
		return map[string]any{"email": emptyToNil(email), "cached": false}, nil
	}
	return map[string]any{"email": email, "cached": true}, nil
}

func (s *TokenService) createToken(ctx context.Context, pool, tokenID, value, status string, meta datatypes.JSONMap) (*model.TokenAccount, error) {
	now := time.Now()
	item := &model.TokenAccount{
		ID:        tokenID,
		Pool:      pool,
		Value:     value,
		Status:    status,
		AddedAt:   &now,
		Meta:      meta,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.tokens.Create(ctx, item); err != nil {
		return nil, err
	}
	return item, nil
}

func accountRow(item model.TokenAccount, inFlight int64) map[string]any {
	remaining, hasRemaining := jsonMapFloat(item.Meta, "cached_quota_remaining")
	total, hasTotal := jsonMapFloat(item.Meta, "cached_quota_total")
	quotaAt, _ := jsonMapInt(item.Meta, "cached_quota_at")
	pending, _ := jsonMapBool(item.Meta, "pending_check")
	// OpenAI email lives in the token's JWT (nested profile claim). Decode it at
	// render time like the Python reference (_account_row) so accounts imported
	// before the email was persisted still show a name; fall back to the cached
	// field for adobe (whose email comes from a network profile fetch).
	email := item.AccountEmail
	if poolToType(item.Pool) == "openai" {
		if decoded := strings.TrimSpace(stringValue(chatgpt.ExtractAccountInfo(item.Value)["email"])); decoded != "" {
			email = decoded
		}
	}
	typeLabel := poolToType(item.Pool)
	capabilities := accountCapabilities(item, typeLabel)
	teamID := ""
	if item.Meta != nil {
		teamID = strings.TrimSpace(stringValue(item.Meta["team_id"]))
	}
	hasQuota := typeLabel == "openai" || typeLabel == "adobe" || typeLabel == "byteplus" || typeLabel == "runway" || typeLabel == "grok" || typeLabel == "oreate"
	firstImageAt, _ := jsonMapInt(item.Meta, oreateFirstImageAtMetaKey)
	firstImageState := strings.TrimSpace(stringValue(item.Meta[oreateFirstImageStateMetaKey]))
	if firstImageState == "" && firstImageAt != 0 {
		// Accounts imported before the state was tracked only carry the timestamp.
		firstImageState = "unknown"
	}
	return map[string]any{
		"id":              item.ID,
		"pool":            item.Pool,
		"type":            typeLabel,
		"email":           emptyToNil(email),
		"team_id":         emptyToNil(teamID),
		"remaining":       valueOrNil(hasQuota && hasRemaining, remaining),
		"quota_total":     valueOrNil(hasQuota && hasTotal, total),
		"reset_after":     emptyToNil(item.CachedQuotaResetAfter),
		"quota_cached_at": valueOrNil(quotaAt != 0, quotaAt),
		"created_at":      unixOrNil(item.AddedAt),
		"last_used_at":    unixOrNil(item.LastUsedAt),
		"expires_at":      jwtExpiryUnix(item.Value),
		"in_flight":       inFlight,
		"success_total":   item.SuccessTotal,
		"fail_total":      item.FailTotal,
		"fails_streak":    item.Fails,
		// Provider-side failures (overload / 5xx) — kept out of fail_total so an
		// upstream outage doesn't make every account look broken.
		"upstream_fails": item.UpstreamFails,
		"status":         item.Status,
		"dead":           item.Dead,
		"image_limited":  item.ImageLimited,
		"video_limited":  item.VideoLimited,
		// Capability state is deliberately separate from the shared media-credit
		// balance. Grok has independent chat/image/video product surfaces; callers
		// must not infer chat availability from the media quota number.
		"capabilities":      capabilities,
		"pending":           pending,
		"quota_supported":   hasQuota,
		"needs_reset_fetch": typeLabel == "adobe" && item.Status == "active" && strings.TrimSpace(item.CachedQuotaResetAfter) == "",
		"weight":            item.Weight,
		"concurrency":       item.Concurrency,
		"base_url":          emptyToNil(strings.TrimSpace(stringValue(item.Meta["base_url"]))),
		"models":            strings.TrimSpace(stringValue(item.Meta["models"])),
		// Import-time image (oreate only): state + upstream message so the table can
		// show whether the account ever generated and claimed its first-use bonus.
		"first_image":       emptyToNil(firstImageState),
		"first_image_error": emptyToNil(strings.TrimSpace(stringValue(item.Meta[oreateFirstImageErrMetaKey]))),
		"first_image_at":    valueOrNil(firstImageAt != 0, firstImageAt),
	}
}

// accountCapabilities describes the provider surfaces an account can be used
// for. It is returned as data (rather than only inferred in the UI) so the
// account test dialog and API consumers agree on routing capabilities.
func accountCapabilities(item model.TokenAccount, typeLabel string) []map[string]any {
	if typeLabel != "grok" {
		return nil
	}
	// Grok's shared quota marker is media-only; a quota-paused row can still
	// serve Web/Build chat. Pending/disabled/dead rows are not callable.
	identityOK := item.Status != "pending" && item.Status != "disabled" && !item.Dead && strings.TrimSpace(item.Value) != ""
	mediaOK := identityOK && item.Status == "active"
	return []map[string]any{
		{"kind": "chat", "available": identityOK, "limited": false},
		{"kind": "image", "available": mediaOK && !item.ImageLimited, "limited": item.ImageLimited},
		{"kind": "video", "available": mediaOK && !item.VideoLimited, "limited": item.VideoLimited},
	}
}

// jwtExpiryUnix returns the access token's exp claim (epoch seconds) for the
// accounts UI, or nil when the token is absent/opaque (e.g. a pending row whose
// value hasn't been minted yet).
func jwtExpiryUnix(token string) any {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	_, exp := parseJWTEmailExpiry(token)
	if exp == nil {
		return nil
	}
	return exp.Unix()
}

// cleanAdobeCookie mirrors the Python admin import preprocessing
// (api/admin.py import_adobe_cookie): tolerate JSON array/object pastes,
// unwrap a one-level {"cookie": "..."} wrapper, strip a leading "Cookie:"
// prefix and collapse stray whitespace/newlines.
func cleanAdobeCookie(cookie string) string {
	cookieStr := strings.TrimSpace(cookie)

	// ① A JSON array/object paste -> turn into a cookie string up front.
	if strings.HasPrefix(cookieStr, "[") {
		if converted := cookieStringFromInput(cookieStr); converted != "" {
			cookieStr = converted
		}
	}

	// ② Tolerate the whole JSON object `{"cookie": "..."}` pasted into the
	// textarea. Unwrap one level of JSON before validating.
	if strings.HasPrefix(cookieStr, "{") && strings.HasSuffix(cookieStr, "}") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(cookieStr), &parsed); err == nil {
			inner, ok := parsed["cookie"]
			if !ok {
				inner = parsed["value"]
			}
			switch v := inner.(type) {
			case string:
				cookieStr = strings.TrimSpace(v)
			case []any, map[string]any:
				if converted := cookieStringFromInputValue(v); converted != "" {
					cookieStr = converted
				}
			}
		}
	}

	// ③ Strip a leading "Cookie: " prefix (case-insensitive).
	if len(cookieStr) >= 7 && strings.EqualFold(cookieStr[:7], "cookie:") {
		cookieStr = strings.TrimSpace(cookieStr[7:])
	}

	// ④ Collapse stray newlines and excess whitespace.
	cookieStr = strings.Join(strings.Fields(cookieStr), " ")
	return cookieStr
}

// cookieStringFromInput parses a JSON string (array or object) describing
// browser cookies into a "name=value; name=value" cookie string, mirroring
// providers/adobe/_auth.py _cookie_string_from_input.
func cookieStringFromInput(raw string) string {
	raw = strings.TrimSpace(raw)
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ""
	}
	return cookieStringFromInputValue(parsed)
}

func cookieStringFromInputValue(raw any) string {
	switch v := raw.(type) {
	case string:
		text := strings.TrimSpace(v)
		if len(text) >= 7 && strings.EqualFold(text[:7], "cookie:") {
			text = strings.TrimSpace(text[7:])
		}
		return text
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name := strings.TrimSpace(stringValue(m["name"]))
			value := stringValue(m["value"])
			if name != "" {
				parts = append(parts, name+"="+value)
			}
		}
		return strings.Join(parts, "; ")
	case map[string]any:
		if cookies, ok := v["cookies"].([]any); ok {
			return cookieStringFromInputValue(cookies)
		}
		if inner, ok := v["cookie"]; ok {
			switch inner.(type) {
			case string, []any:
				return cookieStringFromInputValue(inner)
			}
		}
		return ""
	default:
		return ""
	}
}

func previewSecret(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) <= 16 {
		return "***"
	}
	return v[:6] + "…" + v[len(v)-4:]
}

func normalizePool(pool string) string {
	pool = strings.ToLower(strings.TrimSpace(pool))
	if _, ok := validTokenPools[pool]; ok {
		return pool
	}
	return ""
}

func normalizeTokenStatus(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "active", "disabled", "quota", "pending":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return ""
	}
}

func newTokenID(pool string) string {
	prefix := "TK"
	if pool == "adobe" {
		prefix = "AD"
	}
	if pool == "byteplus" {
		prefix = "BP"
	}
	if pool == "chatgpt" {
		prefix = "OA"
	}
	if pool == "runway" {
		prefix = "RW"
	}
	if pool == "oreate" {
		prefix = "OR"
	}
	return prefix + randomUpper(10)
}

func poolToType(pool string) string {
	if mapped, ok := validTokenPools[pool]; ok {
		return mapped
	}
	return pool
}

func parseJWTEmailExpiry(token string) (string, *time.Time) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return "", nil
	}
	payload := parts[1]
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", nil
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", nil
	}
	email := strings.TrimSpace(stringValue(claims["email"]))
	switch v := claims["exp"].(type) {
	case float64:
		t := time.Unix(int64(v), 0)
		return email, &t
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			t := time.Unix(n, 0)
			return email, &t
		}
	}
	return email, nil
}

func jsonMapInt(m datatypes.JSONMap, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	return intValue(v), true
}

func jsonMapBool(m datatypes.JSONMap, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return false, false
	}
	switch x := v.(type) {
	case bool:
		return x, true
	default:
		return boolValueWithDefault(x, false), true
	}
}

func cloneJSONMap(in datatypes.JSONMap) datatypes.JSONMap {
	out := datatypes.JSONMap{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func unixOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func emptyToNil(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.TrimSpace(v)
}

func valueOrNil(ok bool, v any) any {
	if !ok {
		return nil
	}
	return v
}

type ginToken struct {
	ID           string     `json:"id"`
	ValuePreview string     `json:"value_preview"`
	Status       string     `json:"status"`
	Fails        int        `json:"fails"`
	AddedAt      *time.Time `json:"added_at"`
}

var _ = fmt.Sprint
