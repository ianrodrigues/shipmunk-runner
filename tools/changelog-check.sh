#!/usr/bin/env bash
set -euo pipefail

# A code change without an Unreleased line silently drops operator-facing
# history; the "changelog: none" trailer is the deliberate escape hatch for
# refactors and internal-only changes.

base=${1:?usage: changelog-check.sh <base> <head>}
head=${2:?usage: changelog-check.sh <base> <head>}

if ! git rev-parse --verify --quiet "${base}^{commit}" > /dev/null \
    || ! git rev-parse --verify --quiet "${head}^{commit}" > /dev/null; then
    echo "changelog-check: cannot resolve $base or $head" >&2
    exit 1
fi

changed=$(git diff --name-only "$base" "$head" --)
if ! grep -qE '^(cmd|internal)/' <<< "$changed"; then
    exit 0
fi
if grep -qxF 'CHANGELOG.md' <<< "$changed"; then
    exit 0
fi

while IFS= read -r commit; do
    [[ -n "$commit" ]] || continue
    if git log -1 --format=%B "$commit" \
        | git interpret-trailers --parse \
        | grep -qiE '^changelog:[[:space:]]*none$'; then
        exit 0
    fi
done < <(git rev-list "$base".."$head")

echo 'changelog-check: this range touches cmd/ or internal/ without CHANGELOG.md; add an Unreleased line or a "changelog: none" commit trailer.' >&2
exit 1
