# Shipmunk runner

The runner is a standalone PHP supervisor. It does not load Laravel, Composer's application autoloader, `.env`, `APP_KEY`, database credentials, GitHub credentials, or application configuration. Configure it only with explicit command options and a mode-0600 runner-token file:

```sh
runner/bin/shipmunk-runner \
  --base-url=https://shipmunk.example \
  --token-file=/etc/shipmunk/runner.token \
  --state-dir=/var/lib/shipmunk-runner \
  --image=shipmunk-runner:local
```

The supervisor is privileged infrastructure because it controls the container runtime. Run it on a dedicated execution host. The runtime socket stays on that host and is never mounted into an agent container.

Each attempt uses a new non-root, read-only-root container with all Linux capabilities dropped, `no-new-privileges`, bounded CPU, memory, PIDs, file descriptors, logs, time, and tmpfs-backed writable paths. Repository networking is `none`. The default fixture driver's inert entrypoint waits for an atomic ready marker while the supervisor copies the workspace and sanitized agent input, with lease checkpoints between each bounded setup operation. Repository code cannot start against partial or stale inputs. Codex execution requires explicit selection as described below; restricted repository egress and Claude execution are not implemented.

Native credential homes must be separately scoped per active profile and mediated outside repository subprocesses. Mounting a credential home into the same container as repository code is not an isolation boundary and is not supported by this implementation. Agent input has the entire supervisor section and credential-bearing keys removed. Source archives and trusted instruction bundles are fetched only by ID through the authenticated control-plane client and hash-checked. Gzip/tar source extraction is bounded and rejects links, special files and traversal; instruction bundles remain separate validated JSON files.

The supervisor persists claim state before artifact preparation and records a created sandbox before starting it. It heartbeats immediately after claim and on a timed 10-second cadence through preparation and execution against a 45-second lease. A forked host-side watchdog, requiring PHP's `pcntl` extension, independently tracks renewals with a monotonic timer. Parent death, heartbeat loss, or deadline expiry stops and removes the whole container process tree with TERM and KILL escalation while leaving durable state for restart acknowledgement. Durable state is cleared only after the container and workspace are confirmed absent and the server accepts a fenced `stopped` acknowledgement. Cleanup or acknowledgement failure retains that state, so restart reconciliation must retry it before asking for replacement work.

Run `make runner-check`. The offline check requires a reachable Linux Docker engine and a deliberately preloaded `alpine:3.20` image; it fails with an actionable error instead of pulling or silently skipping when isolation cannot be exercised.

## Guided setup

For the shortest setup path, register a runner in **Connections → Runners & Codex**, then run its displayed command with the downloaded setup file:

```sh
bash runner/bin/shipmunk-setup ~/Downloads/shipmunk-setup-RUNNER.json
```

Run from a checkout as the designated non-root account with PHP 8.5+, `pcntl`, `posix`, and a Linux Docker engine. `PHP_BIN=/path/to/php` selects a non-default PHP executable. The script validates the download and server address, checks Docker and `/up` without credentials, creates private files under `~/.shipmunk/runners`, builds the pinned runtime image, and offers native terminal login. It prints short `connect`, `connect probe`, `run --once` and `run` commands; it never starts queued work automatically. The native image uses a Dockerfile-specific context allowlist so application `.env` files and setup tokens are excluded.

The download uses the dashboard origin. For a separate runner host, append `--server-url=https://your-runner-reachable-server`; loopback HTTP is only for same-host development. Confirm the displayed address before proceeding. The setup file and its separately scoped tokens expire in one hour. Delete the original download after setup; renew using the same runner card and effective server URL. Renewal preserves profile credentials and pending operation journals. The connection wrapper creates a fresh operation ID, while the existing lifecycle reconciles any original pending operation first. Detailed manual commands follow below.

## Native subscription profiles

Build the credential-only image explicitly (this downloads the pinned official packages):

```sh
docker build --tag shipmunk-profile-native:local --file runner/containers/Dockerfile .
```

The image pins Codex **0.154.0** and Claude Code **2.1.269**. The runner resolves its immutable local image ID and verifies the executable version for every operation. The profile's server-side runtime version must match. Changing pins requires repeating the offline checks and designated-account runtime scenarios.

The image includes the system CA bundle required for native HTTPS certificate verification. `make native-image-check` builds the actual pinned image, then checks its default certificate trust and CLI versions with networking disabled and no host mounts. It also exercises the Docker transport, synthetic supervisor lifecycle and the real pinned Codex tool boundary. Building downloads packages; the checks themselves do not access an account. CI runs both `make runner-check` and `make native-image-check`, each with a private temporary directory. The first target includes the bounded parser, driver, session-storage and execution-exclusion regressions; the second builds the exact image required by the container scenarios.

Native clients can explicitly create metadata with broader permissions than the runner's umask. After the native container has stopped, the runner validates the entire owned home for unsafe entries, then reduces regular files to `0600` and subdirectories to `0700` while holding the profile lock. The enclosing home stays `0700` throughout. Links, special files, multiple hard links, foreign ownership and special permission bits are rejected before any permissions change. The native-image check also reproduces Codex's generated metadata in a disposable home with networking disabled; foreign-owner coverage runs on Linux, where bind mounts preserve ownership.

