package client

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestQueryPostgres_MalformedDSN(t *testing.T) {
	t.Parallel()
	connected, _, body, err := QueryPostgres("postgres://%zz", "", false, &Config{Timeout: 5 * time.Second})
	if connected {
		t.Error("should not report connected on a malformed DSN")
	}
	if err == nil {
		t.Fatal("expected an error on a malformed DSN")
	}
	if body != nil {
		t.Error("expected no body when the connection never succeeded")
	}
}

func TestQueryPostgres_ClosedPort(t *testing.T) {
	t.Parallel()
	// Port 1 on loopback has nothing listening.
	connected, duration, _, err := QueryPostgres(
		"postgres://gatus:hunter2@127.0.0.1:1/tenant?sslmode=disable&connect_timeout=2",
		"", false, &Config{Timeout: 3 * time.Second},
	)
	if connected {
		t.Error("should not report connected when nothing is listening")
	}
	if err == nil {
		t.Fatal("expected an error when nothing is listening")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaked the password: %q", err.Error())
	}
	if duration > 10*time.Second {
		t.Errorf("took %s, which suggests the timeout was not honoured", duration)
	}
}

func TestDefaultedProbeQuery(t *testing.T) {
	t.Parallel()
	if got := defaultedProbeQuery("", "SELECT 1"); got != "SELECT 1" {
		t.Errorf("empty query should fall back to the default, got %q", got)
	}
	if got := defaultedProbeQuery("   \n ", "SELECT 1"); got != "SELECT 1" {
		t.Errorf("whitespace-only query should fall back to the default, got %q", got)
	}
	if got := defaultedProbeQuery("SELECT 2", "SELECT 1"); got != "SELECT 2" {
		t.Errorf("explicit query should be preserved, got %q", got)
	}
}

func TestPostgresBody_OmitsUncollectableFields(t *testing.T) {
	t.Parallel()
	body := postgresBody{
		ConnectMS: 12,
		ProbeMS:   3,
		Probe:     &probeResult{Rows: 1, Value: int64(1)},
		Version:   "16.2",
		// Connections and the other pg_monitor-gated fields are left nil,
		// simulating a role without pg_monitor membership.
		MetricsErrors: []string{"pg_monitor not granted"},
	}
	marshalled, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	got := string(marshalled)
	for _, absent := range []string{"connections", "longest_running_query_seconds", "blocked_sessions"} {
		if strings.Contains(got, absent) {
			t.Errorf("field %q must be omitted when uncollectable, got %s", absent, got)
		}
	}
	if !strings.Contains(got, "pg_monitor not granted") {
		t.Errorf("expected metrics_errors to explain the omission, got %s", got)
	}
}

func TestPostgresBody_IncludesCollectedFields(t *testing.T) {
	t.Parallel()
	used, max := int64(42), int64(100)
	longest := 4.2
	body := postgresBody{
		Connections:                &postgresConnections{Used: used, Max: max, UsedPct: 42},
		LongestRunningQuerySeconds: &longest,
	}
	marshalled, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	got := string(marshalled)
	if !strings.Contains(got, `"used_pct":42`) {
		t.Errorf("expected used_pct in body, got %s", got)
	}
	if !strings.Contains(got, `"longest_running_query_seconds":4.2`) {
		t.Errorf("expected longest_running_query_seconds in body, got %s", got)
	}
}
