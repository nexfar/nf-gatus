package key

import "strings"

// ConvertGroupAndNameToKey converts a group and a name to a key
func ConvertGroupAndNameToKey(groupName, name string) string {
	return sanitize(groupName) + "_" + sanitize(name)
}

// ConvertGroupToKey converts a group name to its sanitized key representation.
// This is the same sanitization applied to the group portion of a full key, which
// makes it suitable for matching a value (e.g. a subdomain label) against a group.
func ConvertGroupToKey(groupName string) string {
	return sanitize(groupName)
}

// ExtractGroupFromKey returns the sanitized group portion of a key produced by
// ConvertGroupAndNameToKey. Because sanitize replaces "_" with "-", the first "_"
// in a key is always the separator between the group and the name. Returns an empty
// string for keys with no group (i.e. keys that start with "_").
func ExtractGroupFromKey(key string) string {
	if i := strings.Index(key, "_"); i >= 0 {
		return key[:i]
	}
	return ""
}

func sanitize(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, ",", "-")
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "#", "-")
	s = strings.ReplaceAll(s, "+", "-")
	s = strings.ReplaceAll(s, "&", "-")
	return s
}
