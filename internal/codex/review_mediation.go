package codex

import (
	"bytes"
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
	// MaxReviewScanBytes bounds the total bytes review_search reads across all
	// files in one request, and any single file larger than this is skipped
	// (marked truncated) rather than scanned, so one oversized file cannot
	// consume the whole request's work by itself.
	MaxReviewScanBytes = 16 * 1024 * 1024
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
	headSHA, baselineSHA       string

	loadOnce sync.Once
	loadErr  error
	head     map[string]fileState
	baseline map[string]fileState

	diffOnce sync.Once
	diffErr  error
	diff     *Patch
	// sections holds one raw diff section per changed path, keyed by the
	// verified path from diff.ChangedFiles rather than parsed from the diff
	// text, so file content can never masquerade as another file's section.
	sections map[string]string
}

// NewReviewMediator binds one attempt's fence and request budget to the two
// pinned snapshot directories already opened by the transport. headSHA and
// baselineSHA are the real commit identities so review_list/review_read
// responses can carry snapshot_sha and the model can cite them verbatim.
func NewReviewMediator(fence int64, maxRequests int, headRoot string, headHandle *os.File, baselineRoot string, baselineHandle *os.File, headSHA, baselineSHA string) (*ReviewMediator, error) {
	if fence < 1 || fence > protocol.MaxSafeInteger || maxRequests < 1 || headRoot == "" || headHandle == nil || baselineRoot == "" || baselineHandle == nil || !fullSHAPattern.MatchString(headSHA) || !fullSHAPattern.MatchString(baselineSHA) {
		return nil, errors.New("review mediator configuration is invalid")
	}
	return &ReviewMediator{fence: fence, remaining: maxRequests, headRoot: headRoot, headHandle: headHandle, baselineRoot: baselineRoot, baselineHandle: baselineHandle, headSHA: headSHA, baselineSHA: baselineSHA}, nil
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
	snapshotSHA := ""
	if ok && (request.Op == "review_list" || request.Op == "review_read") {
		snapshotSHA = m.snapshotSHAByName(request.Snapshot)
	}
	return encodeReviewResponse(m.fence, request.ID, ok, output, truncated, snapshotSHA)
}

func (m *ReviewMediator) execute(request reviewRequest) (output string, truncated, ok bool) {
	switch request.Op {
	case "review_list":
		return m.list(request.Snapshot, request.Path, request.Offset)
	case "review_search":
		return m.search(request.Snapshot, request.Path, request.Query)
	case "review_read":
		return m.read(request.Snapshot, request.Path, request.StartLine, request.LineCount)
	case "review_diff":
		return m.reviewDiff(request.Path)
	}
	// decodeReviewRequest only ever produces one of the ops above.
	panic("review request op was not validated: " + request.Op)
}

func (m *ReviewMediator) snapshotByName(name string) map[string]fileState {
	if name == "baseline" {
		return m.baseline
	}
	return m.head
}

func (m *ReviewMediator) snapshotSHAByName(name string) string {
	if name == "baseline" {
		return m.baselineSHA
	}
	return m.headSHA
}

