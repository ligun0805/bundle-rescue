package main

import "sync"

type TelemetryItem struct {
	Time      string `json:"time"`
	Action    string `json:"action"`
	PairIndex int    `json:"pairIndex"`
	Relay     string `json:"relay,omitempty"`
	OK        bool   `json:"ok,omitempty"`
	Error     string `json:"error,omitempty"`
	Raw       string `json:"raw,omitempty"`
}

var (
	telemetry []TelemetryItem
	telMu     sync.Mutex
)

func telAdd(it TelemetryItem) {
	telMu.Lock()
	telemetry = append(telemetry, it)
	telMu.Unlock()
}
