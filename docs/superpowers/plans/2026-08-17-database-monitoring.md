# Per-tenant Database Monitoring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add native `postgres://` and `mongodb://` endpoint types to nf-gatus so each tenant's databases are probed directly for reachability, latency, a representative query result, and engine health.

**Architecture:** Two new URL schemes in `Endpoint.Type()` dispatch to two new self-contained functions in `client/`, each shaped exactly like `client/grpc.go`'s `PerformGRPCHealthCheck`. Each returns `(connected, duration, body, error)`. The body is JSON, so all health thresholds are expressed as ordinary Gatus conditions in config rather than as Go code. No new config package: the existing `body:` field carries the probe query (as SSH already does), and metric collection is gated by the existing `needsToReadBody()`.

**Tech Stack:** Go 1.x, `github.com/lib/pq` (already a direct dependency), `go.mongodb.org/mongo-driver/v2` (new), standard `database/sql`.

**Spec:** `docs/superpowers/specs/2026-08-17-database-monitoring-design.md`

## Global Constraints

- Import path stays `github.com/TwiN/gatus/v5`. This is a fork that tracks upstream; keep changes additive and confined.
- `[RESPONSE_TIME]` = connection establishment + probe query. Metric collection time is **excluded** and reported separately as `metrics_ms`.
- Metrics are gauges and instantaneous measurements only. Never expose a cumulative counter — Gatus checks are stateless and a counter has no previous value to diff against.
- A metric that cannot be collected is **omitted from the body**, never defaulted to a zero value. A missing field fails its condition visibly; a wrong field passes a threshold on fiction.
- Metric-collection failure must **not** fail the check. Only connection failure and probe-query failure fail the check.
- Connections are opened and closed on every check. No pooling, no package-level state — `listenToConfigurationFileChanges` re-runs the full reload cycle in-process.
- Any credentials appearing in an error string must be redacted before the error reaches `result.Errors`, which is serialised to the API.
- PostgreSQL 10+ is the floor (`backend_type` column and `pg_monitor` role both landed in 10).
- Tests use `t.Parallel()` where safe, matching `client/client_test.go`.
- Run `go mod tidy` after dependency changes. Note: `CLAUDE.md` claims `vendor/` is committed, but no `vendor/` directory exists in this repo — do **not** run `go mod vendor` and do not create one.

---

### Task 1: PostgreSQL liveness, latency, and probe result

Delivers a working `postgres://` endpoint type: connects, runs a probe query, reports whether it connected and how long it took, and exposes the probe result in the body. No engine metrics yet.

**Files:**
- Create: `client/dsn.go`
- Create: `client/dsn_test.go`
- Create: `client/postgres.go`
- Create: `client/postgres_test.go`
- Modify: `config/endpoint/endpoint.go` (type constant block at ~line 46; `Type()` at ~line 174; `call()` at ~line 486)
- Modify: `config/endpoint/endpoint_test.go` (the `Type()` scenario table at ~line 360)

**Interfaces:**
- Consumes: `client.Config` (`client/config.go`), `client.GetDefaultConfig()`.
- Produces:
  - `client.QueryPostgres(dsn, probeQuery string, collectMetrics bool, cfg *Config) (bool, time.Duration, []byte, error)`
  - `client.redactCredentials(s string) string` and `client.redactError(err error) error` — used by Task 3.
  - `endpoint.TypePostgres endpoint.Type = "POSTGRES"`
  - Struct `client.postgresBody` with fields `ConnectMS`, `ProbeMS`, `MetricsMS`, `Probe`, `MetricsErrors` — extended by Task 2.

---

- [ ] **Step 1: Write the failing test for credential redaction**

Create `client/dsn_test.go`:

```go
package client

import "testing"

func TestRedactCredentials(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "postgres-dsn-with-password",
			input:    `dial error: postgres://gatus_monitor:hunter2@db.internal:5432/tenant?sslmode=require`,
			expected: `dial error: postgres://***:***@db.internal:5432/tenant?sslmode=require`,
		},
		{
			name:     "mongodb-dsn-with-password",
			input:    `server selection error: mongodb://admin:s3cr3t@mongo.internal:27017/tenant`,
			expected: `server selection error: mongodb://***:***@mongo.internal:27017/tenant`,
		},
		{
			name:     "mongodb-srv-dsn",
			input:    `mongodb+srv://admin:s3cr3t@cluster.example.net/tenant failed`,
			expected: `mongodb+srv://***:***@cluster.example.net/tenant failed`,
		},
		{
			name:     "user-without-password",
			input:    `postgres://gatus_monitor@db.internal:5432/tenant`,
			expected: `postgres://***:***@db.internal:5432/tenant`,
		},
		{
			name:     "no-credentials-left-untouched",
			input:    `connection refused to db.internal:5432`,
			expected: `connection refused to db.internal:5432`,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			if got := redactCredentials(scenario.input); got != scenario.expected {
				t.Errorf("redactCredentials(%q) = %q, expected %q", scenario.input, got, scenario.expected)
			}
		})
	}
}

func TestRedactError(t *testing.T) {
	t.Parallel()
	if redactError(nil) != nil {
		t.Error("expected nil error to stay nil")
	}
	err := redactError(errors.New(`failed: postgres://u:p@h:5432/d`))
	if err.Error() != `failed: postgres://***:***@h:5432/d` {
		t.Errorf("credentials not redacted, got %q", err.Error())
	}
}
```

Add `"errors"` to that file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./client/ -run 'TestRedact' -v`
Expected: FAIL — `undefined: redactCredentials`, `undefined: redactError`.

- [ ] **Step 3: Implement redaction**

Create `client/dsn.go`:

```go
package client

import (
	"errors"
	"regexp"
)

// dsnCredentialsPattern matches the userinfo portion of a URI, e.g. the
// "user:password@" in "postgres://user:password@host:5432/db".
var dsnCredentialsPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)([^/\s:@]+)(:[^/\s@]*)?@`)

// redactCredentials replaces any URI userinfo in s with "***:***", so that a
// connection string embedded in an error message cannot leak a password.
// Result.Errors is serialised to the API, so every driver error must pass
// through this before being surfaced.
func redactCredentials(s string) string {
	return dsnCredentialsPattern.ReplaceAllString(s, "${1}***:***@")
}

// redactError returns err with any credentials in its message redacted.
func redactError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(redactCredentials(err.Error()))
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./client/ -run 'TestRedact' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing test for endpoint type recognition**

In `config/endpoint/endpoint_test.go`, find the scenario table in the `Type()` test (~line 360) and add these entries alongside the existing ones:

```go
		{
			name: "postgres",
			endpoint: Endpoint{URL: "postgres://user:pw@db.internal:5432/tenant"},
			want: TypePostgres,
		},
		{
			name: "postgresql",
			endpoint: Endpoint{URL: "postgresql://db.internal/tenant"},
			want: TypePostgres,
		},
```

Match the exact field names used by the surrounding scenarios in that table — read them first rather than assuming.

- [ ] **Step 6: Run the test to verify it fails**

Run: `go test ./config/endpoint/ -run 'TestEndpoint_Type' -v`
Expected: FAIL — `undefined: TypePostgres`.

