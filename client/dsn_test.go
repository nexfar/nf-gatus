package client

import (
	"errors"
	"testing"
)

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
