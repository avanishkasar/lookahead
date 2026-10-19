# Benchmark

Same traces, same harness, every strategy. All commands are PowerShell 7 (`pwsh`) and work on
Windows, Linux and macOS.

## Prerequisites

Go 1.21+ (the module's `toolchain go1.27.1` line downloads the right toolchain), minikube,
kubectl, helm (HPA baseline only).

## Run

```powershell
./bench/setup.ps1                      # minikube + agent-sandbox v1.0.7 + lookahead + Prometheus + KEDA
./bench/run.ps1                        # static, KEDA and lookahead strategies, 3 seeds x 3 traces
./bench/setup.ps1 -ExternalMetrics prometheus-adapter -SkipClusterStart
./bench/run.ps1 -Strategies hpa        # HPA baseline (cannot share the cluster with KEDA's metrics server)
```

`run.ps1` regenerates the report from every result in `bench/results/` at the end:
`bench/results/report.md`. A full default run is about 3 seeds × 3 traces × 6 strategies ×
(15 min + reset) ≈ 15 hours; use `-Seeds 1 -Duration 10m` for a first look.

## What is measured

| Metric | Source |
|---|---|
| Claim latency (p50/p95/p99) | replayer: `SandboxClaim` create call → `Ready=True` seen on a watch |
| Cold-start rate | `agents.x-k8s.io/launch-type` label (`warm`/`cold`) of the Sandbox bound to each claim |
| Idle sandbox-minutes | integral of the pool's `status.replicas` (unclaimed members), sampled every second |
| Idle ready sandbox-minutes | same for `status.readyReplicas` |

## Traces

| Trace | Shape |
|---|---|
| `steady` | Poisson, 0.3 claims/s |
| `bursty-hinted` | 0.05 claims/s background + 40 claims within 5 s every ~3 min; each burst announced by a hint 45 s ahead |
| `bursty-unhinted` | same bursts, no hints |

Claims are held 30–90 s. The benchmark template becomes Ready ~20 s after its pod starts
(readiness delay in `manifests/base.yaml`), standing in for a runtime's start-up work.

## Strategies

| Strategy | Pool sizing |
|---|---|
| `static-small` | 5 |
| `static-peak` | 45 (one burst plus background) |
| `hpa` | HPA on `rate(agent_sandbox_claim_creation_total[1m])`, target 0.05/replica (20 s of demand), 2–60 |
| `keda` | KEDA Prometheus scaler, same query and target as the upstream example, 2–60 |
| `lookahead-forecast` | LookaheadPolicy, Holt forecast, 30 s lead, 2–60 |
| `lookahead-hints` | LookaheadPolicy, `model: None` (hints only), 2–60 |
| `lookahead-both` | LookaheadPolicy, Holt + hints, 2–60 |

HPA and KEDA use the same sizing rule as the upstream `examples/keda-scale-to-zero` so the
comparison is with what the project documents today.