- [ ] **Step 7: Add the type constant and the Type() cases**

In `config/endpoint/endpoint.go`, add to the constant block (after `TypeSSH`, before `TypeUNKNOWN`):

```go
	TypePostgres Type = "POSTGRES"
```

In `Type()`, add a case before the `default:` arm:

```go
	case strings.HasPrefix(e.URL, "postgres://") || strings.HasPrefix(e.URL, "postgresql://"):
		return TypePostgres
```

There is no ordering trap: `"postgresql://x"` does not carry the prefix `"postgres://"`, because the character after `postgres` is `q`, not `:`.

- [ ] **Step 8: Run the test to verify it passes**

Run: `go test ./config/endpoint/ -run 'TestEndpoint_Type' -v`
Expected: PASS.

- [ ] **Step 9: Write the failing tests for QueryPostgres failure paths**

Create `client/postgres_test.go`:

```go
package client

import (
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
```

- [ ] **Step 10: Run the tests to verify they fail**

Run: `go test ./client/ -run 'TestQueryPostgres|TestDefaultedProbeQuery' -v`
Expected: FAIL — `undefined: QueryPostgres`, `undefined: defaultedProbeQuery`.

- [ ] **Step 11: Implement QueryPostgres**

Create `client/postgres.go`:

```go
package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq" // registers the "postgres" driver
)

// defaultPostgresProbeQuery is used when an endpoint sets no body.
const defaultPostgresProbeQuery = "SELECT 1"

// probeResult is the outcome of the probe query, exposed in the body so that a
// representative read query can be asserted on with [BODY].probe.value.
type probeResult struct {
	Rows  int  `json:"rows"`
	Value any  `json:"value"`
}

// postgresBody is the JSON body exposed to conditions via [BODY].
// Fields that could not be collected are omitted rather than defaulted: a
// missing field fails its condition visibly, a zero value passes on fiction.
type postgresBody struct {
	ConnectMS float64      `json:"connect_ms"`
	ProbeMS   float64      `json:"probe_ms"`
	MetricsMS float64      `json:"metrics_ms,omitempty"`
	Probe     *probeResult `json:"probe,omitempty"`

	MetricsErrors []string `json:"metrics_errors,omitempty"`
}

// defaultedProbeQuery returns query, or fallback when query is blank.
func defaultedProbeQuery(query, fallback string) string {
	if len(strings.TrimSpace(query)) == 0 {
		return fallback
	}
	return query
}

// QueryPostgres connects to a PostgreSQL database, runs a probe query, and
// optionally collects engine health metrics.
//
// The returned duration covers connection establishment plus the probe query.
// Metric collection is deliberately excluded and reported separately as
// metrics_ms, so that [RESPONSE_TIME] stays a function of the database rather
// than of how many conditions were written.
//
// The body is nil unless collectMetrics is true.
func QueryPostgres(dsn, probeQuery string, collectMetrics bool, cfg *Config) (bool, time.Duration, []byte, error) {
	if cfg == nil {
		cfg = GetDefaultConfig()
	}
	probeQuery = defaultedProbeQuery(probeQuery, defaultPostgresProbeQuery)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return false, 0, nil, fmt.Errorf("failed to open postgres connection: %w", redactError(err))
	}
	defer db.Close()
	// One connection serves both the ping and the probe. The handle is closed
	// per check (deferred above), so nothing pools across checks — capping idle
	// connections at 0 would only force a redundant second dial mid-check and
	// fold its handshake into probe_ms.
	db.SetMaxOpenConns(1)
	start := time.Now()
	if err := db.PingContext(ctx); err != nil {
		return false, time.Since(start), nil, fmt.Errorf("failed to connect: %w", redactError(err))
	}
	connectDuration := time.Since(start)
	probe, err := runPostgresProbe(ctx, db, probeQuery)
	if err != nil {
		// The connection succeeded, so report connected=true: this is a query
		// failure, not an outage of the listener.
		return true, time.Since(start), nil, fmt.Errorf("probe query failed: %w", redactError(err))
	}
	duration := time.Since(start)
	if !collectMetrics {
		return true, duration, nil, nil
	}
	body := postgresBody{
		ConnectMS: float64(connectDuration.Microseconds()) / 1000,
		ProbeMS:   float64((duration - connectDuration).Microseconds()) / 1000,
		Probe:     probe,
	}
	marshalled, err := json.Marshal(body)
	if err != nil {
		return true, duration, nil, fmt.Errorf("failed to marshal body: %w", err)
	}
	return true, duration, marshalled, nil
}

// runPostgresProbe executes the probe query and captures the first column of
// the first row. Any number of columns and rows is accepted, so an arbitrary
// representative query can be used as the probe.
func runPostgresProbe(ctx context.Context, db *sql.DB, query string) (*probeResult, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := &probeResult{}
	for rows.Next() {
		result.Rows++
		if result.Rows > 1 {
			continue
		}
		scanned := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range scanned {
			pointers[i] = &scanned[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		if len(scanned) > 0 {
			result.Value = normalizeScannedValue(scanned[0])
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// normalizeScannedValue converts driver-native types into something that
// marshals to a useful JSON scalar. lib/pq returns []byte for several types,
// which would otherwise become a base64 string.
func normalizeScannedValue(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}
```

- [ ] **Step 12: Run the tests to verify they pass**

Run: `go test ./client/ -run 'TestQueryPostgres|TestDefaultedProbeQuery|TestRedact' -v`
Expected: PASS.

- [ ] **Step 13: Wire the dispatch in call()**

In `config/endpoint/endpoint.go`, in `call()`, add a branch immediately before the final `} else {` (the HTTP fallback):

```go
	} else if endpointType == TypePostgres {
		result.Connected, result.Duration, result.Body, err = client.QueryPostgres(e.URL, e.getParsedBody(), e.needsToReadBody(), e.ClientConfig)
		if err != nil {
			result.AddError(err.Error())
			return
		}
```

- [ ] **Step 14: Verify the whole package still builds and passes**

Run: `go build ./... && go test ./config/endpoint/ ./client/`
Expected: PASS.

- [ ] **Step 15: Commit**

```bash
git add client/dsn.go client/dsn_test.go client/postgres.go client/postgres_test.go config/endpoint/endpoint.go config/endpoint/endpoint_test.go
git commit -m "feat(endpoint): postgres:// endpoint type with liveness, latency and probe result"
```

---

### Task 2: PostgreSQL engine health metrics

Extends the `postgres://` body with engine health gauges, guarded against the `pg_monitor` trap.

**Files:**
- Modify: `client/postgres.go`
- Modify: `client/postgres_test.go`

**Interfaces:**
- Consumes: `postgresBody`, `probeResult`, `redactError` from Task 1.
- Produces: `postgresBody` gains `Version`, `InRecovery`, `Connections`, `LongestRunningQuerySeconds`, `LongestIdleInTransactionSeconds`, `BlockedSessions`, `ReplicationLagSeconds`, `CacheHitRatio`, `DatabaseSizeBytes`.

---

- [ ] **Step 1: Write the failing test for the omitted-not-defaulted rule**

Append to `client/postgres_test.go`:

```go
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
```