func (m *ReviewMediator) list(snapshotName, dir string, offset int64) (string, bool, bool) {
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
	total := int64(len(names))
	if total == 0 {
		return "no entries", false, true
	}
	if offset < 0 || offset >= total {
		return "offset is beyond the end of the listing", false, false
	}
	var b strings.Builder
	shown := int64(0)
	truncated := false
	for _, name := range names[offset:] {
		kind := "file"
		if isDir[name] {
			kind = "dir"
		}
		entry := fmt.Sprintf("%s\t%s\n", kind, name)
		// Cap both the entry count and the response bytes: a directory of many
		// long names must truncate with a marker, never silently exceed the
		// response frame and come back as an unqualified failure.
		if shown >= MaxReviewListEntries || b.Len()+len(entry) > MaxReviewOutputBytes {
			truncated = true
			break
		}
		b.WriteString(entry)
		shown++
	}
	if truncated {
		fmt.Fprintf(&b, "... truncated: showing entries %d-%d of %d (pass offset=%d for the next page)\n", offset+1, offset+shown, total, offset+shown)
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
	needle := []byte(query)
	var b strings.Builder
	matches := 0
	truncated := false
	var scanned int64
outer:
	for _, key := range keys {
		content := files[key].data
		if !utf8.Valid(content) {
			continue
		}
		// Bound work per file and in aggregate without ever materializing a
		// per-line slice of the whole file (that scales with line count, not
		// file size, and a newline-dense file can blow it up by two orders of
		// magnitude).
		if int64(len(content)) > MaxReviewScanBytes {
			truncated = true
			continue
		}
		if scanned+int64(len(content)) > MaxReviewScanBytes {
			truncated = true
			break
		}
		scanned += int64(len(content))
		number := int64(1)
		for pos := 0; pos <= len(content); {
			line, next := nextLine(content, pos)
			if bytes.Contains(line, needle) {
				matches++
				if matches > MaxReviewSearchMatches {
					truncated = true
					break outer
				}
				snippet, ellipsis := line, ""
				if len(snippet) > MaxReviewSnippetBytes {
					snippet, ellipsis = snippet[:MaxReviewSnippetBytes], "…"
				}
				entry := fmt.Sprintf("%s:%d: %s%s\n", key, number, snippet, ellipsis)
				if b.Len()+len(entry) > MaxReviewOutputBytes {
					truncated = true
					break outer
				}
				b.WriteString(entry)
			}
			number++
			pos = next
		}
	}
	if matches == 0 {
		return "no matches", truncated, true
	}
	text := strings.TrimRight(b.String(), "\n")
	if truncated {
		text += "\n... truncated: additional matches not shown"
	}
	return text, truncated, true
}

// nextLine returns the next line of data starting at pos (without its
// trailing newline) and the byte offset to resume from. It never allocates:
// the returned slice aliases data. The caller's loop condition must be
// `pos <= len(data)`, and nextLine advances pos past len(data) once the final
// line (which may lack a trailing newline) has been returned.
func nextLine(data []byte, pos int) (line []byte, next int) {
	if newline := bytes.IndexByte(data[pos:], '\n'); newline >= 0 {
		return data[pos : pos+newline], pos + newline + 1
	}
	return data[pos:], len(data) + 1
}

func (m *ReviewMediator) read(snapshotName, path string, startLine, lineCount int64) (string, bool, bool) {
	if path == "" || !safeRepositoryPath(path) {
		return "path is invalid", false, false
	}
	state, exists := m.snapshotByName(snapshotName)[path]
	if !exists {
		return "file not found", false, false
	}
	data := state.data
	if !utf8.Valid(data) {
		return "binary content is not shown", false, false
	}
	// Skip to startLine without materializing any earlier line, so a request
	// near the start of a huge file costs O(requested window), not O(file size).
	number, pos := int64(1), 0
	for number < startLine {
		if pos > len(data) {
			return "start line is beyond the end of the file", false, false
		}
		_, next := nextLine(data, pos)
		pos = next
		number++
	}
	if pos > len(data) {
		return "start line is beyond the end of the file", false, false
	}
	var b strings.Builder
	collected := int64(0)
	budgetHit := false
	partialFirstLine := false
	for collected < lineCount && pos <= len(data) {
		line, next := nextLine(data, pos)
		if int64(b.Len()+len(line)+1) > MaxReviewOutputBytes {
			budgetHit = true
			if collected == 0 {
				// Even the first line alone exceeds the budget (a one-line
				// minified asset or lockfile, say). Emit a bounded,
				// rune-boundary-safe prefix of it instead of leaving the
				// response with nothing but the truncation marker.
				remaining := int(MaxReviewOutputBytes) - b.Len()
				cut := boundedRunePrefixLen(line, remaining)
				b.Write(line[:cut])
				partialFirstLine = true
			}
			break
		}
		b.Write(line)
		b.WriteByte('\n')
		collected++
		pos = next
	}
	moreLines := !budgetHit && pos <= len(data)
	text := strings.TrimRight(b.String(), "\n")
	switch {
	case partialFirstLine:
		text += fmt.Sprintf("\n... truncated: output budget reached partway through line %d", startLine)
	case budgetHit:
		text += fmt.Sprintf("\n... truncated: output budget reached after %d line(s) from line %d", collected, startLine)
	case moreLines:
		text += fmt.Sprintf("\n... more lines follow after line %d", startLine-1+collected)
	}
	return text, budgetHit, true
}

// boundedRunePrefixLen returns the largest n <= limit (and <= len(data)) such
// that data[:n] does not split a multi-byte UTF-8 rune.
func boundedRunePrefixLen(data []byte, limit int) int {
	if limit < 0 {
		limit = 0
	}
	if limit >= len(data) {
		return len(data)
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(data[cut]) {
		cut--
	}
	return cut
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
	chunk, found := m.sections[path]
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
		// detectRenames is false: parseDiffSections keys sections by an exact
		// "diff --git a/<path> b/<path>" match against each independently
		// verified changed path. A combined rename header ("diff --git
		// a/old b/new") would match neither the old nor the new path,
		// silently hiding the hunk from a per-path lookup on either name.
		generated, err := generatePatch(m.baseline, m.head, false)
		if err != nil {
			m.diffErr = err
			return
		}
		patch, err := collectSnapshotPatch(m.baseline, m.head, generated, false)
		if err != nil {
			m.diffErr = err
			return
		}
		m.diff = patch
		if patch != nil {
			m.sections = parseDiffSections(patch.Bytes, patch.ChangedFiles)
			// Defense in depth: safeRepositoryPath already rejects a quote
			// character at snapshot load, which is the known way Git can
			// still C-quote a header despite core.quotePath=false, but if
			// any changed path's section is ever missing for any other
			// reason, fail the whole diff explicitly rather than silently
			// reporting an unaffected file as unchanged.
			if err := verifyDiffSectionCoverage(m.sections, patch.ChangedFiles); err != nil {
				m.diffErr = err
				m.diff = nil
				m.sections = nil
				return
			}
		}
	})
	return m.diffErr
}

