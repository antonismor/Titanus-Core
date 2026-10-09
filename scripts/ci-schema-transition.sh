#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Immutable tested schema-zero fixture; never select a moving branch or tag.
old_revision=d85d76626f72a9f642363a2eaa34a853a792573d
fixture=$(mktemp -d /tmp/titanus-schema.XXXXXX)
trap 'rm -rf "$fixture"' EXIT
git cat-file -e "$old_revision^{commit}" 2>/dev/null || git fetch --no-tags --depth=1 origin "$old_revision"
git worktree add --quiet --detach "$fixture/old" "$old_revision"
(cd "$fixture/old" && make build && TITANUS_RELEASE_OUTPUT="$fixture/old-package" bash scripts/package-release.sh)
make build
schema_one_binary=""
if ./bin/titanus --capabilities-json | python3 -c 'import json,sys;sys.exit(0 if json.load(sys.stdin)["read_schema_max"]>=2 else 1)'; then
 schema_one_revision=547468104002ceea3dbf77f3c1d9716fe153849c
 git cat-file -e "$schema_one_revision^{commit}" 2>/dev/null || git fetch --no-tags --depth=1 origin "$schema_one_revision"
 git worktree add --quiet --detach "$fixture/schema-one" "$schema_one_revision"
 (cd "$fixture/schema-one" && make build && TITANUS_RELEASE_OUTPUT="$fixture/schema-one-package" bash scripts/package-release.sh)
 schema_one_binary="$fixture/schema-one/bin/titanusd"
fi
TITANUS_SCHEMA_ONE_BINARY="$schema_one_binary" TITANUS_SCHEMA_NATIVE_TEST=1 TITANUS_DAEMON_BINARY="$PWD/bin/titanusd" TITANUS_SCHEMA_OLD_BINARY="$fixture/old/bin/titanusd" TITANUS_SCHEMA_OLD_REVISION="$old_revision" go test ./cmd/titanusd -run '^TestNativeSchemaTransition$' -count=1 -v -timeout 180s
# Exercise the actual versioned installer with verified old/new native bundles.
TITANUS_RELEASE_OUTPUT="$fixture/new-package" bash scripts/package-release.sh
for kind in old new; do
 tar -xzf "$(find "$fixture/$kind-package" -maxdepth 1 -name '*.tar.gz' -print -quit)" -C "$fixture/$kind-package"
done
old_bundle=$(find "$fixture/old-package" -mindepth 1 -maxdepth 1 -type d -print -quit)
new_bundle=$(find "$fixture/new-package" -mindepth 1 -maxdepth 1 -type d -print -quit)
stage="$fixture/install"
python3 scripts/install-release.py --root "$stage" --bundle "$old_bundle"
python3 scripts/install-release.py --root "$stage" --bundle "$new_bundle"
python3 scripts/install-release.py --root "$stage" --rollback
python3 scripts/install-release.py --root "$stage" --bundle "$new_bundle"
python3 - "$stage/var/lib/titanus" <<'FLOOR'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1]);(root/'compatibility').mkdir()
floor={'format':'titanus-schema-floor/v1','realm':'INSTALL-LAB','migration_id':'native-installer-floor-001','schema':1}
for path in [root/'compatibility/schema-floor.json',pathlib.Path(str(root)+'.recovery-pending')]:
 path.write_text(json.dumps(floor));path.chmod(0o600)
FLOOR
selection=$(readlink "$stage/usr/local/lib/titanus/current")
if python3 scripts/install-release.py --root "$stage" --rollback >"$fixture/rollback.log" 2>&1; then
 echo 'Legacy schema-one binary rollback accepted' >&2; exit 1
fi
grep -q 'binary rollback lacks advertised schema support' "$fixture/rollback.log"
test "$(readlink "$stage/usr/local/lib/titanus/current")" = "$selection"
if python3 scripts/install-release.py --root "$stage" --bundle "$old_bundle" >"$fixture/old-install.log" 2>&1; then
 echo 'Legacy reinstall bypassed schema floor' >&2; exit 1
fi
grep -q 'binary rollback lacks advertised schema support' "$fixture/old-install.log"
test "$(readlink "$stage/usr/local/lib/titanus/current")" = "$selection"
python3 scripts/install-release.py --root "$stage" --bundle "$new_bundle"
echo TITANUS_NATIVE_SCHEMA_INSTALLER_ROLLBACK_GATES_OK

if [[ -n "$schema_one_binary" ]]; then
 tar -xzf "$(find "$fixture/schema-one-package" -maxdepth 1 -name '*.tar.gz' -print -quit)" -C "$fixture/schema-one-package"
 one_bundle=$(find "$fixture/schema-one-package" -mindepth 1 -maxdepth 1 -type d -print -quit)
 python3 scripts/install-release.py --root "$stage" --bundle "$one_bundle"
 python3 scripts/install-release.py --root "$stage" --bundle "$new_bundle"
 python3 - "$stage/var/lib/titanus" <<'FLOORTWO'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1]);floor={'format':'titanus-schema-floor/v1','realm':'INSTALL-LAB','migration_id':'native-installer-floor-two-001','schema':2}
for path in [root/'compatibility/schema-floor.json',pathlib.Path(str(root)+'.recovery-pending')]:
 path.write_text(json.dumps(floor));path.chmod(0o600)
FLOORTWO
 selection=$(readlink "$stage/usr/local/lib/titanus/current")
 if python3 scripts/install-release.py --root "$stage" --rollback >"$fixture/schema-two-rollback.log" 2>&1; then
  echo 'Schema-one rollback admitted migrated schema-two data' >&2; exit 1
 fi
 grep -q 'binary is incompatible with prepared data schema' "$fixture/schema-two-rollback.log"
 test "$(readlink "$stage/usr/local/lib/titanus/current")" = "$selection"
 echo TITANUS_NATIVE_SCHEMA_TWO_INSTALLER_ROLLBACK_GATE_OK
fi
