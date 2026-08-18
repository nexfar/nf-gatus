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
