// Package runlog writes the installed runner's structured event log: one
// redacted JSON line per event, under the runner's private state directory,
// rotated once it grows past a size cap.
package runlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// FileName is the current log file's name; rotated generations append ".N".
const FileName = "runner.log"

// maxFileBytes and maxGenerations bound the log's total footprint on disk:
// the current file plus its rotated backups.
const (
	maxFileBytes   = 5 * 1024 * 1024
	maxGenerations = 3
)

// sensitiveFieldPattern redacts a field by name as a safety net; callers must
// still never pass secrets, tokens, or repository content as field values.
var sensitiveFieldPattern = regexp.MustCompile(`(?i)token|secret|password|credential|authoriz`)

const redacted = "[redacted]"

// Logger appends structured events for one runner process. All methods are
// safe for concurrent use and for a nil receiver, so an unconfigured Logger
// can be wired in without call sites checking for one.
type Logger struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

// Open creates (or reuses) the log directory under stateDir and opens the
// current log file for appending.
func Open(stateDir string) (*Logger, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("runner log requires a state directory")
	}
	dir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create runner log directory: %w", err)
	}
	logger := &Logger{path: filepath.Join(dir, FileName)}
	if err := logger.openFile(); err != nil {
		return nil, err
	}
	return logger, nil
}

func (l *Logger) openFile() error {
	file, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open runner log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat runner log: %w", err)
	}
	l.file = file
	l.size = info.Size()
	return nil
}

// Path is the current log file's path, safe to surface in an operator-facing message.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Event appends one JSON line recording event and its fields. Logging is
// best effort: a write or rotation failure is dropped rather than
// interrupting supervision, since the log is a diagnostic aid, not the
// attempt's record of truth.
func (l *Logger) Event(event string, fields map[string]any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	record := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		if sensitiveFieldPattern.MatchString(key) {
			record[key] = redacted
			continue
		}
		record[key] = value
	}
	record["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["event"] = event
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if err := l.rotateIfNeeded(int64(len(line))); err != nil {
		return
	}
	if n, err := l.file.Write(line); err == nil {
		l.size += int64(n)
	}
}

// rotateIfNeeded renames the current file down the generation chain once
// writing next would exceed maxFileBytes, dropping the oldest generation.
func (l *Logger) rotateIfNeeded(next int64) error {
	if l.file == nil || l.size+next <= maxFileBytes {
		return nil
	}
	if err := l.file.Close(); err != nil {
		return err
	}
	l.file = nil
	for generation := maxGenerations - 1; generation >= 1; generation-- {
		src := l.generationPath(generation)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, l.generationPath(generation+1)); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(l.path, l.generationPath(1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return l.openFile()
}

func (l *Logger) generationPath(generation int) string {
	return fmt.Sprintf("%s.%d", l.path, generation)
}

// Close releases the underlying file. It is safe to call on a nil Logger.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
