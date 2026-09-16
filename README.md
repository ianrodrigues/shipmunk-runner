# Shipmunk runner

The standalone execution host for Shipmunk. The Go runner runs Codex work in isolated Linux containers and connects to a separately deployed Shipmunk application. It does not load Laravel, Composer dependencies, application environment files, database credentials or GitHub credentials. The verified Go release is the only supported installation path.

The initial version is `v0.1.0-alpha.1`. Runner versions and release tags are independent of application versions; each deployment pins a specific published tag from its own `runner-release.json`. Every tag remains a prerelease until noted otherwise: offline checks establish the tested behavior, not live subscription compatibility or full product release readiness.

## Install and connect

Use **Connections → Runners & Codex** in your Shipmunk application to register a runner and download its short-lived setup file. The dashboard also shows a hosted bootstrap command for the dedicated execution host; running it downloads the exact public Go release pinned by the application, verifies the platform archive's SHA-256 digest against the digests recorded in the release manifest and `SHA256SUMS`, and runs the verified `shipmunk-setup` binary from that archive. No PHP, Go toolchain, application checkout or database access is needed on the runner host — only `curl`, `tar`, `sha256sum`/`shasum`, Bash and a reachable Linux Docker engine (Docker Desktop on macOS can provide it). Run as the designated non-root account.

To install without the hosted bootstrap script, download and verify the release yourself:

```sh
version=vX.Y.Z # the tag pinned by your application, or one you built with `go run ./cmd/shipmunk-package . dist vX.Y.Z`
platform=linux-amd64 # or linux-arm64, darwin-amd64, darwin-arm64

curl -fsSLO "https://github.com/ianrodrigues/shipmunk-runner/releases/download/$version/runner-release.json"
curl -fsSLO "https://github.com/ianrodrigues/shipmunk-runner/releases/download/$version/SHA256SUMS"
curl -fsSLO "https://github.com/ianrodrigues/shipmunk-runner/releases/download/$version/shipmunk-runner-$version-$platform.tar"
if command -v sha256sum >/dev/null 2>&1; then sha256sum --ignore-missing -c SHA256SUMS; else shasum -a 256 --ignore-missing -c SHA256SUMS; fi
tar -xf "shipmunk-runner-$version-$platform.tar" bin/shipmunk-setup

./bin/shipmunk-setup ~/Downloads/shipmunk-setup-RUNNER.json \
  --release-manifest runner-release.json \
  --release-archive "shipmunk-runner-$version-$platform.tar"
```

Run on an operator terminal: stdin, stdout and stderr must all be a TTY. Append `--server-url=https://your-runner-reachable-server` for a runner host separate from the application; loopback HTTP is only for same-host development. Setup verifies the archive against the manifest, confirms the server and runner interactively, checks `/up` and the Docker preflight, builds the pinned runtime image, and activates private configuration and the `run`/`connect` launchers under `~/.shipmunk/runners/<runner-id>/`. It never starts queued work.

The four `platform` values above are all packaged, but validated differently: `linux-amd64` is smoke-tested in CI in a PHP-free container on every push; `linux-arm64` smoke passes locally under QEMU emulation or native hardware and is a tested parameter not yet wired to a CI runner; `darwin-arm64` native operation was demonstrated by the server's published-runner check against `v0.1.0-alpha.10` on an Apple Silicon host, though that host was not PHP-free, so it is native-operation evidence rather than a clean-host smoke test; `darwin-amd64` is compile-only and unvalidated and is not a supported executable platform until a smoke run exists on an Intel host. See [release packaging and application pins](docs/releases.md) for details.

Once installed, use the printed launchers directly:

```sh
~/.shipmunk/runners/RUNNER/connect        # native terminal login
~/.shipmunk/runners/RUNNER/connect probe  # recheck an existing connection
~/.shipmunk/runners/RUNNER/run --once     # handle at most one claim
~/.shipmunk/runners/RUNNER/run            # poll continuously
```

Each launcher invokes the installed `shipmunk-setup` binary by absolute path, which strictly rebuilds the `shipmunk-runner`/`shipmunk-profile` arguments described in [runner operation and isolation](runner/README.md) from the protected installation; setup never starts queued work itself. If a runner process stops during an attempt, `run` recovers it: the next launch removes the sandbox, reports the interrupted attempt to the application as a failed attempt with a stopped acknowledgement, clears the local journals once the application accepts that acknowledgement and then polls again; a refused acknowledgement keeps the journals for the next launch. Since the stopped acknowledgement is refused only when the attempt has been permanently superseded, the third such refusal across separate launches (an unrelated failure in between neither counts nor resets the tally) converges on its own: the journal and workspace are released without waiting for an acknowledgement the application will never accept. An operator who does not want to wait for that bound can run `run --discard-attempt` (with `--yes` to skip the interactive confirmation), which attempts a best-effort sandbox reconciliation and stopped acknowledgement, names the sandbox and warns if that cleanup could not be confirmed, and discards the journal immediately regardless. `connect` and token renewal still refuse until that recovery has run. Renewing tokens (downloading setup again for the same runner) preserves profile credentials and pending operation journals; it never repeats login or discards a confirmed home.

