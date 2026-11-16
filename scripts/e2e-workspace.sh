#!/usr/bin/env bash
# End-to-end check of the workspace commands on an isolated tmux server.
# Never touches your default tmux server (see docs/guide/workspace.md).
set -euo pipefail
cd "$(dirname "$0")/.."

go build ./...
go test -tags e2e -count=1 ./internal/e2e/

cat <<'MSG'

Try it by hand beside your own tmux (separate server "try"):

  mkdir -p /tmp/demo
  cairn project add demo --root /tmp/demo
  cairn --socket try up
  cairn --socket try open demo

Clean up:

  tmux -L try kill-server
  cairn project archive demo
  cairn project rm demo
MSG
