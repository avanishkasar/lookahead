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

## First results (full numbers)

One seed of `bursty-hinted`, 10 minutes, 151 claims. The README shows the same run in plain terms.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../docs/img/timeline-dark.svg">
  <img alt="Pool size over a 10-minute bursty trace. With lookahead the warm pool fills to about 60 before each 40-claim burst and no claim waits more than 5 seconds. With KEDA the pool tops out at 18, empties at each burst, and 84 of 151 claims wait more than 5 seconds." src="../docs/img/timeline-light.svg">
</picture>

| Strategy | p50 | p95 | p99 | Claims waiting > 5 s | Cold starts | Idle sandbox-min |
|---|---:|---:|---:|---:|---:|---:|
| lookahead-both | 0.43 s | 0.89 s | 0.93 s | 0 % | 0 % | 456 |
| lookahead-hints | 0.50 s | 1.24 s | 11.1 s | 2.0 % | 0 % | 337 |
| lookahead-forecast | 20.5 s | 36.3 s | 37.9 s | 66.9 % | 37.1 % | 283 |
| keda | 18.4 s | 21.7 s | 22.6 s | 55.6 % | 4.0 % | 140 |
| static-small (5) | 20.5 s | 25.0 s | 26.1 s | 70.2 % | 29.8 % | 55 |
| static-peak (45) | 0.41 s | 0.80 s | 0.89 s | 0 % | 0 % | 495 |

- Hints do the work; the forecast alone behaves like a slow reactive scaler on bursts and then
  overshoots. Adding it to hints trims the tail (p99 11.1 s → 0.93 s).
- Cold-start rate understates waiting: under KEDA only 4 % of claims are cold, but 56 % wait more
  than 5 s, because they bind to warm-pool members that exist but are not Ready yet.

### Agent demo

One 30-claim fan-out per run, from [`agent-demo/`](agent-demo) (see [docs/agent-demo.md](../docs/agent-demo.md)).

| Run | p50 | p95 | Waited > 5 s | Cold starts |
|---|---:|---:|---:|---:|
| No hint (run 1) | 24.1 s | 28.0 s | 28 of 30 | 28 |
| No hint (run 2) | 24.5 s | 28.1 s | 28 of 30 | 24 |
| Claude Code + MCP | 0.34 s | 0.96 s | 0 of 30 | 0 |
| Codex + MCP | 22.7 s | 23.9 s | 28 of 30 | 24 |
| Codex + MCP + first snippet | 22.6 s | 25.8 s | 28 of 30 | 24 |
| Codex + MCP + `AGENTS.md` snippet | 0.28 s | 0.76 s | 0 of 30 | 0 |

In the 45 s before the fan-out, idle sandbox-minutes were 6.1 (Claude Code) and 5.5 (Codex)
against 1.2 without a hint.
