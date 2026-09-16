package codex

import (
	"os"
	"strings"
	"testing"
)

func TestReviewCharterDocMatchesEmbeddedText(t *testing.T) {
	doc, err := os.ReadFile("../../docs/review-charter.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(doc) != reviewCharterText {
		t.Fatal("docs/review-charter.md has drifted from internal/codex/charter.md")
	}
}

func TestReviewCharterDefinesSeverityGroupingAndProportionality(t *testing.T) {
	sections := []struct {
		name    string
		clauses []string
	}{
		{
			name: "Enumeration",
			clauses: []string{
				"walk the planned changed files in the order the list below gives them",
				"record for each one either a candidate defect with the citation that would support it or an explicit none",
				"a query issued once per row of a result set",
				"a string truncated by bytes where the data is UTF-8",
				"an index that duplicates one the schema already declares",
				"a text field validated with no upper bound",
				"a name that breaks the conventions of its neighbours",
				"When you anchor a candidate, use the first statement line",
				"Each candidate still has to survive the self-refutation pass below.",
			},
		},
		{
			name: "Writing a finding",
			clauses: []string{
				"five to eighty characters on one line naming what is wrong",
				"never the file it lives in or the rubric it matches",
				"good: Note update runs with no authorization check",
				"bad:  Security issue in NoteController.php",
				"do not restate what the anchored lines already show",
				"never argue in it that your severity is the right one",
				"one or two sentences each.",
			},
		},
		{
			name: "Severity",
			clauses: []string{
				"Pick the lowest severity level whose definition is fully met",
				"When two definitions both fit, the one that names your case explicitly wins.",
				"critical: exploitable security boundary crossed, data loss or corruption, or an outage on a normal path.",
				"high:     a reachable functional defect on a normal path, a broken contract for existing callers",
				"medium:   a defect on an edge or error path, a resource leak, or a maintenance obligation that",
				"low:      a correctness or clarity problem with no user-visible consequence today.",
				"info:     an observation worth recording that asks for no action.",
				"write `action` as \"No action needed now\"",
			},
		},
		{
			name: "Grouping",
			clauses: []string{
				"One root cause produces one finding.",
				"five-citation limit",
				"cite the most representative ones and say in the explanation how many others share the pattern",
				"A missing regression test for a root cause is part of that finding, not a second one.",
				"Never report two findings that trace back to the same root cause",
				"One fix, one finding is the other half of that rule",
				"except that a missing regression test still belongs to the finding it covers",
			},
		},
		{
			name: "Proportionality",
			clauses: []string{
				"publish only what would change the maintainer's decision",
				"use `review_list`, `review_search`, `review_read` or `review_diff`",
				"Do not report style, formatting, or a CI failure that is already reported elsewhere.",
			},
		},
	}

	previous := -1
	for _, section := range sections {
		heading := "## " + section.name
		if count := strings.Count(reviewCharterText, heading); count != 1 {
			t.Fatalf("charter contains %d %s headings, want 1", count, heading)
		}
		position := strings.Index(reviewCharterText, heading)
		if position <= previous {
			t.Fatalf("charter section %s is out of order", heading)
		}
		previous = position
		sectionText := reviewCharterText[position+len(heading):]
		if end := strings.Index(sectionText, "\n## "); end >= 0 {
			sectionText = sectionText[:end]
		}
		for _, clause := range section.clauses {
			if !strings.Contains(sectionText, clause) {
				t.Errorf("%s section is missing %q", section.name, clause)
			}
		}
	}
}
