package service

import (
	"testing"
	"time"

	"backend/internal/model"
)

func poolIDs(items []model.TokenAccount) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func poolAccounts(ids ...string) []model.TokenAccount {
	items := make([]model.TokenAccount, 0, len(ids))
	for _, id := range ids {
		items = append(items, model.TokenAccount{ID: id})
	}
	return items
}

// Cooling accounts are demoted behind the healthy ones but never dropped, so
// the fall-through retry chain still reaches them last.
func TestRotateRoundRobinDemotesCoolingAccounts(t *testing.T) {
	s := &V1Service{}
	s.coolDownAccount("adobe", "a1")
	s.coolDownAccount("adobe", "a3")

	items := poolAccounts("a1", "a2", "a3", "a4")
	s.rotateRoundRobin("adobe", items)

	got := poolIDs(items)
	if len(got) != 4 {
		t.Fatalf("rotation dropped accounts: %v", got)
	}
	healthy := map[string]bool{"a2": true, "a4": true}
	if !healthy[got[0]] || !healthy[got[1]] {
		t.Fatalf("healthy accounts not tried first: %v", got)
	}
	cooling := map[string]bool{"a1": true, "a3": true}
	if !cooling[got[2]] || !cooling[got[3]] {
		t.Fatalf("cooling accounts not demoted to the back: %v", got)
	}
}

// An expired cooldown must stop demoting the account.
func TestAccountCoolingExpires(t *testing.T) {
	s := &V1Service{}
	s.acctCooldowns.Store("adobe:a1", time.Now().Add(-time.Second))
	if s.accountCooling("adobe", "a1") {
		t.Fatal("expired cooldown still reported as cooling")
	}
	if _, ok := s.acctCooldowns.Load("adobe:a1"); ok {
		t.Fatal("expired cooldown entry not cleaned up")
	}
}

func TestAdminArtifactOwnershipIsLimitedToAdminTestEvents(t *testing.T) {
	admin := &APIPrincipal{TokenType: "admin"}
	if !eventOwnedByPrincipal(&model.EventLog{Kind: "image", Source: "admin"}, admin, "image") {
		t.Fatal("administrator could not read its test artifact")
	}
	for _, event := range []*model.EventLog{
		{Kind: "image", Source: "v1"},
		{Kind: "video", Source: "admin"},
		{Kind: "image", Source: "admin", APICredentialID: "key-1"},
	} {
		if eventOwnedByPrincipal(event, admin, "image") {
			t.Fatalf("administrator test artifact boundary accepted %#v", event)
		}
	}
}

func TestAdminArtifactPrincipalContainsOnlyOwnershipIdentity(t *testing.T) {
	admin := adminArtifactPrincipal(&model.EventLog{Source: "admin"})
	if admin.TokenType != "admin" || admin.Credential != nil {
		t.Fatalf("admin test principal = %#v", admin)
	}
	credential := adminArtifactPrincipal(&model.EventLog{APICredentialID: "key-1"})
	if credential.TokenType != "" || credential.Credential == nil || credential.Credential.ID != "key-1" {
		t.Fatalf("credential principal = %#v", credential)
	}
	if credential.Credential.KeyHash != "" {
		t.Fatal("administrator artifact principal loaded credential secret material")
	}
}

// Without Redis the cursor still advances per pick, so consecutive requests
// start on different accounts instead of hammering the head of the list.
func TestRotateRoundRobinCursorAdvances(t *testing.T) {
	s := &V1Service{}
	first := poolAccounts("a1", "a2", "a3")
	s.rotateRoundRobin("adobe", first)
	second := poolAccounts("a1", "a2", "a3")
	s.rotateRoundRobin("adobe", second)

	if poolIDs(first)[0] == poolIDs(second)[0] {
		t.Fatalf("cursor did not advance: %v then %v", poolIDs(first), poolIDs(second))
	}
}

// Higher weight = higher priority: weight groups keep their order and only
// rotate within the same weight.
func TestRotateRoundRobinRespectsWeight(t *testing.T) {
	s := &V1Service{}
	items := []model.TokenAccount{
		{ID: "low1", Weight: 1},
		{ID: "high1", Weight: 5},
		{ID: "low2", Weight: 1},
		{ID: "high2", Weight: 5},
	}
	s.rotateRoundRobin("adobe", items)

	got := poolIDs(items)
	high := map[string]bool{"high1": true, "high2": true}
	if !high[got[0]] || !high[got[1]] {
		t.Fatalf("high-weight accounts not first: %v", got)
	}
}
