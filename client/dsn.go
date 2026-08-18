package client

import (
	"errors"
	"regexp"
)

// dsnCredentialsPattern matches the userinfo portion of a URI, e.g. the
// "user:password@" in "postgres://user:password@host:5432/db".
//
// The userinfo group is [^/\s]* rather than something stricter for two reasons.
// It is greedy and cannot cross a "/" or whitespace, so it stops at the LAST
// "@" before the host: that is what redacts a password containing an unencoded
// "@" - by far the most common MongoDB URI mistake - whose tail would otherwise
// survive into the error the driver builds around url.Parse. And it accepts an
// empty username, so "postgres://:password@host" is redacted too. Because the
// group cannot cross a "/", a URL such as "https://example.com/path@thing" is
// left alone: that "@" follows a path separator and is not userinfo.
var dsnCredentialsPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)([^/\s]*)@`)

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
