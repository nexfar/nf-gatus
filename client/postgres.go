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
	Rows  int `json:"rows"`
	Value any `json:"value"`
}

// postgresConnections is the connection saturation gauge. Collecting it
// requires pg_monitor membership; see collectPostgresMetrics.
type postgresConnections struct {
	Used    int64   `json:"used"`
	Max     int64   `json:"max"`
	UsedPct float64 `json:"used_pct"`
}

// postgresBody is the JSON body exposed to conditions via [BODY].
// Fields that could not be collected are omitted rather than defaulted: a
// missing field fails its condition visibly, a zero value passes on fiction.
type postgresBody struct {
	ConnectMS float64      `json:"connect_ms"`
	ProbeMS   float64      `json:"probe_ms"`
	MetricsMS float64      `json:"metrics_ms,omitempty"`
	Probe     *probeResult `json:"probe,omitempty"`

	Version string `json:"version,omitempty"`

	// InRecovery, ReplicationLagSeconds, CacheHitRatioSinceReset and
	// DatabaseSizeBytes are pointers so that a whole-query failure (e.g. a
	// context deadline shared with connect+probe) leaves them absent rather
	// than reporting them as a silently wrong zero.
	InRecovery *bool `json:"in_recovery,omitempty"`

	// The four fields below additionally require pg_monitor membership. They
	// are pointers so that they are omitted entirely when that grant is
	// missing, rather than reported as a silently wrong zero.
	Connections                     *postgresConnections `json:"connections,omitempty"`
	LongestRunningQuerySeconds      *float64             `json:"longest_running_query_seconds,omitempty"`
	LongestIdleInTransactionSeconds *float64             `json:"longest_idle_in_transaction_seconds,omitempty"`
	BlockedSessions                 *int64               `json:"blocked_sessions,omitempty"`

	ReplicationLagSeconds *float64 `json:"replication_lag_seconds,omitempty"`

	// CacheHitRatioSinceReset is blks_hit / (blks_hit + blks_read) accumulated
	// since the last pg_stat_database reset (typically server start), not an
	// instantaneous rate. On a long-lived instance it is dominated by months
	// of history, so a real, ongoing cache regression barely moves it; the
	// name carries that caveat because it is the only thing a person writing
	// a threshold will read.
	CacheHitRatioSinceReset *float64 `json:"cache_hit_ratio_since_reset,omitempty"`

	DatabaseSizeBytes *int64 `json:"database_size_bytes,omitempty"`

	MetricsErrors []string `json:"metrics_errors,omitempty"`
}

// defaultedProbeQuery returns query, or fallback when query is blank.
func defaultedProbeQuery(query, fallback string) string {
	if len(strings.TrimSpace(query)) == 0 {
		return fallback
	}
	return query
}

// postgresMetricsQuery collects every gauge in a single round trip.
//
// Every field here is an instantaneous measurement, with one exception:
// cache_hit_ratio (scanned into CacheHitRatioSinceReset) is a cumulative
// average of blks_hit/blks_read since the last pg_stat_database reset
// (typically server start), so it is slow to reflect a recent regression on
// a long-lived instance. Cumulative counters proper (deadlocks,
// xact_rollback, ...) are deliberately excluded entirely: Gatus checks are
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
            FROM pg_stat_database), 0)                                    AS cache_hit_ratio,
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
		replicationLagSeconds           float64
		cacheHitRatioSinceReset         float64
		databaseSizeBytes               int64
	)
	err := db.QueryRowContext(ctx, postgresMetricsQuery).Scan(
		&version, &inRecovery, &hasPgMonitor,
		&connectionsUsed, &connectionsMax,
		&longestRunningQuery, &longestIdleInTransaction, &blockedSessions,
		&replicationLagSeconds, &cacheHitRatioSinceReset, &databaseSizeBytes,
	)
	if err != nil {
		// Leave every metric field absent: the query failed as a whole, so
		// none of these values were actually measured, and defaulting them
		// to zero would let a condition pass on fiction.
		body.MetricsErrors = append(body.MetricsErrors, "metrics query failed: "+redactCredentials(err.Error()))
		return
	}
	body.Version = version
	body.InRecovery = &inRecovery
	body.ReplicationLagSeconds = &replicationLagSeconds
	body.CacheHitRatioSinceReset = &cacheHitRatioSinceReset
	body.DatabaseSizeBytes = &databaseSizeBytes
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
	// One physical connection serves both the ping and the probe query below;
	// the handle itself is closed per check (see defer above), so nothing
	// pools across checks. Do not also call SetMaxIdleConns(0): that would
	// force database/sql to close the connection the instant Ping releases
	// it, so the probe query would have to open and authenticate a second
	// connection, silently folding a reconnect into probe_ms.
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
	metricsStart := time.Now()
	collectPostgresMetrics(ctx, db, &body)
	body.MetricsMS = float64(time.Since(metricsStart).Microseconds()) / 1000
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
