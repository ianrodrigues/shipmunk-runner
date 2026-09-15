# Third-party notices

This repository is licensed under the GNU Affero General Public License v3.0 (see [LICENSE](LICENSE)). Third-party software distributed or depended on by this project remains under its own license; nothing below is relicensed by this repository's AGPL-3.0-only license.

## Go dependencies

This module has no external Go dependencies today (`go.mod` has zero `require` entries). If dependencies are added, `go list -m all` gives the current resolved set; inspect each listed module's own license separately.

## Native agent clients and container images

The runner downloads native coding-agent clients (for example the Codex CLI) and container base images from their official distributions at build or runtime. These artifacts are not vendored, modified, or relicensed by this repository; they remain under the license terms published by their respective providers.
