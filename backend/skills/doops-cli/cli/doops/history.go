package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type HistoryEntry struct {
	Timestamp string `json:"timestamp"`
	Target    string `json:"target"`
	Session   string `json:"session"`
	Command   string `json:"command"`
	Source    string `json:"source"`
}

func RecordHistory(target, session, command string) error {
	entry := HistoryEntry{
		Timestamp: time.Now().Format(time.RFC3339),
		Target:    target,
		Session:   session,
		Command:   command,
		Source:    "cli",
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	historyPath := historyLogPath()

	f, err := os.OpenFile(historyPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}

	return nil
}

func historyLogPath() string {
	return filepath.Join(ensureDoopsStateDir(), "history.jsonl")
}
