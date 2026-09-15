#!/usr/bin/env bash
set -euo pipefail

repository=$(cd "$(dirname "$0")/.." && pwd)
script="$repository/.github/scripts/validate-pr-title.sh"

fail() { printf 'FAIL %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS %s\n' "$1"; }
expect_ok() {
    local label=$1 title=$2
    "$script" "$title" > /dev/null 2>&1 || fail "$label"
}
expect_failure() {
    local label=$1 title=$2 output
    output=$(mktemp)
    if "$script" "$title" > "$output" 2>&1; then
        fail "$label accepted invalid input"
    fi
    [[ $(wc -l < "$output") -eq 1 ]] || fail "$label did not print a one-line message"
    rm -f "$output"
}

for title in 'feat: add a check' 'fix(parser)!: reject invalid syntax' 'docs(ci/setup): explain hooks' 'revert: restore behavior'; do
    expect_ok "valid title $title" "$title"
done
pass 'accepted titles pass validation'

description=$(printf 'a%.0s' $(seq 1 67))
expect_ok '72 character title' "fix: $description"
description=$(printf 'a%.0s' $(seq 1 68))
expect_failure '73 character title' "fix: $description"
pass 'subject length boundary is enforced'

expect_failure 'missing type' 'add a new feature'
expect_failure 'uppercase description' 'fix: Add feature'
expect_failure 'trailing period' 'fix: add feature.'
expect_failure 'empty scope' 'fix(): add feature'
expect_failure 'unknown type' 'feature: add x'
pass 'rejected titles fail validation with a one-line message'

printf 'fix: preserve base snapshots' | "$script" > /dev/null || fail 'title can be read from stdin'
pass 'title can be read from stdin, matching CI usage'
