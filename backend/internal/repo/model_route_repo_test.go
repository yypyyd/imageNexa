package repo

import (
	"testing"
	"time"

	"backend/internal/model"
)

func TestAccountRouteBindingAllowsCooldownOnlyBlocksNewAdmission(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Minute)
	binding := model.AccountModelRoute{Enabled: true, Entitled: true, CooldownUntil: &future}

	if accountRouteBindingAllows(binding, now, true) {
		t.Fatal("future cooldown allowed a new request into the account route")
	}
	if !accountRouteBindingAllows(binding, now, false) {
		t.Fatal("future cooldown cancelled an already admitted request retry")
	}
}

func TestAccountRouteBindingAllowsInFlightStillRequiresAuthorization(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		binding model.AccountModelRoute
	}{
		{name: "disabled", binding: model.AccountModelRoute{Enabled: false, Entitled: true}},
		{name: "unentitled", binding: model.AccountModelRoute{Enabled: true, Entitled: false}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if accountRouteBindingAllows(test.binding, now, false) {
				t.Fatal("in-flight attempt bypassed disabled or unentitled binding")
			}
		})
	}
}

func TestAccountRouteBindingAllowsExpiredCooldown(t *testing.T) {
	now := time.Now()
	expired := now.Add(-time.Second)
	binding := model.AccountModelRoute{Enabled: true, Entitled: true, CooldownUntil: &expired}
	if !accountRouteBindingAllows(binding, now, true) {
		t.Fatal("expired cooldown blocked a new request")
	}
}
