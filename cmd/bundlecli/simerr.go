package main

import "strings"

// friendlySimErr normalizes common relay errors for readable CLI.
func friendlySimErr(s string) string {
	ls := strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(ls, "400 bad request") {
		if i := strings.Index(ls, "{"); i > 0 { ls = ls[i:] }
	}
	ls = strings.ReplaceAll(ls, "method not found", "simulation not supported by relay")
	ls = strings.ReplaceAll(ls, "method not available", "simulation not supported by relay")
	ls = strings.ReplaceAll(ls, "unsupported: eth_callbundle", "simulation not supported by relay")
	switch {
	case strings.Contains(ls, "unsupported: eth_callbundle"), strings.Contains(ls, "invalid method"), strings.Contains(ls, "method not found"):
		return "simulation not supported by relay"
	case strings.Contains(ls, "insufficient funds for gas"):
		return "insufficient ETH for simulation"
	case strings.Contains(ls, "invalid character '<'"):
		return "non-JSON/HTML response (proxy/cf?)"
	case strings.Contains(ls, "dial tcp"), strings.Contains(ls, "lookup "):
		return "network/DNS error"
	}
	return s
}
