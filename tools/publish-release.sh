#!/usr/bin/env bash
set -euo pipefail

output=${1:?Package directory is required}
: "${RELEASE_TAG:?Release tag is required}"
: "${RELEASE_ID:?Release id is required}"
: "${GITHUB_REPOSITORY:?GitHub repository is required}"
[[ "$RELEASE_ID" =~ ^[0-9]+$ ]]
[[ "$RELEASE_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]
[[ "$RELEASE_TAG" == "$(jq -r .version "$output/runner-release.json")" ]]
release=$(gh api "repos/$GITHUB_REPOSITORY/releases/$RELEASE_ID")
[[ "$(jq -r .tag_name <<<"$release")" == "$RELEASE_TAG" ]]
[[ "$(jq -r .draft <<<"$release")" == false ]]

for name in "shipmunk-runner-$RELEASE_TAG.tar" installer.php runner-release.json SHA256SUMS; do
    artifact="$output/$name"
    digest="sha256:$(sha256sum "$artifact" | cut -d ' ' -f 1)"
    assets=$(gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/releases/$RELEASE_ID/assets?per_page=100" | jq 'add')
    matches=$(jq --arg name "$name" '[.[] | select(.name == $name)]' <<<"$assets")
    count=$(jq length <<<"$matches")
    if [[ "$count" == 0 ]]; then
        gh release upload "$RELEASE_TAG" "$artifact" --repo "$GITHUB_REPOSITORY"
        assets=$(gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/releases/$RELEASE_ID/assets?per_page=100" | jq 'add')
        matches=$(jq --arg name "$name" '[.[] | select(.name == $name)]' <<<"$assets")
    fi
    [[ "$(jq length <<<"$matches")" == 1 ]]
    if [[ "$(jq -r '.[0].digest' <<<"$matches")" != "$digest" ]]; then
        printf 'Release asset %s differs from the checked tag. Refusing to overwrite it.\n' "$name" >&2
        exit 1
    fi
done
