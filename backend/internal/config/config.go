package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Config struct {
	AppEnv              string
	HTTPAddr            string
	PublicBaseURL       string
	AppTitle            string
	PostgresDSN         string
	RedisAddr           string
	RedisPassword       string
	RedisDB             int
	SessionCookieName   string
	CookieSecure        bool
	SessionTTL          time.Duration
	SessionSlideAfter   time.Duration
	CORSOrigins         []string
	TrustedProxyCIDRs   []netip.Prefix
	AdminBootstrapToken string
	GeneratedRoot       string
	RustFSEndpoint      string
	RustFSBucket        string
	RustFSAccessKey     string
	RustFSSecretKey     string
}

func Load() (*Config, error) {
	loadDotEnv()

	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	appEnv := envString("APP_ENV", "development")
	trustedProxyCIDRs, err := parseCIDRs(envList("TRUSTED_PROXY_CIDRS", []string{
		"127.0.0.0/8",
		"::1/128",
	}))
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		AppEnv:              appEnv,
		HTTPAddr:            envString("HTTP_ADDR", ":6061"),
		PublicBaseURL:       strings.TrimRight(envString("PUBLIC_BASE_URL", ""), "/"),
		AppTitle:            envString("APP_TITLE", "2API"),
		PostgresDSN:         envString("POSTGRES_DSN", "host=127.0.0.1 user=postgres password=postgres dbname=vivid_ai port=5432 sslmode=disable TimeZone=Asia/Shanghai"),
		RedisAddr:           envString("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:       envString("REDIS_PASSWORD", ""),
		RedisDB:             envInt("REDIS_DB", 0),
		SessionCookieName:   envString("SESSION_COOKIE_NAME", "twoapi_admin_session"),
		CookieSecure:        envBool("COOKIE_SECURE", appEnv != "development"),
		SessionTTL:          time.Duration(envInt("SESSION_TTL_HOURS", 24)) * time.Hour,
		SessionSlideAfter:   time.Duration(envInt("SESSION_SLIDE_AFTER_HOURS", 22)) * time.Hour,
		CORSOrigins:         envList("CORS_ORIGINS", []string{"http://localhost:5173", "http://127.0.0.1:5173"}),
		TrustedProxyCIDRs:   trustedProxyCIDRs,
		AdminBootstrapToken: envString("ADMIN_BOOTSTRAP_TOKEN", ""),
		GeneratedRoot: filepath.Clean(envString(
			"GENERATED_ROOT",
			// vivid-ai's own data dir (backend/data/generated) — NOT the Python
			// original's tree. Generated outputs and user-uploaded reference
			// images both live here and are served (cookie-authed) via /images.
			filepath.Join(wd, "data", "generated"),
		)),
		RustFSEndpoint:  envString("RUSTFS_ENDPOINT", ""),
		RustFSBucket:    envString("RUSTFS_BUCKET", ""),
		RustFSAccessKey: envString("RUSTFS_ACCESS_KEY", ""),
		RustFSSecretKey: envString("RUSTFS_SECRET_KEY", ""),
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	appEnv := strings.ToLower(strings.TrimSpace(c.AppEnv))
	if appEnv == "development" {
		return nil
	}
	if appEnv != "production" {
		return fmt.Errorf("APP_ENV must be either development or production")
	}
	if err := validateProductionOrigin("PUBLIC_BASE_URL", c.PublicBaseURL); err != nil {
		return err
	}
	if len(c.CORSOrigins) == 0 {
		return fmt.Errorf("CORS_ORIGINS must contain at least one HTTPS origin in production")
	}
	for _, origin := range c.CORSOrigins {
		if err := validateProductionOrigin("CORS_ORIGINS", origin); err != nil {
			return err
		}
	}
	if !c.CookieSecure {
		return fmt.Errorf("COOKIE_SECURE must be true in production")
	}
	if err := validatePostgresDSN(c.PostgresDSN); err != nil {
		return err
	}
	if err := validateServiceEndpoint("RUSTFS_ENDPOINT", c.RustFSEndpoint); err != nil {
		return err
	}
	if err := validateIdentifier("RUSTFS_BUCKET", c.RustFSBucket); err != nil {
		return err
	}
	for _, secret := range []struct {
		name   string
		value  string
		minLen int
	}{
		{name: "ADMIN_BOOTSTRAP_TOKEN", value: c.AdminBootstrapToken, minLen: 32},
		{name: "RUSTFS_ACCESS_KEY", value: c.RustFSAccessKey, minLen: 16},
		{name: "RUSTFS_SECRET_KEY", value: c.RustFSSecretKey, minLen: 32},
	} {
		if err := validateProductionSecret(secret.name, secret.value, secret.minLen); err != nil {
			return err
		}
	}
	return nil
}

func validateProductionOrigin(name, raw string) error {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s must be an absolute HTTPS origin in production", name)
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain a path, query, user info, or fragment in production", name)
	}
	if isLocalHostname(parsed.Hostname()) {
		return fmt.Errorf("%s must not use a loopback or localhost origin in production", name)
	}
	if err := validateURLPort(name, parsed); err != nil {
		return err
	}
	return nil
}

