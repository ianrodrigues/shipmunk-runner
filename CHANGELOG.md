# Changelog

All notable changes to this project are documented in this file, written for the person installing or running the runner.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## Unreleased

### Added

- `make lint` and the pre-push hook now fail a pull request that touches `cmd/` or `internal/` without an `Unreleased` line here or a `changelog: none` commit trailer. (#TBD)

## v0.1.0-alpha.16 - 2026-09-16

### Added

- `shipmunk-setup` and `shipmunk-profile` are merged into one `shipmunk-runner` binary exposing `setup`, `connect`, `probe`, `run` and `disconnect` subcommands. ([#66](https://github.com/ianrodrigues/shipmunk-runner/pull/66))
- Added a `disconnect` subcommand, and `connect`/`probe`/`disconnect` now print readable terminal status ("Runner is ready.") instead of only machine-readable output; pass `--json` for scripts. ([#66](https://github.com/ianrodrigues/shipmunk-runner/pull/66))

### Changed

- Guided setup now ends by actually running `connect` (unless `--skip-connect` is passed) instead of only activating configuration. ([#66](https://github.com/ianrodrigues/shipmunk-runner/pull/66))

### Fixed

- Fixed the runner printing the same failure line every two seconds during a control-plane or network outage; repeated failures now back off with a capped, doubling pause. ([#64](https://github.com/ianrodrigues/shipmunk-runner/pull/64))
- Fixed heartbeat and completion HTTP timeouts being misreported as the attempt's deadline expiring. ([#65](https://github.com/ianrodrigues/shipmunk-runner/pull/65))

## v0.1.0-alpha.15 - 2026-09-16

### Added

- Added a structured, rotating event log at `<state-dir>/logs/runner.log` recording claim, workspace, sandbox and cleanup lifecycle events, with sensitive values redacted. ([#58](https://github.com/ianrodrigues/shipmunk-runner/pull/58))
- The supervised runner now stops polling after three consecutive runner-classified failures instead of retrying indefinitely, and names the reason and the log path. ([#59](https://github.com/ianrodrigues/shipmunk-runner/pull/59))

### Fixed

- Setup now explains why it refused a release manifest or archive (symlink, hard link, unreadable, oversized, and so on) instead of a generic "unsafe or invalid" message. ([#57](https://github.com/ianrodrigues/shipmunk-runner/pull/57))
- Fixed a slow claim request being misreported as the attempt's deadline expiring; claims now get their own 30-second HTTP budget. ([#62](https://github.com/ianrodrigues/shipmunk-runner/pull/62))

## v0.1.0-alpha.14 - 2026-09-15

### Added

- Review findings now carry a severity rating and group a repeated root cause into a single finding with multiple citations instead of one finding per location. ([#51](https://github.com/ianrodrigues/shipmunk-runner/pull/51))
- Added `run --discard-attempt` to manually clear an interrupted attempt after a best-effort sandbox cleanup. ([#52](https://github.com/ianrodrigues/shipmunk-runner/pull/52))

### Changed

- The runner is now licensed under AGPL-3.0-only (previously MIT). ([#50](https://github.com/ianrodrigues/shipmunk-runner/pull/50))
- Review now treats the pull request's declared title and description as an unverified hypothesis to compare against the observed diff, not as ground truth. ([#54](https://github.com/ianrodrigues/shipmunk-runner/pull/54))

### Fixed

- Fixed live reviews on Codex 0.154 failing on additional stream shapes, including unknown item types, duplicate keys and new usage fields. ([#53](https://github.com/ianrodrigues/shipmunk-runner/pull/53))
- Fixed a runner getting stuck retrying a stopped acknowledgement the server will never accept after its run was superseded; it now settles automatically after three refusals. ([#52](https://github.com/ianrodrigues/shipmunk-runner/pull/52))

## v0.1.0-alpha.13 - 2026-09-15

### Changed

- The review tool request budget now scales with the number of changed files (24 plus 8 per file, capped at 160) instead of a fixed per-turn limit. ([#49](https://github.com/ianrodrigues/shipmunk-runner/pull/49))

### Fixed

- Fixed live reviews on Codex 0.154 being discarded outright because of an unrecognized `cache_write_input_tokens` usage field. ([#49](https://github.com/ianrodrigues/shipmunk-runner/pull/49))
- Fixed native Codex profile cleanup refusing to proceed on files Codex 0.154 itself leaves under `.codex/tmp`. ([#49](https://github.com/ianrodrigues/shipmunk-runner/pull/49))

## v0.1.0-alpha.12 - 2026-09-15

### Changed

- Removed the legacy PHP runner implementation and its guided-setup path; the Go release binaries are now the only supported runner. ([#36](https://github.com/ianrodrigues/shipmunk-runner/pull/36))

### Fixed

- Fixed a race in sandbox watchdog teardown that could report a spurious cleanup failure. ([#37](https://github.com/ianrodrigues/shipmunk-runner/pull/37))
- Fixed a race where canceling an attempt could skip cleanup of its sandbox siblings. ([#40](https://github.com/ianrodrigues/shipmunk-runner/pull/40))
- Fixed live Codex reviews failing immediately with an `invalid_json_schema` error from the OpenAI API. ([#47](https://github.com/ianrodrigues/shipmunk-runner/pull/47))
- Fixed the installed runner refusing to start after an interrupted attempt; it now recovers and reports the interruption automatically on the next run. ([#48](https://github.com/ianrodrigues/shipmunk-runner/pull/48))

## v0.1.0-alpha.11 - 2026-09-15

### Added

- Review results now report structured, evidence-cited findings against a review charter, with per-file coverage and a distinction between findings and open questions. ([#35](https://github.com/ianrodrigues/shipmunk-runner/pull/35))

### Changed

- Setup now refuses to install or renew into an older release than the one already installed, and the docs record per-platform validation status (linux-amd64 CI-smoked, linux-arm64 smoke-tested locally, darwin-arm64 demonstrated, darwin-amd64 compile-only). ([#33](https://github.com/ianrodrigues/shipmunk-runner/pull/33))

## v0.1.0-alpha.10 - 2026-09-15

### Changed

- Review runs now use four typed, budget-limited snapshot tools (list/search/read/diff) instead of a general repository shell, and the review workspace is mounted read-only. ([#28](https://github.com/ianrodrigues/shipmunk-runner/pull/28))
- The manifest now carries separate `target_sha`, `diff_base_sha` and `head_sha` review identities instead of a single `base_sha`; this requires the matching server release. ([#30](https://github.com/ianrodrigues/shipmunk-runner/pull/30))

## v0.1.0-alpha.9 - 2026-09-14

### Added

- Review runs can now read the pull request's base snapshot at `/baseline` alongside the head snapshot at `/workspace`, so a review can inspect base-side changes and deleted files. ([#25](https://github.com/ianrodrigues/shipmunk-runner/pull/25))

### Changed

- A Docker client and a Linux engine 26.0 or newer are now required for immutable review snapshot mounts. ([#25](https://github.com/ianrodrigues/shipmunk-runner/pull/25))

### Fixed

- Native Codex token usage counts are normalized consistently; absent observations now report as null instead of failing. ([#24](https://github.com/ianrodrigues/shipmunk-runner/pull/24))
