#!/usr/bin/env bash
# End-to-end check of the workspace commands on an isolated tmux server.
# Never touches your default tmux server (see docs/guide/workspace.md).
set -euo pipefail
cd "$(dirname "$0")/.."

bin_dir=$(mktemp -d)
trap 'rm -rf "$bin_dir"' EXIT
go build -o "$bin_dir/cairn" ./cmd/cairn
go test -tags e2e -count=1 ./internal/e2e/

cat <<MSG

Try it by hand beside your own tmux (separate server "try"). The binary built
above is deleted on exit; use your own cairn, or rebuild: go build -o /tmp/cairn ./cmd/cairn

  mkdir -p /tmp/cairn-try
  cairn project add cairn-try --root /tmp/cairn-try
  cairn --socket try up
  cairn --socket try open cairn-try

Clean up:

  tmux -L try kill-server
  cairn project archive cairn-try
  cairn project rm cairn-try --yes
MSG
