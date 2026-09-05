package service

import (
	"context"
	"strings"
	"time"

	"backend/internal/model"
)

// providerSnapshotScope names the only bucket a provider-wide quota probe can
// authoritatively update. ChatGPT's endpoint is image_gen-only and Grok's is
// media-only; neither may poison text routing with a zero media balance.
func providerSnapshotScope(pool string) (bucketKey, unit string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(pool)) {
	case "chatgpt":
		return "chatgpt.image", "generations", true
	case "byteplus":
		return "byteplus.computing_points", "points", true
	case "adobe":
		return "adobe.credits", "credits", true
	case "runway":
		return "runway.credits", "credits", true
	case "grok":
		return "grok.media", "credits", true
	case "oreate":
		return "oreate.points", "points", true
	default:
		return "", "", false
	}
}

// quotaSnapshotWriter is shared by automatic/manual probes and token recovery.
type quotaSnapshotWriter interface {
	UpsertSnapshot(context.Context, string, string, string, *float64, *float64, *time.Time) (*model.AccountQuotaBucket, error)
}

func (s *TokenService) storeQuotaSnapshot(ctx context.Context, pool, accountID string, snapshot map[string]any) error {
	if s.quotas == nil || boolValueWithDefault(snapshot["unchanged"], false) {
		return nil
	}
	bucket, unit, scoped := providerSnapshotScope(pool)
	remaining, total, resetAt, trusted := trustedQuotaSnapshot(snapshot)
	if !scoped || !trusted {
		return nil
	}
	_, err := s.quotas.UpsertSnapshot(ctx, accountID, bucket, unit, total, remaining, resetAt)
	return err
}

func quotaProbeUntrusted(snapshot map[string]any) bool {
	return snapshot == nil || boolValueWithDefault(snapshot["unknown"], false) || boolValueWithDefault(snapshot["auth_failed"], false)
}

func unknownQuotaSnapshot(snapshot map[string]any) map[string]any {
	return map[string]any{
		"supported": true, "unknown": true, "unchanged": true,
		"auth_failed": boolValueWithDefault(snapshot["auth_failed"], false),
		"remaining":   nil, "total": nil, "error": safeQuotaProbeError(snapshot),
	}
}
