# Codex supervisor integration fixtures

These scenarios execute the real supervisor, protected profile store, HTTP client, workspace preparation, Docker transport, MCP server, parser, patch collection and watchdogs. The HTTP server responses and native Codex executable are synthetic. No personal profile is discovered and no native account or model is contacted.

Build from the explicitly preloaded native fixture image, which must contain Node, Git, tar and the standard shell:

```sh
docker build --pull=false --tag shipmunk-codex-supervisor-test:local \
  --file runner/tests/fixtures/codex-supervisor/Dockerfile .
OPENAI_API_KEY=SYNTHETIC_CONFLICT php runner/tests/CodexSupervisorTest.php
```

Use PHP 8.5 with `pcntl` and `posix` as a non-root runner. Docker must use a Linux engine. The Dockerfile accepts `CODEX_FIXTURE_BASE` to select a different preloaded base. `SHIPMUNK_CODEX_SUPERVISOR_IMAGE` selects the built synthetic image.

The success scenario downloads a real gzip Git archive with a commit-prefixed root and PAX metadata, supplies separately approved instructions, runs the real MCP repository command, and checks the normalized JSON patch envelope delivered through the real HTTP client. The repository command commits its edits to prove patch collection compares against the trusted original snapshot. A background repository child also exercises process quiescence and cleanup.

The cancellation scenario waits until that repository child exists, returns a fenced control-plane stop, and checks every container, workspace volume, host workspace and execution reservation is gone before stopped acknowledgement. A separate parent-death scenario kills the owner of the three real watchdog children and requires all three containers to disappear well before lease expiry.

These are offline integration assertions, not evidence of native Codex account compatibility. They require the source archive handling, durable profile execution reservation, trusted transport diff and patch-envelope integration changes.
