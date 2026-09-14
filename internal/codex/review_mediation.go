package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	MaxReviewRequestBytes  = 4 * 1024
	MaxReviewResponseBytes = 64 * 1024
	MaxReviewOutputBytes   = 48 * 1024
	MaxReviewQueryBytes    = 256
	MaxReviewListEntries   = 500
	MaxReviewSearchMatches = 200
	MaxReviewSnippetBytes  = 300
	MaxReviewLineCount     = 400
)

// ReviewMediator serves bounded, read-only list/search/read/diff operations
// over the two authorized review snapshots. It loads both snapshots into
// memory once, through the same validated snapshotRoot used for trusted patch
// collection, and every operation afterward resolves against those in-memory
// maps only. A request path can therefore never reach the filesystem again:
// traversal, absolute paths and symlink-escape strings simply fail to match
// any entry, and a snapshot containing a symlink, hard link or special file
// fails to load at all. This is the enforced sharing boundary: only bytes
// physically present in the two authorized snapshots, bounded and possibly
// truncated, are ever returned. It is not general data-loss prevention.
type ReviewMediator struct {
	mu        sync.Mutex
	fence     int64
	last      int64
	remaining int
	sealed    bool

	headRoot, baselineRoot     string
	headHandle, baselineHandle *os.File

	loadOnce sync.Once
	loadErr  error
	head     map[string]fileState
	baseline map[string]fileState

	diffOnce sync.Once
	diffErr  error
	diff     *Patch
}

// NewReviewMediator binds one attempt's fence and request budget to the two
// pinned snapshot directories already opened by the transport.
func NewReviewMediator(fence int64, maxRequests int, headRoot string, headHandle *os.File, baselineRoot string, baselineHandle *os.File) (*ReviewMediator, error) {
	if fence < 1 || fence > protocol.MaxSafeInteger || maxRequests < 1 || headRoot == "" || headHandle == nil || baselineRoot == "" || baselineHandle == nil {
		return nil, errors.New("review mediator configuration is invalid")
	}
	return &ReviewMediator{fence: fence, remaining: maxRequests, headRoot: headRoot, headHandle: headHandle, baselineRoot: baselineRoot, baselineHandle: baselineHandle}, nil
}

// Seal permanently rejects new review requests once the attempt is winding down.
func (m *ReviewMediator) Seal() {
	m.mu.Lock()
	m.sealed = true
	m.mu.Unlock()
}

func (m *ReviewMediator) load() error {
	m.loadOnce.Do(func() {
		head, err := snapshotPinned(m.headRoot, m.headHandle)
		if err != nil {
			m.loadErr = fmt.Errorf("review workspace snapshot: %w", err)
			return
		}
		baseline, err := snapshotPinned(m.baselineRoot, m.baselineHandle)
		if err != nil {
			m.loadErr = fmt.Errorf("review baseline snapshot: %w", err)
			return
		}
		m.head, m.baseline = head, baseline
	})
	return m.loadErr
}

