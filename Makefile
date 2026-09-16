SHELL := /bin/bash
.DEFAULT_GOAL := check
VERSION ?= development
GO_VERSION_PACKAGE := github.com/ianrodrigues/shipmunk-runner/internal/command
GO_RELEASE_LDFLAGS := -s -w -X $(GO_VERSION_PACKAGE).Version=$(VERSION)

.PHONY: hooks hooks-check pr-title-check check lint changelog-check package-check runner-check native-image-check go-check go-build go-runtime-check install-smoke-check install-smoke-host-check

check: hooks-check pr-title-check lint package-check runner-check native-image-check go-check go-runtime-check

hooks:
	@set -eu; \
	if current="$$(git config --get core.hooksPath)"; then \
		if test "$$current" != .githooks; then \
			echo 'hooks: existing core.hooksPath is custom; integrate it explicitly before installing.' >&2; exit 1; \
		fi; \
	else \
		status=$$?; \
		if test "$$status" -ne 1; then \
			echo 'hooks: could not read core.hooksPath; configuration was not changed.' >&2; exit "$$status"; \
		fi; \
		common_dir="$$(git rev-parse --git-common-dir)"; \
		for hook in pre-commit commit-msg pre-push; do \
			if test -f "$$common_dir/hooks/$$hook" && test -x "$$common_dir/hooks/$$hook"; then \
				echo "hooks: preserve or integrate existing local $$hook before installing; commit-msg extras can use hooks/commit-msg.d/." >&2; exit 1; \
			fi; \
		done; \
		git config --local core.hooksPath .githooks; \
	fi; \
	echo 'Git hooks enabled for this clone and its linked worktrees.'

hooks-check:
	bash tests/GitHooksTest.sh

pr-title-check:
	bash tests/PRTitleTest.sh

go-runtime-check: runner-check
	@set -eu; build_dir="$$(mktemp -d "$${TMPDIR:-/tmp}/shipmunk-go-runtime.XXXXXX")"; trap 'rm -rf "$$build_dir"' EXIT; \
	go build -trimpath -buildvcs=false -ldflags '$(GO_RELEASE_LDFLAGS)' -o "$$build_dir/shipmunk-watchdog" ./cmd/shipmunk-watchdog; \
	SHIPMUNK_PROFILE_DOCKER_TEST=1 SHIPMUNK_PROFILE_IMAGE=shipmunk-profile-test:local go test -race -count=1 ./internal/profile; \
	SHIPMUNK_SANDBOX_DOCKER_TEST=1 SHIPMUNK_RUNNER_IMAGE=shipmunk-runner-test:local SHIPMUNK_WATCHDOG_BINARY="$$build_dir/shipmunk-watchdog" go test -race -count=1 ./internal/sandbox ./internal/supervisor

install-smoke-check:
	SHIPMUNK_INSTALL_SMOKE_TEST=1 go test -race -count=1 -timeout=10m ./internal/installsmoke

# Manual, opt-in darwin-only target; not part of `check`. See docs/releases.md.
install-smoke-host-check:
	SHIPMUNK_INSTALL_SMOKE_TEST=1 SHIPMUNK_INSTALL_SMOKE_HOST=1 go test -race -count=1 -timeout=10m -run TestHostNativeInstallsReleaseWithoutPHPOrGo -v ./internal/installsmoke

go-check:
	@test -z "$$(gofmt -l cmd internal)" || { echo 'Go files require gofmt.' >&2; gofmt -l cmd internal >&2; exit 1; }
	go vet ./...
	go test -race -count=1 ./...
	$(MAKE) go-build

go-build:
	@set -eu; build_dir="$$(mktemp -d "$${TMPDIR:-/tmp}/shipmunk-go-build.XXXXXX")"; trap 'rm -rf "$$build_dir"' EXIT; \
	go build -trimpath -buildvcs=false -ldflags '$(GO_RELEASE_LDFLAGS)' -o "$$build_dir/shipmunk-runner" ./cmd/shipmunk-runner; \
	go build -trimpath -buildvcs=false -ldflags '$(GO_RELEASE_LDFLAGS)' -o "$$build_dir/shipmunk-watchdog" ./cmd/shipmunk-watchdog

lint: changelog-check
	@bash -n tools/publish-release.sh
	@bash -n tools/changelog-check.sh

changelog-check:
	bash tools/changelog-check.sh origin/main HEAD

package-check:
	@cmp -s LICENSE runner/LICENSE || { echo 'package-check: LICENSE and runner/LICENSE have drifted; runner/LICENSE is the one that ships in release archives.' >&2; exit 1; }
	bash tests/ReleaseUploadTest.sh

runner-check:
	@command -v docker >/dev/null 2>&1 || { echo 'runner-check: Docker is missing; a Linux Docker engine is required.' >&2; exit 1; }
	@docker info >/dev/null 2>&1 || { echo 'runner-check: Docker is unavailable; start a Linux Docker engine.' >&2; exit 1; }
	@test "$$(docker version --format '{{.Server.Os}}')" = linux || { echo 'runner-check: the Docker server is not Linux.' >&2; exit 1; }
	@docker image inspect alpine:3.20 >/dev/null 2>&1 || { echo 'runner-check: alpine:3.20 is not present locally; load it explicitly before running this offline check.' >&2; exit 1; }
	@sh -n containers/runner/fake-native containers/runner/sandbox-entrypoint
	DOCKER_BUILDKIT=1 docker build --pull=false --tag shipmunk-runner-test:local --file containers/runner/Dockerfile .
	DOCKER_BUILDKIT=1 docker build --pull=false --tag shipmunk-profile-test:local --file runner/tests/fixtures/profile-Dockerfile .

native-image-check:
	DOCKER_BUILDKIT=1 docker build --pull=false --tag shipmunk-profile-native-test:local --file runner/containers/Dockerfile .
	SHIPMUNK_CODEX_DOCKER_TEST=1 SHIPMUNK_CODEX_TEST_IMAGE=shipmunk-profile-native-test:local go test -race -count=1 ./internal/profile ./internal/codex
