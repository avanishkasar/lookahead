#Requires -Version 7.0
<#
.SYNOPSIS
Creates the benchmark cluster: minikube, agent-sandbox, lookahead, Prometheus and one external
metrics provider.

.DESCRIPTION
Only one component can serve external.metrics.k8s.io, so the HPA baseline (prometheus-adapter)
and the KEDA baseline cannot be measured in the same setup. Run with -ExternalMetrics keda, run
the benchmark, then re-run with -ExternalMetrics prometheus-adapter -SkipClusterStart and run the
HPA strategy. Idempotent.

.EXAMPLE
./bench/setup.ps1
./bench/setup.ps1 -ExternalMetrics prometheus-adapter -SkipClusterStart
#>
[CmdletBinding()]
param(
    [ValidateSet('keda', 'prometheus-adapter')]
    [string]$ExternalMetrics = 'keda',
    [string]$MinikubeProfile = 'lookahead',
    [string]$AgentSandboxVersion = 'v1.0.7',
    [string]$KedaVersion = '2.20.0',
    [string]$PrometheusAdapterChartVersion = '5.3.0',
    [switch]$SkipClusterStart
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$PSNativeCommandUseErrorActionPreference = $true

$repo = Split-Path $PSScriptRoot -Parent
$manifests = Join-Path $PSScriptRoot 'manifests'
$ns = 'lookahead-bench'
$kedaUrl = "https://github.com/kedacore/keda/releases/download/v$KedaVersion/keda-$KedaVersion.yaml"

foreach ($tool in 'minikube', 'kubectl', 'helm') {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        throw "$tool is not on PATH"
    }
}

if (-not $SkipClusterStart) {
    Write-Host '==> Starting minikube'
    # A burst holds up to ~100 sandboxes plus the warm pool; the default 110 pods per node is too tight.
    minikube start -p $MinikubeProfile --cpus 4 --memory 8g --extra-config=kubelet.max-pods=250
}

Write-Host "==> Installing agent-sandbox $AgentSandboxVersion with extensions"
kubectl apply --server-side -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$AgentSandboxVersion/sandbox-with-extensions.yaml"
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=300s

Write-Host '==> Building and deploying lookahead-controller'
minikube -p $MinikubeProfile image build -t lookahead-controller:dev $repo
kubectl apply -k (Join-Path $repo 'config/default')
kubectl -n lookahead-system rollout restart deployment/lookahead-controller
kubectl -n lookahead-system rollout status deployment/lookahead-controller --timeout=300s

Write-Host '==> Benchmark workload and Prometheus'
kubectl apply -f (Join-Path $manifests 'base.yaml')
kubectl apply -f (Join-Path $manifests 'prometheus.yaml')
kubectl -n $ns rollout status deployment/prometheus --timeout=300s

if ($ExternalMetrics -eq 'keda') {
    Write-Host "==> External metrics: KEDA $KedaVersion"
    helm uninstall prometheus-adapter -n $ns --ignore-not-found
    kubectl apply --server-side --force-conflicts -f $kedaUrl
    kubectl -n keda rollout status deployment/keda-operator --timeout=300s
    kubectl -n keda rollout status deployment/keda-operator-metrics-apiserver --timeout=300s
} else {
    Write-Host "==> External metrics: prometheus-adapter chart $PrometheusAdapterChartVersion"
    kubectl delete -f $kedaUrl --ignore-not-found
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
    helm upgrade --install prometheus-adapter prometheus-community/prometheus-adapter `
        --version $PrometheusAdapterChartVersion -n $ns `
        -f (Join-Path $manifests 'prometheus-adapter-values.yaml') --wait --timeout 5m
}

Write-Host '==> Ready. Next: ./bench/run.ps1'
