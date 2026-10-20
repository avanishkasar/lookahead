#Requires -Version 7.0
<#
.SYNOPSIS
Runs the agent-demo task in a headless Claude Code session with the lookahead MCP server attached,
and saves the session transcript (stream-json) next to the fan-out result.

.DESCRIPTION
The MCP server is passed with --mcp-config/--strict-mcp-config, so the user's own Claude Code
configuration is neither used nor changed. The task does not mention lookahead or hints.
#>
[CmdletBinding()]
param(
    [string]$Label = 'claude-code',
    [string]$Namespace = 'lookahead-bench'
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repo = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$outDir = Join-Path $repo 'bench/results/agent-demo'
New-Item -ItemType Directory -Force $outDir | Out-Null
$mcpConfig = Join-Path $outDir 'claude-mcp.json'
@{ mcpServers = @{ lookahead = @{ command = (Get-Command lookahead).Source; args = @('mcp', '-n', $Namespace) } } } |
    ConvertTo-Json -Depth 5 | Set-Content $mcpConfig
$task = (Get-Content (Join-Path $PSScriptRoot 'task.txt') -Raw).Replace('{label}', $Label).Trim()

Push-Location $repo
try {
    claude -p $task --output-format stream-json --verbose --mcp-config $mcpConfig --strict-mcp-config `
        --allowedTools 'mcp__lookahead__pool_status,mcp__lookahead__create_hint,mcp__lookahead__list_hints,mcp__lookahead__delete_hint,PowerShell,Bash' |
        Set-Content (Join-Path $outDir "$Label.session.jsonl")
} finally { Pop-Location }
"Transcript: $(Join-Path $outDir "$Label.session.jsonl")"