Add `"encoding/json"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./client/ -run 'TestPostgresBody' -v`
Expected: FAIL — `postgresBody` has no field `Version`, and `postgresConnections` is undefined.

- [ ] **Step 3: Extend postgresBody and add the metrics collector**

In `client/postgres.go`, add the connections struct and extend `postgresBody`:

```go
// postgresConnections is the connection saturation gauge. Collecting it
// requires pg_monitor membership; see collectPostgresMetrics.
type postgresConnections struct {
	Used    int64   `json:"used"`
	Max     int64   `json:"max"`
	UsedPct float64 `json:"used_pct"`
}
```

Add these fields to `postgresBody`, between `Probe` and `MetricsErrors`:

```go
	Version string `json:"version,omitempty"`

	// Every field below is a pointer so that it is omitted entirely when it
	// could not be collected, rather than reported as a silently wrong zero.
	// InRecovery in particular must be a *bool: a plain bool with omitempty
	// would drop a legitimate false.
	//
	// The first four additionally require pg_monitor membership; the rest are
	// absent only when the metrics query fails as a whole.
	Connections                     *postgresConnections `json:"connections,omitempty"`
	LongestRunningQuerySeconds      *float64             `json:"longest_running_query_seconds,omitempty"`
	LongestIdleInTransactionSeconds *float64             `json:"longest_idle_in_transaction_seconds,omitempty"`
	BlockedSessions                 *int64               `json:"blocked_sessions,omitempty"`

	InRecovery              *bool    `json:"in_recovery,omitempty"`
	ReplicationLagSeconds   *float64 `json:"replication_lag_seconds,omitempty"`
	CacheHitRatioSinceReset *float64 `json:"cache_hit_ratio_since_reset,omitempty"`
	DatabaseSizeBytes       *int64   `json:"database_size_bytes,omitempty"`
```

Then add the collector:

```go
// postgresMetricsQuery collects every gauge in a single round trip.
//
// Only gauges and instantaneous measurements appear here. Cumulative counters
// (deadlocks, xact_rollback, ...) are deliberately excluded: Gatus checks are
// stateless, so a counter has no previous value to diff against and cannot
// express a meaningful threshold.
//
// Requires PostgreSQL 10 or later: backend_type and pg_monitor both landed in 10.
const postgresMetricsQuery = `
SELECT
  current_setting('server_version')                                       AS version,
  pg_is_in_recovery()                                                     AS in_recovery,
  pg_has_role(current_user, 'pg_monitor', 'member')                       AS has_pg_monitor,
  (SELECT count(*) FROM pg_stat_activity)                                 AS connections_used,
  current_setting('max_connections')::bigint                              AS connections_max,
  COALESCE((SELECT max(extract(epoch FROM now() - query_start))
            FROM pg_stat_activity
            WHERE state = 'active' AND backend_type = 'client backend'), 0) AS longest_running_query_seconds,
  COALESCE((SELECT max(extract(epoch FROM now() - state_change))
            FROM pg_stat_activity
            WHERE state = 'idle in transaction'), 0)                      AS longest_idle_in_transaction_seconds,
  (SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock')  AS blocked_sessions,
  CASE WHEN pg_is_in_recovery()
       THEN COALESCE(extract(epoch FROM now() - pg_last_xact_replay_timestamp()), 0)
       ELSE 0 END                                                         AS replication_lag_seconds,
  COALESCE((SELECT sum(blks_hit)::float8 / NULLIF(sum(blks_hit) + sum(blks_read), 0)
            FROM pg_stat_database), 0)                                    AS cache_hit_ratio_since_reset,
  pg_database_size(current_database())                                    AS database_size_bytes`

// collectPostgresMetrics fills the engine health fields of body.
//
// It never returns an error that should fail the check: a missing grant is a
// configuration problem, not an outage. Problems are appended to
// body.MetricsErrors and the affected fields are left absent.
func collectPostgresMetrics(ctx context.Context, db *sql.DB, body *postgresBody) {
	var (
		version                         string
		inRecovery, hasPgMonitor        bool
		connectionsUsed, connectionsMax int64
		longestRunningQuery             float64
		longestIdleInTransaction        float64
		blockedSessions                 int64
	)
	err := db.QueryRowContext(ctx, postgresMetricsQuery).Scan(
		&version, &inRecovery, &hasPgMonitor,
		&connectionsUsed, &connectionsMax,
		&longestRunningQuery, &longestIdleInTransaction, &blockedSessions,
		&body.ReplicationLagSeconds, &body.CacheHitRatio, &body.DatabaseSizeBytes,
	)
	if err != nil {
		body.MetricsErrors = append(body.MetricsErrors, "metrics query failed: "+redactCredentials(err.Error()))
		return
	}
	body.Version = version
	body.InRecovery = inRecovery
	if !hasPgMonitor {
		// Without pg_monitor, pg_stat_activity shows only this role's own
		// sessions, so these four values would be silently wrong rather than
		// merely missing. Omit them and say why.
		body.MetricsErrors = append(body.MetricsErrors,
			"connections, longest_running_query_seconds, longest_idle_in_transaction_seconds and blocked_sessions omitted: "+
				"the monitoring role lacks pg_monitor membership, so pg_stat_activity would report only its own sessions")
		return
	}
	connections := &postgresConnections{Used: connectionsUsed, Max: connectionsMax}
	if connectionsMax > 0 {
		connections.UsedPct = float64(connectionsUsed) / float64(connectionsMax) * 100
	}
	body.Connections = connections
	body.LongestRunningQuerySeconds = &longestRunningQuery
	body.LongestIdleInTransactionSeconds = &longestIdleInTransaction
	body.BlockedSessions = &blockedSessions
}
```

- [ ] **Step 4: Call the collector from QueryPostgres**

In `QueryPostgres`, replace the block that builds and marshals the body with:

```go
	body := postgresBody{
		ConnectMS: float64(connectDuration.Microseconds()) / 1000,
		ProbeMS:   float64((duration - connectDuration).Microseconds()) / 1000,
		Probe:     probe,
	}
	metricsStart := time.Now()
	collectPostgresMetrics(ctx, db, &body)
	body.MetricsMS = float64(time.Since(metricsStart).Microseconds()) / 1000
	marshalled, err := json.Marshal(body)
	if err != nil {
		return true, duration, nil, fmt.Errorf("failed to marshal body: %w", err)
	}
	return true, duration, marshalled, nil
```

Note that `duration` is captured **before** metric collection starts, so `[RESPONSE_TIME]` excludes it.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./client/ -run 'TestPostgres|TestQueryPostgres' -v`
Expected: PASS.

- [ ] **Step 6: Verify the build**

Run: `go build ./... && go test ./client/ ./config/endpoint/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add client/postgres.go client/postgres_test.go
git commit -m "feat(client): expose postgres engine health metrics in the endpoint body"
```

---

### Task 3: MongoDB liveness, latency, and probe result

Delivers a working `mongodb://` endpoint type. Same shape as Task 1, different driver.

**Files:**
- Create: `client/mongodb.go`
- Create: `client/mongodb_test.go`
- Modify: `config/endpoint/endpoint.go`
- Modify: `config/endpoint/endpoint_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `redactError`, `redactCredentials`, `defaultedProbeQuery` from Task 1.
- Produces:
  - `client.QueryMongoDB(uri, probeCommand string, collectMetrics bool, cfg *Config) (bool, time.Duration, []byte, error)`
  - `endpoint.TypeMongoDB endpoint.Type = "MONGODB"`
  - Struct `client.mongoBody` — extended by Task 4.

---

- [ ] **Step 1: Add the dependency**

Run:

```bash
go get go.mongodb.org/mongo-driver/v2@latest
go mod tidy
```

Expected: `go.mod` gains `go.mongodb.org/mongo-driver/v2`. Do **not** run `go mod vendor`; this repo has no `vendor/` directory despite what `CLAUDE.md` says.

- [ ] **Step 2: Write the failing test for endpoint type recognition**

In `config/endpoint/endpoint_test.go`, add to the same `Type()` scenario table used in Task 1:

```go
		{
			name: "mongodb",
			endpoint: Endpoint{URL: "mongodb://user:pw@mongo.internal:27017/tenant"},
			want: TypeMongoDB,
		},
		{
			name: "mongodb-srv",
			endpoint: Endpoint{URL: "mongodb+srv://user:pw@cluster.example.net/tenant"},
			want: TypeMongoDB,
		},
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./config/endpoint/ -run 'TestEndpoint_Type' -v`
Expected: FAIL — `undefined: TypeMongoDB`.

- [ ] **Step 4: Add the type constant and Type() case**

In `config/endpoint/endpoint.go`, add to the constant block after `TypePostgres`:

```go
	TypeMongoDB  Type = "MONGODB"
```

And in `Type()`, before `default:`:

```go
	case strings.HasPrefix(e.URL, "mongodb://") || strings.HasPrefix(e.URL, "mongodb+srv://"):
		return TypeMongoDB
```

`"mongodb+srv://x"` does not carry the prefix `"mongodb://"`, so the two cases are independent of ordering.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./config/endpoint/ -run 'TestEndpoint_Type' -v`
Expected: PASS.

- [ ] **Step 6: Write the failing tests for QueryMongoDB**

Create `client/mongodb_test.go`:

```go
package client

import (
	"strings"
	"testing"
	"time"
)

func TestQueryMongoDB_MalformedURI(t *testing.T) {
	t.Parallel()
	connected, _, body, err := QueryMongoDB("mongodb://", "", false, &Config{Timeout: 5 * time.Second})
	if connected {
		t.Error("should not report connected on a malformed URI")
	}
	if err == nil {
		t.Fatal("expected an error on a malformed URI")
	}
	if body != nil {
		t.Error("expected no body when the connection never succeeded")
	}
}

func TestQueryMongoDB_ClosedPort(t *testing.T) {
	t.Parallel()
	connected, duration, _, err := QueryMongoDB(
		"mongodb://gatus:hunter2@127.0.0.1:1/tenant", "", false, &Config{Timeout: 3 * time.Second},
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
	if duration > 15*time.Second {
		t.Errorf("took %s, which suggests the timeout was not honoured", duration)
	}
}

func TestParseMongoProbeCommand(t *testing.T) {
	t.Parallel()
	cmd, err := parseMongoProbeCommand("")
	if err != nil {
		t.Fatalf("empty command should fall back to the default: %s", err)
	}
	if len(cmd) != 1 || cmd[0].Key != "ping" {
		t.Errorf("expected the default {ping:1}, got %v", cmd)
	}
	cmd, err = parseMongoProbeCommand(`{"dbStats": 1}`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if len(cmd) != 1 || cmd[0].Key != "dbStats" {
		t.Errorf("expected dbStats, got %v", cmd)
	}
	if _, err = parseMongoProbeCommand(`not json`); err == nil {
		t.Error("expected an error on a malformed command document")
	}
}
```

- [ ] **Step 7: Run the tests to verify they fail**

Run: `go test ./client/ -run 'TestQueryMongoDB|TestParseMongoProbeCommand' -v`
Expected: FAIL — `undefined: QueryMongoDB`, `undefined: parseMongoProbeCommand`.

- [ ] **Step 8: Implement QueryMongoDB**

Create `client/mongodb.go`:

```go
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// defaultMongoProbeCommand is used when an endpoint sets no body.
const defaultMongoProbeCommand = `{"ping": 1}`

// mongoBody is the JSON body exposed to conditions via [BODY].
//
// The same struct serves real MongoDB and FerretDB. FerretDB has no mongod
// internals to report — its connections, locks and replication live in the
// backing PostgreSQL — so its bodies are thinner, with the reason recorded in
// MetricsErrors. Fields are omitted rather than defaulted.
type mongoBody struct {
	ConnectMS float64      `json:"connect_ms"`
	ProbeMS   float64      `json:"probe_ms"`
	MetricsMS float64      `json:"metrics_ms,omitempty"`
	Probe     map[string]any `json:"probe,omitempty"`

	MetricsErrors []string `json:"metrics_errors,omitempty"`
}

// parseMongoProbeCommand turns the endpoint body into a command document,
// falling back to {ping: 1} when the body is blank.
func parseMongoProbeCommand(body string) (bson.D, error) {
	body = defaultedProbeQuery(body, defaultMongoProbeCommand)
	var command bson.D
	if err := bson.UnmarshalExtJSON([]byte(body), true, &command); err != nil {
		return nil, fmt.Errorf("invalid probe command document: %w", err)
	}
	if len(command) == 0 {
		return nil, fmt.Errorf("probe command document is empty")
	}
	return command, nil
}

// QueryMongoDB connects to a MongoDB-compatible server, runs a probe command,
// and optionally collects engine health metrics.
//
// The returned duration covers connection establishment plus the probe command.
// Metric collection is excluded and reported as metrics_ms.
//
// The read preference is taken from the URI (?readPreference=...), so the same
// function serves a replica set primary, a secondary, and FerretDB — which has
// no primary concept at all.
func QueryMongoDB(uri, probeCommand string, collectMetrics bool, cfg *Config) (bool, time.Duration, []byte, error) {
	if cfg == nil {
		cfg = GetDefaultConfig()
	}
	command, err := parseMongoProbeCommand(probeCommand)
	if err != nil {
		return false, 0, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	opts := options.Client().
		ApplyURI(uri).
		SetServerSelectionTimeout(cfg.Timeout).
		SetConnectTimeout(cfg.Timeout)
	start := time.Now()
	cli, err := mongo.Connect(opts)
	if err != nil {
		return false, time.Since(start), nil, fmt.Errorf("failed to connect: %w", redactError(err))
	}
	defer func() {
		disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), cfg.Timeout)
		defer disconnectCancel()
		_ = cli.Disconnect(disconnectCtx)
	}()
	if err := cli.Ping(ctx, nil); err != nil {
		return false, time.Since(start), nil, fmt.Errorf("failed to connect: %w", redactError(err))
	}
	connectDuration := time.Since(start)
	database := mongoDatabaseFromURI(uri)
	var probe bson.M
	if err := cli.Database(database).RunCommand(ctx, command).Decode(&probe); err != nil {
		return true, time.Since(start), nil, fmt.Errorf("probe command failed: %w", redactError(err))
	}
	duration := time.Since(start)
	if !collectMetrics {
		return true, duration, nil, nil
	}
	body := mongoBody{
		ConnectMS: float64(connectDuration.Microseconds()) / 1000,
		ProbeMS:   float64((duration - connectDuration).Microseconds()) / 1000,
		Probe:     probe,
	}
	marshalled, err := json.Marshal(body)
	if err != nil {
		return true, duration, nil, fmt.Errorf("failed to marshal body: %w", err)
	}
	return true, duration, marshalled, nil
}

// mongoDatabaseFromURI extracts the database name from a MongoDB URI, falling
// back to "admin" when the URI names none. Diagnostic commands run against
// admin; a probe against tenant data needs the tenant database.
func mongoDatabaseFromURI(uri string) string {
	withoutScheme := uri
	for _, scheme := range []string{"mongodb+srv://", "mongodb://"} {
		if strings.HasPrefix(withoutScheme, scheme) {
			withoutScheme = strings.TrimPrefix(withoutScheme, scheme)
			break
		}
	}
	slash := strings.Index(withoutScheme, "/")
	if slash < 0 {
		return "admin"
	}
	database := withoutScheme[slash+1:]
	if question := strings.Index(database, "?"); question >= 0 {
		database = database[:question]
	}
	if len(database) == 0 {
		return "admin"
	}
	return database
}
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./client/ -run 'TestQueryMongoDB|TestParseMongoProbeCommand' -v`
Expected: PASS.

The v2 API used above was compiled and run against `go.mongodb.org/mongo-driver/v2@v2.8.0` while this plan was written, so `mongo.Connect(opts)` taking no context, `bson.UnmarshalExtJSON` into a `bson.D`, `cli.Ping(ctx, nil)`, and `RunCommand(...).Decode(&bson.M{})` are all confirmed. Note that `mongo.Connect` does **not** error on an unreachable host — it is lazy, and `Ping` is what surfaces the failure. If `go get` resolves a newer major version whose API differs, fix the call sites and keep the behaviour identical; do not change the function signature.

- [ ] **Step 10: Wire the dispatch in call()**

In `config/endpoint/endpoint.go`, add after the `TypePostgres` branch:

```go
	} else if endpointType == TypeMongoDB {
		result.Connected, result.Duration, result.Body, err = client.QueryMongoDB(e.URL, e.getParsedBody(), e.needsToReadBody(), e.ClientConfig)
		if err != nil {
			result.AddError(err.Error())
			return
		}
```

- [ ] **Step 11: Verify the build**

Run: `go build ./... && go test ./client/ ./config/endpoint/`
Expected: PASS.

- [ ] **Step 12: Commit**

```bash
git add go.mod go.sum client/mongodb.go client/mongodb_test.go config/endpoint/endpoint.go config/endpoint/endpoint_test.go
git commit -m "feat(endpoint): mongodb:// endpoint type with liveness, latency and probe result"
```

---

### Task 4: MongoDB and FerretDB engine health metrics

Extends the `mongodb://` body with engine health, degrading gracefully on FerretDB.

**Files:**
- Modify: `client/mongodb.go`
- Modify: `client/mongodb_test.go`

**Interfaces:**
- Consumes: `mongoBody` from Task 3.
- Produces: `mongoBody` gains `Backend`, `Version`, `IsWritablePrimary`, `ReplSetState`, `ReplicationLagSeconds`, `Connections`, `GlobalLockQueueTotal`, `ActiveClientsTotal`, `UptimeSeconds`.

---

- [ ] **Step 1: Write the failing test for graceful degradation**

Append to `client/mongodb_test.go`:

```go
func TestMongoBody_FerretDBOmitsMongodInternals(t *testing.T) {
	t.Parallel()
	body := mongoBody{
		ConnectMS: 20,
		ProbeMS:   2,
		Backend:   "ferretdb",
		Version:   "2.0.0",
		MetricsErrors: []string{
			"serverStatus: connections and globalLock are not reported by this backend",
			"replSetGetStatus: not supported by this backend",
		},
	}
	marshalled, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	got := string(marshalled)
	// Assert on the quoted JSON key, not the bare word: MetricsErrors above
	// contains the English word "connections" in its prose, which a bare
	// substring check would match, failing the test for the wrong reason.
	for _, absent := range []string{`"connections":`, `"global_lock_queue_total":`, `"replication_lag_seconds":`, `"repl_set_state":`} {
		if strings.Contains(got, absent) {
			t.Errorf("field %s must be omitted on a backend that cannot report it, got %s", absent, got)
		}
	}
	if !strings.Contains(got, `"backend":"ferretdb"`) {
		t.Errorf("expected the backend to be labelled, got %s", got)
	}
}

func TestDetectMongoBackend(t *testing.T) {
	t.Parallel()
	if got := detectMongoBackend(bson.M{"version": "7.0.5"}); got != "mongodb" {
		t.Errorf("expected mongodb, got %q", got)
	}
	if got := detectMongoBackend(bson.M{"version": "7.0.42", "ferretdb": bson.M{"version": "2.0.0"}}); got != "ferretdb" {
		t.Errorf("expected ferretdb from the ferretdb key, got %q", got)
	}
	if got := detectMongoBackend(bson.M{"version": "2.0.0-FerretDB"}); got != "ferretdb" {
		t.Errorf("expected ferretdb from the version string, got %q", got)
	}
}
```

Add `"encoding/json"` and the bson import to the test file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./client/ -run 'TestMongoBody|TestDetectMongoBackend' -v`
Expected: FAIL — `mongoBody` has no field `Backend`, `detectMongoBackend` undefined.

- [ ] **Step 3: Extend mongoBody and implement collection**

In `client/mongodb.go`, add:

```go
// mongoConnections is the connection gauge from serverStatus. FerretDB does not
// report it, so it is a pointer and omitted there.
type mongoConnections struct {
	Current   int64   `json:"current"`
	Available int64   `json:"available"`
	UsedPct   float64 `json:"used_pct"`
}
```

Add these fields to `mongoBody`, between `Probe` and `MetricsErrors`:

```go
	Backend string `json:"backend,omitempty"`
	Version string `json:"version,omitempty"`

	// Every field below is absent on backends that cannot report it.
	IsWritablePrimary     *bool             `json:"is_writable_primary,omitempty"`
	ReplSetState          string            `json:"repl_set_state,omitempty"`
	ReplicationLagSeconds *float64          `json:"replication_lag_seconds,omitempty"`
	Connections           *mongoConnections `json:"connections,omitempty"`
	GlobalLockQueueTotal  *int64            `json:"global_lock_queue_total,omitempty"`
	ActiveClientsTotal    *int64            `json:"active_clients_total,omitempty"`
	UptimeSeconds         *int64            `json:"uptime_seconds,omitempty"`
```

Then the collector:

```go
// detectMongoBackend labels the server as "mongodb" or "ferretdb" from its
// buildInfo response. FerretDB advertises itself both as a top-level key and
// inside the version string, depending on version; both are checked.
func detectMongoBackend(buildInfo bson.M) string {
	if _, ok := buildInfo["ferretdb"]; ok {
		return "ferretdb"
	}
	if version, ok := buildInfo["version"].(string); ok && strings.Contains(strings.ToLower(version), "ferretdb") {
		return "ferretdb"
	}
	return "mongodb"
}

// collectMongoMetrics fills the engine health fields of body.
//
// It never fails the check. Every command is attempted independently, and one
// that a backend does not implement only adds an entry to MetricsErrors. This
// is what lets a single implementation serve both real MongoDB and FerretDB.
//
// opLatencies is deliberately not collected: it is a cumulative average since
// process start, so it drifts and cannot express current latency.
// [RESPONSE_TIME] is the latency signal.
func collectMongoMetrics(ctx context.Context, cli *mongo.Client, body *mongoBody) {
	admin := cli.Database("admin")
	var buildInfo bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&buildInfo); err != nil {
		body.MetricsErrors = append(body.MetricsErrors, "buildInfo: "+redactCredentials(err.Error()))
	} else {
		body.Backend = detectMongoBackend(buildInfo)
		if version, ok := buildInfo["version"].(string); ok {
			body.Version = version
		}
	}
	var hello bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		body.MetricsErrors = append(body.MetricsErrors, "hello: "+redactCredentials(err.Error()))
	} else if writable, ok := hello["isWritablePrimary"].(bool); ok {
		body.IsWritablePrimary = &writable
	}
	collectMongoServerStatus(ctx, admin, body)
	collectMongoReplicationLag(ctx, admin, body)
}

