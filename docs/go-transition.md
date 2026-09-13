# Go runner compatibility foundation

The Go module in this repository is pinned to Go 1.25.4 and currently uses
only the standard library. `make go-check` runs formatting verification, vet,
unit tests, the race detector, and reproducible local builds of all three
operator commands.

The pinned `contracts-source` submodule is the server's protocol authority at
commit `6fba1a6502caf7843e41aefbc35f7548fd6ec6be`. Go tests load its schemas and
fixtures directly, so a server schema change cannot silently be copied into a
runner implementation. Updating the pin is a deliberate compatibility change.

The planned binary release matrix is Linux `amd64` and `arm64`, plus macOS
`amd64` and `arm64`. Linux remains the required Docker *engine* platform.
macOS is therefore supported only as a dedicated operator host using Docker
Desktop's Linux engine; it does not make macOS containers or credential
isolation a supported runtime. Windows is not a release target. Issue #58 owns
cross-compilation, installer manifests, and publishing those artifacts.

The commands expose the current PHP command names and flags but deliberately
refuse to claim work in this slice. Issue #55 first supplies durable attempt
recovery and supervision; issue #56 supplies profile lifecycle; issue #58
supplies installation. This prevents an incomplete binary from mutating
control-plane state while preserving a testable CLI contract.
