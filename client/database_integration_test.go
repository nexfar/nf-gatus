package client

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// These tests are the only ones that exercise the real catalog queries and
// command shapes. They are skipped unless pointed at a live server; see
// .examples/docker-compose-database-monitoring for a local stack.

func TestQueryPostgres_Integration(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("GATUS_TEST_POSTGRES_DSN")
	if len(dsn) == 0 {
		t.Skip("GATUS_TEST_POSTGRES_DSN not set")
	}
	connected, duration, body, err := QueryPostgres(dsn, "SELECT 1", true, &Config{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !connected {
		t.Fatal("expected to connect")
	}
	if duration <= 0 {
		t.Error("expected a positive duration")
	}
	var decoded postgresBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %s", err)
	}
	if len(decoded.Version) == 0 {
		t.Error("expected a version")
	}
	if decoded.Probe == nil || decoded.Probe.Rows != 1 {
		t.Errorf("expected one probe row, got %+v", decoded.Probe)
	}
	t.Logf("postgres body: %s", body)
	if decoded.Connections == nil {
		t.Logf("connections absent — grant pg_monitor to exercise that path; metrics_errors=%v", decoded.MetricsErrors)
	}
}

func TestQueryMongoDB_Integration(t *testing.T) {
	t.Parallel()
	uri := os.Getenv("GATUS_TEST_MONGODB_URI")
	if len(uri) == 0 {
		t.Skip("GATUS_TEST_MONGODB_URI not set")
	}
	connected, duration, body, err := QueryMongoDB(uri, "", true, &Config{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !connected {
		t.Fatal("expected to connect")
	}
	if duration <= 0 {
		t.Error("expected a positive duration")
	}
	var decoded mongoBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %s", err)
	}
	if decoded.Backend != "mongodb" {
		t.Errorf("expected backend mongodb, got %q", decoded.Backend)
	}
	if decoded.Connections == nil {
		t.Error("expected real MongoDB to report connections")
	}
	t.Logf("mongodb body: %s", body)
}

// TestQueryFerretDB_Integration confirms the graceful-degradation path against a
// real FerretDB. It asserts the backend label and that the check still succeeds;
// which specific fields FerretDB omits is recorded in the log rather than
// asserted, because that set varies by FerretDB version.
func TestQueryFerretDB_Integration(t *testing.T) {
	t.Parallel()
	uri := os.Getenv("GATUS_TEST_FERRETDB_URI")
	if len(uri) == 0 {
		t.Skip("GATUS_TEST_FERRETDB_URI not set")
	}
	connected, _, body, err := QueryMongoDB(uri, "", true, &Config{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !connected {
		t.Fatal("expected to connect")
	}
	var decoded mongoBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %s", err)
	}
	if decoded.Backend != "ferretdb" {
		t.Errorf("expected backend ferretdb, got %q — update detectMongoBackend", decoded.Backend)
	}
	t.Logf("ferretdb body: %s", body)
	t.Logf("ferretdb metrics_errors: %v", decoded.MetricsErrors)
}
