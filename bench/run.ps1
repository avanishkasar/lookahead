#Requires -Version 7.0
<#
.SYNOPSIS
Replays the same traces against every strategy and writes results/report.md.

.DESCRIPTION
For each seed, trace and strategy: reset the pool to the strategy's starting size, restart the
lookahead controller (so no arrival history leaks between runs), apply the strategy, replay the
trace, clean up, and wait for Prometheus' rate() window to drain. Strategies whose prerequisites
are missing from the cluster (see setup.ps1) are skipped with a warning.

.EXAMPLE
./bench/run.ps1
./bench/run.ps1 -Strategies hpa -Seeds 1,2,3
#>
[CmdletBinding()]
param(
    [ValidateSet('static-small', 'static-peak', 'hpa', 'keda', 'lookahead-forecast', 'lookahead-hints', 'lookahead-both')]
    [string[]]$Strategies = @('static-small', 'static-peak', 'keda', 'lookahead-forecast', 'lookahead-hints', 'lookahead-both'),
    [ValidateSet('steady', 'bursty-hinted', 'bursty-unhinted')]
    [string[]]$Traces = @('steady', 'bursty-hinted', 'bursty-unhinted'),
    [int[]]$Seeds = @(1, 2, 3),
    [string]$Duration = '15m',
    [int]$StaticSmall = 5,
    [int]$StaticPeak = 45,
    [int]$AutoscaledStart = 2,
    [int]$CooldownSeconds = 90,
    [string]$OutDir = (Join-Path $PSScriptRoot 'results')
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$PSNativeCommandUseErrorActionPreference = $true

$repo = Split-Path $PSScriptRoot -Parent
$manifests = Join-Path $PSScriptRoot 'manifests'
$traceRoot = Join-Path $PSScriptRoot 'traces'
$ns = 'lookahead-bench'
$pool = 'bench-pool'
$policyResource = 'lookaheadpolicies.lookahead.avanishkasar.github.io'

New-Item -ItemType Directory -Force -Path $OutDir, $traceRoot | Out-Null
$cli = Join-Path $repo 'bin' ($IsWindows ? 'lookahead.exe' : 'lookahead')
Push-Location $repo
try { go build -o $cli ./cmd/lookahead } finally { Pop-Location }

# Bursts of 40 claims within 5s, longer than nothing and much shorter than replenishment
# (~20s readiness delay in manifests/base.yaml). Hints arrive 45s ahead, more than one lead time.
$common = @('--duration', $Duration, '--hold', '60s')
$tracePresets = @{
    'steady'          = @('--base-rate', '0.3', '--burst-every', '0')
    'bursty-hinted'   = @('--base-rate', '0.05', '--burst-every', '3m', '--burst-size', '40', '--burst-duration', '5s', '--hint-fraction', '1', '--hint-lead', '45s', '--hint-ttl', '90s')
    'bursty-unhinted' = @('--base-rate', '0.05', '--burst-every', '3m', '--burst-size', '40', '--burst-duration', '5s', '--hint-fraction', '0')
}

function Test-Exists([string]$kind, [string]$name) {
    $out = kubectl get $kind $name --ignore-not-found -o name
    return [bool]$out
}

function Get-ExternalMetricsProvider {
    if (-not (Test-Exists 'apiservice' 'v1beta1.external.metrics.k8s.io')) { return '' }
    return (kubectl get apiservice v1beta1.external.metrics.k8s.io -o 'jsonpath={.spec.service.name}')
}

function Test-StrategyAvailable([string]$strategy) {
    switch -Wildcard ($strategy) {
        'hpa' { return (Get-ExternalMetricsProvider) -like '*prometheus-adapter*' }
        'keda' { return (Test-Exists 'crd' 'scaledobjects.keda.sh') -and ((Get-ExternalMetricsProvider) -like 'keda-*') }
        'lookahead-*' { return Test-Exists 'crd' $policyResource }
        default { return $true }
    }
}

function Wait-PoolReady([int]$replicas, [int]$timeoutSeconds = 600) {
    $deadline = (Get-Date).AddSeconds($timeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $ready = kubectl get sandboxwarmpool $pool -n $ns -o 'jsonpath={.status.readyReplicas}'
        $total = kubectl get sandboxwarmpool $pool -n $ns -o 'jsonpath={.status.replicas}'
        if ([int]("0$ready") -eq $replicas -and [int]("0$total") -eq $replicas) { return }
        Start-Sleep -Seconds 2
    }
    throw "pool did not reach $replicas ready replicas within $timeoutSeconds s"
}

function Reset-Strategy([int]$startReplicas) {
    # ScaledObjects first: KEDA would re-create its HPA otherwise.
    if (Test-Exists 'crd' 'scaledobjects.keda.sh') {
        kubectl delete scaledobjects.keda.sh --all -n $ns --ignore-not-found --wait=true
    }
    kubectl delete hpa --all -n $ns --ignore-not-found --wait=true
    if (Test-Exists 'crd' $policyResource) {
        kubectl delete $policyResource --all -n $ns --ignore-not-found --wait=true
        kubectl delete demandhints.lookahead.avanishkasar.github.io --all -n $ns --ignore-not-found --wait=true
    }
    kubectl delete sandboxclaims --all -n $ns --ignore-not-found --wait=true
    kubectl scale sandboxwarmpool/$pool -n $ns --replicas=$startReplicas
    Wait-PoolReady $startReplicas
    if (Test-Exists 'crd' $policyResource) {
        # Arrival history lives in memory; a restart gives every run the same empty start.
        kubectl -n lookahead-system rollout restart deployment/lookahead-controller
        kubectl -n lookahead-system rollout status deployment/lookahead-controller --timeout=300s
    }
}

function Start-Strategy([string]$strategy) {
    switch ($strategy) {
        'static-small' { return @{ Start = $StaticSmall; Hints = $false } }
        'static-peak' { return @{ Start = $StaticPeak; Hints = $false } }
        'hpa' { return @{ Start = $AutoscaledStart; Hints = $false; Manifest = 'hpa.yaml' } }
        'keda' { return @{ Start = $AutoscaledStart; Hints = $false; Manifest = 'keda.yaml' } }
        'lookahead-forecast' { return @{ Start = $AutoscaledStart; Hints = $false; Manifest = 'policy-forecast.yaml' } }
        'lookahead-hints' { return @{ Start = $AutoscaledStart; Hints = $true; Manifest = 'policy-hints.yaml' } }
        'lookahead-both' { return @{ Start = $AutoscaledStart; Hints = $true; Manifest = 'policy-both.yaml' } }
    }
}

$available = @($Strategies | Where-Object {
        $ok = Test-StrategyAvailable $_
        if (-not $ok) { Write-Warning "skipping $_ : prerequisites missing (see bench/setup.ps1)" }
        $ok
    })
if ($available.Count -eq 0) { throw 'no strategy can run on this cluster' }

foreach ($seed in $Seeds) {
    $seedDir = Join-Path $traceRoot "s$seed"
    New-Item -ItemType Directory -Force -Path $seedDir | Out-Null
    foreach ($traceName in $Traces) {
        $tracePath = Join-Path $seedDir "$traceName.jsonl"
        & $cli trace generate --out $tracePath --seed $seed @common @($tracePresets[$traceName])
        foreach ($strategy in $available) {
            $s = Start-Strategy $strategy
            Write-Host "==> seed $seed, trace $traceName, strategy $strategy"
            Reset-Strategy $s.Start
            if ($s.ContainsKey('Manifest')) {
                kubectl apply -f (Join-Path $manifests $s.Manifest)
            }
            $result = Join-Path $OutDir "$traceName-$strategy-s$seed.json"
            $replayArgs = @('replay', '--trace', $tracePath, '-n', $ns, '--pool', $pool, '--strategy', $strategy,
                '--run-id', "la-$seed-$strategy", '--out', $result)
            if ($s.Hints) { $replayArgs += '--hints' }
            & $cli @replayArgs
            Reset-Strategy $AutoscaledStart
            Start-Sleep -Seconds $CooldownSeconds
        }
    }
}

$all = @(Get-ChildItem -Path $OutDir -Filter '*.json' | ForEach-Object FullName)
& $cli report --out (Join-Path $OutDir 'report.md') @all
Write-Host "==> Report: $(Join-Path $OutDir 'report.md')"
