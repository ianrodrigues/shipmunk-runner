package codex

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

var fullSHAPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// Bounds the changed-file list injected into the prompt; a larger list is truncated with a marker pointing at review_diff.
const maxReviewChangedFileListBytes = 16 * 1024

// Uses a descriptor path on Linux and macOS only; elsewhere it falls back to handle.Name(), which snapshotPinned then checks with os.SameFile.
func stableFDPath(handle *os.File) string {
	switch runtime.GOOS {
	case "linux":
		return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), handle.Fd())
	case "darwin":
		return fmt.Sprintf("/dev/fd/%d", handle.Fd())
	default:
		return handle.Name()
	}
}

// Line count and binary flag only, to avoid a second full snapshot copy alongside ReviewMediator's own.
type reviewLineIndex struct {
	lines  int64
	binary bool
}

// Computed the same way review_diff computes its own; see loadDiff.
type reviewEvidence struct {
	baselineSHA, headSHA     string
	changedFiles             []string
	baselineIndex, headIndex map[string]reviewLineIndex
}

func reviewSHAs(claim protocol.Claim) (base, head string, ok bool) {
	base, baseOK := claim.Manifest["diff_base_sha"].(string)
	head, headOK := claim.Manifest["head_sha"].(string)
	return base, head, baseOK && headOK && fullSHAPattern.MatchString(base) && fullSHAPattern.MatchString(head)
}

// Requires sources to be the same executionSources already validated and handed to the transport.
func buildReviewEvidence(claim protocol.Claim, sources *executionSources) (*reviewEvidence, error) {
	base, head, ok := reviewSHAs(claim)
	if !ok {
		return nil, errors.New("Codex review claim SHAs are invalid")
	}
	// Uses stableFDPath, not sources.baselinePath()/sources.head.Name(), so a legitimate transport replacement at that path isn't seen by snapshotPinned as "changed while opening".
	baselineStates, err := snapshotPinned(stableFDPath(sources.baseline), sources.baseline)
	if err != nil {
		return nil, errors.New("Codex review baseline snapshot is unavailable")
	}
	headStates, err := snapshotPinned(stableFDPath(sources.head), sources.head)
	if err != nil {
		return nil, errors.New("Codex review workspace snapshot is unavailable")
	}
	generated, err := generatePatch(baselineStates, headStates, false)
	if err != nil {
		return nil, errors.New("Codex review changed-file list is unavailable")
	}
	patch, err := collectSnapshotPatch(baselineStates, headStates, generated, false)
	if err != nil {
		return nil, errors.New("Codex review changed-file list is unavailable")
	}
	changed := make([]string, 0)
	if patch != nil {
		for _, change := range patch.ChangedFiles {
			changed = append(changed, change.Path)
		}
	}
	sort.Strings(changed)
	return &reviewEvidence{
		baselineSHA: base, headSHA: head, changedFiles: changed,
		baselineIndex: buildLineIndex(baselineStates), headIndex: buildLineIndex(headStates),
	}, nil
}

func buildLineIndex(states map[string]fileState) map[string]reviewLineIndex {
	index := make(map[string]reviewLineIndex, len(states))
	for path, state := range states {
		index[path] = reviewLineIndex{lines: countLines(state.data), binary: !utf8.Valid(state.data)}
	}
	return index
}

// Rejects a syntactically valid but wrong snapshot ID.
func (r *reviewEvidence) resolveSnapshot(sha string) (map[string]reviewLineIndex, bool) {
	switch sha {
	case r.baselineSHA:
		return r.baselineIndex, true
	case r.headSHA:
		return r.headIndex, true
	default:
		return nil, false
	}
}

func (r *reviewEvidence) lookup(sha, path string) (reviewLineIndex, bool) {
	index, ok := r.resolveSnapshot(sha)
	if !ok {
		return reviewLineIndex{}, false
	}
	entry, exists := index[path]
	return entry, exists
}

func (r *reviewEvidence) lineCount(sha, path string) (int64, bool) {
	entry, ok := r.lookup(sha, path)
	return entry.lines, ok
}

// Matches nextLine's own convention (review_mediation.go): a trailing newline lets review_read reach one further, empty line, and evidence validation must accept exactly that.
func countLines(data []byte) int64 {
	return int64(bytes.Count(data, []byte("\n"))) + 1
}

func (r *reviewEvidence) changedFileSet() map[string]bool {
	set := make(map[string]bool, len(r.changedFiles))
	for _, path := range r.changedFiles {
		set[path] = true
	}
	return set
}

// Checks what only the runner can, against snapshot content it holds; the server re-checks scope from its own frozen inventory.
func validateReviewResult(result Result, review *reviewEvidence) error {
	if review == nil {
		return errors.New("Codex review evidence is unavailable")
	}
	if result.CharterVersion != ReviewCharterVersion {
		return errors.New("Codex review charter version is invalid")
	}
	if result.VerificationState != "none" {
		return errors.New("Codex review verification state is invalid")
	}
	if result.Coverage == nil {
		return errors.New("Codex review coverage is missing")
	}
	if err := validateCoverageFileSet(result.Coverage.Files, review.changedFileSet()); err != nil {
		return err
	}
	if result.Outcome == "no_findings" {
		if len(result.Coverage.ContextGaps) > 0 {
			return errors.New("Codex review cannot report no_findings with a context gap")
		}
		for _, file := range result.Coverage.Files {
			if file.Status == "unreviewed" {
				return errors.New("Codex review cannot report no_findings with an unreviewed file")
			}
		}
	}
	changed := review.changedFileSet()
	for _, finding := range result.Findings {
		if err := validateEvidenceRefs(finding.Evidence, review); err != nil {
			return err
		}
		if finding.Relation == "introduced" || finding.Relation == "modified" {
			if !citesChangedFile(finding.Evidence, changed) {
				return errors.New("Codex review finding relation requires evidence on a changed file")
			}
		}
	}
	for _, question := range result.Questions {
		if err := validateEvidenceRefs(question.Evidence, review); err != nil {
			return err
		}
	}
	return nil
}

