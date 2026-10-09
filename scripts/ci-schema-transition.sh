#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Immutable tested schema-zero fixture; never select a moving branch or tag.
old_revision=d85d76626f72a9f642363a2eaa34a853a792573d
fixture=$(mktemp -d /tmp/titanus-schema.XXXXXX)
trap 'rm -rf "$fixture"' EXIT
git cat-file -e "$old_revision^{commit}" 2>/dev/null || git fetch --no-tags --depth=1 origin "$old_revision"
git archive "$old_revision" | tar -x -C "$fixture"
(cd "$fixture" && CGO_ENABLED=0 go build -ldflags "-X github.com/antonismor/Titanus-Core/internal/version.Revision=$old_revision" -o "$fixture/legacy-titanusd" ./cmd/titanusd)
make build
TITANUS_SCHEMA_NATIVE_TEST=1 TITANUS_DAEMON_BINARY="$PWD/bin/titanusd" TITANUS_SCHEMA_OLD_BINARY="$fixture/legacy-titanusd" TITANUS_SCHEMA_OLD_REVISION="$old_revision" go test ./cmd/titanusd -run '^TestNativeSchemaTransition$' -count=1 -v -timeout 180s
