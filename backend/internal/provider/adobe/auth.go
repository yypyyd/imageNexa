package adobe

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	refreshURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.54.0-3-g58cfcb7"
	clientID   = "clio-playground-web"
	scopeValue = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext,additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles,account_cluster.read,creative_production,tk_platform,tk_platform_sync,profile"
)

var ErrAdobeCookieEmpty = errors.New("cookie is empty")

type CookieExchangeResult struct {
	AccessToken string
	ExpiresIn   int
	Raw         map[string]any
}

// NewARPSessionToken reproduces the base session token emitted by Adobe's
// SherlockSdk before its optional browser-fingerprint vendors finish loading.
// The web SDK uses a random v4 UUID as sid, compact-JSON encodes it, then uses
// standard padded base64. No Adobe round trip or fingerprint payload is needed
// for this initial x-arp-session-id value.
func NewARPSessionToken() string {
	payload, _ := json.Marshal(struct {
		SessionID string `json:"sid"`
	}{SessionID: uuid.NewString()})
	return base64.StdEncoding.EncodeToString(payload)
}

// SubmissionARPSessionToken upgrades the locally generated sid-only token to
// the browser-shaped value expected by the partner generation gateway. A
// structured browser export may already contain Adobe/Forter feature fields;
// preserve that opaque value exactly instead of trying to reconstruct it.
func SubmissionARPSessionToken(stored string) string {
	stored = strings.TrimSpace(stored)
	if stored != "" && !isBaseARPSessionToken(stored) {
		return stored
	}
	payload, _ := json.Marshal(map[string]string{
		"sid": uuid.NewString(),
		"ftr": forterFeatureToken(),
	})
	return base64.StdEncoding.EncodeToString(payload)
}

func isBaseARPSessionToken(token string) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil || len(payload) != 1 {
		return false
	}
	_, ok := payload["sid"].(string)
	return ok
}

func forterFeatureToken() string {
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		stamp := time.Now().UnixNano()
		for i := range randomBytes {
			randomBytes[i] = byte(stamp >> ((i % 8) * 8))
		}
	}
	// This is the same browser-vendor feature shape carried by Firefly's
	// Sherlock/Forter callback. It is local telemetry data, not an Adobe API
	// round trip, and is regenerated for every submit so its timestamp is fresh.
	return hex.EncodeToString(randomBytes) + "_" +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" +
		strconv.FormatInt(time.Now().UnixNano()%900000+100000, 10) +
		"_dUAL43-mnts-ants-d4_31ck__tt"
}

func normalizeCookie(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(v), "cookie:") {
		v = strings.TrimSpace(v[len("cookie:"):])
	}
	return v
}
