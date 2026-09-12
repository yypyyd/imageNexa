package service

import (
	"errors"

	"backend/internal/provider/adobe"
	"backend/internal/provider/byteplus"
	"backend/internal/provider/chatgpt"
	"backend/internal/provider/custom"
	"backend/internal/provider/dola"
	"backend/internal/provider/grok"
	"backend/internal/provider/oreate"
	"backend/internal/provider/runway"
)

// Unique temporary failures one request may spend. A second wave of unused
// accounts is allowed; a pool-wide outage is stopped earlier by signature.
const maxTempFailoverAccounts = 6

// Identical temporary signatures in a row mean the pool, not the account.
const maxCorrelatedTempFailures = 3

type tempFailoverState struct {
	unique  int
	lastSig string
	sameSig int
}

func tempFailureSignature(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, adobe.ErrTemporaryUpstream):
		return "adobe.temporary"
	case errors.Is(err, byteplus.ErrTemporaryUpstream):
		return "byteplus.temporary"
	case errors.Is(err, chatgpt.ErrTemporaryUpstream):
		return "chatgpt.temporary"
	case errors.Is(err, runway.ErrTemporaryUpstream):
		return "runway.temporary"
	case errors.Is(err, grok.ErrTemporaryUpstream):
		return "grok.temporary"
	case errors.Is(err, oreate.ErrTemporaryUpstream):
		return "oreate.temporary"
	case errors.Is(err, dola.ErrTemporaryUpstream):
		return "dola.temporary"
	case errors.Is(err, custom.ErrTemporaryUpstream):
		return "custom.temporary"
	default:
		return "temporary"
	}
}

func excludeAfterAccountFailure(isAuth, isQuota, isDead, tempDead bool, err error) bool {
	return isAuth || isQuota || tempDead || (isDead && !tempDead) || errors.Is(err, ErrNoProviderAccount)
}

// note records one unique account's temporary failure. stop=true means the
// request should surface the error instead of trying another account.
func (state *tempFailoverState) note(err error) (stop bool) {
	if state == nil {
		return true
	}
	sig := tempFailureSignature(err)
	if sig != "" && sig == state.lastSig {
		state.sameSig++
	} else {
		state.lastSig = sig
		state.sameSig = 1
	}
	state.unique++
	return state.sameSig >= maxCorrelatedTempFailures || state.unique >= maxTempFailoverAccounts
}
