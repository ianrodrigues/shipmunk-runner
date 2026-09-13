SHELL := /bin/bash
.DEFAULT_GOAL := check

.PHONY: check lint package-check runner-check native-image-check go-check go-build go-parity-check go-runtime-check

check: lint package-check runner-check native-image-check go-check go-parity-check go-runtime-check

go-runtime-check: runner-check
	@set -eu; build_dir="$$(mktemp -d "$${TMPDIR:-/tmp}/shipmunk-go-runtime.XXXXXX")"; trap 'rm -rf "$$build_dir"' EXIT; \
	go build -trimpath -buildvcs=false -o "$$build_dir/shipmunk-watchdog" ./cmd/shipmunk-watchdog; \
	SHIPMUNK_SANDBOX_DOCKER_TEST=1 SHIPMUNK_RUNNER_IMAGE=shipmunk-runner-test:local SHIPMUNK_WATCHDOG_BINARY="$$build_dir/shipmunk-watchdog" go test -race -count=1 ./internal/sandbox ./internal/supervisor

go-parity-check:
	@php -r 'exit(PHP_VERSION_ID >= 80500 ? 0 : 1);' || { echo 'The temporary PHP baseline requires PHP 8.5.' >&2; exit 1; }
	go test -race -count=1 -tags=phpbaseline ./internal/protocol

go-check:
	@test -z "$$(gofmt -l cmd internal)" || { echo 'Go files require gofmt.' >&2; gofmt -l cmd internal >&2; exit 1; }
	go vet ./...
	go test -race -count=1 ./...
	$(MAKE) go-build

go-build:
	@set -eu; build_dir="$$(mktemp -d "$${TMPDIR:-/tmp}/shipmunk-go-build.XXXXXX")"; trap 'rm -rf "$$build_dir"' EXIT; \
	go build -trimpath -buildvcs=false -o "$$build_dir/shipmunk-runner" ./cmd/shipmunk-runner; \
	go build -trimpath -buildvcs=false -o "$$build_dir/shipmunk-profile" ./cmd/shipmunk-profile; \
	go build -trimpath -buildvcs=false -o "$$build_dir/shipmunk-setup" ./cmd/shipmunk-setup; \
	go build -trimpath -buildvcs=false -o "$$build_dir/shipmunk-watchdog" ./cmd/shipmunk-watchdog

lint:
	@php -r 'exit(PHP_VERSION_ID >= 80500 ? 0 : 1);' || { echo 'PHP 8.5 or newer is required.' >&2; exit 1; }
	@find runner tools tests -type f \( -name '*.php' -o -name shipmunk-runner -o -name shipmunk-profile \) -exec php -l {} \; >/dev/null
	@bash -n runner/bin/shipmunk-setup tools/publish-release.sh

package-check:
	php runner/tests/PackageTest.php
	php tests/PackagingTest.php
	bash tests/ReleaseUploadTest.sh

runner-check:
	@command -v php >/dev/null 2>&1 || { echo 'runner-check: php is missing. Install PHP 8.5.' >&2; exit 1; }
	@php -r 'exit(PHP_VERSION_ID >= 80500 ? 0 : 1);' || { echo "runner-check: PHP 8.5 or newer is required, found $$(php -r 'echo PHP_VERSION;')." >&2; exit 1; }
	@php -r 'exit(extension_loaded("pcntl") ? 0 : 1);' || { echo 'runner-check: the PHP pcntl extension is required for the independent host watchdog.' >&2; exit 1; }
	@php -r 'exit(extension_loaded("posix") ? 0 : 1);' || { echo 'runner-check: the PHP posix extension is required for protected profile storage.' >&2; exit 1; }
	@command -v docker >/dev/null 2>&1 || { echo 'runner-check: Docker is missing; a Linux Docker engine is required.' >&2; exit 1; }
	@docker info >/dev/null 2>&1 || { echo 'runner-check: Docker is unavailable; start a Linux Docker engine.' >&2; exit 1; }
	@test "$$(docker version --format '{{.Server.Os}}')" = linux || { echo 'runner-check: the Docker server is not Linux.' >&2; exit 1; }
	@docker image inspect alpine:3.20 >/dev/null 2>&1 || { echo 'runner-check: alpine:3.20 is not present locally; load it explicitly before running this offline check.' >&2; exit 1; }
	@find runner -type f \( -name '*.php' -o -path 'runner/bin/shipmunk-runner' -o -path 'runner/bin/shipmunk-profile' \) -exec php -l {} \; >/dev/null
	@sh -n containers/runner/fake-native containers/runner/sandbox-entrypoint
	DOCKER_BUILDKIT=1 docker build --pull=false --tag shipmunk-runner-test:local --file containers/runner/Dockerfile .
	SHIPMUNK_RUNNER_IMAGE=shipmunk-runner-test:local php runner/tests/run.php
	php runner/tests/SourceArchiveTest.php
	php runner/tests/ProfileTest.php
	php runner/tests/CodexEventParserTest.php
	php runner/tests/CodexDriverTest.php
	php runner/tests/CodexExclusionTest.php
	php runner/tests/AgentSessionStoreTest.php
	bash -n runner/bin/shipmunk-setup
	php runner/tests/SetupTest.php
	php runner/tests/PackageTest.php
	DOCKER_BUILDKIT=0 php runner/tests/SetupContextTest.php
	php runner/tests/SetupImageTest.php
	DOCKER_BUILDKIT=1 docker build --pull=false --tag shipmunk-profile-test:local --file runner/tests/fixtures/profile-Dockerfile .
	php runner/tests/ProfileContainerTest.php

native-image-check:
	SHIPMUNK_PROFILE_IMAGE=shipmunk-profile-native-test:local php runner/tests/InstalledNativeImageTest.php
	php runner/tests/NativeImageTest.php
	php runner/tests/NativeHomeTest.php
	SHIPMUNK_CODEX_TEST_IMAGE=shipmunk-profile-native-test:local php runner/tests/DockerAgentTransportTest.php
	DOCKER_BUILDKIT=1 docker build --pull=false --build-arg CODEX_FIXTURE_BASE=shipmunk-profile-native-test:local --tag shipmunk-codex-supervisor-test:local --file runner/tests/fixtures/codex-supervisor/Dockerfile .
	OPENAI_API_KEY=SYNTHETIC_CONFLICT SHIPMUNK_CODEX_SUPERVISOR_IMAGE=shipmunk-codex-supervisor-test:local php runner/tests/CodexSupervisorTest.php
	SHIPMUNK_CODEX_BOUNDARY_IMAGE=shipmunk-profile-native-test:local php runner/tests/CodexBoundaryTest.php
