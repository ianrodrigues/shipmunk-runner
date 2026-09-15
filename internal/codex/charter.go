package codex

import _ "embed"

// ReviewCharterVersion is the charter_version normalizeExecution requires on
// every findings/no_findings review result.
const ReviewCharterVersion = "1"

// docs/review-charter.md is the canonical text; go:embed cannot reach outside
// this package directory, so this copy is what reaches the model.
// TestReviewCharterDocMatchesEmbeddedText keeps the two byte-identical.
//
//go:embed charter.md
var reviewCharterText string
