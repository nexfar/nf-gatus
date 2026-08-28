package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TwiN/gatus/v5/alerting"
	"github.com/TwiN/gatus/v5/alerting/alert"
	"github.com/TwiN/gatus/v5/alerting/provider/discord"
	"github.com/TwiN/gatus/v5/config"
	"github.com/TwiN/gatus/v5/config/endpoint"
	"github.com/TwiN/gatus/v5/config/maintenance"
	"github.com/TwiN/gatus/v5/storage/store"
	"github.com/TwiN/gatus/v5/storage/store/common/paging"
)

func TestCreateExternalEndpointResult(t *testing.T) {
	defer store.Get().Clear()
	defer cache.Clear()
	cfg := &config.Config{
		Alerting: &alerting.Config{
			Discord: &discord.AlertProvider{},
		},
		ExternalEndpoints: []*endpoint.ExternalEndpoint{
			{
				Name:  "n",
				Group: "g",
				Token: "token",
				Alerts: []*alert.Alert{
					{
						Type:             alert.TypeDiscord,
						FailureThreshold: 2,
						SuccessThreshold: 2,
					},
				},
			},
		},
		Maintenance: &maintenance.Config{},
	}
	api := New(cfg)
	router := api.Router()
	scenarios := []struct {
		Name                           string
		Path                           string
		AuthorizationHeaderBearerToken string
		ExpectedCode                   int
	}{
		{
			Name:                           "no-token",
			Path:                           "/api/v1/endpoints/g_n/external?success=true",
			AuthorizationHeaderBearerToken: "",
			ExpectedCode:                   401,
		},
		{
			Name:                           "bad-token",
			Path:                           "/api/v1/endpoints/g_n/external?success=true",
			AuthorizationHeaderBearerToken: "Bearer bad-token",
			ExpectedCode:                   401,
		},
		{
			Name:                           "bad-key",
			Path:                           "/api/v1/endpoints/bad_key/external?success=true",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   404,
		},
		{
			Name:                           "bad-success-value",
			Path:                           "/api/v1/endpoints/g_n/external?success=invalid",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   400,
		},
		{
			Name:                           "bad-duration-value",
			Path:                           "/api/v1/endpoints/g_n/external?success=true&duration=invalid",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   400,
		},
		{
			Name:                           "good-token-success-true",
			Path:                           "/api/v1/endpoints/g_n/external?success=true",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   200,
		},
		{
			Name:                           "good-token-success-true-with-ignored-error-because-success-true",
			Path:                           "/api/v1/endpoints/g_n/external?success=true&error=failed",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   200,
		},
		{
			Name:                           "good-duration-success-true",
			Path:                           "/api/v1/endpoints/g_n/external?success=true&duration=10s",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   200,
		},
		{
			Name:                           "good-token-success-false",
			Path:                           "/api/v1/endpoints/g_n/external?success=false",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   200,
		},
		{
			Name:                           "good-token-success-false-again",
			Path:                           "/api/v1/endpoints/g_n/external?success=false",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   200,
		},
		{
			Name:                           "good-token-success-false-with-error",
			Path:                           "/api/v1/endpoints/g_n/external?success=false&error=failed",
			AuthorizationHeaderBearerToken: "Bearer token",
			ExpectedCode:                   200,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			request := httptest.NewRequest("POST", scenario.Path, http.NoBody)
			if len(scenario.AuthorizationHeaderBearerToken) > 0 {
				request.Header.Set("Authorization", scenario.AuthorizationHeaderBearerToken)
			}
			response, err := router.Test(request)
			if err != nil {
				return
			}
			defer response.Body.Close()
			if response.StatusCode != scenario.ExpectedCode {
				t.Errorf("%s %s should have returned %d, but returned %d instead", request.Method, request.URL, scenario.ExpectedCode, response.StatusCode)
			}
		})
	}
	t.Run("verify-end-results", func(t *testing.T) {
		endpointStatus, err := store.Get().GetEndpointStatus("g", "n", paging.NewEndpointStatusParams().WithResults(1, 11))
		if err != nil {
			t.Errorf("failed to get endpoint status: %s", err.Error())
			return
		}
		if endpointStatus.Key != "g_n" {
			t.Errorf("expected key to be g_n but got %s", endpointStatus.Key)
		}
		if len(endpointStatus.Results) != 6 {
			t.Errorf("expected 6 results but got %d", len(endpointStatus.Results))
		}
		if !endpointStatus.Results[0].Success {
			t.Errorf("expected first result to be successful")
		}
		if !endpointStatus.Results[1].Success {
			t.Errorf("expected second result to be successful")
		}
		if len(endpointStatus.Results[1].Errors) > 0 {
			t.Errorf("expected second result to have no errors")
		}
		if endpointStatus.Results[2].Duration == 0 || endpointStatus.Results[2].Duration.Seconds() != 10 {
			t.Errorf("expected third result to have a duration of 10 seconds")
		}
		if endpointStatus.Results[3].Success {
			t.Errorf("expected fourth result to be unsuccessful")
		}
		if endpointStatus.Results[4].Success {
			t.Errorf("expected fifth result to be unsuccessful")
		}
		if endpointStatus.Results[5].Success {
			t.Errorf("expected sixth result to be unsuccessful")
		}
		if len(endpointStatus.Results[5].Errors) == 0 || endpointStatus.Results[5].Errors[0] != "failed" {
			t.Errorf("expected sixth result to have errors: failed")
		}
		externalEndpointFromConfig := cfg.GetExternalEndpointByKey("g_n")
		if externalEndpointFromConfig.NumberOfFailuresInARow != 3 {
			t.Errorf("expected 3 failures in a row but got %d", externalEndpointFromConfig.NumberOfFailuresInARow)
		}
		if externalEndpointFromConfig.NumberOfSuccessesInARow != 0 {
			t.Errorf("expected 0 successes in a row but got %d", externalEndpointFromConfig.NumberOfSuccessesInARow)
		}
	})
}

