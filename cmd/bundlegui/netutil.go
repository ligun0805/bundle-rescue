package main

import "strings"

// isRPCTimeout detects common timeout/cancellation substrings.
func isRPCTimeout(err error) bool {
	if err == nil { return false }
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "timed out") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "context canceled")
}