// Handle validates and executes exactly one closed-schema review request.
// Only protocol-level violations (bad schema, fence, sequence, exhausted
// budget, an unsafe snapshot) fail the call outright; an ordinary business
// rejection such as an unknown path is returned as a normal ok:false result
// so a single mistaken argument does not end the attempt.
func (m *ReviewMediator) Handle(ctx context.Context, raw []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	request, err := decodeReviewRequest(raw, true)
	if err != nil {
		return nil, err
	}
	if request.Fence != m.fence {
		return nil, errors.New("review request fence is invalid")
	}
	if request.ID != m.last+1 {
		return nil, errors.New("review request sequence is invalid")
	}
	if m.sealed {
		return nil, errors.New("review snapshot is sealed")
	}
	if m.remaining == 0 {
		return nil, errors.New("review request budget is exhausted")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Spend the sequence before execution, matching the shell mediator.
	m.last = request.ID
	m.remaining--

	if err := m.load(); err != nil {
		return nil, errors.New("review snapshot is unavailable")
	}

	output, truncated, ok := m.execute(request)
	return encodeReviewResponse(m.fence, request.ID, ok, output, truncated)
}

func (m *ReviewMediator) execute(request reviewRequest) (output string, truncated, ok bool) {
	switch request.Op {
	case "review_list":
		return m.list(request.Snapshot, request.Path)
	case "review_search":
		return m.search(request.Snapshot, request.Path, request.Query)
	case "review_read":
		return m.read(request.Snapshot, request.Path, request.StartLine, request.LineCount)
	case "review_diff":
		return m.reviewDiff(request.Path)
	default:
		return "unsupported review operation", false, false
	}
}

func (m *ReviewMediator) snapshotByName(name string) map[string]fileState {
	if name == "baseline" {
		return m.baseline
	}
	return m.head
}

func (m *ReviewMediator) list(snapshotName, dir string) (string, bool, bool) {
	if dir != "" && !safeRepositoryPath(dir) {
		return "path is invalid", false, false
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	isDir := make(map[string]bool)
	seen := make(map[string]bool)
	var names []string
	for key := range m.snapshotByName(snapshotName) {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := key[len(prefix):]
		if rest == "" {
			continue
		}
		segment := rest
		if index := strings.IndexByte(rest, '/'); index >= 0 {
			segment = rest[:index]
			isDir[segment] = true
		}
		if !seen[segment] {
			seen[segment] = true
			names = append(names, segment)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "no entries", false, true
	}
	total := len(names)
	truncated := total > MaxReviewListEntries
	if truncated {
		names = names[:MaxReviewListEntries]
	}
	var b strings.Builder
	for _, name := range names {
		kind := "file"
		if isDir[name] {
			kind = "dir"
		}
		fmt.Fprintf(&b, "%s\t%s\n", kind, name)
	}
	if truncated {
		fmt.Fprintf(&b, "... truncated: showing %d of %d entries\n", len(names), total)
	}
	return strings.TrimRight(b.String(), "\n"), truncated, true
}

func (m *ReviewMediator) search(snapshotName, dir, query string) (string, bool, bool) {
	if dir != "" && !safeRepositoryPath(dir) {
		return "path is invalid", false, false
	}
	files := m.snapshotByName(snapshotName)
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	keys := make([]string, 0, len(files))
	for key := range files {
		if dir == "" || key == dir || strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	matches := 0
	truncated := false
outer:
	for _, key := range keys {
		content := files[key].data
		if !utf8.Valid(content) {
			continue
		}
		for number, line := range strings.Split(string(content), "\n") {
			if !strings.Contains(line, query) {
				continue
			}
			matches++
			if matches > MaxReviewSearchMatches {
				truncated = true
				break outer
			}
			snippet := line
			if len(snippet) > MaxReviewSnippetBytes {
				snippet = snippet[:MaxReviewSnippetBytes] + "…"
			}
			entry := fmt.Sprintf("%s:%d: %s\n", key, number+1, snippet)
			if b.Len()+len(entry) > MaxReviewOutputBytes {
				truncated = true
				break outer
			}
			b.WriteString(entry)
		}
	}
	if matches == 0 {
		return "no matches", false, true
	}
	text := strings.TrimRight(b.String(), "\n")
	if truncated {
		text += "\n... truncated: additional matches not shown"
	}
	return text, truncated, true
}

func (m *ReviewMediator) read(snapshotName, path string, startLine, lineCount int64) (string, bool, bool) {
	if path == "" || !safeRepositoryPath(path) {
		return "path is invalid", false, false
	}
	state, exists := m.snapshotByName(snapshotName)[path]
	if !exists {
		return "file not found", false, false
	}
	if !utf8.Valid(state.data) {
		return "binary content is not shown", false, false
	}
	lines := strings.Split(string(state.data), "\n")
	total := int64(len(lines))
	if startLine > total {
		return "start line is beyond the end of the file", false, false
	}
	end := startLine - 1 + lineCount
	if end > total {
		end = total
	}
	window := lines[startLine-1 : end]
	var b strings.Builder
	truncated := end < total
	shown := len(window)
	for index, line := range window {
		if int64(b.Len()+len(line)+1) > MaxReviewOutputBytes {
			truncated = true
			shown = index
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	text := strings.TrimRight(b.String(), "\n")
	if truncated {
		text += fmt.Sprintf("\n... truncated: showing lines %d-%d of %d", startLine, startLine-1+int64(shown), total)
	}
	return text, truncated, true
}

func (m *ReviewMediator) reviewDiff(path string) (string, bool, bool) {
	if path != "" && !safeRepositoryPath(path) {
		return "path is invalid", false, false
	}
	if err := m.loadDiff(); err != nil {
		return "diff is unavailable", false, false
	}
	if path == "" {
		if m.diff == nil || len(m.diff.ChangedFiles) == 0 {
			return "no changes", false, true
		}
		var b strings.Builder
		truncated := false
		for _, change := range m.diff.ChangedFiles {
			status := "modified"
			switch {
			case change.BeforeSHA256 == nil:
				status = "added"
			case change.AfterSHA256 == nil:
				status = "deleted"
			}
			entry := fmt.Sprintf("%s\t%s\n", status, change.Path)
			if b.Len()+len(entry) > MaxReviewOutputBytes {
				truncated = true
				break
			}
			b.WriteString(entry)
		}
		return strings.TrimRight(b.String(), "\n"), truncated, true
	}
	_, inBaseline := m.baseline[path]
	_, inHead := m.head[path]
	if !inBaseline && !inHead {
		return "file not found in either snapshot", false, false
	}
	if m.diff == nil {
		return "no differences", false, true
	}
	chunk, found := extractFileDiff(m.diff.Bytes, path)
	if !found {
		return "no differences", false, true
	}
	truncated := false
	if len(chunk) > MaxReviewOutputBytes {
		chunk = chunk[:MaxReviewOutputBytes]
		truncated = true
	}
	if truncated {
		chunk += "\n... truncated: diff exceeds the output budget"
	}
	return chunk, truncated, true
}

func (m *ReviewMediator) loadDiff() error {
	m.diffOnce.Do(func() {
		generated, err := generatePatch(m.baseline, m.head)
		if err != nil {
			m.diffErr = err
			return
		}
		patch, err := collectSnapshotPatch(m.baseline, m.head, generated)
		if err != nil {
			m.diffErr = err
			return
		}
		m.diff = patch
	})
	return m.diffErr
}

// extractFileDiff returns the single-file section of a unified diff produced
// without rename detection, so each file's header is exactly one line.
func extractFileDiff(diff []byte, path string) (string, bool) {
	marker := "diff --git a/" + path + " b/" + path
	text := string(diff)
	index := strings.Index(text, marker)
	if index < 0 {
		return "", false
	}
	rest := text[index:]
	if next := strings.Index(rest[1:], "\ndiff --git a/"); next >= 0 {
		rest = rest[:next+1]
	}
	return strings.TrimRight(rest, "\n"), true
}

type reviewRequest struct {
	Fence     int64
	ID        int64
	Op        string
	Snapshot  string
	Path      string
	Query     string
	StartLine int64
	LineCount int64
}

// decodeReviewRequest independently re-validates a closed schema per
// operation. requireFence distinguishes the host-trusted, fence-bound frame
// handled here from the untrusted bridge frame the transport decodes first
// (which must never itself carry a fence).
func decodeReviewRequest(raw []byte, requireFence bool) (reviewRequest, error) {
	value, err := protocol.Decode(raw, MaxReviewRequestBytes)
	if err != nil {
		return reviewRequest{}, errors.New("review request schema is invalid")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return reviewRequest{}, errors.New("review request schema is invalid")
	}
	var fence int64
	extra := 0
	if requireFence {
		var fenceOK bool
		fence, fenceOK = safeInteger(object["fence"])
		if !fenceOK {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		extra = 1
	} else if _, exists := object["fence"]; exists {
		return reviewRequest{}, errors.New("review request schema is invalid")
	}
	id, idOK := safeInteger(object["id"])
	op, opOK := object["op"].(string)
	if !idOK || !opOK {
		return reviewRequest{}, errors.New("review request schema is invalid")
	}
	request := reviewRequest{Fence: fence, ID: id, Op: op}
	baseFields := 2 + extra
	switch op {
	case "review_list":
		if len(object) != baseFields+2 {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		snapshot, path, ok := reviewSnapshotAndPath(object)
		if !ok {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		request.Snapshot, request.Path = snapshot, path
	case "review_search":
		if len(object) != baseFields+3 {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		snapshot, path, ok := reviewSnapshotAndPath(object)
		query, queryOK := object["query"].(string)
		if !ok || !queryOK || query == "" || len(query) > MaxReviewQueryBytes || strings.IndexByte(query, 0) >= 0 || !utf8.ValidString(query) {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		request.Snapshot, request.Path, request.Query = snapshot, path, query
	case "review_read":
		if len(object) != baseFields+4 {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		snapshot, path, ok := reviewSnapshotAndPath(object)
		startLine, startOK := safeInteger(object["start_line"])
		lineCount, countOK := safeInteger(object["line_count"])
		if !ok || !startOK || !countOK || lineCount > MaxReviewLineCount {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		request.Snapshot, request.Path, request.StartLine, request.LineCount = snapshot, path, startLine, lineCount
	case "review_diff":
		if len(object) != baseFields+1 {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		path, pathOK := object["path"].(string)
		if !pathOK || len(path) > MaxPathBytes || strings.IndexByte(path, 0) >= 0 {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		request.Path = path
	default:
		return reviewRequest{}, errors.New("review request schema is invalid")
	}
	return request, nil
}

// reviewSnapshotAndPath validates only the request's shape (type, length, no
// NUL byte). Whether the path is actually safe to resolve is a per-operation
// business decision, checked again by list/search/read/diff themselves so an
// unsafe path is rejected as an ordinary ok:false result rather than aborting
// the whole attempt.
func reviewSnapshotAndPath(object map[string]any) (string, string, bool) {
	snapshot, snapshotOK := object["snapshot"].(string)
	path, pathOK := object["path"].(string)
	if !snapshotOK || (snapshot != "baseline" && snapshot != "workspace") || !pathOK || len(path) > MaxPathBytes || strings.IndexByte(path, 0) >= 0 {
		return "", "", false
	}
	return snapshot, path, true
}

// encodeFencedReviewRequest re-serializes a request validated without a fence
// (from the untrusted bridge frame) into the canonical, fence-bound wire form.
func encodeFencedReviewRequest(fence int64, request reviewRequest) ([]byte, error) {
	object := map[string]any{"fence": fence, "id": request.ID, "op": request.Op}
	switch request.Op {
	case "review_list":
		object["snapshot"], object["path"] = request.Snapshot, request.Path
	case "review_search":
		object["snapshot"], object["path"], object["query"] = request.Snapshot, request.Path, request.Query
	case "review_read":
		object["snapshot"], object["path"] = request.Snapshot, request.Path
		object["start_line"], object["line_count"] = request.StartLine, request.LineCount
	case "review_diff":
		object["path"] = request.Path
	default:
		return nil, errors.New("review request schema is invalid")
	}
	return json.Marshal(object)
}

type reviewResponse struct {
	Fence     int64  `json:"fence"`
	ID        int64  `json:"id"`
	OK        bool   `json:"ok"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

func encodeReviewResponse(fence, id int64, ok bool, output string, truncated bool) ([]byte, error) {
	response := reviewResponse{Fence: fence, ID: id, OK: ok, Output: output, Truncated: truncated}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > MaxReviewResponseBytes {
		response = reviewResponse{Fence: fence, ID: id, OK: false, Output: "review response exceeds its frame limit."}
		encoded, err = json.Marshal(response)
	}
	if err != nil || len(encoded) > MaxReviewResponseBytes {
		return nil, errors.New("review response is invalid")
	}
	return encoded, nil
}
