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

func TestEventRedactsNestedMapsAndSecretShapedValues(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	logger.Event("claim", map[string]any{
		"headers": map[string]any{"api_key": "abc123", "x_id": "run1"},
		"raw":     "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij",
	})

	lines := readLines(t, logger.Path())
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if strings.Contains(lines[0], "abc123") || strings.Contains(lines[0], "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatalf("secret-shaped or nested-key value leaked into log line: %s", lines[0])
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	headers, ok := record["headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested headers map: %v", record)
	}
	if headers["api_key"] != redacted {
		t.Fatalf("expected nested sensitive key redacted: %v", headers)
	}
	if headers["x_id"] != "run1" {
		t.Fatalf("expected nested non-sensitive key to survive: %v", headers)
	}
	if record["raw"] != redacted {
		t.Fatalf("expected bearer-shaped value redacted: %v", record)
	}
}

func TestOpenPinsPrivateDirectoryAndFileModes(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	dirInfo, err := os.Stat(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatalf("stat log dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Fatalf("expected log dir mode 0700, got %o", mode)
	}
	fileInfo, err := os.Stat(logger.Path())
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != 0o600 {
		t.Fatalf("expected log file mode 0600, got %o", mode)
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
		if strings.HasPrefix(entry.Name(), fileName+".") {
			generations++
		}
	}
	if generations > maxGenerations {
		t.Fatalf("expected at most %d rotated backups, found %d", maxGenerations, generations)
	}
}

func TestEventDropsARecordLargerThanTheGenerationCap(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	logger.Event("progress", map[string]any{"filler": strings.Repeat("x", maxFileBytes+1)})

	lines := readLines(t, logger.Path())
	if len(lines) != 0 {
		t.Fatalf("expected the oversized record to be dropped, got %d lines", len(lines))
	}
	info, err := os.Stat(logger.Path())
	if err != nil {
		t.Fatalf("stat current log: %v", err)
	}
	if info.Size() > maxFileBytes {
		t.Fatalf("current log exceeded its size cap: %d bytes", info.Size())
	}
	if _, err := os.Stat(logger.path + ".1"); err == nil {
		t.Fatalf("expected no rotation to be triggered by a record that can never fit")
	}

	logger.Event("claim", map[string]any{"run_id": "run1"})
	lines = readLines(t, logger.Path())
	if len(lines) != 1 {
		t.Fatalf("expected the logger to keep working after dropping an oversized record, got %d lines", len(lines))
	}
}

func TestEventReopensAfterFileLost(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	logger.mu.Lock()
	_ = logger.file.Close()
	logger.file = nil
	logger.mu.Unlock()

	logger.Event("claim", map[string]any{"run_id": "run1"})

	lines := readLines(t, logger.Path())
	if len(lines) != 1 {
		t.Fatalf("expected the logger to reopen and write the event, got %d lines", len(lines))
	}
}

func TestEventReopensAfterAWriteError(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	logger.mu.Lock()
	_ = logger.file.Close() // closed out from under l.file, so the next Write fails.
	logger.mu.Unlock()

	logger.Event("claim", map[string]any{"run_id": "dropped"})
	logger.mu.Lock()
	stillSet := logger.file != nil
	logger.mu.Unlock()
	if stillSet {
		t.Fatalf("expected a write error to clear the file so the next Event reopens it")
	}

	logger.Event("claim", map[string]any{"run_id": "recovered"})
	lines := readLines(t, logger.Path())
	if len(lines) != 1 {
		t.Fatalf("expected the logger to recover and write the second event, got %d lines", len(lines))
	}
}

func TestRotateClearsTheFileEvenWhenCloseFails(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	logger.mu.Lock()
	logger.size = maxFileBytes
	_ = logger.file.Close() // rotateIfNeeded's own Close then fails on the already-closed fd.
	rotateErr := logger.rotateIfNeeded(1)
	stillSet := logger.file != nil
	logger.mu.Unlock()
	if rotateErr == nil {
		t.Fatalf("expected rotateIfNeeded to surface the close error")
	}
	if stillSet {
		t.Fatalf("expected rotateIfNeeded to clear l.file even when Close fails, leaving no dangling closed fd")
	}

	logger.Event("claim", map[string]any{"run_id": "run1"})
	lines := readLines(t, logger.Path())
	if len(lines) != 1 {
		t.Fatalf("expected the logger to reopen after a failed rotation, got %d lines", len(lines))
	}
}

func TestEventDropsSilentlyWhenReopenFails(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer logger.Close()

	logDir := filepath.Dir(logger.Path())
	logger.mu.Lock()
	_ = logger.file.Close()
	logger.file = nil
	logger.mu.Unlock()
	if err := os.RemoveAll(logDir); err != nil {
		t.Fatalf("remove log dir: %v", err)
	}

	logger.Event("claim", map[string]any{"run_id": "run1"})
	logger.Event("claim", map[string]any{"run_id": "run2"})

	if _, err := os.Stat(logger.Path()); err == nil {
		t.Fatalf("expected no log file to reappear once the directory is gone")
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