// collectMongoServerStatus pulls the connection, lock and uptime gauges.
// FerretDB accepts serverStatus but does not populate the mongod-internal
// sections, so each is checked for presence rather than assumed.
func collectMongoServerStatus(ctx context.Context, admin *mongo.Database, body *mongoBody) {
	var status bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&status); err != nil {
		body.MetricsErrors = append(body.MetricsErrors, "serverStatus: "+redactCredentials(err.Error()))
		return
	}
	if uptime, ok := toInt64(status["uptime"]); ok {
		body.UptimeSeconds = &uptime
	}
	connections, ok := status["connections"].(bson.M)
	if !ok {
		body.MetricsErrors = append(body.MetricsErrors,
			"serverStatus: connections and globalLock are not reported by this backend")
		return
	}
	current, currentOK := toInt64(connections["current"])
	available, availableOK := toInt64(connections["available"])
	if currentOK && availableOK {
		gauge := &mongoConnections{Current: current, Available: available}
		if total := current + available; total > 0 {
			gauge.UsedPct = float64(current) / float64(total) * 100
		}
		body.Connections = gauge
	}
	globalLock, ok := status["globalLock"].(bson.M)
	if !ok {
		return
	}
	if queue, ok := globalLock["currentQueue"].(bson.M); ok {
		if total, ok := toInt64(queue["total"]); ok {
			body.GlobalLockQueueTotal = &total
		}
	}
	if clients, ok := globalLock["activeClients"].(bson.M); ok {
		if total, ok := toInt64(clients["total"]); ok {
			body.ActiveClientsTotal = &total
		}
	}
}

