package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/TwiN/gatus/v5/client"
	"github.com/TwiN/gatus/v5/config"
	"github.com/TwiN/gatus/v5/config/endpoint"
	"github.com/TwiN/gatus/v5/config/remote"
	"github.com/TwiN/gatus/v5/storage/store"
	"github.com/TwiN/gatus/v5/storage/store/common"
	"github.com/TwiN/gatus/v5/storage/store/common/paging"
	"github.com/TwiN/logr"
	"github.com/gofiber/fiber/v2"
)

// EndpointStatuses handles requests to retrieve all EndpointStatus
// Due to how intensive this operation can be on the storage, this function leverages a cache.
func EndpointStatuses(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		page, pageSize := extractPageAndPageSizeFromRequest(c, cfg.Storage.MaximumNumberOfResults)
		// tenant scopes the response to a single group based on the request's subdomain ("" = full view)
		tenant := tenantFromRequest(c)
		cacheKey := fmt.Sprintf("endpoint-status-%s-%d-%d", tenant, page, pageSize)
		value, exists := cache.Get(cacheKey)
		var data []byte
		if !exists {
			endpointStatuses, err := store.Get().GetAllEndpointStatuses(paging.NewEndpointStatusParams().WithResults(page, pageSize))
			if err != nil {
				logr.Errorf("[api.EndpointStatuses] Failed to retrieve endpoint statuses: %s", err.Error())
				return c.Status(500).SendString(err.Error())
			}
			// ALPHA: Retrieve endpoint statuses from remote instances
			if endpointStatusesFromRemote, err := getEndpointStatusesFromRemoteInstances(cfg.Remote); err != nil {
				logr.Errorf("[handler.EndpointStatuses] Silently failed to retrieve endpoint statuses from remote: %s", err.Error())
			} else if endpointStatusesFromRemote != nil {
				endpointStatuses = append(endpointStatuses, endpointStatusesFromRemote...)
			}
			// Filter down to the tenant's group when the request is scoped to a subdomain
			endpointStatuses = filterEndpointStatusesByTenant(endpointStatuses, tenant)
			// Marshal endpoint statuses to JSON
			data, err = json.Marshal(endpointStatuses)
			if err != nil {
				logr.Errorf("[api.EndpointStatuses] Unable to marshal object to JSON: %s", err.Error())
				return c.Status(500).SendString("unable to marshal object to JSON")
			}
			cache.SetWithTTL(cacheKey, data, cacheTTL)
		} else {
			data = value.([]byte)
		}
		c.Set("Content-Type", "application/json")
		return c.Status(200).Send(data)
	}
}

// filterEndpointStatusesByTenant returns only the endpoint statuses visible to the
// given tenant. An empty tenant (apex/unscoped request) returns the input unchanged.
func filterEndpointStatusesByTenant(endpointStatuses []*endpoint.Status, tenant string) []*endpoint.Status {
	if tenant == "" {
		return endpointStatuses
	}
	filtered := make([]*endpoint.Status, 0, len(endpointStatuses))
	for _, endpointStatus := range endpointStatuses {
		if statusBelongsToTenant(endpointStatus.Key, tenant) {
			filtered = append(filtered, endpointStatus)
		}
	}
	return filtered
}

func getEndpointStatusesFromRemoteInstances(remoteConfig *remote.Config) ([]*endpoint.Status, error) {
	if remoteConfig == nil || len(remoteConfig.Instances) == 0 {
		return nil, nil
	}
	var endpointStatusesFromAllRemotes []*endpoint.Status
	httpClient := client.GetHTTPClient(remoteConfig.ClientConfig)
	for _, instance := range remoteConfig.Instances {
		response, err := httpClient.Get(instance.URL)
		if err != nil {
			// Log the error but continue with other instances
			logr.Errorf("[api.getEndpointStatusesFromRemoteInstances] Failed to retrieve endpoint statuses from %s: %s", instance.URL, err.Error())
			continue
		}
		var endpointStatuses []*endpoint.Status
		if err = json.NewDecoder(response.Body).Decode(&endpointStatuses); err != nil {
			_ = response.Body.Close()
			logr.Errorf("[api.getEndpointStatusesFromRemoteInstances] Failed to decode endpoint statuses from %s: %s", instance.URL, err.Error())
			continue
		}
		_ = response.Body.Close()
		for _, endpointStatus := range endpointStatuses {
			endpointStatus.Name = instance.EndpointPrefix + endpointStatus.Name
		}
		endpointStatusesFromAllRemotes = append(endpointStatusesFromAllRemotes, endpointStatuses...)
	}
	// Only return nil, error if no remote instances were successfully processed
	if len(endpointStatusesFromAllRemotes) == 0 && remoteConfig.Instances != nil {
		return nil, fmt.Errorf("failed to retrieve endpoint statuses from all remote instances")
	}
	return endpointStatusesFromAllRemotes, nil
}

// EndpointStatus retrieves a single endpoint.Status by group and endpoint name
func EndpointStatus(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		page, pageSize := extractPageAndPageSizeFromRequest(c, cfg.Storage.MaximumNumberOfResults)
		key, err := url.QueryUnescape(c.Params("key"))
		if err != nil {
			logr.Errorf("[api.EndpointStatus] Failed to decode key: %s", err.Error())
			return c.Status(400).SendString("invalid key encoding")
		}
		if denyKeyForTenant(c, key) {
			return c.Status(404).SendString("not found")
		}
		endpointStatus, err := store.Get().GetEndpointStatusByKey(key, paging.NewEndpointStatusParams().WithResults(page, pageSize).WithEvents(1, cfg.Storage.MaximumNumberOfEvents))
		if err != nil {
			if errors.Is(err, common.ErrEndpointNotFound) {
				return c.Status(404).SendString(err.Error())
			}
			logr.Errorf("[api.EndpointStatus] Failed to retrieve endpoint status: %s", err.Error())
			return c.Status(500).SendString(err.Error())
		}
		if endpointStatus == nil { // XXX: is this check necessary?
			logr.Errorf("[api.EndpointStatus] Endpoint with key=%s not found", key)
			return c.Status(404).SendString("not found")
		}
		output, err := json.Marshal(endpointStatus)
		if err != nil {
			logr.Errorf("[api.EndpointStatus] Unable to marshal object to JSON: %s", err.Error())
			return c.Status(500).SendString("unable to marshal object to JSON")
		}
		c.Set("Content-Type", "application/json")
		return c.Status(200).Send(output)
	}
}
