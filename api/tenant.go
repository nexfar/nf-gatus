package api

import (
	"strings"

	"github.com/TwiN/gatus/v5/config/key"
	"github.com/TwiN/gatus/v5/config/tenancy"
	"github.com/gofiber/fiber/v2"
)

// tenancyConfig holds the active tenancy configuration. It is (re)assigned in
// createRouter, which is also re-run on every hot-reload of the configuration.
// When nil (e.g. in unit tests that build handlers directly), tenancy is disabled
// and no filtering is applied.
var tenancyConfig *tenancy.Config

// tenantFromRequest returns the tenant group slug derived from the request's host,
// or an empty string when the request is not scoped to a tenant.
func tenantFromRequest(c *fiber.Ctx) string {
	if tenancyConfig == nil {
		return ""
	}
	return tenancyConfig.TenantFromHost(c.Hostname())
}

// statusBelongsToTenant reports whether an endpoint/suite with the given key is
// visible to the provided tenant. An empty tenant (apex/unscoped) sees everything.
func statusBelongsToTenant(statusKey, tenant string) bool {
	if tenant == "" {
		return true
	}
	return key.ExtractGroupFromKey(strings.ToLower(statusKey)) == tenant
}

// denyKeyForTenant returns true if the request's tenant is not allowed to access the
// endpoint/suite identified by key. Handlers should respond with 404 when this is
// true, so that the existence of another tenant's endpoints is not leaked.
func denyKeyForTenant(c *fiber.Ctx, statusKey string) bool {
	return !statusBelongsToTenant(statusKey, tenantFromRequest(c))
}
