package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/model"
	"backend/internal/provider/byteplus"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestImportBytePlusCookieRejectsIncompleteCredentials(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
	}{
		{name: "missing csrf", cookie: "sessionid=account-session; locale=en-US"},
		{name: "anonymous csrf only", cookie: "csrfToken=csrf-value; locale=en-US; lang=en"},
		{name: "tracking cookies only", cookie: "csrfToken=csrf-value; __spti=tracking; locale=en-US"},
	}

	// Invalid inputs fail before TokenService reaches its repository, so a nil
	// repository is intentional here and keeps this validation test dependency-free.
	svc := &TokenService{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, err := svc.ImportBytePlusCookie(context.Background(), tt.cookie)
			if err == nil {
				t.Fatalf("ImportBytePlusCookie(%q) returned item %#v, want validation error", tt.cookie, item)
			}
			if !strings.Contains(err.Error(), "not a byteplus lumina cookie") {
				t.Fatalf("ImportBytePlusCookie() error = %q, want BytePlus cookie validation error", err)
			}
		})
	}
}

func TestBytePlusCookieNormalizationPreservesCompleteSession(t *testing.T) {
	raw := "Cookie: sessionid=account-session; csrfToken=csrf%2Fvalue; passport_auth_status=enabled; locale=en-US"
	want := "sessionid=account-session; csrfToken=csrf%2Fvalue; passport_auth_status=enabled; locale=en-US"

	got := cleanAdobeCookie(raw)
	if got != want {
		t.Fatalf("normalized BytePlus cookie = %q, want complete cookie %q", got, want)
	}
	for _, field := range []string{"sessionid=account-session", "csrfToken=csrf%2Fvalue", "passport_auth_status=enabled", "locale=en-US"} {
		if !strings.Contains(got, field) {
			t.Errorf("normalized BytePlus cookie dropped %q: %q", field, got)
		}
	}
	if !byteplus.IsBytePlusCookie(got) {
		t.Fatal("normalized complete session was not recognized as a BytePlus cookie")
	}
	if csrf := byteplus.CSRFTokenFromCookie(got); csrf != "csrf/value" {
		t.Fatalf("CSRFTokenFromCookie() = %q, want csrf/value", csrf)
	}
}

func TestBytePlusAccountRowPreservesFractionalQuota(t *testing.T) {
	row := accountRow(model.TokenAccount{
		ID: "BP-FRACTIONAL", Pool: "byteplus", Status: "active",
		Meta: datatypes.JSONMap{
			"cached_quota_remaining": 64.5,
			"cached_quota_total":     68,
		},
	}, 0)
	if got := row["remaining"]; got != 64.5 {
		t.Fatalf("account row remaining = %#v, want 64.5", got)
	}
	if got := row["quota_total"]; got != float64(68) {
		t.Fatalf("account row total = %#v, want 68", got)
	}
}

func TestBytePlusTokenIDUsesCompleteCredentialFingerprint(t *testing.T) {
	first := "sessionid=session-a; csrfToken=shared-csrf; passport_auth_status=enabled; locale=en-US"
	rotated := "sessionid=session-b; csrfToken=shared-csrf; passport_auth_status=enabled; locale=en-US"
	formatted := "Cookie: sessionid=session-a;csrfToken=shared-csrf;  passport_auth_status=enabled; locale=en-US"

	firstID := bytePlusTokenIDFromFingerprint(bytePlusCredentialFingerprint(first))
	rotatedID := bytePlusTokenIDFromFingerprint(bytePlusCredentialFingerprint(rotated))
	formattedID := bytePlusTokenIDFromFingerprint(bytePlusCredentialFingerprint(formatted))
	if firstID == rotatedID {
		t.Fatalf("rotated session reused account id %q even though the complete credential changed", firstID)
	}
	if firstID != formattedID {
		t.Fatalf("equivalent Cookie formatting changed account id: %q != %q", firstID, formattedID)
	}
	if !strings.HasPrefix(firstID, "BP") || len(firstID) != 42 {
		t.Fatalf("BytePlus account id = %q, want BP plus a 160-bit fingerprint", firstID)
	}
}

type bytePlusProbeCompletion struct {
	version string
	status  string
	applied bool
}

type memoryBytePlusPendingStore struct {
	mu          sync.Mutex
	items       map[string]*model.TokenAccount
	completions chan bytePlusProbeCompletion
}

