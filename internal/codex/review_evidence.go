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

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

var fullSHAPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// stableFDPath mirrors the Docker transport's Source/Baseline substitution:
// derived from the fd, not the original path, so replacing what now lives at
// that path cannot affect what gets read.
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

// reviewEvidence is one review attempt's snapshot identity and planned
// changed-file set, computed the same way review_diff computes its own (see
// loadDiff), since the manifest carries no structured changed-file list. It
// backs both the prompt injection in executionCommand and
// normalizeExecution's evidence/coverage checks. Loading both snapshots here
// duplicates ReviewMediator's own lazy load; see the "diff generated twice"
// tradeoff in docs/compatibility/codex.md.
type reviewEvidence struct {
	baselineSHA, headSHA string
	changedFiles         []string
	baseline, head       map[string]fileState
}

func reviewSHAs(claim protocol.Claim) (base, head string, ok bool) {
	base, baseOK := claim.Manifest["diff_base_sha"].(string)
	head, headOK := claim.Manifest["head_sha"].(string)
	return base, head, baseOK && headOK && fullSHAPattern.MatchString(base) && fullSHAPattern.MatchString(head)
}

// buildReviewEvidence loads both review snapshots and computes the planned
// changed-file set for one review attempt. sources must be the same
// executionSources already validated and handed to the transport.
func buildReviewEvidence(claim protocol.Claim, sources *executionSources) (*reviewEvidence, error) {
	base, head, ok := reviewSHAs(claim)
	if !ok {
		return nil, errors.New("Codex review claim SHAs are invalid")
	}
	// stableFDPath, not sources.baselinePath()/sources.head.Name(): the
	// transport may legitimately replace what now lives at that original
	// path, and snapshotPinned would otherwise treat that as "changed while
	// opening".
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
	return &reviewEvidence{baselineSHA: base, headSHA: head, changedFiles: changed, baseline: baselineStates, head: headStates}, nil
}

// resolveSnapshot maps an evidence citation's real SHA back to which loaded
// snapshot it names, rejecting a syntactically valid but wrong snapshot ID.
func (r *reviewEvidence) resolveSnapshot(sha string) (map[string]fileState, bool) {
	switch sha {
	case r.baselineSHA:
		return r.baseline, true
	case r.headSHA:
		return r.head, true
	default:
		return nil, false
	}
}

// lineCount reports whether path exists in the snapshot named by sha and, if
// so, how many lines it has, matching the counting convention review_read
// uses (a final line without a trailing newline still counts).
func (r *reviewEvidence) lineCount(sha, path string) (int64, bool) {
	states, ok := r.resolveSnapshot(sha)
	if !ok {
		return 0, false
	}
	state, exists := states[path]
	if !exists {
		return 0, false
	}
	return countLines(state.data), true
}

func countLines(data []byte) int64 {
	if len(data) == 0 {
		return 0
	}
	lines := int64(bytes.Count(data, []byte("\n")))
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

func (r *reviewEvidence) changedFileSet() map[string]bool {
	set := make(map[string]bool, len(r.changedFiles))
	for _, path := range r.changedFiles {
		set[path] = true
	}
	return set
}

// validateReviewResult checks what only the runner can, against snapshot
// content it actually holds; the server re-checks scope from its own frozen
// changed-file inventory (contracts/v1/result.schema.json's ProtocolValidator).
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
			if !citesChangedWorkspaceFile(finding.Evidence, review, changed) {
				return errors.New("Codex review finding relation requires workspace evidence on a changed file")
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
		lineCount, exists := review.lineCount(ref.Snapshot, ref.Path)
		if !exists {
			return errors.New("Codex review evidence cites a path absent from its cited snapshot")
		}
		if ref.LineEnd > lineCount {
			return errors.New("Codex review evidence line range exceeds the cited file")
		}
	}
	return nil
}

func citesChangedWorkspaceFile(refs []EvidenceRef, review *reviewEvidence, changed map[string]bool) bool {
	for _, ref := range refs {
		if ref.Snapshot == review.headSHA && changed[ref.Path] {
			return true
		}
	}
	return false
}

// findingToWire, coverageToWire and questionsToWire mirror the parsed Result
// fields into contracts/v1/result.schema.json's finding/coverage/questions shape.
func findingToWire(f Finding) map[string]any {
	wire := map[string]any{
		"category": f.Category, "severity": f.Severity, "relation": f.Relation,
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

// reviewEvidencePreamble injects this attempt's real snapshot identity and
// its planned changed-file list after the trusted charter text, so the model
// cites the actual baseline/head SHAs rather than a label, and accounts for
// coverage against the exact set normalizeExecution will check it against.
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
	for _, path := range evidence.changedFiles {
		b.WriteString("- ")
		b.WriteString(path)
		b.WriteByte('\n')
	}
	return b.String()
}