func validateServiceEndpoint(name, raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s must be an absolute HTTP(S) endpoint in production", name)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain a query, user info, or fragment in production", name)
	}
	if err := validateURLPort(name, parsed); err != nil {
		return err
	}
	return nil
}

func validateURLPort(name string, parsed *url.URL) error {
	if parsed == nil || parsed.Port() == "" {
		return nil
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s contains an invalid port in production", name)
	}
	return nil
}

func validatePostgresDSN(raw string) error {
	parsed, err := pgx.ParseConfig(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("POSTGRES_DSN is invalid in production")
	}
	if err := validateProductionSecret("POSTGRES_DSN password", parsed.Password, 16); err != nil {
		return err
	}
	return nil
}

func validateIdentifier(name, raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" || exampleValue(value) {
		return fmt.Errorf("%s is required and cannot use an example value in production", name)
	}
	return nil
}

func validateProductionSecret(name, raw string, minLen int) error {
	value := strings.TrimSpace(raw)
	if len([]byte(value)) < minLen || exampleValue(value) {
		return fmt.Errorf("%s must be at least %d bytes and cannot use a default or example value in production", name, minLen)
	}
	return nil
}

func exampleValue(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(normalized, "replace-with-") || strings.Contains(normalized, "changeme") || strings.Contains(normalized, "example") {
		return true
	}
	switch normalized {
	case "postgres", "password", "secret", "admin", "rustfs", "minioadmin":
		return true
	default:
		return false
	}
}

func isLocalHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

func parseCIDRs(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS entry %q: %w", value, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// loadDotEnv loads a .env file (KEY=VALUE per line) into the process environment
// before config is read. Real environment variables always win — .env only fills
// keys that aren't already set. Searches ENV_FILE, then walks up from the working
// directory so it works whether the binary runs from backend/ or the repo root.
func loadDotEnv() {
	for _, path := range dotEnvCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		applyDotEnv(string(data))
		return
	}
}

func dotEnvCandidates() []string {
	var out []string
	if v := strings.TrimSpace(os.Getenv("ENV_FILE")); v != "" {
		out = append(out, v)
	}
	wd, err := os.Getwd()
	if err != nil {
		return out
	}
	dir := wd
	for i := 0; i < 4; i++ {
		out = append(out, filepath.Join(dir, ".env"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

func applyDotEnv(content string) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" {
			continue
		}
		// Real env wins: only set keys that aren't already present.
		if _, ok := os.LookupEnv(key); !ok {
			_ = os.Setenv(key, val)
		}
	}
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return fallback
}

func envList(key string, fallback []string) []string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			s := strings.TrimSpace(part)
			if s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}
