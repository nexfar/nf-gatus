package client

import (
	"encoding/json"
	"net/url"
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
	for _, absent := range []string{"connections", "longest_running_query_seconds", "longest_idle_in_transaction_seconds", "blocked_sessions"} {
		if strings.Contains(got, absent) {
			t.Errorf("field %q must be omitted when uncollectable, got %s", absent, got)
		}
	}
	if !strings.Contains(got, "pg_monitor not granted") {
		t.Errorf("expected metrics_errors to explain the omission, got %s", got)
	}
}

func TestPostgresBody_OmitsAllMetricsWhenMetricsQueryFails(t *testing.T) {
	t.Parallel()
	// Simulates collectPostgresMetrics returning early because the metrics
	// query itself failed (e.g. context deadline exceeded): only the fields
	// set before the query ran are populated, everything the query would
	// have produced must be absent rather than zero-defaulted.
	body := postgresBody{
		ConnectMS:     12,
		ProbeMS:       3,
		Probe:         &probeResult{Rows: 1, Value: int64(1)},
		MetricsErrors: []string{"metrics query failed: context deadline exceeded"},
	}
	marshalled, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	got := string(marshalled)
	for _, absent := range []string{"in_recovery", "replication_lag_seconds", "cache_hit_pct_since_reset", "database_size_bytes"} {
		if strings.Contains(got, absent) {
			t.Errorf("field %q must be omitted when the metrics query failed, got %s", absent, got)
		}
	}
	if !strings.Contains(got, "metrics query failed") {
		t.Errorf("expected metrics_errors to explain the failure, got %s", got)
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

func TestWithPostgresConnectTimeout(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name     string
		dsn      string
		timeout  time.Duration
		expected string
	}{
		{
			name:     "injected-when-absent",
			dsn:      "postgres://gatus:hunter2@db.internal:5432/tenant?sslmode=require",
			timeout:  5 * time.Second,
			expected: "postgres://gatus:hunter2@db.internal:5432/tenant?connect_timeout=5&sslmode=require",
		},
		{
			name:     "injected-when-no-query-string-at-all",
			dsn:      "postgres://db.internal:5432/tenant",
			timeout:  5 * time.Second,
			expected: "postgres://db.internal:5432/tenant?connect_timeout=5",
		},
		{
			name:     "explicit-value-is-never-overridden",
			dsn:      "postgres://db.internal:5432/tenant?connect_timeout=30",
			timeout:  2 * time.Second,
			expected: "postgres://db.internal:5432/tenant?connect_timeout=30",
		},
		{
			name:     "explicit-empty-value-is-still-an-operator-choice",
			dsn:      "postgres://db.internal:5432/tenant?connect_timeout=",
			timeout:  2 * time.Second,
			expected: "postgres://db.internal:5432/tenant?connect_timeout=",
		},
		{
			name:     "rounds-up-to-the-next-whole-second",
			dsn:      "postgresql://db.internal/tenant",
			timeout:  2500 * time.Millisecond,
			expected: "postgresql://db.internal/tenant?connect_timeout=3",
		},
		{
			name:     "floors-at-one-second",
			dsn:      "postgres://db.internal/tenant",
			timeout:  10 * time.Millisecond,
			expected: "postgres://db.internal/tenant?connect_timeout=1",
		},
		{
			name:     "non-positive-timeout-also-floors-at-one-second",
			dsn:      "postgres://db.internal/tenant",
			timeout:  0,
			expected: "postgres://db.internal/tenant?connect_timeout=1",
		},
		{
			name:     "unparseable-dsn-is-returned-unchanged",
			dsn:      "postgres://%zz",
			timeout:  5 * time.Second,
			expected: "postgres://%zz",
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			if got := withPostgresConnectTimeout(scenario.dsn, scenario.timeout); got != scenario.expected {
				t.Errorf("withPostgresConnectTimeout(%q, %s) = %q, expected %q", scenario.dsn, scenario.timeout, got, scenario.expected)
			}
		})
	}
}

func TestWithPostgresConnectTimeout_PreservesCredentialsAndDatabase(t *testing.T) {
	t.Parallel()
	got := withPostgresConnectTimeout("postgres://gatus:hunter2@db.internal:5432/tenant", 2*time.Second)
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result is no longer parseable: %s", err)
	}
	if parsed.User.Username() != "gatus" {
		t.Errorf("username was mangled, got %q", parsed.User.Username())
	}
	if password, _ := parsed.User.Password(); password != "hunter2" {
		t.Errorf("password was mangled, got %q", password)
	}
	if parsed.Path != "/tenant" {
		t.Errorf("database was mangled, got %q", parsed.Path)
	}
	if parsed.Query().Get("connect_timeout") != "2" {
		t.Errorf("expected connect_timeout=2, got %q", parsed.Query().Get("connect_timeout"))
	}
}

// TestPostgresBody_CacheHitIsAPercentage pins the unit of the field. A 0..1
// ratio is unusable in a condition: sanitizeAndResolveNumerical casts both
// sides of a comparison to int64, so the value and its threshold both truncate
// to 0 and no threshold works at all. The SQL expression must keep the * 100.
func TestPostgresBody_CacheHitIsAPercentage(t *testing.T) {
	t.Parallel()
	pct := 99.7
	marshalled, err := json.Marshal(postgresBody{CacheHitPctSinceReset: &pct})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	got := string(marshalled)
	if !strings.Contains(got, `"cache_hit_pct_since_reset":99.7`) {
		t.Errorf("expected cache_hit_pct_since_reset in body, got %s", got)
	}
	if strings.Contains(got, "cache_hit_ratio") {
		t.Errorf("the old ratio field name must be gone, got %s", got)
	}
	if !strings.Contains(postgresMetricsQuery, "AS cache_hit_pct_since_reset") {
		t.Error("the column alias must match the body field name")
	}
	if !strings.Contains(postgresMetricsQuery, "100 * sum(blks_hit)") {
		t.Error("the metrics query must emit a percentage, not a 0..1 ratio")
	}
}
