# Contributing

Contributions to this repository are accepted under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only). By submitting a change, you agree it is licensed under those same terms; there is no separate contributor license agreement.

## Developer Certificate of Origin

Every commit must carry a `Signed-off-by` trailer certifying you wrote it or otherwise have the right to submit it under the [Developer Certificate of Origin](https://developercertificate.org/). Add the trailer automatically with:

```sh
git commit -s
```

The commit-message hook in `.githooks/commit-msg` warns, without rejecting, on a missing `Signed-off-by` trailer. This is a manual warning-only state; a maintainer will change the hook to reject missing trailers once the first external contribution lands, rather than the hook enforcing that transition itself.

## Changelog

A pull request that touches `cmd/` or `internal/` must add an operator-facing line under `CHANGELOG.md`'s `## Unreleased` section, or carry a `changelog: none` commit trailer when the change has no operator-visible effect; `make lint` and the pre-push hook enforce this.

## Before opening a pull request

Run `make hooks` once to enable the tracked git hooks, then `make check`. See [README.md](README.md) for the full local-development and validation workflow.
