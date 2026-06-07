// Package tenancy provides multi-tenant filtering of the dashboard based on the
// subdomain of the incoming request.
//
// When a RootDomain is configured (e.g. "status.nexfar.com.br"), a request to a
// subdomain of that root (e.g. "navarromed.status.nexfar.com.br") is scoped to the
// endpoint/suite group whose sanitized name matches the subdomain label
// ("navarromed"). Requests to the root domain itself, or to any host that is not a
// subdomain of the root (localhost, an IP, a direct hostname during development),
// are not scoped and see every group.
package tenancy

import (
	"strings"

	"github.com/TwiN/gatus/v5/config/key"
)

// Config is the configuration for subdomain-based multi-tenancy.
type Config struct {
	// RootDomain is the apex domain the dashboard is served from, without any
	// subdomain (e.g. "status.nexfar.com.br"). When empty, multi-tenancy is disabled
	// and every request sees every group.
	RootDomain string `yaml:"root-domain,omitempty"`
}

// ValidateAndSetDefaults validates the tenancy configuration and normalizes it.
func (c *Config) ValidateAndSetDefaults() error {
	c.RootDomain = strings.ToLower(strings.Trim(strings.TrimSpace(c.RootDomain), "."))
	return nil
}

// IsEnabled returns whether subdomain-based tenancy is active.
func (c *Config) IsEnabled() bool {
	return c != nil && len(c.RootDomain) > 0
}

// TenantFromHost returns the tenant group slug for a request host, or an empty
// string when the request is not scoped to a tenant (apex domain, non-matching
// host, or tenancy disabled). The returned slug is sanitized the same way group
// names are, so it can be compared directly against a group's key prefix.
func (c *Config) TenantFromHost(host string) string {
	if !c.IsEnabled() {
		return ""
	}
	// Strip the port, if any, and normalize.
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "."))
	if host == c.RootDomain {
		// Apex domain: full view.
		return ""
	}
	suffix := "." + c.RootDomain
	if !strings.HasSuffix(host, suffix) {
		// Not a subdomain of the root domain (localhost, IP, direct hostname, ...): full view.
		return ""
	}
	label := strings.TrimSuffix(host, suffix)
	return key.ConvertGroupToKey(label)
}
