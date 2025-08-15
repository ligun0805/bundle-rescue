package main

import (
	"encoding/json"
	"os"
)

const sessionFile = "pairs_session.json"

func saveQueueToFile() {
	f, err := os.Create(sessionFile)
	if err != nil { return }
	defer f.Close()
	json.NewEncoder(f).Encode(pairs)
}

func loadQueueFromFile() {
	f, err := os.Open(sessionFile)
	if err != nil { return }
	defer f.Close()
	var arr []pairRow
	if err := json.NewDecoder(f).Decode(&arr); err == nil {
		pairs = arr
	}
}