// collectMongoReplicationLag derives lag as primary optime minus this member's
// optime. The command fails on a standalone deployment and on FerretDB, which
// has no real replica sets — the flag and oplog collection it offers exist for
// change-stream compatibility, not replication.
func collectMongoReplicationLag(ctx context.Context, admin *mongo.Database, body *mongoBody) {
	var status bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&status); err != nil {
		body.MetricsErrors = append(body.MetricsErrors, "replSetGetStatus: not supported by this backend or not a replica set member")
		return
	}
	if state, ok := status["myState"]; ok {
		if stateStr, ok := replSetStateName(state); ok {
			body.ReplSetState = stateStr
		}
	}
	members, ok := status["members"].(bson.A)
	if !ok {
		return
	}
	var selfOptime, primaryOptime time.Time
	var haveSelf, havePrimary bool
	for _, raw := range members {
		member, ok := raw.(bson.M)
		if !ok {
			continue
		}
		optime, ok := member["optimeDate"].(bson.DateTime)
		if !ok {
			continue
		}
		if isSelf, _ := member["self"].(bool); isSelf {
			selfOptime, haveSelf = optime.Time(), true
		}
		if stateStr, _ := member["stateStr"].(string); stateStr == "PRIMARY" {
			primaryOptime, havePrimary = optime.Time(), true
		}
	}
	if haveSelf && havePrimary {
		lag := primaryOptime.Sub(selfOptime).Seconds()
		if lag < 0 {
			lag = 0
		}
		body.ReplicationLagSeconds = &lag
	}
}

// replSetStateName maps a replica set member state code to its name.
func replSetStateName(state any) (string, bool) {
	code, ok := toInt64(state)
	if !ok {
		return "", false
	}
	names := map[int64]string{
		0: "STARTUP", 1: "PRIMARY", 2: "SECONDARY", 3: "RECOVERING",
		5: "STARTUP2", 6: "UNKNOWN", 7: "ARBITER", 8: "DOWN",
		9: "ROLLBACK", 10: "REMOVED",
	}
	name, ok := names[code]
	return name, ok
}

// toInt64 normalises the several numeric types BSON can decode into.
func toInt64(v any) (int64, bool) {
	switch typed := v.(type) {
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	}
	return 0, false
}
```

- [ ] **Step 4: Call the collector from QueryMongoDB**

In `QueryMongoDB`, replace the body-building block with:

```go
	body := mongoBody{
		ConnectMS: float64(connectDuration.Microseconds()) / 1000,
		ProbeMS:   float64((duration - connectDuration).Microseconds()) / 1000,
		Probe:     probe,
	}
	metricsStart := time.Now()
	collectMongoMetrics(ctx, cli, &body)
	body.MetricsMS = float64(time.Since(metricsStart).Microseconds()) / 1000
	marshalled, err := json.Marshal(body)
	if err != nil {
		return true, duration, nil, fmt.Errorf("failed to marshal body: %w", err)
	}
	return true, duration, marshalled, nil
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./client/ -run 'TestMongo|TestQueryMongoDB|TestDetect' -v`
Expected: PASS.

- [ ] **Step 6: Verify the build**

Run: `go build ./... && go test ./client/ ./config/endpoint/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add client/mongodb.go client/mongodb_test.go
git commit -m "feat(client): expose mongodb and ferretdb engine health in the endpoint body"
```

---

### Task 5: Startup validation of database connection strings

Spec §9. A `postgres://` or `mongodb://` URL with no host parses fine as a URL but is useless as a DSN. Without this, the failure is a runtime error on every interval forever instead of a startup panic with a clear message — Gatus treats invalid config as fatal by design.

The same reasoning covers a second case, surfaced by Task 3's review: a `mongodb://` endpoint whose `body:` is not a valid command document fails inside `parseMongoProbeCommand` *before any connection is attempted*, and reports `connected=false`. A misconfigured endpoint is therefore indistinguishable from a real outage on the dashboard — forever. Validating the document at startup converts that into a boot-time panic naming the problem.

**Files:**
- Modify: `config/endpoint/endpoint.go` (`ValidateAndSetDefaults` at ~line 204, error block at ~line 60)
- Modify: `config/endpoint/endpoint_test.go`

