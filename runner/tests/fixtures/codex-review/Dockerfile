ARG CODEX_FIXTURE_BASE=shipmunk-profile-native-test:local
FROM ${CODEX_FIXTURE_BASE}

RUN rm /usr/local/bin/codex
COPY --chmod=0755 runner/tests/fixtures/codex-review/native.cjs /usr/local/bin/codex
COPY runner/containers/codex-mcp.mjs /usr/local/lib/shipmunk/codex-mcp.mjs
