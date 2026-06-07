package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TwiN/gatus/v5/config"
	"github.com/TwiN/gatus/v5/config/endpoint"
	"github.com/TwiN/gatus/v5/config/tenancy"
	"github.com/TwiN/gatus/v5/storage"
	"github.com/TwiN/gatus/v5/storage/store"
	"github.com/TwiN/gatus/v5/watchdog"
)

func TestTenantFiltering(t *testing.T) {
	defer store.Get().Clear()
	defer cache.Clear()
	cfg := &config.Config{
		Tenancy: &tenancy.Config{RootDomain: "status.nexfar.com.br"},
		Endpoints: []*endpoint.Endpoint{
			{Name: "api", Group: "navarromed"},
			{Name: "api", Group: "plena"},
		},
		Storage: &storage.Config{
			MaximumNumberOfResults: storage.DefaultMaximumNumberOfResults,
			MaximumNumberOfEvents:  storage.DefaultMaximumNumberOfEvents,
		},
	}
	if err := cfg.Tenancy.ValidateAndSetDefaults(); err != nil {
		t.Fatalf("failed to validate tenancy config: %s", err)
	}
	watchdog.UpdateEndpointStatus(cfg.Endpoints[0], &endpoint.Result{Success: true, Duration: time.Millisecond, Timestamp: time.Now()})
	watchdog.UpdateEndpointStatus(cfg.Endpoints[1], &endpoint.Result{Success: true, Duration: time.Millisecond, Timestamp: time.Now()})
	router := New(cfg).Router()

	t.Run("list-apex-shows-all-groups", func(t *testing.T) {
		body := doGet(t, router, "/api/v1/endpoints/statuses", "status.nexfar.com.br")
		if !strings.Contains(body, "navarromed_api") || !strings.Contains(body, "plena_api") {
			t.Errorf("apex should list every group, got: %s", body)
		}
	})

	t.Run("list-tenant-shows-only-its-group", func(t *testing.T) {
		body := doGet(t, router, "/api/v1/endpoints/statuses", "navarromed.status.nexfar.com.br")
		if !strings.Contains(body, "navarromed_api") {
			t.Errorf("tenant should see its own group, got: %s", body)
		}
		if strings.Contains(body, "plena_api") {
			t.Errorf("tenant must not see another group, got: %s", body)
		}
	})

	t.Run("list-unknown-subdomain-shows-nothing", func(t *testing.T) {
		body := doGet(t, router, "/api/v1/endpoints/statuses", "unknown.status.nexfar.com.br")
		if strings.Contains(body, "navarromed_api") || strings.Contains(body, "plena_api") {
			t.Errorf("unknown tenant should see nothing, got: %s", body)
		}
	})

	scenarios := []struct {
		name         string
		path         string
		host         string
		expectedCode int
	}{
		{name: "single-status-own-group", path: "/api/v1/endpoints/navarromed_api/statuses", host: "navarromed.status.nexfar.com.br", expectedCode: http.StatusOK},
		{name: "single-status-other-group-denied", path: "/api/v1/endpoints/plena_api/statuses", host: "navarromed.status.nexfar.com.br", expectedCode: http.StatusNotFound},
		{name: "single-status-apex-allowed", path: "/api/v1/endpoints/plena_api/statuses", host: "status.nexfar.com.br", expectedCode: http.StatusOK},
		{name: "health-badge-own-group", path: "/api/v1/endpoints/navarromed_api/health/badge.svg", host: "navarromed.status.nexfar.com.br", expectedCode: http.StatusOK},
		{name: "health-badge-other-group-denied", path: "/api/v1/endpoints/plena_api/health/badge.svg", host: "navarromed.status.nexfar.com.br", expectedCode: http.StatusNotFound},
		{name: "uptime-raw-other-group-denied", path: "/api/v1/endpoints/plena_api/uptimes/24h", host: "navarromed.status.nexfar.com.br", expectedCode: http.StatusNotFound},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", scenario.path, http.NoBody)
			request.Host = scenario.host
			response, err := router.Test(request)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if response.StatusCode != scenario.expectedCode {
				t.Errorf("GET %s (host=%s) should have returned %d, got %d", scenario.path, scenario.host, scenario.expectedCode, response.StatusCode)
			}
		})
	}
}

func doGet(t *testing.T, router interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, path, host string) string {
	t.Helper()
	request := httptest.NewRequest("GET", path, http.NoBody)
	request.Host = host
	response, err := router.Test(request)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read body: %s", err)
	}
	return string(body)
}