**Interfaces:**
- Consumes: `TypePostgres` (Task 1), `TypeMongoDB` (Task 3), `client.ValidateMongoProbeCommand` (added in this task).
- Produces: `endpoint.ErrEndpointWithInvalidDatabaseURL`, `endpoint.ErrEndpointWithInvalidProbeCommand`, `client.ValidateMongoProbeCommand`.

---

- [ ] **Step 1: Write the failing test**

Append to `config/endpoint/endpoint_test.go`:

```go
func TestEndpoint_ValidateAndSetDefaultsWithDatabaseURL(t *testing.T) {
	scenarios := []struct {
		name        string
		url         string
		expectedErr error
	}{
		{name: "postgres-without-host", url: "postgres:///tenant", expectedErr: ErrEndpointWithInvalidDatabaseURL},
		{name: "mongodb-without-host", url: "mongodb://", expectedErr: ErrEndpointWithInvalidDatabaseURL},
		{name: "valid-postgres", url: "postgres://u:p@db.internal:5432/tenant", expectedErr: nil},
		{name: "valid-mongodb", url: "mongodb://u:p@mongo.internal:27017/tenant", expectedErr: nil},
		{name: "valid-mongodb-srv", url: "mongodb+srv://u:p@cluster.example.net/tenant", expectedErr: nil},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			endpoint := &Endpoint{
				Name:       "database",
				URL:        scenario.url,
				Conditions: []Condition{"[CONNECTED] == true"},
			}
			err := endpoint.ValidateAndSetDefaults()
			if !errors.Is(err, scenario.expectedErr) {
				t.Errorf("expected error %v, got %v", scenario.expectedErr, err)
			}
		})
	}
}
```

If `errors` is not already imported in that test file, add it.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./config/endpoint/ -run 'TestEndpoint_ValidateAndSetDefaultsWithDatabaseURL' -v`
Expected: FAIL — `undefined: ErrEndpointWithInvalidDatabaseURL`.

- [ ] **Step 3: Add the named error**

In `config/endpoint/endpoint.go`, add to the `var (...)` error block, after `ErrUnknownEndpointType`:

```go
	// ErrEndpointWithInvalidDatabaseURL is the error with which Gatus will panic if a database endpoint's URL is not a usable connection string
	ErrEndpointWithInvalidDatabaseURL = errors.New("a postgres:// or mongodb:// endpoint must have a host in its url")
```

- [ ] **Step 4: Add the validation**

In `ValidateAndSetDefaults`, immediately after the existing `if e.Type() == TypeUNKNOWN { return ErrUnknownEndpointType }` block:

```go
	if endpointType := e.Type(); endpointType == TypePostgres || endpointType == TypeMongoDB {
		// url.Parse accepts "mongodb://" and "postgres:///tenant", which parse but
		// cannot be connected to. Catch them at startup rather than every interval.
		if parsedURL, err := url.Parse(e.URL); err != nil || len(parsedURL.Host) == 0 {
			return ErrEndpointWithInvalidDatabaseURL
		}
	}
```

`net/url` is already imported by this file.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./config/endpoint/ -run 'TestEndpoint_ValidateAndSetDefaults' -v`
Expected: PASS, including the pre-existing validation scenarios.

- [ ] **Step 6: Verify the url validation in isolation**

Run: `go build ./... && go test ./config/endpoint/`
Expected: PASS.

- [ ] **Step 7: Write the failing test for probe-command validation**

Append to `config/endpoint/endpoint_test.go`:

```go
func TestEndpoint_ValidateAndSetDefaultsWithMongoProbeCommand(t *testing.T) {
	scenarios := []struct {
		name        string
		body        string
		expectedErr error
	}{
		{name: "empty-body-uses-default", body: "", expectedErr: nil},
		{name: "valid-command-document", body: `{"dbStats": 1}`, expectedErr: nil},
		{name: "not-json", body: "not json", expectedErr: ErrEndpointWithInvalidProbeCommand},
		{name: "empty-document", body: "{}", expectedErr: ErrEndpointWithInvalidProbeCommand},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			endpoint := &Endpoint{
				Name:       "mongo",
				URL:        "mongodb://u:p@mongo.internal:27017/tenant",
				Body:       scenario.body,
				Conditions: []Condition{"[CONNECTED] == true"},
			}
			err := endpoint.ValidateAndSetDefaults()
			if !errors.Is(err, scenario.expectedErr) {
				t.Errorf("expected error %v, got %v", scenario.expectedErr, err)
			}
		})
	}
}
```

- [ ] **Step 8: Run the test to verify it fails**

Run: `go test ./config/endpoint/ -run 'TestEndpoint_ValidateAndSetDefaultsWithMongoProbeCommand' -v`
Expected: FAIL — `undefined: ErrEndpointWithInvalidProbeCommand`.

- [ ] **Step 9: Export a validator from the client package**

`parseMongoProbeCommand` is unexported and lives in `client`. Rather than duplicating its parsing rules in `config/endpoint` — which would let the two drift apart silently — export a thin wrapper in `client/mongodb.go`:

```go
// ValidateMongoProbeCommand reports whether body is a usable probe command
// document. It exists so that config validation rejects a malformed document at
// startup instead of letting every check report a false outage forever.
func ValidateMongoProbeCommand(body string) error {
	_, err := parseMongoProbeCommand(body)
	return err
}
```

- [ ] **Step 10: Add the named error and the validation**

In `config/endpoint/endpoint.go`, add to the `var (...)` error block, after `ErrEndpointWithInvalidDatabaseURL`:

```go
	// ErrEndpointWithInvalidProbeCommand is the error with which Gatus will panic if a mongodb endpoint's body is not a valid command document
	ErrEndpointWithInvalidProbeCommand = errors.New("the body of a mongodb:// endpoint must be a valid command document, e.g. {\"ping\": 1}")
```

Then extend the database branch added in Step 4 so the whole block reads:

```go
	if endpointType := e.Type(); endpointType == TypePostgres || endpointType == TypeMongoDB {
		// url.Parse accepts "mongodb://" and "postgres:///tenant", which parse but
		// cannot be connected to. Catch them at startup rather than every interval.
		if parsedURL, err := url.Parse(e.URL); err != nil || len(parsedURL.Host) == 0 {
			return ErrEndpointWithInvalidDatabaseURL
		}
		if endpointType == TypeMongoDB {
			// A malformed command document fails before any connection is attempted,
			// so at runtime it is indistinguishable from a real outage. Reject it here.
			if err := client.ValidateMongoProbeCommand(e.getParsedBody()); err != nil {
				return fmt.Errorf("%w: %s", ErrEndpointWithInvalidProbeCommand, err)
			}
		}
	}
```

`fmt` and the `client` package are already imported by this file.

- [ ] **Step 11: Run the tests to verify they pass**

Run: `go test ./config/endpoint/ -run 'TestEndpoint_ValidateAndSetDefaults' -v`
Expected: PASS, including the pre-existing validation scenarios.

- [ ] **Step 12: Verify nothing else regressed**

