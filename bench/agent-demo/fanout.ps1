#Requires -Version 7.0
<#
.SYNOPSIS
Claims -Count sandboxes from a warm pool at once, holds them, and reports how fast they became
Ready. It stands in for an agent's fan-out (a batch of evals, an RL step) in the agent demo.

.DESCRIPTION
The claims arrive over about a second, like a real fan-out. Latency is measured by the same
replayer as the benchmark (claim create -> Ready=True), and the full result is written to
bench/results/agent-demo/<Label>.json. The script does not send a DemandHint itself.

.EXAMPLE
pwsh -File bench/agent-demo/fanout.ps1 -Count 30 -Label baseline
#>
[CmdletBinding()]
param(
    [ValidateRange(1, 500)]
    [int]$Count = 30,
    [string]$Label = 'run',
    [string]$Namespace = 'lookahead-bench',
    [string]$Pool = 'bench-pool',
    [ValidateRange(5, 600)]
    [int]$HoldSeconds = 30
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$PSNativeCommandUseErrorActionPreference = $true

$repo = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$outDir = Join-Path $repo 'bench/results/agent-demo'
New-Item -ItemType Directory -Force $outDir | Out-Null
$tracePath = Join-Path $outDir "$Label.trace.jsonl"
$resultPath = Join-Path $outDir "$Label.json"

# One claim every 30 ms: the whole fan-out lands within about a second.
$lines = for ($i = 0; $i -lt $Count; $i++) {
    '{{"atMs":{0},"kind":"claim","holdMs":{1}}}' -f ($i * 30), ($HoldSeconds * 1000)
}
Set-Content -Path $tracePath -Value $lines

$lookahead = Get-Command lookahead -ErrorAction SilentlyContinue
Write-Host "==> Claiming $Count sandboxes from $Namespace/$Pool"
if ($lookahead) {
    & $lookahead.Source replay --trace $tracePath -n $Namespace --pool $Pool --strategy $Label --run-id "demo-$Label" --out $resultPath | Out-Null
} else {
    Push-Location $repo
    try { go run ./cmd/lookahead replay --trace $tracePath -n $Namespace --pool $Pool --strategy $Label --run-id "demo-$Label" --out $resultPath | Out-Null }
    finally { Pop-Location }
}

$r = Get-Content $resultPath -Raw | ConvertFrom-Json
$waits = @($r.claims | Where-Object { $_.readyAt } | ForEach-Object {
        ([datetimeoffset]$_.readyAt - [datetimeoffset]$_.createdAt).TotalSeconds })
$slow = @($waits | Where-Object { $_ -gt 5 }).Count
$s = $r.summary
'{0} sandboxes: p50 {1:N1} s, p95 {2:N1} s, slowest {3:N1} s; {4} of {0} waited more than 5 s; {5} cold starts.' -f `
    $s.claims, ($s.p50Ms / 1000), ($s.p95Ms / 1000), (($waits | Measure-Object -Maximum).Maximum), $slow, $s.coldStarts
"Result: $resultPath"