// verifyDiffSectionCoverage confirms every changed path parsed a section.
func verifyDiffSectionCoverage(sections map[string]string, changedFiles []FileChange) error {
	for _, change := range changedFiles {
		if _, ok := sections[change.Path]; !ok {
			return fmt.Errorf("review diff section missing for changed path %q", change.Path)
		}
	}
	return nil
}

// parseDiffSections splits a unified diff produced without rename detection
// into one raw section per changed path. It never searches for a path's
// header as a substring of the diff text: file content is fully attacker
// controlled and a content line that happens to read "diff --git a/x b/x"
// would otherwise be indistinguishable from a real header. Instead it looks
// for an exact, whole-line match against one of the header strings implied by
// the independently verified ChangedFiles list, at the true start of a diff
// line, immediately followed by a genuine header continuation line. A forged
// header can never satisfy both: every content line inside a unified diff
// hunk carries a mandatory " "/"+"/"-" prefix, so it can never be byte-equal
// to a bare "diff --git a/<path> b/<path>" line.
func parseDiffSections(diff []byte, changedFiles []FileChange) map[string]string {
	sections := make(map[string]string, len(changedFiles))
	if len(diff) == 0 || len(changedFiles) == 0 {
		return sections
	}
	headerToPath := make(map[string]string, len(changedFiles))
	for _, change := range changedFiles {
		headerToPath["diff --git a/"+change.Path+" b/"+change.Path] = change.Path
	}
	lines := strings.Split(strings.TrimRight(string(diff), "\n"), "\n")
	var starts []int
	var paths []string
	for i, line := range lines {
		path, known := headerToPath[line]
		if !known || !hasDiffHeaderContinuation(lines, i) {
			continue
		}
		starts = append(starts, i)
		paths = append(paths, path)
	}
	for i, start := range starts {
		end := len(lines)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		sections[paths[i]] = strings.Join(lines[start:end], "\n")
	}
	return sections
}

