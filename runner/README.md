# Runner operation and isolation

This documents how the installed runner actually operates: the `shipmunk-runner`/`shipmunk-profile` flag surface the [top-level README](../README.md)'s `run`/`connect` launchers invoke from a protected installation, and the isolation guarantees behind it. See the [top-level README](../README.md) for installing and connecting a runner; this assumes an installed runner, or a local checkout for the offline checks below.

The runner does not load Laravel, Composer's application autoloader, `.env`, `APP_KEY`, database credentials, GitHub credentials, or application configuration. Configure it only with explicit command options and a mode-0600 runner-token file:

```sh
./bin/shipmunk-runner \
  --base-url=https://shipmunk.example \
  --token-file=/etc/shipmunk/runner.token \
  --state-dir=/var/lib/shipmunk-runner \
  --image=shipmunk-runner:local
```

The runner is privileged infrastructure because it controls the container runtime. Run it on a dedicated execution host. The runtime socket stays on that host and is never mounted into an agent container.

Each attempt uses a new non-root, read-only-root container with all Linux capabilities dropped, `no-new-privileges`, bounded CPU, memory, PIDs, file descriptors, logs, time, and tmpfs-backed writable paths. Repository networking is `none`. The default fixture driver's inert entrypoint waits for an atomic ready marker while the runner copies the workspace and sanitized agent input, with lease checkpoints between each bounded setup operation. Repository code cannot start against partial or stale inputs. Codex execution requires explicit selection as described below; restricted repository egress and Claude execution are not implemented.

Native credential homes must be separately scoped per active profile and mediated outside repository subprocesses. Mounting a credential home into the same container as repository code is not an isolation boundary and is not supported by this implementation. Agent input has the entire supervisor section and credential-bearing keys removed. Source archives and trusted instruction bundles are fetched only by ID through the authenticated control-plane client and hash-checked. Gzip/tar source extraction is bounded and rejects links, special files and traversal; instruction bundles remain separate validated JSON files.

The runner persists claim state before artifact preparation and records a created sandbox before starting it. It heartbeats immediately after claim and on a timed 10-second cadence through preparation and execution against a 45-second lease. An independent `shipmunk-watchdog` process, launched as its own binary rather than a forked child, tracks renewals with a monotonic timer. Parent death, heartbeat loss, or deadline expiry stops and removes the whole container process tree with TERM and KILL escalation while leaving durable state for restart acknowledgement. Durable state is cleared only after the container and workspace are confirmed absent and the server accepts a fenced `stopped` acknowledgement. Cleanup or acknowledgement failure retains that state, so restart reconciliation must retry it before asking for replacement work.

Docker preflight, lifecycle commands and the independent watchdog share only the host's Docker client configuration (`HOME`, `DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG`, `DOCKER_CERT_PATH` and `DOCKER_TLS_VERIFY`); provider and control-plane credential variables are excluded. This configuration belongs only to trusted host-side Docker clients and is never injected into repository or native containers, which explicitly clear every upper/lowercase proxy variable so Docker's client-config defaults cannot inject a credential-bearing proxy URL. Keep the selected Docker endpoint and context stable while an attempt or its recovery journal exists.

Run `make runner-check`. The offline check requires a reachable Linux Docker engine and a deliberately preloaded `alpine:3.20` image; it fails with an actionable error instead of pulling or silently skipping when isolation cannot be exercised. It builds the sandbox and lightweight profile fixture images. `make go-runtime-check` sets the Docker test variables. It then exercises these images in the `internal/sandbox`, `internal/supervisor` and `internal/profile` suites. `make go-check` does not set these variables.

## Native subscription profiles

Build the credential-only image explicitly (this downloads the pinned official packages):

```sh
DOCKER_BUILDKIT=1 docker build --tag shipmunk-profile-native:local --file runner/containers/Dockerfile .
```

The image pins Codex **0.154.0** and Claude Code **2.1.269**. Setup persists the built immutable image ID for each installation; later builds cannot redirect another installation to a different image. Manual CLI commands resolve their selected image to an immutable local ID. Every operation verifies the executable version. The profile's server-side runtime version must match. Changing pins requires repeating the offline checks and designated-account runtime scenarios.

The image includes the system CA bundle required for native HTTPS certificate verification. `make native-image-check` builds this exact pinned image, then checks its default certificate trust, pinned CLI versions and baked schema/MCP asset permissions with networking disabled and no host mounts, and proves the Dockerfile's own build-context isolation against a checkout's secrets. It also runs the `internal/codex` suite's Docker transport, synthetic supervisor lifecycle and real pinned Codex tool boundary regressions against that image. Building downloads packages; the checks themselves do not access an account.

