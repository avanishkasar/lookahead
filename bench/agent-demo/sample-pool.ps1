#Requires -Version 7.0
<#
.SYNOPSIS
Appends the pool's spec.replicas and readyReplicas, and the policy's hint count, to a CSV once a
second until stopped. The agent demo uses it to show pre-warming before the fan-out, which the
replayer's own samples (taken only while claims run) do not cover.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$Out,
    [string]$Namespace = 'lookahead-bench',
    [string]$Pool = 'bench-pool'
)
$ErrorActionPreference = 'Continue'
if (-not (Test-Path $Out)) { Set-Content $Out 'at,spec,ready,hints,desired' }
while ($true) {
    $swp = kubectl get sandboxwarmpool $Pool -n $Namespace -o jsonpath='{.spec.replicas},{.status.readyReplicas}' 2>$null
    $lap = kubectl get lookaheadpolicy $Pool -n $Namespace -o jsonpath='{.status.hintReplicas},{.status.desiredReplicas}' 2>$null
    if ($swp) {
        $p = "$swp".Split(','); $q = "$lap".Split(',')
        $v = { param($s) if ([string]::IsNullOrEmpty($s)) { 0 } else { $s } }
        Add-Content $Out ('{0},{1},{2},{3},{4}' -f (Get-Date).ToString('o'), (& $v $p[0]), (& $v $p[1]), (& $v $q[0]), (& $v $q[1]))
    }
    Start-Sleep -Milliseconds 1000
}
