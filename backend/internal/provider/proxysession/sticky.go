// Package proxysession pins rotating residential proxies to one account.
//
// Most residential pools accept a session label in the username
// (`user-session-<id>`). Without that label every CONNECT can land on a new
// exit IP, which website providers treat as a stolen session. Dola already
// assigns a sticky exit per account through its session API; ChatGPT, Grok, and
// Oreate reuse this helper so generation, quota probes, and keep-alive TLS for
// one credential stay on the same labelled session.
package proxysession

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	// Marker is appended to the proxy username. It matches the residential-pool
	// convention already used by the Oreate signer.
	Marker = "-session-"
	// DisableEnv turns the rewrite off for pools that reject session labels.
	DisableEnv = "PROXY_STICKY_SESSION"
)

// Key returns a short, username-safe identifier for one credential or account.
func Key(material string) string {
	material = strings.TrimSpace(material)
	if material == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:8])
}

// KeyWithEpoch is Key(material) until the account is rotated onto a new
// residential session after a temporary upstream failure.
func KeyWithEpoch(material string, epoch int) string {
	if epoch <= 0 {
		return Key(material)
	}
	return Key(material + "\x00" + strconv.Itoa(epoch))
}

// URL rewrites an authenticated proxy so the given session shares one exit IP.
// Unauthenticated URLs, empty sessions, and PROXY_STICKY_SESSION=false are left
// unchanged. A username that already contains the session marker is preserved.
func URL(raw, session string) string {
	raw = strings.TrimSpace(raw)
	session = strings.TrimSpace(session)
	if raw == "" || session == "" || strings.EqualFold(strings.TrimSpace(os.Getenv(DisableEnv)), "false") {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	name := parsed.User.Username()
	if name == "" || strings.Contains(name, Marker) {
		return raw
	}
	password, _ := parsed.User.Password()
	parsed.User = url.UserPassword(name+Marker+session, password)
	return parsed.String()
}

// DeviceID is a stable UUID derived from the credential so one ChatGPT account
// does not share oai-device-id / oai-session-id with every other account on the
// process.
func DeviceID(material, kind string) string {
	material = strings.TrimSpace(material)
	if material == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(material + "\x00" + strings.TrimSpace(kind)))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}
