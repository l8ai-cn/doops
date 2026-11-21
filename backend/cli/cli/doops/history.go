package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type HistoryEntry struct {
	Timestamp string `json:"timestamp"`
	Target    string `json:"target"`
	Session   string `json:"session"`
	Command   string `json:"command"`
	Source    string `json:"source"`
	*HistoryResult
}

type HistoryResult struct {
	UpgradeOperationID string `json:"upgrade_operation_id"`
	Status             string `json:"status"`
	Output             string `json:"output"`
	Error              string `json:"error,omitempty"`
}

func RecordHistory(target, session, command string, result ...HistoryResult) error {
	entry := HistoryEntry{
		Timestamp: time.Now().Format(time.RFC3339),
		Target:    target,
		Session:   session,
		Command:   command,
		Source:    "cli",
	}
	if len(result) > 0 {
		entry.HistoryResult = &result[0]
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	historyPath := historyLogPath()

	f, err := os.OpenFile(historyPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
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
	dir := ensureDoopsStateDir()
	legacy := filepath.Join(dir, "history.jsonl")
	if info, err := os.Lstat(legacy); err == nil && info.Mode().IsRegular() {
		// Keep the operator's historical file but stop displaying its raw input.
		_ = os.Chmod(legacy, 0600)
	}
	return filepath.Join(dir, "history-v2.jsonl")
}

var upgradeSecretKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|private[_-]?key|authorization|cookie|payload)$`)

var upgradeTextRedactions = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]"},
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`), "${1}[REDACTED]@"},
	{regexp.MustCompile(`(?i)((?:Bearer|Basic)\s+)[A-Za-z0-9._~+/=-]+`), "${1}[REDACTED]"},
	{regexp.MustCompile(`(?i)((?:[A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|private[_-]?key|signature|credential)|sig|auth|authorization|cookie)["']?\s*[=:]\s*)(?:"(?:\\.|[^"\\])*"|'[^']*'|[^\s,;}&"']+)`), "${1}[REDACTED]"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`), "[REDACTED]"},
}

// Sanitize values before JSON encoding; rewriting an encoded receipt can corrupt
// its quotes. Keep operation/job/commit evidence and full non-sensitive log text.
func redactUpgradeText(text, token string) string {
	if token != "" {
		text = strings.ReplaceAll(text, token, "[REDACTED]")
	}
	for _, rule := range upgradeTextRedactions {
		text = rule.pattern.ReplaceAllString(text, rule.replace)
	}
	return text
}

func redactUpgradeValue(value interface{}, token string) interface{} {
	switch value := value.(type) {
	case string:
		return redactUpgradeText(value, token)
	case map[string]interface{}:
		if value == nil {
			return nil
		}
		clean := make(map[string]interface{}, len(value))
		for key, item := range value {
			if upgradeSecretKey.MatchString(key) {
				clean[key] = "[REDACTED]"
			} else {
				clean[key] = redactUpgradeValue(item, token)
			}
		}
		return clean
	case []interface{}:
		if value == nil {
			return nil
		}
		clean := make([]interface{}, len(value))
		for i, item := range value {
			clean[i] = redactUpgradeValue(item, token)
		}
		return clean
	default:
		return value
	}
}
