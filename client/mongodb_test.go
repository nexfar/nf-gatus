package client

import (
	"encoding/json"
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
