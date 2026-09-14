#!/usr/bin/env bash
set -euo pipefail

output=${1:?Package directory is required}
: "${RELEASE_TAG:?Release tag is required}"
: "${RELEASE_ID:?Release id is required}"
: "${GITHUB_REPOSITORY:?GitHub repository is required}"
[[ "$RELEASE_ID" =~ ^[0-9]+$ ]]
[[ "$RELEASE_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]
[[ -d "$output" && ! -L "$output" ]]

manifest=runner-release.json
checksums=SHA256SUMS
[[ -f "$output/$manifest" && ! -L "$output/$manifest" ]]
[[ -f "$output/$checksums" && ! -L "$output/$checksums" ]]
[[ "$RELEASE_TAG" == "$(jq -er '.version | select(type == "string")' "$output/$manifest")" ]]

declare -a assets=()
declare -a digests=()
manifest_present=false
contains_asset() {
    local wanted=$1 existing
    for existing in "${assets[@]-}"; do
        [[ "$existing" == "$wanted" ]] && return 0
    done
    return 1
}
while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ ! "$line" =~ ^([a-f0-9]{64})\ \ ([A-Za-z0-9][A-Za-z0-9._+-]*)$ ]]; then
        printf 'Invalid SHA256SUMS entry.\n' >&2
        exit 1
    fi
    digest=${BASH_REMATCH[1]}
    name=${BASH_REMATCH[2]}
    if contains_asset "$name" || [[ "$name" == "$checksums" ]]; then
        printf 'Duplicate or recursive release asset %s.\n' "$name" >&2
        exit 1
    fi
    artifact="$output/$name"
    if [[ ! -f "$artifact" || -L "$artifact" || "$(sha256sum "$artifact" | cut -d ' ' -f 1)" != "$digest" ]]; then
        printf 'Release asset %s does not match SHA256SUMS.\n' "$name" >&2
        exit 1
    fi
    assets+=("$name")
    digests+=("$digest")
    [[ "$name" == "$manifest" ]] && manifest_present=true
done < "$output/$checksums"

if [[ ${#assets[@]} -eq 0 || "$manifest_present" != true ]]; then
    printf 'Release inventory must include %s.\n' "$manifest" >&2
    exit 1
fi

while IFS= read -r -d '' artifact; do
    name=${artifact#"$output"/}
    if [[ ! -f "$artifact" || -L "$artifact" ]]; then
        printf 'Release output %s is not a regular file.\n' "$name" >&2
        exit 1
    fi
    if [[ "$name" != "$checksums" ]] && ! contains_asset "$name"; then
        printf 'Unlisted release asset %s.\n' "$name" >&2
        exit 1
    fi
done < <(find "$output" -mindepth 1 -maxdepth 1 -print0)

release=$(gh api "repos/$GITHUB_REPOSITORY/releases/$RELEASE_ID")
[[ "$(jq -r .tag_name <<<"$release")" == "$RELEASE_TAG" ]]
[[ "$(jq -r .draft <<<"$release")" == false ]]

remote_assets() {
    gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/releases/$RELEASE_ID/assets?per_page=100" | jq 'add // []'
}

publish_asset() {
    local name=$1 expected_digest=$2 artifact digest matches count assets_json
    artifact="$output/$name"
    digest="sha256:$expected_digest"
    assets_json=$(remote_assets)
    matches=$(jq --arg name "$name" '[.[] | select(.name == $name)]' <<<"$assets_json")
    count=$(jq length <<<"$matches")
    if [[ "$count" == 0 ]]; then
        gh release upload "$RELEASE_TAG" "$artifact" --repo "$GITHUB_REPOSITORY"
        assets_json=$(remote_assets)
        matches=$(jq --arg name "$name" '[.[] | select(.name == $name)]' <<<"$assets_json")
        count=$(jq length <<<"$matches")
    fi
    if [[ "$count" != 1 ]]; then
        printf 'Release asset %s must exist exactly once.\n' "$name" >&2
        exit 1
    fi
    if [[ "$(jq -r '.[0].digest // empty' <<<"$matches")" != "$digest" ]]; then
        printf 'Release asset %s differs from the checked tag. Refusing to overwrite it.\n' "$name" >&2
        exit 1
    fi
}

for index in "${!assets[@]}"; do
    name=${assets[$index]}
    [[ "$name" == "$manifest" ]] || publish_asset "$name" "${digests[$index]}"
done
publish_asset "$checksums" "$(sha256sum "$output/$checksums" | cut -d ' ' -f 1)"
for index in "${!assets[@]}"; do
    if [[ "${assets[$index]}" == "$manifest" ]]; then
        publish_asset "$manifest" "${digests[$index]}"
    fi
done
