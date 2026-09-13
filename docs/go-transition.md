# Go runner compatibility foundation

The Go module is pinned to Go 1.27.1 and uses only the standard library. `make go-check` runs formatting verification, vet, unit tests, the race detector, and local builds of all three operator commands. A failure to build any command fails the build target. Go's toolchain download can acquire the pinned version when the installed Go executable is older.

The schema authority is the server's `contracts/v1` at commit `6fba1a6502caf7843e41aefbc35f7548fd6ec6be`. Only that immutable schema and synthetic fixture corpus is included here; the private server repository is not a build dependency. [The source inventory](../internal/protocol/contracts-source.json) records the upstream paths and SHA-256 digests, and an integrity test checks every included file. Updating the corpus and its pin is a deliberate compatibility change. The runner embeds the schemas, so HTTP validation does not depend on a checkout or the process working directory.

The planned binary targets are Linux `amd64` and `arm64`, plus macOS `amd64` and `arm64`. Linux remains the required Docker engine platform. macOS host support requires validation with a Linux Docker engine, including filesystem ownership and process cleanup; compiling a binary does not establish that support. Windows is not a release target. The release-packaging work owns the tested platform matrix, installer manifests, and published artifacts.

The command boundary exposes the current PHP command names and flags but refuses to claim work, authenticate profiles, or install packages in this foundation. Supervision and durable attempt recovery, native profile lifecycle, and installation are separate dependent slices. Keep using the published PHP runner until a verified Go release is adopted explicitly by the server. Tests of options, setup bundles, strict decoding and synthetic HTTP responses do not establish operational CLI or live subscription compatibility.

The compatibility map below separates implemented coverage from the remaining port. The PHP reference is runner commit `8b90bcff2519ea86e00e3d76fef22335144081f9`.

| Existing boundary | Go coverage or next owner |
| --- | --- |
| Protocol/claim and HTTP cases in `runner/tests/run.php`; server positive/negative corpus | `internal/protocol` strict/schema/HTTP tests and optional wire parity |
| CLI arguments and setup bundle fields in `SetupTest.php` | `internal/command` parser and no-side-effect refusal tests; operational exit behavior belongs to supervision/profile/installer slices |
| Supervisor/watchdog, `SourceArchiveTest.php`, `AttemptStateStore.php` | Supervision slice: port process/container recovery and the unversioned attempt JSON with run/attempt/fence, nullable profile/sandbox IDs, lease/deadline and workspace |
| Profile/home/container/exclusion tests and `Profiles/ProfileStore.php` | Profile slice: port locked `active`, `pending`, `completed`, and `execution` journals (16 KiB read bound), protected homes and PHP-state recovery |
| Codex driver/event/transport/supervisor tests and `Drivers/AgentSessionStore.php` | Codex slice: port native protocol and optional run-keyed session JSON (`id`, `binding`, 1 KiB read bound) |
| Package/setup/release tests | Packaging slice: verified compiled artifacts, platform execution tests, state-preserving installation and rollback |

The foundation deliberately does not read or rewrite any of these durable state formats. Each owning slice must demonstrate compatibility or explicit non-mutating refusal before CLI activation. HTTP calls retain the PHP baseline's five-second total timeout; longer artifact transfers require supervision-aware renewal and cancellation rather than an isolated timeout increase that can exceed the lease.

`make go-check` needs neither PHP nor the private server. The opt-in `make go-parity-check` compares claim identities and the actual PHP and Go wire requests for claims, heartbeats, stopped acknowledgements, events, artifact upload/download, completion, and profile operations using synthetic data and an in-memory transport. Its PHP oracle is excluded from default Go tests by the `phpbaseline` build tag.

During the transition, `make check` also runs that parity check and the existing PHP runner, packaging and native-container fixtures against synthetic accounts. It requires PHP 8.5, a Linux Docker engine and the deliberately acquired fixture image described in the repository README. Those existing tests remain the behavioral reference until their Go equivalents and the separate live validation gates pass.
