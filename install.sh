#!/usr/bin/env sh
# Build agent-bus, copy it to ~/.local/bin and wire it into every harness found:
# Claude Code and Codex hooks, a pi extension, an opencode plugin, and the
# "Agent bus" section in each harness's global instructions. Safe to rerun.
set -eu
cd "$(dirname "$0")"
go build -o agent-bus ./cmd/agent-bus
go build -o agent-bus-web ./cmd/agent-bus-web
./agent-bus install
