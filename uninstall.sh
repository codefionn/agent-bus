#!/usr/bin/env sh
# Remove agent-bus from ~/.local/bin and unwire every harness it found: Claude
# Code and Codex hooks, the pi extension, opencode and Smelt plugins, and the
# "Agent bus" section in each harness's global instructions. Safe to rerun.
set -eu
cd "$(dirname "$0")"
go build -o agent-bus ./cmd/agent-bus
./agent-bus uninstall