Run: `go build ./... && go test ./config/... ./client/`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add config/endpoint/endpoint.go config/endpoint/endpoint_test.go client/mongodb.go
git commit -m "feat(endpoint): validate database urls and mongodb probe commands at startup"
```

---

### Task 6: Opt-in integration tests and a local stack

The unit tests cannot exercise the actual catalog queries or command shapes. This task adds the only tests that can, plus the stack to run them against. Until they run against a real FerretDB, the shape of its thin body is a prediction, not a fact.

**Files:**
- Create: `.examples/docker-compose-database-monitoring/compose.yaml`
- Create: `.examples/docker-compose-database-monitoring/README.md`
- Create: `client/database_integration_test.go`

**Interfaces:**
- Consumes: `QueryPostgres`, `QueryMongoDB`.
- Produces: nothing consumed by later tasks.

---

- [ ] **Step 1: Create the local stack**

Create `.examples/docker-compose-database-monitoring/compose.yaml`:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: tenant
    ports:
      - "5432:5432"

  mongodb:
    image: mongo:7
    environment:
      MONGO_INITDB_ROOT_USERNAME: root
      MONGO_INITDB_ROOT_PASSWORD: root
    ports:
      - "27017:27017"

  ferretdb-postgres:
    image: ghcr.io/ferretdb/postgres-documentdb:latest
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: postgres
    ports:
      - "5433:5432"

  ferretdb:
    image: ghcr.io/ferretdb/ferretdb:latest
    environment:
      FERRETDB_POSTGRESQL_URL: postgres://postgres:postgres@ferretdb-postgres:5432/postgres
      # Bind the debug handler to all interfaces so /debug/readyz is reachable
      # from outside the container. It defaults to 127.0.0.1:8088.
      FERRETDB_DEBUG_ADDR: ":8088"
    ports:
      - "27018:27017"
      - "8088:8088"
    depends_on:
      - ferretdb-postgres
```

Create `.examples/docker-compose-database-monitoring/README.md` explaining that this stack exists to run the opt-in integration tests, with the exact `docker compose up -d` and the three environment variables from Step 2.

- [ ] **Step 2: Write the integration tests**

Create `client/database_integration_test.go`:

```go
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
```

- [ ] **Step 3: Verify the tests skip cleanly with no server**

Run: `go test ./client/ -run 'Integration' -v`
Expected: three SKIP lines, no failures.

- [ ] **Step 4: Run them against the local stack**

```bash
docker compose -f .examples/docker-compose-database-monitoring/compose.yaml up -d
sleep 20
docker compose -f .examples/docker-compose-database-monitoring/compose.yaml exec -T postgres \
  psql -U postgres -d tenant -c "CREATE ROLE gatus_monitor WITH LOGIN PASSWORD 'monitor'; GRANT pg_monitor TO gatus_monitor; GRANT CONNECT ON DATABASE tenant TO gatus_monitor;"
GATUS_TEST_POSTGRES_DSN="postgres://gatus_monitor:monitor@127.0.0.1:5432/tenant?sslmode=disable" \
GATUS_TEST_MONGODB_URI="mongodb://root:root@127.0.0.1:27017/admin" \
GATUS_TEST_FERRETDB_URI="mongodb://username:password@127.0.0.1:27018/postgres" \
  go test ./client/ -run 'Integration' -v
```

Expected: all three pass. Record the logged FerretDB body — it is the first real confirmation of §5.2 of the spec. If `detectMongoBackend` fails to label FerretDB, fix it now against the actual `buildInfo` output; that is spec open question 1.

The FerretDB credentials depend on how the DocumentDB image is initialised; read the stack's README output if the connection is refused.

- [ ] **Step 5: Tear down and commit**

```bash
docker compose -f .examples/docker-compose-database-monitoring/compose.yaml down -v
git add .examples/docker-compose-database-monitoring client/database_integration_test.go
git commit -m "test(client): opt-in database integration tests and a local stack"
```

---

### Task 7: Documentation

**Files:**
- Create: `docs/database-monitoring.md`
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-08-17-database-monitoring-design.md` (resolve open questions confirmed in Task 6)

**Interfaces:**
- Consumes: everything from Tasks 1-6.
- Produces: nothing.

---

- [ ] **Step 1: Write docs/database-monitoring.md**

Follow the structure of `docs/multi-tenancy.md`: an intro paragraph, a table of contents, then sections. It must contain:

- What it does and which schemes map to which type.
- The `body:` convention for probe queries, the defaults, and `[BODY].probe.value`.
- That metric collection is skipped entirely unless a condition references `[BODY]`.
- That `[RESPONSE_TIME]` covers connect + probe and excludes metric collection, with `connect_ms` / `probe_ms` / `metrics_ms` in the body for attribution.
- The full body schema for PostgreSQL, real MongoDB, and FerretDB — copy them from spec §5, updated with whatever Task 6 actually observed.
- The required monitoring roles, verbatim:

```sql
CREATE ROLE gatus_monitor WITH LOGIN PASSWORD '...';
GRANT pg_monitor TO gatus_monitor;
GRANT CONNECT ON DATABASE <tenant> TO gatus_monitor;
```

  and for MongoDB, the `clusterMonitor` role.
- A prominent warning that without `pg_monitor`, four fields are omitted and why — that omission is deliberate, because `pg_stat_activity` would otherwise report a silently wrong number.
- The FerretDB section: why its body is thin, and that `/debug/readyz`, `/debug/livez` and `/debug/metrics` (default `127.0.0.1:8088`, moved with `--debug-addr`) carry the real signals, plus that its backing PostgreSQL can be monitored with the `postgres://` type.
- Worked per-tenant examples for both stacks, copied from spec §8.
- A note that every threshold shown is a placeholder needing a production baseline.

- [ ] **Step 2: Add the types to README.md**

`README.md` has a "Monitoring a ..." section series starting at ~line 2990 and matching Table of Contents entries at ~line 121. Add two sections in the style of the existing ones — `### Monitoring a PostgreSQL database` and `### Monitoring a MongoDB or FerretDB database` — each with a complete, copy-pasteable YAML example, and add the matching Table of Contents links. Keep the alphabetical/positional convention of the surrounding entries.

- [ ] **Step 3: Resolve the spec's open questions**

In `docs/superpowers/specs/2026-08-17-database-monitoring-design.md` §13, replace each question that Task 6 answered with the answer. Leave genuinely open ones (the Nexfar deployment's `--debug-addr` binding, whether each FerretDB tenant's backing PostgreSQL is distinct, and the thresholds) marked as open.

- [ ] **Step 4: Verify docs build nothing and tests still pass**

Run: `go build ./... && make test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add docs/database-monitoring.md README.md docs/superpowers/specs/2026-08-17-database-monitoring-design.md
git commit -m "docs: document postgres and mongodb endpoint types"
```

---

## Out of scope

Deliberately not in this plan, per the spec's non-goals:

- **Alerting.** No `alerts:` blocks. Endpoints are per-tenant already, so adding them later is a config change, not a code change.
- **Threshold values.** Every number in every example is a placeholder. Real values need a production baseline.
- **Config generation for the tenant loop.** That lives in the deployment repo.
- **Frontend changes.** The new types render through the existing dashboard with no `web/app/` change, so no `make frontend-build` is required.
