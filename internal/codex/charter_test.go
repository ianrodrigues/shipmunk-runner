package codex

import (
	"os"
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
