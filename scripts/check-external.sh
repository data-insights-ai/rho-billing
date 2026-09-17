#!/bin/sh
# Compile independent consumers without a workspace granting accidental access.
set -eu
cd "$(dirname "$0")/.."
for module in testdata/adapter testdata/host; do
 (cd "$module" && GOWORK=off go test ./...)
done

