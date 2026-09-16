package runlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEventWritesOneJSONLinePerCall(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	logger.Event("claim", map[string]any{"run_id": "run1", "attempt_id": "attempt1", "fence": 1})
	logger.Event("heartbeat", map[string]any{"attempt_id": "attempt1"})

	lines := readLines(t, logger.Path())
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(lines), lines)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first line: %v", err)
	}
	if first["event"] != "claim" || first["run_id"] != "run1" {
		t.Fatalf("unexpected first record: %v", first)
	}
	if _, ok := first["time"]; !ok {
		t.Fatalf("expected a time field: %v", first)
	}
}

func TestEventRedactsSensitiveFieldNames(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	logger.Event("claim", map[string]any{"token": "super-secret", "authorization_header": "Bearer x", "run_id": "run1"})

	lines := readLines(t, logger.Path())
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if strings.Contains(lines[0], "super-secret") || strings.Contains(lines[0], "Bearer x") {
		t.Fatalf("sensitive field leaked into log line: %s", lines[0])
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if record["token"] != redacted || record["authorization_header"] != redacted {
		t.Fatalf("expected redacted sensitive fields: %v", record)
	}
	if record["run_id"] != "run1" {
		t.Fatalf("expected non-sensitive field to survive: %v", record)
	}
}

func TestEventRotatesPastSizeCap(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	filler := strings.Repeat("x", 4096)
	for i := 0; i < 4*((maxFileBytes/4096)+50); i++ {
		logger.Event("progress", map[string]any{"filler": filler})
	}

	info, err := os.Stat(logger.Path())
	if err != nil {
		t.Fatalf("stat current log: %v", err)
	}
	if info.Size() > maxFileBytes {
		t.Fatalf("current log exceeded its size cap: %d bytes", info.Size())
	}
	if _, err := os.Stat(logger.path + ".1"); err != nil {
		t.Fatalf("expected a rotated backup: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(logger.Path()))
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	generations := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), FileName+".") {
			generations++
		}
	}
	if generations > maxGenerations {
		t.Fatalf("expected at most %d rotated backups, found %d", maxGenerations, generations)
	}
}

func TestEventOnNilLoggerIsNoOp(t *testing.T) {
	var logger *Logger
	logger.Event("claim", map[string]any{"run_id": "run1"})
	if logger.Close() != nil {
		t.Fatalf("expected nil Logger Close to be a no-op")
	}
	if logger.Path() != "" {
		t.Fatalf("expected nil Logger Path to be empty")
	}
}

func TestOpenRejectsEmptyStateDir(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatalf("expected an error for an empty state directory")
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan log file: %v", err)
	}
	return lines
}
