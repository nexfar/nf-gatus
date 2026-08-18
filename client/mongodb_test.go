package client

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
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

// TestMongoDatabaseFromURI locks down which database a probe command runs
// against. Getting this wrong is the worst kind of silent failure this
// package can produce: a per-tenant endpoint would report the health of a
// different tenant's database while looking green.
func TestMongoDatabaseFromURI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{
			name: "no database and no query string",
			uri:  "mongodb://host:27017",
			want: "admin",
		},
		{
			name: "no database but with query params",
			uri:  "mongodb://host:27017/?readPreference=secondary",
			want: "admin",
		},
		{
			name: "a normal database",
			uri:  "mongodb://user:pw@host:27017/tenant",
			want: "tenant",
		},
		{
			name: "mongodb+srv with a database",
			uri:  "mongodb+srv://user:pw@cluster.example.net/tenant",
			want: "tenant",
		},
		{
			name: "mongodb+srv trailing slash, nothing after",
			uri:  "mongodb+srv://cluster.example.net/",
			want: "admin",
		},
		{
			name: "password containing an @",
			uri:  "mongodb://user:p@ss@host:27017/tenant",
			want: "tenant",
		},
		{
			name: "database followed by query params",
			uri:  "mongodb://host:27017/tenant?readPreference=secondary",
			want: "tenant",
		},
		{
			name: "comma-separated replica-set host list",
			uri:  "mongodb://a:27017,b:27017/tenant?replicaSet=rs0",
			want: "tenant",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := mongoDatabaseFromURI(tt.uri); got != tt.want {
				t.Errorf("mongoDatabaseFromURI(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}
}

// TestMongoBody_OmitsUnpopulatedFields locks in the "cannot be collected ->
// omitted, never defaulted" contract for mongoBody before Task 4 starts
// populating metrics_ms, metrics_errors and probe. A silently-defaulted zero
// value would let a condition pass on fiction.
func TestMongoBody_OmitsUnpopulatedFields(t *testing.T) {
	t.Parallel()
	body := mongoBody{
		ConnectMS: 12,
		ProbeMS:   3,
	}
	marshalled, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	got := string(marshalled)
	for _, absent := range []string{`"metrics_ms"`, `"metrics_errors"`, `"probe"`} {
		if strings.Contains(got, absent) {
			t.Errorf("field %q must be omitted when unpopulated, got %s", absent, got)
		}
	}
	if !strings.Contains(got, "connect_ms") || !strings.Contains(got, "probe_ms") {
		t.Errorf("expected connect_ms and probe_ms to be present, got %s", got)
	}
}

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
	// Checked as a JSON key (quoted, with trailing colon) rather than a bare
	// substring: "connections" also appears as an ordinary English word inside
	// the MetricsErrors prose above, which would otherwise false-positive.
	for _, absent := range []string{"connections", "global_lock_queue_total", "replication_lag_seconds", "repl_set_state"} {
		key := `"` + absent + `":`
		if strings.Contains(got, key) {
			t.Errorf("field %q must be omitted on a backend that cannot report it, got %s", absent, got)
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

func TestApplyMongoReplSetStatus(t *testing.T) {
	t.Parallel()
	primaryOptime := bson.NewDateTimeFromTime(time.Unix(1000, 0))
	secondaryOptime := bson.NewDateTimeFromTime(time.Unix(995, 0))
	scenarios := []struct {
		name              string
		status            bson.M
		expectedState     string
		expectedLag       *float64
		expectedErrSubstr string
	}{
		{
			name: "primary-and-self-present",
			status: bson.M{
				"myState": int32(2),
				"members": bson.A{
					bson.M{"stateStr": "PRIMARY", "optimeDate": primaryOptime},
					bson.M{"self": true, "stateStr": "SECONDARY", "optimeDate": secondaryOptime},
				},
			},
			expectedState: "SECONDARY",
			expectedLag:   float64Pointer(5),
		},
		{
			name: "no-primary-explains-the-omission",
			status: bson.M{
				"myState": int32(2),
				"members": bson.A{
					bson.M{"self": true, "stateStr": "SECONDARY", "optimeDate": secondaryOptime},
					bson.M{"stateStr": "SECONDARY", "optimeDate": secondaryOptime},
				},
			},
			expectedState:     "SECONDARY",
			expectedErrSubstr: "no member reports stateStr PRIMARY",
		},
		{
			name: "no-self-explains-the-omission",
			status: bson.M{
				"myState": int32(1),
				"members": bson.A{
					bson.M{"stateStr": "PRIMARY", "optimeDate": primaryOptime},
				},
			},
			expectedState:     "PRIMARY",
			expectedErrSubstr: "no member is flagged as self",
		},
		{
			name:              "no-members-array-explains-the-omission",
			status:            bson.M{"myState": int32(1)},
			expectedState:     "PRIMARY",
			expectedErrSubstr: "reported no members array",
		},
		{
			name:              "unrecognised-state-code-explains-the-omission",
			status:            bson.M{"myState": int32(99)},
			expectedErrSubstr: "unrecognised myState value",
		},
		{
			name:              "fatal-state-code-is-mapped",
			status:            bson.M{"myState": int32(4), "members": bson.A{}},
			expectedState:     "FATAL",
			expectedErrSubstr: "no member reports stateStr PRIMARY",
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			var body mongoBody
			applyMongoReplSetStatus(scenario.status, &body)
			if body.ReplSetState != scenario.expectedState {
				t.Errorf("expected repl_set_state %q, got %q", scenario.expectedState, body.ReplSetState)
			}
			if scenario.expectedLag == nil {
				if body.ReplicationLagSeconds != nil {
					t.Errorf("expected replication_lag_seconds to be absent, got %v", *body.ReplicationLagSeconds)
				}
			} else if body.ReplicationLagSeconds == nil {
				t.Error("expected replication_lag_seconds to be present")
			} else if *body.ReplicationLagSeconds != *scenario.expectedLag {
				t.Errorf("expected replication_lag_seconds %v, got %v", *scenario.expectedLag, *body.ReplicationLagSeconds)
			}
			if len(scenario.expectedErrSubstr) > 0 {
				if !strings.Contains(strings.Join(body.MetricsErrors, "|"), scenario.expectedErrSubstr) {
					t.Errorf("expected metrics_errors to mention %q, got %v", scenario.expectedErrSubstr, body.MetricsErrors)
				}
			} else if len(body.MetricsErrors) > 0 {
				t.Errorf("expected no metrics_errors, got %v", body.MetricsErrors)
			}
		})
	}
}

func TestReplSetStateName(t *testing.T) {
	t.Parallel()
	// State 4 (FATAL) is easy to miss because it is absent from some
	// documentation tables; an unmapped code must not silently vanish.
	if name, ok := replSetStateName(int32(4)); !ok || name != "FATAL" {
		t.Errorf("expected state 4 to map to FATAL, got %q (ok=%v)", name, ok)
	}
	if _, ok := replSetStateName(int32(42)); ok {
		t.Error("expected an unknown state code to report ok=false")
	}
	if _, ok := replSetStateName("PRIMARY"); ok {
		t.Error("expected a non-numeric state to report ok=false")
	}
}

func float64Pointer(v float64) *float64 {
	return &v
}
