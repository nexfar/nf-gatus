package tenancy

import "testing"

func TestConfig_ValidateAndSetDefaults(t *testing.T) {
	cfg := &Config{RootDomain: "  Status.Nexfar.com.br. "}
	if err := cfg.ValidateAndSetDefaults(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if cfg.RootDomain != "status.nexfar.com.br" {
		t.Errorf("expected normalized root domain, got %q", cfg.RootDomain)
	}
}

func TestConfig_IsEnabled(t *testing.T) {
	if (&Config{}).IsEnabled() {
		t.Error("expected tenancy to be disabled when root domain is empty")
	}
	if (*Config)(nil).IsEnabled() {
		t.Error("expected nil config to be disabled")
	}
	if !(&Config{RootDomain: "status.nexfar.com.br"}).IsEnabled() {
		t.Error("expected tenancy to be enabled when root domain is set")
	}
}

func TestConfig_TenantFromHost(t *testing.T) {
	cfg := &Config{RootDomain: "status.nexfar.com.br"}
	scenarios := []struct {
		name     string
		host     string
		expected string
	}{
		{name: "apex", host: "status.nexfar.com.br", expected: ""},
		{name: "apex-with-port", host: "status.nexfar.com.br:8080", expected: ""},
		{name: "tenant", host: "navarromed.status.nexfar.com.br", expected: "navarromed"},
		{name: "tenant-uppercase", host: "NavarroMed.status.nexfar.com.br", expected: "navarromed"},
		{name: "tenant-with-port", host: "plena.status.nexfar.com.br:8080", expected: "plena"},
		{name: "tenant-with-hyphen", host: "navarro-med.status.nexfar.com.br", expected: "navarro-med"},
		{name: "deeper-subdomain", host: "a.b.status.nexfar.com.br", expected: "a-b"},
		{name: "localhost", host: "localhost", expected: ""},
		{name: "localhost-with-port", host: "localhost:8080", expected: ""},
		{name: "ip", host: "10.0.0.1", expected: ""},
		{name: "unrelated-domain", host: "example.com", expected: ""},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if got := cfg.TenantFromHost(scenario.host); got != scenario.expected {
				t.Errorf("TenantFromHost(%q) = %q, expected %q", scenario.host, got, scenario.expected)
			}
		})
	}
}

func TestConfig_TenantFromHost_Disabled(t *testing.T) {
	cfg := &Config{}
	if got := cfg.TenantFromHost("navarromed.status.nexfar.com.br"); got != "" {
		t.Errorf("expected empty tenant when disabled, got %q", got)
	}
}
