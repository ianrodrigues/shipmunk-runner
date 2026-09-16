// Package runlog writes the installed runner's structured event log: one
// redacted JSON line per event, under the runner's private state directory,
// rotated once it grows past a size cap.
//
// Event holds the Logger's mutex across the file write, and a caller such as
// leaseGuard.renew logs while holding its own lock; a stalled filesystem can
// therefore delay lease renewal. This is an accepted tradeoff since the log
// write is normally fast and losing an event is worse than never trying.
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

// fileName is the current log file's name; rotated generations append ".N".
const fileName = "runner.log"

// maxFileBytes and maxGenerations bound the log's total footprint on disk:
// the current file plus its rotated backups.
const (
	maxFileBytes   = 5 * 1024 * 1024
	maxGenerations = 3
)

// sensitiveFieldPattern redacts a field by name as a safety net; callers must
// still never pass secrets, tokens, or repository content as field values.
// "key" and "session" are broad on purpose: a future field such as
// cache_key, sandbox_key, or session_id redacts by default rather than
// silently slipping past the net.
var sensitiveFieldPattern = regexp.MustCompile(`(?i)token|secret|password|credential|authoriz|key|bearer|jwt|cookie|session|signature|signed_url`)

// secretShapePattern catches a bearer-prefixed or JWT-shaped value even under
// a field name the key pattern above misses.
var secretShapePattern = regexp.MustCompile(`(?i)^bearer\s+\S+$|^[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}$`)

const redacted = "[redacted]"

// Logger appends structured events for one runner process. All methods are
// safe for concurrent use and for a nil receiver, so an unconfigured Logger
// can be wired in without call sites checking for one.
type Logger struct {
	mu           sync.Mutex
	path         string
	file         *os.File
	size         int64
	gaveUpNotice bool
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
	// MkdirAll leaves an already-existing, more permissive directory as-is; tighten it.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure runner log directory: %w", err)
	}
	logger := &Logger{path: filepath.Join(dir, fileName)}
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
	// O_CREATE's mode only applies when the file is new; tighten an already-existing, more permissive file.
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure runner log: %w", err)
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
		if err := l.openFile(); err != nil {
			if !l.gaveUpNotice {
				fmt.Fprintf(os.Stderr, "runner: giving up on the event log at %s: %v\n", l.path, err)
				l.gaveUpNotice = true
			}
			return
		}
		l.gaveUpNotice = false
	}
	record := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		record[key] = sanitizeValue(key, value)
	}
	record["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["event"] = event
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if int64(len(line)) > maxFileBytes {
		// A record larger than a whole generation can never fit after
		// rotation; writing it would break the per-file and total size
		// bounds, so drop it, matching the best-effort contract.
		return
	}
	if err := l.rotateIfNeeded(int64(len(line))); err != nil {
		return
	}
	if n, err := l.file.Write(line); err != nil {
		// A write error leaves the fd in an unknown state; drop it so the
		// next Event reopens (and can emit the give-up notice) instead of
		// silently discarding every subsequent event through a dead file.
		l.file = nil
	} else {
		l.size += int64(n)
	}
}

// sanitizeValue redacts by field name, recurses into nested maps so a
// secret cannot hide a level down, and stringifies anything that is not a
// scalar or a map before checking its shape, since an unredacted struct or
// slice could otherwise carry arbitrary content into the log verbatim.
func sanitizeValue(key string, value any) any {
	if sensitiveFieldPattern.MatchString(key) {
		return redacted
	}
	if nested, ok := value.(map[string]any); ok {
		sanitized := make(map[string]any, len(nested))
		for nestedKey, nestedValue := range nested {
			sanitized[nestedKey] = sanitizeValue(nestedKey, nestedValue)
		}
		return sanitized
	}
	if !isScalar(value) {
		value = fmt.Sprintf("%v", value)
	}
	if text, ok := value.(string); ok && secretShapePattern.MatchString(strings.TrimSpace(text)) {
		return redacted
	}
	return value
}

func isScalar(value any) bool {
	switch value.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	default:
		return false
	}
}

// rotateIfNeeded renames the current file down the generation chain once
// writing next would exceed maxFileBytes, dropping the oldest generation.
func (l *Logger) rotateIfNeeded(next int64) error {
	if l.file == nil || l.size+next <= maxFileBytes {
		return nil
	}
	closeErr := l.file.Close()
	l.file = nil
	if closeErr != nil {
		return closeErr
	}
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
