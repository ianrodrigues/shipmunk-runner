package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

func testReviewEvidence() *reviewEvidence {
	return &reviewEvidence{
		baselineSHA:  sampleBaselineSHA,
		headSHA:      sampleHeadSHA,
		changedFiles: []string{"a.go", "b.go"},
		baseline:     map[string]fileState{"a.go": {data: []byte("one\ntwo\nthree\n")}},
		head:         map[string]fileState{"a.go": {data: []byte("one\ntwo\nthree\nfour\n")}, "b.go": {data: []byte("new file\n")}},
	}
}

func cleanCoverage() Coverage {
	return Coverage{Files: []CoverageFile{{Path: "a.go", Status: "reviewed"}, {Path: "b.go", Status: "reviewed"}}}
}

func cleanFinding() Finding {
	return Finding{
		Category: "correctness", Severity: "high", Relation: "introduced",
		Scenario: "s", Consequence: "c", Action: "a", Explanation: "e",
		Evidence: []EvidenceRef{{Snapshot: sampleHeadSHA, Path: "b.go", LineStart: 1, LineEnd: 1}},
	}
}

func TestValidateReviewResultAcceptsACleanNoFindingsResult(t *testing.T) {
	result := Result{Outcome: "no_findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: cleanCoverage().Files, ContextGaps: []string{}}}
	if err := validateReviewResult(result, testReviewEvidence()); err != nil {
		t.Fatalf("clean result rejected: %v", err)
	}
}

func TestValidateReviewResultAcceptsACleanFindingsResult(t *testing.T) {
	result := Result{Outcome: "findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: cleanCoverage().Files, ContextGaps: []string{}}, Findings: []Finding{cleanFinding()}}
	if err := validateReviewResult(result, testReviewEvidence()); err != nil {
		t.Fatalf("clean result rejected: %v", err)
	}
}

func TestValidateReviewResultRejectsWrongCharterVersion(t *testing.T) {
	result := Result{Outcome: "no_findings", CharterVersion: "2", VerificationState: "none", Coverage: &Coverage{Files: cleanCoverage().Files}}
	if err := validateReviewResult(result, testReviewEvidence()); err == nil {
		t.Fatal("wrong charter version accepted")
	}
}

func TestValidateReviewResultRejectsWrongVerificationState(t *testing.T) {
	result := Result{Outcome: "no_findings", CharterVersion: ReviewCharterVersion, VerificationState: "partial", Coverage: &Coverage{Files: cleanCoverage().Files}}
	if err := validateReviewResult(result, testReviewEvidence()); err == nil {
		t.Fatal("wrong verification state accepted")
	}
}

func TestValidateReviewResultRejectsWrongSnapshotSHA(t *testing.T) {
	finding := cleanFinding()
	finding.Evidence[0].Snapshot = strings.Repeat("c", 40)
	result := Result{Outcome: "findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: cleanCoverage().Files}, Findings: []Finding{finding}}
	if err := validateReviewResult(result, testReviewEvidence()); err == nil {
		t.Fatal("evidence citing a wrong snapshot ID was accepted")
	}
}

func TestValidateReviewResultRejectsNonexistentPath(t *testing.T) {
	finding := cleanFinding()
	finding.Evidence[0].Path = "missing.go"
	result := Result{Outcome: "findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: cleanCoverage().Files}, Findings: []Finding{finding}}
	if err := validateReviewResult(result, testReviewEvidence()); err == nil {
		t.Fatal("evidence citing a nonexistent path was accepted")
	}
}

func TestValidateReviewResultRejectsOutOfRangeLine(t *testing.T) {
	finding := cleanFinding()
	finding.Evidence[0].Path, finding.Evidence[0].LineStart, finding.Evidence[0].LineEnd = "a.go", 1, 100
	finding.Evidence[0].Snapshot = sampleHeadSHA
	result := Result{Outcome: "findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: cleanCoverage().Files}, Findings: []Finding{finding}}
	if err := validateReviewResult(result, testReviewEvidence()); err == nil {
		t.Fatal("evidence range beyond the file's own length was accepted")
	}
}

func TestValidateReviewResultRejectsCoverageMismatch(t *testing.T) {
	for name, files := range map[string][]CoverageFile{
		"extra file":     {{Path: "a.go", Status: "reviewed"}, {Path: "b.go", Status: "reviewed"}, {Path: "c.go", Status: "reviewed"}},
		"missing file":   {{Path: "a.go", Status: "reviewed"}},
		"duplicate file": {{Path: "a.go", Status: "reviewed"}, {Path: "a.go", Status: "reviewed"}},
	} {
		t.Run(name, func(t *testing.T) {
			result := Result{Outcome: "no_findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: files}}
			if err := validateReviewResult(result, testReviewEvidence()); err == nil {
				t.Fatalf("contradictory coverage scope (%s) was accepted", name)
			}
		})
	}
}

func TestValidateReviewResultRejectsNoFindingsWithIncompleteCoverage(t *testing.T) {
	unreviewed := []CoverageFile{{Path: "a.go", Status: "reviewed"}, {Path: "b.go", Status: "unreviewed", Reason: "ran out of budget"}}
	if err := validateReviewResult(Result{Outcome: "no_findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: unreviewed}}, testReviewEvidence()); err == nil {
		t.Fatal("no_findings with an unreviewed file was accepted")
	}
	withGap := Coverage{Files: cleanCoverage().Files, ContextGaps: []string{"an external policy could not be read"}}
	if err := validateReviewResult(Result{Outcome: "no_findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &withGap}, testReviewEvidence()); err == nil {
		t.Fatal("no_findings with a context gap was accepted")
	}
	// findings tolerates the same incomplete coverage: only no_findings is restricted.
	result := Result{Outcome: "findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: unreviewed}, Findings: []Finding{cleanFinding()}}
	if err := validateReviewResult(result, testReviewEvidence()); err != nil {
		t.Fatalf("findings with an unreviewed file was rejected: %v", err)
	}
}

func TestValidateReviewResultRequiresWorkspaceEvidenceForIntroducedOrModified(t *testing.T) {
	for _, relation := range []string{"introduced", "modified"} {
		t.Run(relation, func(t *testing.T) {
			finding := cleanFinding()
			finding.Relation = relation
			finding.Evidence = []EvidenceRef{{Snapshot: sampleBaselineSHA, Path: "a.go", LineStart: 1, LineEnd: 1}}
			result := Result{Outcome: "findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: cleanCoverage().Files}, Findings: []Finding{finding}}
			if err := validateReviewResult(result, testReviewEvidence()); err == nil {
				t.Fatalf("%s finding without workspace evidence on a changed file was accepted", relation)
			}
		})
	}
	preexisting := cleanFinding()
	preexisting.Relation = "preexisting"
	preexisting.Evidence = []EvidenceRef{{Snapshot: sampleBaselineSHA, Path: "a.go", LineStart: 1, LineEnd: 1}}
	result := Result{Outcome: "findings", CharterVersion: ReviewCharterVersion, VerificationState: "none", Coverage: &Coverage{Files: cleanCoverage().Files}, Findings: []Finding{preexisting}}
	if err := validateReviewResult(result, testReviewEvidence()); err != nil {
		t.Fatalf("preexisting finding evidenced only in baseline was rejected: %v", err)
	}
}

func TestBuildReviewEvidenceComputesRealSHAsAndChangedFiles(t *testing.T) {
	root := t.TempDir()
	baseline, head := filepath.Join(root, "baseline"), filepath.Join(root, "head")
	for _, dir := range []string{baseline, head} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, baseline, "unchanged.txt", "same\n", 0644)
	writeFile(t, head, "unchanged.txt", "same\n", 0644)
	writeFile(t, baseline, "removed.txt", "gone\n", 0644)
	writeFile(t, head, "added.txt", "new\n", 0644)

	baselineHandle, err := os.Open(baseline)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = baselineHandle.Close() })
	headHandle, err := os.Open(head)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = headHandle.Close() })

	claim := protocol.Claim{Manifest: map[string]any{"diff_base_sha": sampleBaselineSHA, "head_sha": sampleHeadSHA}}
	sources := &executionSources{baseline: baselineHandle, head: headHandle}
	evidence, err := buildReviewEvidence(claim, sources)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.baselineSHA != sampleBaselineSHA || evidence.headSHA != sampleHeadSHA {
		t.Fatalf("unexpected snapshot identity: %#v", evidence)
	}
	if got := strings.Join(evidence.changedFiles, ","); got != "added.txt,removed.txt" {
		t.Fatalf("unexpected changed-file set: %v", evidence.changedFiles)
	}
	if lines, ok := evidence.lineCount(sampleHeadSHA, "added.txt"); !ok || lines != 1 {
		t.Fatalf("unexpected line count: lines=%d ok=%t", lines, ok)
	}
	if _, ok := evidence.lineCount(sampleHeadSHA, "removed.txt"); ok {
		t.Fatal("a baseline-only path was found in the head snapshot")
	}
}

func TestBuildReviewEvidenceRejectsInvalidClaimSHAs(t *testing.T) {
	root := t.TempDir()
	baseline, head := filepath.Join(root, "baseline"), filepath.Join(root, "head")
	for _, dir := range []string{baseline, head} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	baselineHandle, err := os.Open(baseline)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = baselineHandle.Close() })
	headHandle, err := os.Open(head)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = headHandle.Close() })
	sources := &executionSources{baseline: baselineHandle, head: headHandle}

	for name, manifest := range map[string]map[string]any{
		"missing":   {},
		"too short": {"diff_base_sha": "abc", "head_sha": sampleHeadSHA},
		"uppercase": {"diff_base_sha": strings.ToUpper(sampleBaselineSHA), "head_sha": sampleHeadSHA},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildReviewEvidence(protocol.Claim{Manifest: manifest}, sources); err == nil {
				t.Fatal("invalid claim SHAs were accepted")
			}
		})
	}
}

