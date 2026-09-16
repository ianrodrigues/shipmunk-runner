# Contributing

Contributions to this repository are accepted under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only). By submitting a change, you agree it is licensed under those same terms; there is no separate contributor license agreement.

## Developer Certificate of Origin

Every commit must carry a `Signed-off-by` trailer certifying you wrote it or otherwise have the right to submit it under the [Developer Certificate of Origin](https://developercertificate.org/). Add the trailer automatically with:

```sh
git commit -s
```

The commit-message hook in `.githooks/commit-msg` warns, without rejecting, on a missing `Signed-off-by` trailer. This is a manual warning-only state; a maintainer will change the hook to reject missing trailers once the first external contribution lands, rather than the hook enforcing that transition itself.

## Changelog

A pull request that touches `cmd/`, `internal/`, `containers/` or `runner/containers/` must also touch `CHANGELOG.md`, typically adding an operator-facing line under `Unreleased`; `make lint` and the pre-push hook only check that the file changed, not where. The escape hatch is a `changelog: none` commit trailer for a change with no operator-visible effect; one such trailer anywhere in the pushed range exempts the whole range, so it does not need to sit on every commit.

## Before opening a pull request

Run `make hooks` once to enable the tracked git hooks, then `make check`. See [README.md](README.md) for the full local-development and validation workflow.