Create the profile through the application API with the assigned runner and exact runtime version, then use its lowercase ULID and a fresh lowercase ULID for the operation. From a terminal on the assigned runner:

```sh
runner/bin/shipmunk-profile \
  --base-url=https://shipmunk.example \
  --token-file=/etc/shipmunk/profile-runner.token \
  --profiles-dir=/var/lib/shipmunk-profiles \
  --image=shipmunk-profile-native:local \
  --profile=<profile-ulid> \
  --operation=login \
  --operation-id=<fresh-operation-ulid>
```

The token needs `runner:profiles` and belongs to the assigned runner. The root and token must belong to the dedicated, non-root runner account; directory permissions are 0700 and token permissions 0600. Do not use a desktop credential directory. Login requires an interactive terminal: native URLs/codes go directly there, never through application events. Codex uses `login --device-auth`; Claude uses `auth login --claudeai`. No personal credentials are discovered or copied.

`--operation=probe` rechecks the existing profile; `--operation=disconnect` removes local access after the application requests disconnection. Each operation has a 45-second renewable control-plane lease, a 15-minute overall bound and independent process-tree watchdog. Restart by repeating the command with the original operation ID to reconcile interrupted cleanup/acknowledgement; after reconciliation, use a fresh ID for new work. Definitive rejected begins do not leave a local journal. Ambiguous responses retain recovery state and block replacement operations.

The application reserves the profile against both execution and lifecycle work. A host `flock` also excludes native refresh, login and disconnect. A pending home is unavailable until native mode/version checks, a bounded authenticated request, a post-request mode check and the server's completion acknowledgement all succeed. A lost successful acknowledgement is retried with the original durable outcome; it never repeats login or destroys confirmed credentials merely because a response was lost. Old completed operation replays cannot overwrite a newer home.

The authenticated preflight uses a fixed `SHIPMUNK_AUTH_OK` prompt in an empty directory and validates structured terminal success. Claude has tools and customizations disabled; Codex has shell execution and web search disabled, a read-only sandbox, no user configuration, no rules and an ephemeral session. Native refresh stays delegated to the official executable. Ambiguous, expired, failed or wrong-mode responses fail closed without API-key fallback. The native output is discarded after parsing. Health never invents quota, cost or reset information.

Each native command receives a clean allowlist environment inside a dedicated Linux container with only its own home mounted. Repository code, other profile homes, the Docker socket and the runner token are absent. Codex helper aliases live in disposable tmpfs rather than the persistent store. The store rejects links, hardlinks, wrong ownership and broad permissions. The host supervisor is trusted infrastructure; same-UID host processes are not mutually isolated. Native execution integrations hold `ProfileExecutionGate` for their entire native process lifetime, do a fresh fenced authorization check, and keep repository subprocesses in a separate sandbox.

## Codex execution

After connecting a designated Codex profile, select the native driver explicitly on its assigned dedicated Linux runner:

```sh
runner/bin/shipmunk-runner \
  --base-url=https://shipmunk.example \
  --token-file=/etc/shipmunk/runner.token \
  --state-dir=/var/lib/shipmunk-runner \
  --driver=codex \
  --profiles-dir=/var/lib/shipmunk-profiles \
  --image=shipmunk-profile-native:local \
  --repository-image=shipmunk-profile-native:local
```

Use an execution runner token and the protected profile root already connected through the profile lifecycle. Pre-create canonical state and profile directories owned by the dedicated non-root runner account with mode `0700`; the state directory must not contain symlink components. Add `--once` to handle at most one claim. Without `--driver=codex`, the runner selects the offline fixture driver.

The native container can contact the provider but cannot execute candidate repository code. Codex accesses the separate network-disabled repository container through the bounded, fenced `repository_command` MCP tool. The optional repository image must provide Git, GNU find and GNU tar with the capabilities described in [Codex compatibility](../docs/compatibility/codex.md). The bundled native image supplies them and is the default when `--repository-image` is omitted. Only the native container receives the assigned profile home; using the same image does not share mounts or credentials.

Local fixtures validate result normalization, patch collection, cancellation and cleanup. They do not establish live subscription compatibility. Complete [RT-02](../docs/runtime/RT-02.md) and [RT-03](../docs/runtime/RT-03.md) with designated infrastructure before claiming that compatibility.

Disconnect is local product disconnection, **not provider-side token revocation**. Revoke the session with the provider separately when needed. Application reconnect requires confirmed local disconnect cleanup and rotates the opaque credential reference before a new login.

`make runner-check` includes simulated-account lifecycle races, real OS locks and a real Linux credential-container fixture. To verify only the pinned actual executables and container/store provenance without contacting accounts:

```sh
SHIPMUNK_PROFILE_IMAGE=shipmunk-profile-native:local php runner/tests/ProfileContainerTest.php
```

The real login, authenticated preflight, expiry, native refresh and reconnect scenarios remain unexecuted until designated accounts are supplied. Offline fixtures are not evidence of live subscription compatibility.

Official references: [Codex authentication](https://developers.openai.com/codex/auth/), [Claude CLI commands](https://code.claude.com/docs/en/cli-usage), and [Claude authentication](https://code.claude.com/docs/en/authentication). The pinned image's `--version`, login help and execution help were checked without network access or any credential mount from the host.
