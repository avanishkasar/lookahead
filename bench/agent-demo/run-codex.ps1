#Requires -Version 7.0
<#
.SYNOPSIS
Runs the agent-demo task in a headless Codex session (codex exec) with the lookahead MCP server
attached, and saves the JSONL event stream next to the fan-out result.

.DESCRIPTION
The MCP server is passed as -c overrides, so ~/.codex/config.toml is not changed. The task does
not mention lookahead or hints.

On Windows, Codex's workspace-write sandbox cannot read ~/.kube/config, so the fan-out cannot
reach the cluster; -Sandbox danger-full-access runs shell commands unsandboxed instead.
#>
[CmdletBinding()]
param(
    [string]$Label = 'codex',
    [string]$Namespace = 'lookahead-bench',
    # Path to the codex CLI; defaults to the one on PATH.
    [string]$Codex = 'codex',
    [ValidateSet('workspace-write', 'danger-full-access')]
    [string]$Sandbox = 'workspace-write'
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repo = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$outDir = Join-Path $repo 'bench/results/agent-demo'
New-Item -ItemType Directory -Force $outDir | Out-Null
$server = (Get-Command lookahead).Source -replace '\\', '/'
$task = (Get-Content (Join-Path $PSScriptRoot 'task.txt') -Raw).Replace('{label}', $Label).Trim()

Push-Location $repo
try {
    # Empty stdin: codex exec otherwise waits for more prompt input when stdin is not a terminal.
    '' | & $Codex exec --json --sandbox $Sandbox `
        -c 'sandbox_workspace_write.network_access=true' `
        -c "mcp_servers.lookahead.command=`"$server`"" `
        -c "mcp_servers.lookahead.args=[`"mcp`", `"-n`", `"$Namespace`"]" `
        $task | Set-Content (Join-Path $outDir "$Label.session.jsonl")
} finally { Pop-Location }
"Transcript: $(Join-Path $outDir "$Label.session.jsonl")"