func TestCreateExternalEndpointResultWithMessage(t *testing.T) {
	defer store.Get().Clear()
	defer cache.Clear()
	cfg := &config.Config{
		Alerting:    &alerting.Config{},
		Maintenance: &maintenance.Config{},
		ExternalEndpoints: []*endpoint.ExternalEndpoint{
			{Name: "nm", Group: "gm", Token: "token"},
		},
	}
	api := New(cfg)
	router := api.Router()
	longMessage := strings.Repeat("a", maximumMessageLength+50)
	scenarios := []struct {
		Name            string
		Path            string
		ExpectedMessage string
	}{
		{
			Name:            "message-is-kept-on-success",
			Path:            "/api/v1/endpoints/gm_nm/external?success=true&message=1523+registros+sincronizados",
			ExpectedMessage: "1523 registros sincronizados",
		},
		{
			Name:            "message-is-kept-on-failure-alongside-error",
			Path:            "/api/v1/endpoints/gm_nm/external?success=false&error=boom&message=lote+parcial",
			ExpectedMessage: "lote parcial",
		},
		{
			Name:            "message-longer-than-maximum-is-truncated",
			Path:            "/api/v1/endpoints/gm_nm/external?success=true&message=" + longMessage,
			ExpectedMessage: strings.Repeat("a", maximumMessageLength),
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			request := httptest.NewRequest("POST", scenario.Path, http.NoBody)
			request.Header.Set("Authorization", "Bearer token")
			response, err := router.Test(request)
			if err != nil {
				t.Fatalf("failed to send request: %s", err.Error())
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("%s %s should have returned 200, but returned %d instead", request.Method, request.URL, response.StatusCode)
			}
		})
	}
	t.Run("verify-end-results", func(t *testing.T) {
		endpointStatus, err := store.Get().GetEndpointStatus("gm", "nm", paging.NewEndpointStatusParams().WithResults(1, 10))
		if err != nil {
			t.Fatalf("failed to get endpoint status: %s", err.Error())
		}
		if len(endpointStatus.Results) != len(scenarios) {
			t.Fatalf("expected %d results but got %d", len(scenarios), len(endpointStatus.Results))
		}
		for i, scenario := range scenarios {
			if endpointStatus.Results[i].Message != scenario.ExpectedMessage {
				t.Errorf("%s: expected message %q but got %q", scenario.Name, scenario.ExpectedMessage, endpointStatus.Results[i].Message)
			}
		}
		if len(endpointStatus.Results[1].Errors) != 1 || endpointStatus.Results[1].Errors[0] != "boom" {
			t.Errorf("expected the failed result to keep its error, but got %v", endpointStatus.Results[1].Errors)
		}
	})
}
