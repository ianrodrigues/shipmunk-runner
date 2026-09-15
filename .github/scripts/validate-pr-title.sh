#!/usr/bin/env bash
set -euo pipefail

# This script copies the type, scope and description grammar from .githooks/commit-msg.
# That hook defines the Conventional Commit rules for this project.
# The hook also requires a blank second line and allows fixup, squash and amend commits.
# This script skips those rules because a pull request title is always one line.

if [[ $# -gt 1 ]]; then
    echo 'usage: validate-pr-title.sh [title] (reads stdin when no argument is given)' >&2
    exit 2
fi

if [[ $# -eq 1 ]]; then
    title=$1
else
    title=$(cat)
fi

if [[ ${#title} -gt 72 ]] || ! [[ "$title" =~ ^(feat|fix|docs|test|chore|ci|refactor|perf|build|style|revert)(\([a-z0-9._/-]+\))?!?:\ [^[:space:]].*$ ]]; then
    echo 'pr-title: use a Conventional Commit title of at most 72 characters, e.g. fix: preserve base snapshots.' >&2
    exit 1
fi

description=${title#*: }
if [[ "$description" =~ ^[[:upper:]] ]] || [[ "$title" == *. ]]; then
    echo 'pr-title: start the description in lower case and omit the final period.' >&2
    exit 1
fi
