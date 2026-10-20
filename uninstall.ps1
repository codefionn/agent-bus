# Remove agent-bus from ~/.local/bin and unwire every harness it found: Claude
# Code and Codex hooks, the pi extension, opencode and Smelt plugins, and the
# "Agent bus" section in each harness's global instructions. Safe to rerun.
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
go build -o agent-bus.exe ./cmd/agent-bus
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& .\agent-bus.exe uninstall
exit $LASTEXITCODE