func TestExecutionCommandInjectsCharterAndEvidenceOnlyForReview(t *testing.T) {
	claim := protocol.Claim{Manifest: map[string]any{
		"task_context":     "Review carefully.",
		"effective_config": map[string]any{"model": "gpt-5", "instructions": "Stay focused."},
	}}
	evidence := &reviewEvidence{baselineSHA: sampleBaselineSHA, headSHA: sampleHeadSHA, changedFiles: []string{"a.go"}}

	argv, _, err := executionCommand(claim, nil, "", true, evidence)
	if err != nil {
		t.Fatal(err)
	}
	developer := developerInstructionsArg(t, argv)
	for _, want := range []string{ReviewCharterVersion, sampleBaselineSHA, sampleHeadSHA, "a.go", "review_list"} {
		if !strings.Contains(developer, want) {
			t.Fatalf("review prompt missing %q:\n%s", want, developer)
		}
	}

	argv, _, err = executionCommand(claim, nil, "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	developer = developerInstructionsArg(t, argv)
	for _, absent := range []string{sampleBaselineSHA, sampleHeadSHA, "review_list", "charter_version"} {
		if strings.Contains(developer, absent) {
			t.Fatalf("implement prompt unexpectedly contains %q:\n%s", absent, developer)
		}
	}
}

func TestExecutionCommandRejectsMismatchedReviewEvidence(t *testing.T) {
	claim := protocol.Claim{Manifest: map[string]any{
		"task_context":     "Review carefully.",
		"effective_config": map[string]any{"model": "gpt-5", "instructions": "Stay focused."},
	}}
	if _, _, err := executionCommand(claim, nil, "", true, nil); err == nil {
		t.Fatal("review without evidence was accepted")
	}
	evidence := &reviewEvidence{baselineSHA: sampleBaselineSHA, headSHA: sampleHeadSHA}
	if _, _, err := executionCommand(claim, nil, "", false, evidence); err == nil {
		t.Fatal("non-review execution with review evidence was accepted")
	}
}

func developerInstructionsArg(t *testing.T, argv []string) string {
	t.Helper()
	for i, arg := range argv {
		if strings.HasPrefix(arg, "developer_instructions=") && i > 0 && argv[i-1] == "-c" {
			var value string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(arg, "developer_instructions=")), &value); err != nil {
				t.Fatal(err)
			}
			return value
		}
	}
	t.Fatal("developer_instructions argument not found")
	return ""
}
