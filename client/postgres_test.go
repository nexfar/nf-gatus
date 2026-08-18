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
