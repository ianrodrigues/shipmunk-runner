#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir "$fixture/bin" "$fixture/dist"
export RELEASE_TAG=v0.2.0-alpha.1 RELEASE_ID=123 GITHUB_REPOSITORY=ianrodrigues/shipmunk-runner
export GH_FAKE_STATE="$fixture/state.json"
names=(
    "shipmunk-runner-$RELEASE_TAG-linux-amd64.tar"
    "shipmunk-runner-$RELEASE_TAG-linux-arm64.tar"
    "shipmunk-runner-$RELEASE_TAG-darwin-amd64.tar"
    "shipmunk-runner-$RELEASE_TAG-darwin-arm64.tar"
    runner-release.json
)
for name in "${names[@]}"; do
    printf 'SYNTHETIC_RELEASE_%s\n' "$name" > "$fixture/dist/$name"
done
printf '{"version":"%s"}\n' "$RELEASE_TAG" > "$fixture/dist/runner-release.json"
(cd "$fixture/dist" && sha256sum "${names[@]}" > SHA256SUMS)
cat > "$fixture/bin/gh" <<'PY'
#!/usr/bin/env python3
import hashlib,json,os,sys
from pathlib import Path
state_path=Path(os.environ['GH_FAKE_STATE'])
state=json.loads(state_path.read_text())
args=sys.argv[1:]
if args[0]=='api':
    if any('/assets?' in arg for arg in args):
        pages=[state['assets'][i:i+100] for i in range(0,len(state['assets']),100)] or [[]]
        print(json.dumps(pages if '--paginate' in args and '--slurp' in args else pages[0]))
    else:print(json.dumps({'tag_name':os.environ['RELEASE_TAG'],'draft':False}))
elif args[:2]==['release','upload']:
    artifact=Path(args[3])
    if state.get('fail_name') == artifact.name:
        sys.exit(1)
    if any(asset['name']==artifact.name for asset in state['assets']):sys.exit(1)
    state['assets'].append({'name':artifact.name,'digest':'sha256:'+hashlib.sha256(artifact.read_bytes()).hexdigest()})
    state['uploads'].append(artifact.name)
    state_path.write_text(json.dumps(state))
else:sys.exit(2)
PY
chmod +x "$fixture/bin/gh"
export PATH="$fixture/bin:$PATH"
printf '{"assets":[],"uploads":[]}' > "$GH_FAKE_STATE"
cd "$root"
bash tools/publish-release.sh "$fixture/dist"
[[ "$(jq '.uploads | length' "$GH_FAKE_STATE")" == 6 ]]
[[ "$(jq -r '.uploads[-2:] | join(" ")' "$GH_FAKE_STATE")" == "SHA256SUMS runner-release.json" ]]
bash tools/publish-release.sh "$fixture/dist"
[[ "$(jq '.uploads | length' "$GH_FAKE_STATE")" == 6 ]]

printf '{"assets":[],"uploads":[],"fail_name":"%s"}' "${names[2]}" > "$GH_FAKE_STATE"
if bash tools/publish-release.sh "$fixture/dist" >/dev/null 2>&1; then
    echo 'Expected a partial upload failure.' >&2
    exit 1
fi
jq 'del(.fail_name)' "$GH_FAKE_STATE" > "$fixture/resume.json"
mv "$fixture/resume.json" "$GH_FAKE_STATE"
bash tools/publish-release.sh "$fixture/dist"
[[ "$(jq '.uploads | length' "$GH_FAKE_STATE")" == 6 ]]
[[ "$(jq -r '.uploads[-1]' "$GH_FAKE_STATE")" == runner-release.json ]]

jq '.assets = ([range(0;101) | {name:("unrelated-" + tostring),digest:"sha256:unrelated"}] + .assets)' "$GH_FAKE_STATE" > "$fixture/paginated.json"
cp "$fixture/paginated.json" "$GH_FAKE_STATE"
bash tools/publish-release.sh "$fixture/dist"
[[ "$(jq '.uploads | length' "$GH_FAKE_STATE")" == 6 ]]

expect_rejection() {
    if bash tools/publish-release.sh "$fixture/dist" > "$fixture/output" 2>&1; then
        echo "Expected release state to be rejected: $1" >&2
        exit 1
    fi
}

jq --arg name "${names[0]}" '(.assets[] | select(.name == $name).digest) = "sha256:changed"' "$GH_FAKE_STATE" > "$fixture/changed.json"
cp "$fixture/changed.json" "$GH_FAKE_STATE"
expect_rejection changed
jq --arg name "${names[0]}" '(.assets[] | select(.name == $name).digest) = null' "$fixture/paginated.json" > "$GH_FAKE_STATE"
expect_rejection null-digest
jq --arg name "${names[0]}" '.assets += [.assets[] | select(.name == $name)]' "$fixture/paginated.json" > "$GH_FAKE_STATE"
expect_rejection duplicate

cp "$fixture/paginated.json" "$GH_FAKE_STATE"
printf 'unlisted\n' > "$fixture/dist/unlisted.bin"
expect_rejection unlisted
rm "$fixture/dist/unlisted.bin"
cp "$fixture/dist/${names[0]}" "$fixture/original-asset"
printf 'tampered\n' > "$fixture/dist/${names[0]}"
expect_rejection checksum
mv "$fixture/original-asset" "$fixture/dist/${names[0]}"

printf 'PASS release upload publishes the platform inventory manifest-last, resumes safely, and rejects ambiguous or changed assets\n'
