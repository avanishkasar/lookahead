#Requires -Version 7.0
<#
.SYNOPSIS
Puts the demo pool in the same idle state before each agent-demo run: the forecast+hints policy
applied, no hints, no forecast left from earlier claims, and the pool settled at minReplicas.
#>
[CmdletBinding()]
param(
    [string]$Namespace = 'lookahead-bench',
    [string]$Pool = 'bench-pool',
    [int]$TimeoutSeconds = 900
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$PSNativeCommandUseErrorActionPreference = $true

$repo = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
kubectl apply -f (Join-Path $repo 'bench/manifests/policy-both.yaml') | Out-Null
kubectl delete demandhints --all -n $Namespace --ignore-not-found | Out-Null

$deadline = (Get-Date).AddSeconds($TimeoutSeconds)
Write-Host "==> Waiting for $Pool to idle at minReplicas with no forecast"
while ($true) {
    $lap = kubectl get lookaheadpolicy $Pool -n $Namespace -o json | ConvertFrom-Json
    $swp = kubectl get sandboxwarmpool $Pool -n $Namespace -o json | ConvertFrom-Json
    $min = $lap.spec.minReplicas
    $st = $lap.status
    $forecast = if ($st.PSObject.Properties['forecastReplicas']) { $st.forecastReplicas } else { 0 }
    $desired = if ($st.PSObject.Properties['desiredReplicas']) { $st.desiredReplicas } else { -1 }
    $ready = if ($swp.status.PSObject.Properties['readyReplicas']) { $swp.status.readyReplicas } else { 0 }
    $claims = @(kubectl get sandboxclaims -n $Namespace -o name).Count
    if ($forecast -eq 0 -and $desired -eq $min -and $ready -eq $min -and $claims -eq 0) { break }
    if ((Get-Date) -gt $deadline) { throw "pool did not idle: forecast $forecast, desired $desired, ready $ready, claims $claims" }
    Start-Sleep -Seconds 5
}
Write-Host "==> Idle: $Pool at $min ready, forecast 0, no hints, no claims"
