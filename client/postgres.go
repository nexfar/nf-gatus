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
