package api

import (
	"strings"

	"github.com/TwiN/gatus/v5/config"
	"github.com/TwiN/gatus/v5/config/key"
	"github.com/TwiN/gatus/v5/config/tenancy"
	"github.com/gofiber/fiber/v2"
)

// tenancyConfig holds the active tenancy configuration. It is (re)assigned in
// createRouter, which is also re-run on every hot-reload of the configuration.
// When nil (e.g. in unit tests that build handlers directly), tenancy is disabled
// and no filtering is applied.
var tenancyConfig *tenancy.Config

// tenantPublicKeys holds the keys of every endpoint/suite whose visibility is
// "public". Tenant-scoped (subdomain) requests only see keys present in this set;
// the apex/unscoped view is unaffected. Like tenancyConfig, it is rebuilt in
// createRouter on startup and on every hot-reload of the configuration.
var tenantPublicKeys map[string]bool

// configureTenancy (re)initializes the tenancy state used by request handlers from
// the active configuration.
func configureTenancy(cfg *config.Config) {
	tenancyConfig = cfg.Tenancy
	if !cfg.Tenancy.IsEnabled() {
		tenantPublicKeys = nil
		return
	}
	publicKeys := make(map[string]bool)
	for _, ep := range cfg.Endpoints {
		if ep.Visibility.IsPublic() {
			publicKeys[ep.Key()] = true
		}
	}
	for _, externalEndpoint := range cfg.ExternalEndpoints {
		if externalEndpoint.Visibility.IsPublic() {
			publicKeys[externalEndpoint.Key()] = true
		}
	}
	for _, s := range cfg.Suites {
		if s.Visibility.IsPublic() {
			publicKeys[s.Key()] = true
		}
	}
	tenantPublicKeys = publicKeys
}

// tenantFromRequest returns the tenant group slug derived from the request's host,
// or an empty string when the request is not scoped to a tenant.
func tenantFromRequest(c *fiber.Ctx) string {
	if tenancyConfig == nil {
		return ""
	}
	return tenancyConfig.TenantFromHost(c.Hostname())
}

// statusBelongsToTenant reports whether an endpoint/suite with the given key is
// visible to the provided tenant. An empty tenant (apex/unscoped) sees everything;
// a tenant only sees keys that belong to its group AND are marked public.
func statusBelongsToTenant(statusKey, tenant string) bool {
	if tenant == "" {
		return true
	}
	statusKey = strings.ToLower(statusKey)
	if key.ExtractGroupFromKey(statusKey) != tenant {
		return false
	}
	return tenantPublicKeys[statusKey]
}

// denyKeyForTenant returns true if the request's tenant is not allowed to access the
// endpoint/suite identified by key. Handlers should respond with 404 when this is
// true, so that the existence of another tenant's endpoints is not leaked.
func denyKeyForTenant(c *fiber.Ctx, statusKey string) bool {
	return !statusBelongsToTenant(statusKey, tenantFromRequest(c))
}
