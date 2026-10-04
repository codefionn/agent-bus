# Build agent-bus, copy it to ~/.local/bin and wire it into every harness found:
# Claude Code and Codex hooks, a pi extension, an opencode plugin, and the
# "Agent bus" section in each harness's global instructions. Safe to rerun.
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
go build -o agent-bus.exe ./cmd/agent-bus
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go build -o agent-bus-web.exe ./cmd/agent-bus-web
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& .\agent-bus.exe install
exit $LASTEXITCODE
