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
