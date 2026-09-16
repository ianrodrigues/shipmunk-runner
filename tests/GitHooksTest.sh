#!/usr/bin/env bash
set -euo pipefail

repository=$(cd "$(dirname "$0")/.." && pwd)
# Tests may run from inside a hook. Never inherit the hook's real index or worktree.
git_variables=$(git rev-parse --local-env-vars)
for variable in $git_variables; do unset "$variable"; done
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

temporary=$(mktemp -d "${TMPDIR:-/tmp}/shipmunk-hooks-test.XXXXXX")
trap 'rm -rf "$temporary"' EXIT
fixture="$temporary/repo with spaces"
git init --quiet --initial-branch=main "$fixture"
cd "$fixture"
git config user.name 'Hook Fixture'
git config user.email 'hooks@example.test'
mkdir .githooks tools
cp "$repository"/.githooks/* .githooks/
cp "$repository/Makefile" Makefile
cp "$repository/tools/changelog-check.sh" tools/changelog-check.sh

fail() { printf 'FAIL %s\n' "$*" >&2; cat "$temporary/output" >&2 2>/dev/null || true; exit 1; }
pass() { printf 'PASS %s\n' "$1"; }
expect_ok() {
    local label=$1; shift
    "$@" > "$temporary/output" 2>&1 || fail "$label"
}
expect_failure() {
    local label=$1; shift
    if "$@" > "$temporary/output" 2>&1; then fail "$label accepted invalid input"; fi
}
assert_unchanged() {
    [[ "$(git write-tree)" == "$staged_tree" ]] || fail 'hook mutated the index'
    [[ "$(git hash-object -- "$checked_path")" == "$worktree_hash" ]] || fail 'hook mutated the worktree'
}

printf '[broken config\n' > "$temporary/broken-config"
expect_failure 'invalid global config' env GIT_CONFIG_GLOBAL="$temporary/broken-config" make hooks
if git config --local --get core.hooksPath > /dev/null; then fail 'config error changed the local hook path'; fi

git config core.hooksPath custom-hooks
expect_failure 'custom hook path' make hooks
[[ "$(git config core.hooksPath)" == custom-hooks ]] || fail 'custom hooksPath changed'
git config --unset core.hooksPath
printf '#!/bin/sh\nexit 0\n' > .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit
expect_failure 'existing default hook' make hooks
if git config --get core.hooksPath > /dev/null; then fail 'default hook was shadowed'; fi
rm .git/hooks/pre-commit
expect_ok 'install hooks' make hooks
expect_ok 'repeat installation' make hooks
[[ "$(git config core.hooksPath)" == .githooks ]] || fail 'hook path not installed'
pass 'installation preserves custom hook paths and existing local hooks'

git add .githooks Makefile tools
expect_ok 'initial real commit' git commit --quiet -m 'test: install fixture hooks'

checked_path='source with spaces.go'
printf 'package fixture\nfunc answer( ){ }\n' > "$checked_path"
git add -- "$checked_path"
printf 'package fixture\n\nfunc answer() {}\n' > "$checked_path"
staged_tree=$(git write-tree); worktree_hash=$(git hash-object -- "$checked_path")
expect_failure 'staged Go format' git commit --quiet -m 'test: reject staged formatting'
assert_unchanged
git add -- "$checked_path"
printf 'package broken\nfunc (\n' > "$checked_path"
staged_tree=$(git write-tree); worktree_hash=$(git hash-object -- "$checked_path")
expect_ok 'valid staged Go with invalid working copy' git commit --quiet -m 'test: respect partial staging'
assert_unchanged
pass 'Go checks inspect staged bytes and preserve partially staged files'

# Newlines, leading dashes, and pathspec metacharacters remain literal filenames.
for checked_path in $'odd\nname.go' '-leading.go' 'literal[1].go' ':1:literal.go'; do
    printf 'package fixture\n' > "$checked_path"
    git --literal-pathspecs add -- "$checked_path"
done
expect_ok 'unusual staged names' git commit --quiet -m 'test: support unusual filenames'
ln -s missing-target symlink.go
git add symlink.go
expect_ok 'staged symlink' git commit --quiet -m 'test: skip staged links'
git rm --quiet -- '-leading.go'
expect_ok 'staged deletion' git commit --quiet -m 'test: allow deleted sources'
pass 'unusual filenames, symlinks, and deletions'

printf 'staged trailing space \n' > whitespace.txt
git add whitespace.txt
printf 'clean working copy\n' > whitespace.txt
expect_failure 'staged whitespace' git commit --quiet -m 'test: reject whitespace'
git reset --quiet -- whitespace.txt
printf '<<<<<<< conflict\n=======\n>>>>>>> conflict\n' > conflict.txt
git add conflict.txt
expect_failure 'staged conflict markers' git commit --quiet -m 'test: reject conflict markers'
git reset --quiet -- conflict.txt
pass 'staged whitespace and conflict markers are rejected'

message="$temporary/message"
for subject in 'feat: add a check' 'fix(parser)!: reject invalid syntax' 'docs(ci/setup): explain hooks' 'revert: restore behavior'; do
    printf '%s\n\nUseful body.\n' "$subject" > "$message"
    expect_ok "valid message $subject" .githooks/commit-msg "$message"
done
printf -v description '%067d' 0
printf 'fix: %s\n' "$description" > "$message"
expect_ok '72 character subject' .githooks/commit-msg "$message"
printf 'fix: %s0\n' "$description" > "$message"
expect_failure '73 character subject' .githooks/commit-msg "$message"
for subject in 'plain subject' 'fix: Uppercase' 'fix: ALLCAPS' 'fix: final period.' 'fix:  doubled space' 'fix: ' 'fix(UPPER): invalid scope'; do
    printf '%s\n' "$subject" > "$message"
    expect_failure "invalid message $subject" .githooks/commit-msg "$message"
done
printf 'fix: missing blank line\nBody\n' > "$message"
expect_failure 'body separator' .githooks/commit-msg "$message"
for prefix in fixup squash amend; do
    printf '%s! Original nonconventional commit %s\nGenerated content\n' "$prefix" "$description" > "$message"
    expect_ok 'generated autosquash message' .githooks/commit-msg "$message"
done
pass 'Conventional Commit grammar, entire subject length, and autosquash exception'

printf 'fix: warn without a sign-off\n\nbody\n' > "$message"
sign_off_warning=$(.githooks/commit-msg "$message" 2>&1 >/dev/null)
[[ "$sign_off_warning" == *'Signed-off-by'* ]] || fail 'commit-msg did not warn about a missing Signed-off-by trailer'
printf 'fix: accept a sign-off\n\nbody\n\nSigned-off-by: Hook Fixture <hooks@example.test>\n' > "$message"
signed_off_output=$(.githooks/commit-msg "$message" 2>&1 >/dev/null)
[[ -z "$signed_off_output" ]] || fail 'commit-msg warned despite a present Signed-off-by trailer'
printf 'fixup! generated message without a sign-off\n' > "$message"
fixup_output=$(.githooks/commit-msg "$message" 2>&1 >/dev/null)
[[ -z "$fixup_output" ]] || fail 'commit-msg warned about a sign-off on an autosquash message'
printf 'fix: reject body text mentioning a sign-off\n\nSigned-off-by mentioned in prose, not as a trailer.\n' > "$message"
body_text_warning=$(.githooks/commit-msg "$message" 2>&1 >/dev/null)
[[ "$body_text_warning" == *'Signed-off-by'* ]] || fail 'commit-msg accepted a Signed-off-by mention in body text as a trailer'
printf 'fix: reject an empty sign-off trailer\n\nbody\n\nSigned-off-by:\n' > "$message"
empty_trailer_warning=$(.githooks/commit-msg "$message" 2>&1 >/dev/null)
[[ "$empty_trailer_warning" == *'Signed-off-by'* ]] || fail 'commit-msg accepted an empty Signed-off-by trailer value'
pass 'missing Signed-off-by trailer warns without rejecting the commit'

# No tests, Git process, or network is required to interpret pre-push records.
object=$(git rev-parse HEAD)
zero=0000000000000000000000000000000000000000
push_record="$temporary/push"
for local_ref in refs/heads/feature refs/heads/main '(delete)'; do
    printf '%s %s refs/heads/main %s\n' "$local_ref" "$object" "$zero" > "$push_record"
    expect_failure 'remote main protected' .githooks/pre-push origin 'unused remote' < "$push_record"
done
printf '(delete) %s refs/heads/main %s\n' "$zero" "$object" > "$push_record"
expect_failure 'main deletion protected' .githooks/pre-push < "$push_record"
printf 'refs/heads/feature %s refs/heads/main %s' "$object" "$zero" > "$push_record"
expect_failure 'unterminated main update' .githooks/pre-push < "$push_record"
printf 'refs/heads/main %s refs/heads/feature %s\nrefs/tags/main %s refs/tags/main %s\n' "$object" "$zero" "$object" "$zero" > "$push_record"
expect_ok 'feature and tag push' .githooks/pre-push < "$push_record"
expect_ok 'empty push' .githooks/pre-push < /dev/null
printf 'refs/heads/feature missing-fields\n' > "$push_record"
expect_failure 'malformed ref input' .githooks/pre-push < "$push_record"
pass 'pre-push rejects only remote main updates and deletion'

# Git invokes the same tracked hooks in a linked worktree. Local extensions
# remain in the common Git directory, not the per-worktree administrative path.
linked="$temporary/linked worktree"
git worktree add --quiet -b linked "$linked"
cd "$linked"
expect_ok 'linked worktree install' make hooks
common_dir=$(git rev-parse --git-common-dir)
mkdir -p "$common_dir/hooks/commit-msg.d"
cat > "$common_dir/hooks/commit-msg.d/check message" <<'HOOK'
#!/usr/bin/env bash
[[ -f "$1" ]] || exit 9
if grep -q 'reject-local' "$1"; then exit 1; fi
HOOK
chmod +x "$common_dir/hooks/commit-msg.d/check message"
printf 'fix: reject-local extension\n' > "$message"
expect_failure 'common local extension' .githooks/commit-msg "$message"
printf 'fixup! reject-local extension\n' > "$message"
expect_failure 'autosquash local extension' .githooks/commit-msg "$message"
expect_ok 'linked real commit' git commit --quiet --allow-empty -m 'test: exercise linked hooks'
printf 'fix: missing blank line\nBody\n' > "$message"
expect_failure 'linked real commit message' git commit --quiet --allow-empty -F "$message"
pass 'linked worktrees share installation and optional local extensions'

# changelog-check: a cmd/ or internal/ change needs an Unreleased line or an
# explicit changelog: none trailer.
changelog_base=$(git rev-parse HEAD)
mkdir -p cmd
printf 'package main\n' > cmd/example.go
git add cmd/example.go
expect_ok 'runner code commit' git commit --quiet -m 'feat: add example command'
touched_head=$(git rev-parse HEAD)
expect_failure 'code change without changelog' bash tools/changelog-check.sh "$changelog_base" "$touched_head"

git checkout --quiet -b with-changelog "$changelog_base"
mkdir -p cmd
printf 'package main\n' > cmd/example.go
printf '# Changelog\n\n## Unreleased\n\n### Added\n\n- Add example command.\n' > CHANGELOG.md
git add cmd/example.go CHANGELOG.md
expect_ok 'runner code with changelog commit' git commit --quiet -m 'feat: add example command'
expect_ok 'code change with changelog' bash tools/changelog-check.sh "$changelog_base" HEAD

git checkout --quiet -b with-trailer "$changelog_base"
mkdir -p cmd
printf 'package main\n' > cmd/example.go
git add cmd/example.go
expect_ok 'runner code with trailer commit' git commit --quiet -m "$(printf 'refactor: rename internal helper\n\nchangelog: none\n')"
expect_ok 'code change with trailer' bash tools/changelog-check.sh "$changelog_base" HEAD

git checkout --quiet -b docs-only "$changelog_base"
mkdir -p docs
printf '# Notes\n' > docs/notes.md
git add docs/notes.md
expect_ok 'docs-only commit' git commit --quiet -m 'docs: add notes'
expect_ok 'docs-only change' bash tools/changelog-check.sh "$changelog_base" HEAD
pass 'changelog-check enforces an Unreleased line or a changelog: none trailer'

# pre-push wiring: a brand-new branch (no remote object) resolves its base
# through origin/main, matching the first push of a feature branch.
git remote add origin "$fixture"
git fetch --quiet origin main
push_record_changelog="$temporary/push-changelog"
printf 'refs/heads/feature %s refs/heads/feature %s\n' "$touched_head" "$zero" > "$push_record_changelog"
expect_failure 'pre-push blocks code without changelog' .githooks/pre-push < "$push_record_changelog"
with_changelog_head=$(git rev-parse with-changelog)
printf 'refs/heads/feature %s refs/heads/feature %s\n' "$with_changelog_head" "$zero" > "$push_record_changelog"
expect_ok 'pre-push allows code with changelog' .githooks/pre-push < "$push_record_changelog"
git checkout --quiet linked
pass 'pre-push wires changelog-check for a new branch push'
