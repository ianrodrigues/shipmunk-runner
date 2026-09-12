#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir "$fixture/bin" "$fixture/dist"
export RELEASE_TAG=v0.2.0-alpha.1 RELEASE_ID=123 GITHUB_REPOSITORY=ianrodrigues/shipmunk-runner
export GH_FAKE_STATE="$fixture/state.json"
for name in "shipmunk-runner-$RELEASE_TAG.tar" installer.php runner-release.json SHA256SUMS; do
    printf 'SYNTHETIC_RELEASE_%s\n' "$name" > "$fixture/dist/$name"
done
printf '{"version":"%s"}\n' "$RELEASE_TAG" > "$fixture/dist/runner-release.json"
cat > "$fixture/bin/gh" <<'PY'
#!/usr/bin/env python3
import hashlib,json,os,sys
from pathlib import Path
state_path=Path(os.environ['GH_FAKE_STATE'])
state=json.loads(state_path.read_text())
args=sys.argv[1:]
if args[0]=='api':
    if '/assets?' in args[1]:print(json.dumps(state['assets']))
    else:print(json.dumps({'tag_name':os.environ['RELEASE_TAG'],'draft':False}))
elif args[:2]==['release','upload']:
    artifact=Path(args[3])
    if any(asset['name']==artifact.name for asset in state['assets']):sys.exit(1)
    state['assets'].append({'name':artifact.name,'digest':'sha256:'+hashlib.sha256(artifact.read_bytes()).hexdigest()})
    state['uploads']+=1
    state_path.write_text(json.dumps(state))
else:sys.exit(2)
PY
chmod +x "$fixture/bin/gh"
export PATH="$fixture/bin:$PATH"
printf '{"assets":[],"uploads":0}\n' > "$GH_FAKE_STATE"
cd "$root"
bash tools/publish-release.sh "$fixture/dist"
[[ "$(jq .uploads "$GH_FAKE_STATE")" == 4 ]]
bash tools/publish-release.sh "$fixture/dist"
[[ "$(jq .uploads "$GH_FAKE_STATE")" == 4 ]]
# Resume a partial upload without replacing the first matching asset.
jq '.assets = [.assets[0]] | .uploads = 1' "$GH_FAKE_STATE" > "$fixture/partial.json"
mv "$fixture/partial.json" "$GH_FAKE_STATE"
bash tools/publish-release.sh "$fixture/dist"
[[ "$(jq .uploads "$GH_FAKE_STATE")" == 4 ]]
# A changed existing asset must stop the release rather than overwrite it.
jq '.assets[0].digest = "sha256:changed"' "$GH_FAKE_STATE" > "$fixture/changed.json"
mv "$fixture/changed.json" "$GH_FAKE_STATE"
if bash tools/publish-release.sh "$fixture/dist" > "$fixture/output" 2>&1; then
    echo 'Expected mismatched existing asset to be rejected.' >&2
    exit 1
fi
[[ "$(jq .uploads "$GH_FAKE_STATE")" == 4 ]]
printf 'PASS release upload verifies new assets, resumes partial uploads and refuses changed existing assets\n'
