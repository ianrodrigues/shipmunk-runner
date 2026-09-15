# Codex review execution fixture

This scenario drives the real Executor, Docker transport, review MCP server and mediator, parser and normalizer end to end for a review attempt, with a synthetic native Codex CLI standing in for the pinned executable. No account or provider is contacted.

The Go test that uses this fixture (`TestDockerReviewExecutionProducesACharterCompliantResult` in `internal/codex`) builds its own uniquely tagged image from `CODEX_FIXTURE_BASE` (the same preloaded native fixture image used by the other review Docker tests) and removes it again at the end of the run; there is no separate `docker build` step to run by hand.

The synthetic `native.cjs` answers `--version`, `login status` and the `--ephemeral` preflight the same way the real CLI's pinned behavior is asserted elsewhere, then for the real review turn connects to the bundled review MCP server exactly as the production `codex exec` process does, calls `review_diff` and `review_list` over that real bridge, and reports a `no_findings` result whose `coverage.files` matches the changed-file set it discovered through the tool, with the charter fields the runner's normalizer requires. It emits those fields in the strict output schema's own shape, so the parser's null-to-absent normalization is exercised on the real path rather than only in unit tests.