See [runner event log](docs/runner/logging.md) for the structured log every installed runner writes under `<state-dir>/logs/`, and [runner-classified failure breaker](docs/runner/failure-breaker.md) for when and why continuous polling stops on its own.

## Check the standalone repository

```sh
# Deliberate fixture image acquisition, separate from the offline checks:
docker pull alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
docker tag alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc alpine:3.20
make check
```

`make check` runs syntax checks, package and release-upload fixtures, the supervisor/container suite, and pinned native executable checks. Native image construction downloads the pinned official packages; tests use synthetic data without account access. Go Codex setup and execution require Docker client and Linux engine 26.0 or newer for immutable review snapshot mounts. No application, database, Redis, Composer installation or application checkout is required. Trusted pushes, internal pull requests and releases use the existing `github-runner-01` self-hosted Linux runner. The configured check job skips fork pull requests, and there is no GitHub-hosted fallback. The repository also requires approval for all external contributors: a fork can edit workflow files, so the job condition alone is not an isolation boundary. Do not approve external fork workflow runs. Review a fork contribution and promote the accepted code to a trusted internal branch before running checks.

`make go-build VERSION=v1.2.3` builds the four operational Go commands with that exact version embedded. An ordinary local build reports `development`; release packaging must always supply its validated release tag. Each resulting `shipmunk-runner`, `shipmunk-profile`, `shipmunk-setup`, and `shipmunk-watchdog` binary reports the embedded value with `--version`.

## Local Git hooks

Run `make hooks` once per clone to enable the tracked hooks, including linked worktrees. Installation is repeatable and refuses to overwrite a custom `core.hooksPath` or hide executable default hooks; combine your existing hooks explicitly first. Hooks use Bash and the existing Go tools, perform no network calls or test suites, and never rewrite or stage files. Pre-commit checks whitespace/conflict markers and the exact staged Go formatting, so partial staging is respected. `make hooks-check` runs their isolated Git regressions and is included in `make check`.

Use Conventional Commits such as `fix: preserve base snapshots`: the entire subject must be at most 72 characters, start its description in lower case, omit a final period, and have a blank line before the body. Git-generated `fixup!`, `squash!`, and `amend!` messages are exempt; autosquash them before merging. Optional executable checks in `$(git rev-parse --git-common-dir)/hooks/commit-msg.d/` receive every commit message, including autosquash messages. Pre-push rejects creating, updating, or deleting the remote `main` branch; push a feature branch for review. Release tags are allowed.

The [`pr-title` workflow](.github/workflows/pr-title.yml) enforces the same Conventional Commit grammar, minus the blank-body line, on every pull request title.

## Publish a release

1. Pass the checks and merge the reviewed runner changes.
2. Create a GitHub release with a semantic version tag such as `v0.1.0-alpha.1`, targeting that commit. Mark alpha versions as prereleases.
3. Publish the release. The [release workflow](.github/workflows/release.yml) runs the standalone checks, then builds `shipmunk-runner-VERSION-{linux,darwin}-{amd64,arm64}.tar`, `SHA256SUMS`, and `runner-release.json` from that tag's commit. A separate write-scoped job uploads those exact checked artifacts and publishes the manifest last.

Publishing either a stable release or a prerelease triggers the workflow. Draft creation alone does not. A rerun verifies and reuses matching assets, resumes missing uploads, and refuses to replace different bytes. Only the upload job has repository write permission, and its token is exposed only to the upload step. See [release packaging and application pins](docs/releases.md).

## Source relationship and license

This repository is the public source for runner releases. Runner source and native checks live here. The application consumes the pinned public release for setup and does not retain a tracked runner source copy. Runner-coupled application tests are temporarily disabled until a separate cross-repository integration strategy is implemented. [source-snapshot.json](source-snapshot.json) records the initial snapshot's origin and file hashes without copying private repository history. Future runner changes should be reviewed here, released under an independent version, and then adopted through an explicit application release-pin update.

Copyright (C) 2026 Ian Rodrigues. The runner is licensed under AGPL-3.0-only. See [LICENSE](LICENSE) and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md); see [CONTRIBUTING.md](CONTRIBUTING.md) for contribution terms. Native clients and container dependencies retain their own licenses; they are downloaded from their official distributions rather than committed here.