func newMemoryBytePlusPendingStore() *memoryBytePlusPendingStore {
	return &memoryBytePlusPendingStore{
		items:       map[string]*model.TokenAccount{},
		completions: make(chan bytePlusProbeCompletion, 8),
	}
}

func cloneBytePlusTestAccount(item *model.TokenAccount) *model.TokenAccount {
	if item == nil {
		return nil
	}
	cloned := *item
	cloned.Meta = cloneJSONMap(item.Meta)
	return &cloned
}

func (s *memoryBytePlusPendingStore) Create(_ context.Context, item *model.TokenAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[item.ID]; exists {
		return gorm.ErrDuplicatedKey
	}
	s.items[item.ID] = cloneBytePlusTestAccount(item)
	return nil
}

func (s *memoryBytePlusPendingStore) Get(_ context.Context, id string) (*model.TokenAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, exists := s.items[id]
	if !exists {
		return nil, gorm.ErrRecordNotFound
	}
	return cloneBytePlusTestAccount(item), nil
}

func (s *memoryBytePlusPendingStore) RefreshCredential(_ context.Context, id, cookie, fingerprint, version string) (*model.TokenAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, exists := s.items[id]
	if !exists {
		return nil, gorm.ErrRecordNotFound
	}
	item.Value = cookie
	item.Status = "pending"
	item.Dead = false
	item.Fails = 0
	if item.Meta == nil {
		item.Meta = datatypes.JSONMap{}
	}
	item.Meta["pending_check"] = true
	item.Meta[bytePlusCredentialFingerprintMetaKey] = fingerprint
	item.Meta[bytePlusProbeVersionMetaKey] = version
	return cloneBytePlusTestAccount(item), nil
}

func (s *memoryBytePlusPendingStore) CompleteProbe(_ context.Context, id, cookie, fingerprint, version string, result bytePlusProbeResult) (bool, error) {
	s.mu.Lock()
	item, exists := s.items[id]
	applied := exists && bytePlusProbeStillCurrent(item, cookie, fingerprint, version)
	if applied {
		item.Status = result.Status
		item.Dead = result.Dead
		item.AccountEmail = result.Email
		item.AccountDisplayName = result.DisplayName
		item.Meta["pending_check"] = false
		for key, value := range result.QuotaMeta {
			item.Meta[key] = value
		}
	}
	s.mu.Unlock()
	s.completions <- bytePlusProbeCompletion{version: version, status: result.Status, applied: applied}
	return applied, nil
}

func TestBytePlusProbeCASChecksCredentialAndVersion(t *testing.T) {
	store := newMemoryBytePlusPendingStore()
	oldCookie := normalizeBytePlusCookie("sessionid=old; csrfToken=csrf; passport_auth_status=enabled")
	newCookie := normalizeBytePlusCookie("sessionid=new; csrfToken=csrf; passport_auth_status=enabled")
	fingerprint := bytePlusCredentialFingerprint(oldCookie)
	const version = "PROBEVERSION"
	item := &model.TokenAccount{
		ID: "BP-CAS", Pool: "byteplus", Value: newCookie, Status: "pending",
		Meta: datatypes.JSONMap{
			"pending_check":                      true,
			bytePlusCredentialFingerprintMetaKey: fingerprint,
			bytePlusProbeVersionMetaKey:          version,
		},
	}
	if err := store.Create(context.Background(), item); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	applied, err := store.CompleteProbe(context.Background(), item.ID, oldCookie, fingerprint, version, bytePlusProbeResult{Status: "disabled", Dead: true})
	if err != nil {
		t.Fatalf("CompleteProbe() error = %v", err)
	}
	if applied {
		t.Fatal("probe with a stale credential applied even though fingerprint/version metadata happened to match")
	}
	final, err := store.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if final.Status != "pending" || final.Dead || final.Value != newCookie {
		t.Fatalf("stale credential changed row: %#v", final)
	}
}