func hasDiffHeaderContinuation(lines []string, headerIndex int) bool {
	if headerIndex+1 >= len(lines) {
		return false
	}
	next := lines[headerIndex+1]
	// "similarity index" and "rename from" never actually appear in review's
	// own diff, which always requests --no-renames (see generatePatch), so
	// every rename surfaces as a plain delete plus add keyed by its real path.
	// They stay here so this parser remains correct on its own terms if it is
	// ever pointed at a diff generated with rename detection enabled.
	for _, prefix := range []string{"index ", "--- ", "new file mode", "deleted file mode", "similarity index", "rename from", "old mode"} {
		if strings.HasPrefix(next, prefix) {
			return true
		}
	}
	return false
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
	Offset    int64
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
		if len(object) != baseFields+3 {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		snapshot, path, ok := reviewSnapshotAndPath(object)
		offset, offsetOK := safeNonNegativeInteger(object["offset"])
		if !ok || !offsetOK {
			return reviewRequest{}, errors.New("review request schema is invalid")
		}
		request.Snapshot, request.Path, request.Offset = snapshot, path, offset
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

func safeNonNegativeInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return parsed, err == nil && parsed >= 0 && parsed <= protocol.MaxSafeInteger
}

// encodeFencedReviewRequest re-serializes a request validated without a fence
// (from the untrusted bridge frame) into the canonical, fence-bound wire form.
func encodeFencedReviewRequest(fence int64, request reviewRequest) ([]byte, error) {
	object := map[string]any{"fence": fence, "id": request.ID, "op": request.Op}
	switch request.Op {
	case "review_list":
		object["snapshot"], object["path"], object["offset"] = request.Snapshot, request.Path, request.Offset
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
	Fence       int64  `json:"fence"`
	ID          int64  `json:"id"`
	OK          bool   `json:"ok"`
	Output      string `json:"output"`
	Truncated   bool   `json:"truncated"`
	SnapshotSHA string `json:"snapshot_sha,omitempty"`
}

// reviewResponseOverhead generously bounds the encoded size of every
// reviewResponse field except Output: braces, field names, punctuation, the
// widest fence/id/bool renderings and one optional 40-character snapshot_sha.
const reviewResponseOverhead = 192

const jsonTruncationMarker = "\n... [truncated: response exceeds the frame limit]"

// encodeReviewResponse enforces the response budget on the JSON-encoded wire
// size, not the raw string length: a string dense in quotes, backslashes or
// control characters can expand well past its raw byte count once escaped,
// and budgeting on raw bytes alone lets that content silently fail closed
// with no output at all instead of a bounded, truncated one.
func encodeReviewResponse(fence, id int64, ok bool, output string, truncated bool, snapshotSHA string) ([]byte, error) {
	budget := MaxReviewResponseBytes - reviewResponseOverhead
	if budget < 0 {
		budget = 0
	}
	if cut, didTruncate := truncateForJSONBudget(output, budget); didTruncate {
		output, truncated = cut, true
	}
	response := reviewResponse{Fence: fence, ID: id, OK: ok, Output: output, Truncated: truncated, SnapshotSHA: snapshotSHA}
	encoded, err := marshalReviewResponse(response)
	if err != nil || len(encoded) > MaxReviewResponseBytes {
		response = reviewResponse{Fence: fence, ID: id, OK: false, Output: "review response exceeds its frame limit."}
		encoded, err = marshalReviewResponse(response)
	}
	if err != nil || len(encoded) > MaxReviewResponseBytes {
		return nil, errors.New("review response is invalid")
	}
	return encoded, nil
}

// marshalReviewResponse encodes without HTML escaping: the response travels
// over a local file bridge to a JSON-RPC client, never into HTML or a
// <script> context, so escaping "<", ">" and "&" would only inflate ordinary
// source text (comparison operators, XML, HTML fixtures) for no benefit.
func marshalReviewResponse(response reviewResponse) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// truncateForJSONBudget returns a prefix of s (plus an explicit marker) whose
// JSON-encoded length, matching marshalReviewResponse's escaping rules, fits
// within budget bytes. It cuts at a rune boundary so the result stays valid
// UTF-8.
func truncateForJSONBudget(s string, budget int) (string, bool) {
	if jsonEncodedLen(s) <= budget {
		return s, false
	}
	available := budget - jsonEncodedLen(jsonTruncationMarker)
	if available < 0 {
		available = 0
	}
	used, cut := 0, 0
	for _, r := range s {
		add := jsonEncodedRuneLen(r)
		if used+add > available {
			break
		}
		used += add
		cut += utf8.RuneLen(r)
	}
	// range decodes an invalid UTF-8 byte as one U+FFFD rune while advancing
	// by exactly one byte, but utf8.RuneLen(U+FFFD) is 3: on such input cut
	// could otherwise overshoot len(s). s is UTF-8-gated by every caller
	// today, so this is unreachable, but slicing must stay safe regardless.
	if cut > len(s) {
		cut = len(s)
	}
	return s[:cut] + jsonTruncationMarker, true
}

func jsonEncodedLen(s string) int {
	total := 0
	for _, r := range s {
		total += jsonEncodedRuneLen(r)
	}
	return total
}

// jsonEncodedRuneLen mirrors encoding/json's escaping with HTML escaping
// disabled: '"' and '\\' become two-byte escapes, '\n'/'\r'/'\t' become their
// short two-byte escapes, every other C0 control character becomes a six-byte
// "\u00XX" escape, and everything else is emitted as its own UTF-8 bytes.
func jsonEncodedRuneLen(r rune) int {
	switch r {
	case '"', '\\', '\n', '\r', '\t':
		return 2
	}
	if r < 0x20 {
		return 6
	}
	if size := utf8.RuneLen(r); size > 0 {
		return size
	}
	return 1
}
