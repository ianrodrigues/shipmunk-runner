# Codex 0.154.0 stream corpus

Replayed through `Parse` by `TestParseCorpus` in `internal/codex/parser_test.go`.

`success.jsonl` is a real `codex exec --json` recording from a live review run, redacted by rewriting values only: `thread_id` and every finding's `evidence[].snapshot` were replaced with fixed placeholders, `mcp_tool_call` result bodies and the early preview `agent_message` text were replaced with placeholder strings of the same JSON type, and the final `agent_message` text was replaced with a synthetic result carrying the same shape (two findings, five reviewed files, no questions) as the original. No key present in the raw recording was deleted.

`transient-error-then-success.jsonl` and `turn-failed.jsonl` are constructed, not recorded. They follow the shapes verified against `codex-rs` at tag `rust-v0.154.0`: a `ModelRerouted`/`ConfigWarning`-class notification surfaces as a completed `error` item while the turn keeps running (`event_processor_with_jsonl_output.rs`, `CodexStatus::Running`), and a `turn.failed` event carries only the bare `error.message` the backend returned, because `protocol/src/error.rs`'s `extract_error_message` unwraps the provider's JSON envelope before the CLI ever writes it to the stream.
