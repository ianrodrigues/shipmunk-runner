#!/usr/bin/env bash
set -euo pipefail

repository=$(cd "$(dirname "$0")/.." && pwd)
# Tests may themselves run from a hook; never inherit its real index/worktree.
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
mkdir .githooks
cp "$repository"/.githooks/* .githooks/
cp "$repository/Makefile" Makefile

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

# Installation must distinguish an absent setting from an unreadable config.
printf '[broken config\n' > "$temporary/broken-config"
expect_failure 'invalid global config' env GIT_CONFIG_GLOBAL="$temporary/broken-config" make hooks
if git config --local --get core.hooksPath > /dev/null; then fail 'config error changed the local hook path'; fi

# Installation must preserve both an explicit hook path and default local hooks.
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

git add .githooks Makefile
expect_ok 'initial real commit' git commit --quiet -m 'test: install fixture hooks'

# Reject invalid staged Go despite a valid working copy, then invert the two.
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

checked_path='PHP with spaces.php'
printf '<?php function broken( {\n' > "$checked_path"
git add -- "$checked_path"
printf '<?php echo "valid";\n' > "$checked_path"
staged_tree=$(git write-tree); worktree_hash=$(git hash-object -- "$checked_path")
expect_failure 'staged PHP syntax' git commit --quiet -m 'test: reject staged PHP'
assert_unchanged
git add -- "$checked_path"
printf '<?php broken(\n' > "$checked_path"
staged_tree=$(git write-tree); worktree_hash=$(git hash-object -- "$checked_path")
expect_ok 'valid staged PHP with invalid working copy' git commit --quiet -m 'test: lint staged PHP only'
assert_unchanged
pass 'PHP checks inspect staged bytes and preserve partially staged files'

# Newlines, leading dashes, and pathspec metacharacters remain literal filenames.
for checked_path in $'odd\nname.go' '-leading.php' 'literal[1].go' ':1:literal.go'; do
    case "$checked_path" in *.go) printf 'package fixture\n' > "$checked_path" ;; *) printf '<?php echo "ok";\n' > "$checked_path" ;; esac
    git --literal-pathspecs add -- "$checked_path"
done
expect_ok 'unusual staged names' git commit --quiet -m 'test: support unusual filenames'
mkdir -p runner/bin
printf '<?php broken(\n' > runner/bin/shipmunk-runner
git add runner/bin/shipmunk-runner
expect_failure 'extensionless PHP entrypoint' .githooks/pre-commit
git reset --quiet -- runner/bin/shipmunk-runner
rm runner/bin/shipmunk-runner
ln -s missing-target symlink.go
git add symlink.go
expect_ok 'staged symlink' git commit --quiet -m 'test: skip staged links'
git rm --quiet -- '-leading.php'
expect_ok 'staged deletion' git commit --quiet -m 'test: allow deleted sources'
pass 'unusual filenames, PHP entrypoints, symlinks, and deletions'

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

# Git invokes the same tracked hooks in a linked worktree and local extensions
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