Native clients can explicitly create metadata with broader permissions than the runner's umask. After the native container has stopped, the runner validates the entire owned home for unsafe entries, then reduces regular files to `0600` and subdirectories to `0700` while holding the profile lock. The enclosing home stays `0700` throughout. Links, special files, multiple hard links, foreign ownership and special permission bits are rejected before any permissions change; an unsafe entry keeps the execution reservation quarantined rather than releasing it. `.codex/tmp` is the native runtime's own scratch directory: its contents are pruned inside the home, without following links, before that validation runs, so a helper symlink the runtime left behind cannot quarantine the profile; the directory itself stays and every other path stays strict. Foreign-owner coverage runs on Linux, where bind mounts preserve ownership.

Create the profile through the application API with the assigned runner and exact runtime version, then use its lowercase ULID and a fresh lowercase ULID for the operation. From a terminal on the assigned runner:

```sh
./bin/shipmunk-profile \
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

Each native command receives a clean allowlist environment inside a dedicated Linux container with only its own home mounted. Repository code, other profile homes, the Docker socket and the runner token are absent. Codex helper aliases live in disposable tmpfs rather than the persistent store: every container that mounts a profile home, for lifecycle operations and for native execution alike, masks `.codex/tmp` with its own tmpfs. The store rejects links, hardlinks, wrong ownership and broad permissions. The host runner is trusted infrastructure; same-UID host processes are not mutually isolated. Native execution holds the profile's execution reservation for its entire native process lifetime, does a fresh fenced authorization check, and keeps repository subprocesses in a separate sandbox.

## Codex execution

After connecting a designated Codex profile, select the native driver explicitly on its assigned dedicated Linux runner:

```sh
./bin/shipmunk-runner \
  --base-url=https://shipmunk.example \
  --token-file=/etc/shipmunk/runner.token \
  --state-dir=/var/lib/shipmunk-runner \
  --driver=codex \
  --profiles-dir=/var/lib/shipmunk-profiles \
  --image=shipmunk-profile-native:local \
  --repository-image=shipmunk-profile-native:local
```

Use an execution runner token and the protected profile root already connected through the profile lifecycle. Pre-create canonical state and profile directories owned by the dedicated non-root runner account with mode `0700`; the state directory must not contain symlink components. Add `--once` to handle at most one claim. Without `--driver=codex`, the runner selects the offline fixture driver.

`run --once` prints the run ID, attempt ID and outcome after the application accepts the result and the runner confirms cleanup. Successful review or change outcomes exit with code `0`; `incomplete` and `needs_input` exit with code `1`. An idle poll prints that no eligible work was returned and exits with code `0`. Connection, execution or cleanup errors exit with code `1` without claiming completion. Continuous `run` prints each completed outcome and keeps polling. Terminal output excludes native output and model-generated summaries.

For native failures, open the application run to see the sanitized stage, reason and native exit code in its progress and result summary. The stages distinguish version inspection, account verification, authenticated preflight, execution and result validation. An unavailable exit code is reported explicitly. Raw provider messages, stderr and credential contents are discarded.

The native container can contact the provider. Its Code Mode host evaluates JavaScript without direct filesystem, process or network APIs; repository commands execute only in the separate repository container. Codex accesses the separate network-disabled repository container through the bounded, fenced `repository_command` MCP tool. The optional repository image must provide Git, GNU find and GNU tar with the capabilities described in [Codex compatibility](../docs/compatibility/codex.md). The bundled native image supplies them and is the default when `--repository-image` is omitted. Only the native container receives the assigned profile home; using the same image does not share mounts or credentials.

Local fixtures validate result normalization, patch collection, cancellation and cleanup. They do not establish live subscription compatibility. Complete [RT-02](../docs/runtime/RT-02.md) and [RT-03](../docs/runtime/RT-03.md) with designated infrastructure before claiming that compatibility.

Disconnect is local product disconnection, **not provider-side token revocation**. Revoke the session with the provider separately when needed. Application reconnect requires confirmed local disconnect cleanup and rotates the opaque credential reference before a new login.

`make runner-check` and `make native-image-check` include simulated-account lifecycle races, real OS locks and a real Linux credential-container fixture. To verify only the pinned actual executables and container/store provenance without contacting accounts:

```sh
SHIPMUNK_CODEX_DOCKER_TEST=1 SHIPMUNK_CODEX_TEST_IMAGE=shipmunk-profile-native:local go test ./internal/profile -run TestNativeImage
```

The real login, authenticated preflight, expiry, native refresh and reconnect scenarios remain unexecuted until designated accounts are supplied. Offline fixtures are not evidence of live subscription compatibility.

Official references: [Codex authentication](https://developers.openai.com/codex/auth/), [Claude CLI commands](https://code.claude.com/docs/en/cli-usage), and [Claude authentication](https://code.claude.com/docs/en/authentication). The pinned image's `--version`, login help and execution help were checked without network access or any credential mount from the host.