func validateCoverageFileSet(files []CoverageFile, planned map[string]bool) error {
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if !planned[file.Path] {
			return errors.New("Codex review coverage lists a file outside the planned change set")
		}
		if seen[file.Path] {
			return errors.New("Codex review coverage lists a file more than once")
		}
		seen[file.Path] = true
	}
	if len(seen) != len(planned) {
		return errors.New("Codex review coverage omits a planned changed file")
	}
	return nil
}

func validateEvidenceRefs(refs []EvidenceRef, review *reviewEvidence) error {
	for _, ref := range refs {
		entry, exists := review.lookup(ref.Snapshot, ref.Path)
		if !exists {
			return errors.New("Codex review evidence cites a path absent from its cited snapshot")
		}
		if entry.binary {
			return errors.New("Codex review evidence cites a binary file")
		}
		if ref.LineEnd > entry.lines {
			return errors.New("Codex review evidence line range exceeds the cited file")
		}
	}
	return nil
}

// A changed path may be cited on whichever snapshot holds it; requiring the workspace snapshot would reject an honest deletion finding.
func citesChangedFile(refs []EvidenceRef, changed map[string]bool) bool {
	for _, ref := range refs {
		if changed[ref.Path] {
			return true
		}
	}
	return false
}

// findingToWire, coverageToWire and questionsToWire mirror Result fields into contracts/v1/result.schema.json's finding, coverage and questions shapes.
func findingToWire(f Finding) map[string]any {
	wire := map[string]any{
		"category": f.Category, "title": f.Title, "severity": f.Severity, "relation": f.Relation,
		"scenario": f.Scenario, "consequence": f.Consequence, "action": f.Action,
		"explanation": f.Explanation, "evidence": evidenceToWire(f.Evidence),
	}
	if f.Anchor != nil {
		wire["anchor"] = map[string]any{"path": f.Anchor.Path, "line": f.Anchor.Line, "side": f.Anchor.Side}
	}
	return wire
}

func evidenceToWire(refs []EvidenceRef) []any {
	wire := make([]any, len(refs))
	for i, ref := range refs {
		wire[i] = map[string]any{"snapshot": ref.Snapshot, "path": ref.Path, "line_start": ref.LineStart, "line_end": ref.LineEnd}
	}
	return wire
}

func coverageToWire(coverage *Coverage) map[string]any {
	if coverage == nil {
		return map[string]any{"files": []any{}, "context_gaps": []any{}}
	}
	files := make([]any, len(coverage.Files))
	for i, file := range coverage.Files {
		entry := map[string]any{"path": file.Path, "status": file.Status}
		if file.Status == "unreviewed" {
			entry["reason"] = file.Reason
		}
		files[i] = entry
	}
	gaps := make([]any, len(coverage.ContextGaps))
	for i, gap := range coverage.ContextGaps {
		gaps[i] = gap
	}
	return map[string]any{"files": files, "context_gaps": gaps}
}

func questionsToWire(questions []Question) []any {
	wire := make([]any, len(questions))
	for i, question := range questions {
		entry := map[string]any{"topic": question.Topic, "question": question.Question, "why_material": question.WhyMaterial}
		if len(question.Evidence) > 0 {
			entry["evidence"] = evidenceToWire(question.Evidence)
		}
		wire[i] = entry
	}
	return wire
}

// Injects this attempt's real snapshot identity and changed-file list so the model cites real SHAs and covers the exact checked set.
func reviewEvidencePreamble(evidence *reviewEvidence) string {
	var b strings.Builder
	b.WriteString("This attempt's charter_version is \"")
	b.WriteString(ReviewCharterVersion)
	b.WriteString("\". Cite the baseline snapshot as \"")
	b.WriteString(evidence.baselineSHA)
	b.WriteString("\" and the workspace (head) snapshot as \"")
	b.WriteString(evidence.headSHA)
	b.WriteString("\" in every evidence citation's snapshot field; these are the only two valid values. ")
	if len(evidence.changedFiles) == 0 {
		b.WriteString("The planned changed-file list for coverage is empty: this attempt has no changed files, so coverage.files must be an empty array.")
		return b.String()
	}
	b.WriteString("The planned changed-file list for coverage.files is exactly these paths, no more and no fewer:\n")
	listStart := b.Len()
	shown := 0
	for _, path := range evidence.changedFiles {
		entry := "- " + path + "\n"
		if b.Len()-listStart+len(entry) > maxReviewChangedFileListBytes {
			break
		}
		b.WriteString(entry)
		shown++
	}
	if shown < len(evidence.changedFiles) {
		fmt.Fprintf(&b, "... %d more planned file(s) omitted here; call review_diff with path \"\" for the complete list.\n", len(evidence.changedFiles)-shown)
	}
	return b.String()
}