func (s *memoryBytePlusPendingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

type reorderedBytePlusProbeClient struct {
	mu              sync.Mutex
	profileCalls    int
	firstStarted    chan struct{}
	releaseFirst    chan struct{}
	firstStartedOne sync.Once
}

func (c *reorderedBytePlusProbeClient) FetchProfile(ctx context.Context, _ string) (map[string]any, error) {
	c.mu.Lock()
	c.profileCalls++
	call := c.profileCalls
	c.mu.Unlock()
	if call == 1 {
		c.firstStartedOne.Do(func() { close(c.firstStarted) })
		select {
		case <-c.releaseFirst:
			return nil, byteplus.ErrAuth
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return map[string]any{"email": "fresh@example.com", "display_name": "Fresh Account"}, nil
}

func (c *reorderedBytePlusProbeClient) FetchCreditsBalance(context.Context, string) (map[string]any, error) {
	return map[string]any{"remaining": 7, "used": 1, "total": 8}, nil
}

func waitBytePlusCompletion(t *testing.T, completions <-chan bytePlusProbeCompletion) bytePlusProbeCompletion {
	t.Helper()
	select {
	case completion := <-completions:
		return completion
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for BytePlus pending probe")
		return bytePlusProbeCompletion{}
	}
}

func TestImportBytePlusCookieOldProbeCannotOverrideReimport(t *testing.T) {
	store := newMemoryBytePlusPendingStore()
	client := &reorderedBytePlusProbeClient{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	svc := &TokenService{
		byteplus:        client,
		byteplusPending: store,
		sem:             make(chan struct{}, 10),
	}
	var releaseFirst sync.Once
	defer releaseFirst.Do(func() { close(client.releaseFirst) })
	cookie := "sessionid=session-a; csrfToken=csrf-a; passport_auth_status=enabled; locale=en-US"

	first, err := svc.ImportBytePlusCookie(context.Background(), cookie)
	if err != nil {
		t.Fatalf("first ImportBytePlusCookie() error = %v", err)
	}
	select {
	case <-client.firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first BytePlus probe did not start")
	}
	firstVersion := strings.TrimSpace(stringValue(first.Meta[bytePlusProbeVersionMetaKey]))

	second, err := svc.ImportBytePlusCookie(context.Background(), "Cookie: sessionid=session-a;csrfToken=csrf-a; passport_auth_status=enabled; locale=en-US")
	if err != nil {
		t.Fatalf("second ImportBytePlusCookie() error = %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("same complete Cookie created two ids: %q and %q", first.ID, second.ID)
	}
	if store.count() != 1 {
		t.Fatalf("same complete Cookie created %d rows, want 1", store.count())
	}
	secondVersion := strings.TrimSpace(stringValue(second.Meta[bytePlusProbeVersionMetaKey]))
	if firstVersion == "" || secondVersion == "" || firstVersion == secondVersion {
		t.Fatalf("probe generations were not advanced: first=%q second=%q", firstVersion, secondVersion)
	}

	freshCompletion := waitBytePlusCompletion(t, store.completions)
	if freshCompletion.version != secondVersion || freshCompletion.status != "active" || !freshCompletion.applied {
		t.Fatalf("fresh probe completion = %#v, want active applied generation %q", freshCompletion, secondVersion)
	}
	releaseFirst.Do(func() { close(client.releaseFirst) })
	staleCompletion := waitBytePlusCompletion(t, store.completions)
	if staleCompletion.version != firstVersion || staleCompletion.status != "disabled" || staleCompletion.applied {
		t.Fatalf("stale probe completion = %#v, want rejected disabled generation %q", staleCompletion, firstVersion)
	}

	final, err := store.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Get(final BytePlus account) error = %v", err)
	}
	if final.Status != "active" || final.Dead {
		t.Fatalf("stale auth failure changed fresh account state: status=%q dead=%v", final.Status, final.Dead)
	}
	if final.Value != normalizeBytePlusCookie(cookie) || final.AccountEmail != "fresh@example.com" {
		t.Fatalf("fresh credential/profile was overwritten: value=%q email=%q", final.Value, final.AccountEmail)
	}
	if pending, _ := jsonMapBool(final.Meta, "pending_check"); pending {
		t.Fatal("fresh BytePlus account remained pending")
	}
	if remaining, ok := jsonMapInt(final.Meta, "cached_quota_remaining"); !ok || remaining != 7 {
		t.Fatalf("fresh BytePlus quota = %d, %v; want 7, true", remaining, ok)
	}
}

var _ bytePlusPendingStore = (*memoryBytePlusPendingStore)(nil)
var _ bytePlusAccountClient = (*reorderedBytePlusProbeClient)(nil)
