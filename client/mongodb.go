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
	ConnectMS float64        `json:"connect_ms"`
	ProbeMS   float64        `json:"probe_ms"`
	MetricsMS float64        `json:"metrics_ms,omitempty"`
	Probe     map[string]any `json:"probe,omitempty"`

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

	MetricsErrors []string `json:"metrics_errors,omitempty"`
}

// mongoConnections is the connection gauge from serverStatus. FerretDB does not
// report it, so it is a pointer and omitted there.
type mongoConnections struct {
	Current   int64   `json:"current"`
	Available int64   `json:"available"`
	UsedPct   float64 `json:"used_pct"`
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

// ValidateMongoProbeCommand reports whether body is a usable probe command
// document. It exists so that config validation rejects a malformed document at
// startup instead of letting every check report a false outage forever.
func ValidateMongoProbeCommand(body string) error {
	_, err := parseMongoProbeCommand(body)
	return err
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
	metricsStart := time.Now()
	collectMongoMetrics(ctx, cli, &body)
	body.MetricsMS = float64(time.Since(metricsStart).Microseconds()) / 1000
	marshalled, err := json.Marshal(body)
	if err != nil {
		return true, duration, nil, fmt.Errorf("failed to marshal body: %w", redactError(err))
	}
	return true, duration, marshalled, nil
}

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
	applyMongoReplSetStatus(status, body)
}

// applyMongoReplSetStatus maps a decoded replSetGetStatus document onto body. It
// is separate from the command round trip so that every omission path can be
// tested without a server.
//
// Every path that leaves ReplicationLagSeconds or ReplSetState absent records
// why. A "<" condition passes silently on a missing field, so an unexplained
// omission renders "replication_lag_seconds < 10" green during exactly the
// incident - an election with no PRIMARY - that the condition exists to catch.
func applyMongoReplSetStatus(status bson.M, body *mongoBody) {
	if state, ok := status["myState"]; ok {
		if stateStr, ok := replSetStateName(state); ok {
			body.ReplSetState = stateStr
		} else {
			body.MetricsErrors = append(body.MetricsErrors,
				fmt.Sprintf("repl_set_state omitted: replSetGetStatus reported an unrecognised myState value (%v)", state))
		}
	}
	members, ok := status["members"].(bson.A)
	if !ok {
		body.MetricsErrors = append(body.MetricsErrors,
			"replication_lag_seconds omitted: replSetGetStatus reported no members array")
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
	if !havePrimary {
		body.MetricsErrors = append(body.MetricsErrors,
			"replication_lag_seconds omitted: no member reports stateStr PRIMARY, so there is no reference optime (an election may be in progress)")
	}
	if !haveSelf {
		body.MetricsErrors = append(body.MetricsErrors,
			"replication_lag_seconds omitted: no member is flagged as self, so this member's optime is unknown")
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
		4: "FATAL", 5: "STARTUP2", 6: "UNKNOWN", 7: "ARBITER", 8: "DOWN",
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
