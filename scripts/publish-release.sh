#!/usr/bin/env bash
set -euo pipefail
python3 - <<'PY'
import hashlib,json,os,pathlib,re,tarfile
expected=os.environ['TITANUS_VERIFIED_SHA'];assert re.fullmatch('[0-9a-f]{40}',expected)
archives=sorted(pathlib.Path('release-packages').glob('*.tar.gz'));assert len(archives)==2
versions=set();arches=set()
source=pathlib.Path('internal/version/version.go').read_text()
declared=re.search(r'^var Version = "([^"]+)"',source,re.M).group(1)
profile=re.search(r'^const StateProfile = "([^"]+)"',source,re.M).group(1)
for archive in archives:
 line=archive.with_name(archive.name+'.sha256').read_text().strip();digest,name=line.split('  ');assert name==archive.name and hashlib.sha256(archive.read_bytes()).hexdigest()==digest
 with tarfile.open(archive) as tar:
  manifests=[m for m in tar.getmembers() if m.name.endswith('/manifest.json')];assert len(manifests)==1 and manifests[0].isfile()
  meta=json.load(tar.extractfile(manifests[0]));assert meta['revision']==expected and meta['os']=='linux' and meta['state_profile']==profile
  assert meta['version']==declared and re.fullmatch(r'\d+\.\d+\.\d+-[a-z0-9.-]+',meta['version']);assert archive.name==f"titanus-{meta['version']}-linux-{meta['arch']}.tar.gz";versions.add(meta['version']);arches.add(meta['arch'])
assert len(versions)==1 and arches=={'amd64','arm64'}
pathlib.Path('release-version.txt').write_text(versions.pop())
PY
version=$(cat release-version.txt)
python3 scripts/verify-release.py --revision "$TITANUS_VERIFIED_SHA" --version "$version" release-packages/*.tar.gz
tag="v$version"
repo="$GITHUB_REPOSITORY"
# Draft releases may not materialize their tag until publication. Create and
# verify the immutable lightweight tag first, without moving an existing tag.
refs=$(gh api "repos/$repo/git/matching-refs/tags/$tag")
exists=$(printf '%s' "$refs" | python3 -c 'import json,sys; print(any(r["ref"]=="refs/tags/"+sys.argv[1] for r in json.load(sys.stdin)))' "$tag")
if [[ "$exists" == False ]]; then
 gh api --method POST "repos/$repo/git/refs" -f "ref=refs/tags/$tag" -f "sha=$TITANUS_VERIFIED_SHA" >/dev/null
fi
actual=$(gh api "repos/$repo/commits/$tag" --jq .sha)
if [[ "$actual" != "$TITANUS_VERIFIED_SHA" ]]; then echo 'Existing version tag points to a different revision; refusing replacement.' >&2; exit 1; fi
if gh release view "$tag" --repo "$repo" --json tagName,isDraft >/dev/null 2>&1; then
 actual=$(gh api "repos/$repo/commits/$tag" --jq .sha)
 if [[ "$actual" != "$TITANUS_VERIFIED_SHA" ]]; then echo 'Existing version points to a different revision; refusing replacement.' >&2; exit 1; fi
 mkdir -p release-existing
 gh release download "$tag" --repo "$repo" --dir release-existing
 for file in release-packages/*; do
  name=$(basename "$file")
  if [[ -f "release-existing/$name" ]]; then cmp "$file" "release-existing/$name"; else gh release upload "$tag" "$file" --repo "$repo"; fi
 done
else
 cat > release-notes.txt <<NOTES
Native Linux AMD64/ARM64 release candidate from verified main revision $TITANUS_VERIFIED_SHA.

All native runtime, isolation, HA, multi-node transport/failure, live install/upgrade/binary rollback and real Ceph tests passed in https://github.com/$repo/actions/runs/$TITANUS_VERIFIED_RUN .

See docs/RELEASE.md and subsystem documents for precise limits, dedicated-node prerequisites, state/key backups and opt-in storage fencing/failover boundaries. This is a release candidate; physical host/site acceptance and installation on user servers are not implied.
NOTES
 gh release create "$tag" release-packages/* --repo "$repo" --target "$TITANUS_VERIFIED_SHA" --draft --prerelease --title "Titanus Core $version" --notes-file release-notes.txt
fi
actual=$(gh api "repos/$repo/commits/$tag" --jq .sha)
test "$actual" = "$TITANUS_VERIFIED_SHA"
gh release edit "$tag" --repo "$repo" --draft=false --prerelease
