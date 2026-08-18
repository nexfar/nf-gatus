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
