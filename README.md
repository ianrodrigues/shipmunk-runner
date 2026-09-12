# Shipmunk runner

The standalone PHP execution host for Shipmunk. It runs Codex work in isolated Linux containers and connects to a separately deployed Shipmunk application. It does not load Laravel, Composer dependencies, application environment files, database credentials or GitHub credentials.

The initial version is `v0.1.0-alpha.1`. This is a prerelease: offline checks establish the tested behavior, not live subscription compatibility or full product release readiness. Runner versions and release tags are independent of application versions.

## Install and connect

Use **Connections → Runners & Codex** in your Shipmunk application to register a runner and download its short-lived setup file. Run the displayed setup command on the dedicated execution host. The hosted bootstrap downloads the exact public runner release pinned by the application, checks its SHA-256 digest and complete file manifest, and installs into a private directory before loading the downloaded code. The public release contains code and its license; registration tokens remain in your private setup download.

For a source checkout, use:

```sh
bash runner/bin/shipmunk-setup ~/Downloads/shipmunk-setup-RUNNER.json
```

The host needs PHP 8.5 with `pcntl` and `posix`, Bash, and a reachable Linux Docker engine. Run as the designated non-root account. `PHP_BIN=/path/to/php` selects another PHP executable. Setup offers native terminal login and prints runner commands; it does not automatically start queued work. See [runner operation and isolation](runner/README.md) for manual commands and supported boundaries.

## Check the standalone repository

```sh
# Deliberate fixture image acquisition, separate from the offline checks:
docker pull alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
docker tag alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc alpine:3.20
make check
```

`make check` runs syntax checks, package and release-upload fixtures, the supervisor/container suite, and pinned native executable checks. Native image construction downloads the pinned official packages; tests use synthetic data without account access. No application, database, Redis, Composer installation or application checkout is required. CI uses disposable GitHub-hosted Linux runners; public pull requests never use the application's private self-hosted runner.

## Publish a release

1. Pass the checks and merge the reviewed runner changes.
2. Create a GitHub release with a semantic version tag such as `v0.1.0-alpha.1`, targeting that commit. Mark alpha versions as prereleases.
3. Publish the release. The [release workflow](.github/workflows/release.yml) runs the standalone checks, then builds and uploads `shipmunk-runner-VERSION.tar`, `SHA256SUMS` and `runner-release.json` from that tag's commit.

Publishing either a stable release or a prerelease triggers the workflow. Draft creation alone does not. A rerun verifies and reuses matching assets, resumes missing uploads, and refuses to replace different bytes. Only the upload job has repository write permission, and its token is exposed only to the upload step. See [release packaging and application pins](docs/releases.md).

## Source relationship and license

This repository is the public source for runner releases. The application currently retains a runner snapshot for integrated tests; removing that copy or moving application CI is outside this initial extraction. [source-snapshot.json](source-snapshot.json) records the initial snapshot's origin and file hashes without copying private repository history. Future runner changes should be reviewed here, released under an independent version, and then synchronized into the application's test snapshot and release pin.

The runner follows the application's existing MIT declaration. See [LICENSE](LICENSE). Native clients and container dependencies retain their own licenses; they are downloaded from their official distributions rather than committed here.
