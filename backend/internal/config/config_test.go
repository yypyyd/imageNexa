package config

import (
	"strings"
	"testing"
)

func TestProductionRequiresAdminBootstrapToken(t *testing.T) {
	invalidTokens := []string{
		"",
		"too-short",
		"replace-with-a-long-random-bootstrap-token",
	}
	for _, token := range invalidTokens {
		cfg := validProductionConfig()
		cfg.AdminBootstrapToken = token
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "ADMIN_BOOTSTRAP_TOKEN") {
			t.Fatalf("validate() with %q = %v, want bootstrap token error", token, err)
		}
	}

	cfg := validProductionConfig()
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() with token = %v", err)
	}
}

func TestProductionConfigurationFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		fieldName string
		mutate    func(*Config)
	}{
		{name: "missing public URL", fieldName: "PUBLIC_BASE_URL", mutate: func(c *Config) { c.PublicBaseURL = "" }},
		{name: "insecure public URL", fieldName: "PUBLIC_BASE_URL", mutate: func(c *Config) { c.PublicBaseURL = "http://api.example.test" }},
		{name: "public URL path", fieldName: "PUBLIC_BASE_URL", mutate: func(c *Config) { c.PublicBaseURL = "https://api.example.test/prefix" }},
		{name: "invalid public port", fieldName: "PUBLIC_BASE_URL", mutate: func(c *Config) { c.PublicBaseURL = "https://api.example.test:99999" }},
		{name: "localhost CORS", fieldName: "CORS_ORIGINS", mutate: func(c *Config) { c.CORSOrigins = []string{"http://localhost:2000"} }},
		{name: "insecure cookie", fieldName: "COOKIE_SECURE", mutate: func(c *Config) { c.CookieSecure = false }},
		{name: "default postgres password", fieldName: "POSTGRES_DSN", mutate: func(c *Config) { c.PostgresDSN = "host=postgres user=postgres password=postgres dbname=vivid_ai" }},
		{name: "missing object endpoint", fieldName: "RUSTFS_ENDPOINT", mutate: func(c *Config) { c.RustFSEndpoint = "" }},
		{name: "example access key", fieldName: "RUSTFS_ACCESS_KEY", mutate: func(c *Config) { c.RustFSAccessKey = "replace-with-a-random-access-key" }},
		{name: "short object secret", fieldName: "RUSTFS_SECRET_KEY", mutate: func(c *Config) { c.RustFSSecretKey = "short" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validProductionConfig()
			test.mutate(cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), test.fieldName) {
				t.Fatalf("validate() = %v, want %s error", err, test.fieldName)
			}
		})
	}
}

func validProductionConfig() *Config {
	return &Config{
		AppEnv:              "production",
		PublicBaseURL:       "https://api.example.test",
		PostgresDSN:         "host=postgres user=twoapi password=0123456789abcdef0123456789abcdef dbname=twoapi sslmode=disable",
		CookieSecure:        true,
		CORSOrigins:         []string{"https://api.example.test"},
		AdminBootstrapToken: "6bd360579da86c7e8d3c94d806b55846",
		RustFSEndpoint:      "http://rustfs:9000",
		RustFSBucket:        "twoapi",
		RustFSAccessKey:     "0123456789abcdef",
		RustFSSecretKey:     "0123456789abcdef0123456789abcdef",
	}
}

func TestDevelopmentStillStartsWithoutAdminBootstrapToken(t *testing.T) {
	if err := (&Config{AppEnv: "development"}).validate(); err != nil {
		t.Fatalf("development validate() = %v", err)
	}
	if err := (&Config{AppEnv: "prod"}).validate(); err == nil || !strings.Contains(err.Error(), "APP_ENV") {
		t.Fatalf("unknown APP_ENV validate() = %v, want fail-closed error", err)
	}
}

func TestParseTrustedProxyCIDRsRejectsInvalidEntries(t *testing.T) {
	if _, err := parseCIDRs([]string{"127.0.0.0/8", "not-a-network"}); err == nil {
		t.Fatal("parseCIDRs accepted an invalid network")
	}

	prefixes, err := parseCIDRs([]string{"127.0.0.42/8", "::1/128"})
	if err != nil {
		t.Fatalf("parseCIDRs valid entries: %v", err)
	}
	if got := prefixes[0].String(); got != "127.0.0.0/8" {
		t.Fatalf("masked prefix = %q, want 127.0.0.0/8", got)
	}
}
