# KEP-NNNN: Predictive and Hint-Driven Warm Pool Sizing

<!--
NNNN is replaced by the tracking issue number once the issue is filed.
TOC is auto-generated via `make toc-update` upstream.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [The reactive-lag argument](#the-reactive-lag-argument)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories (Optional)](#user-stories-optional)
  - [High-Level Design](#high-level-design)
    - [Sizing rule](#sizing-rule)
    - [Forecast signal](#forecast-signal)
    - [Hint signal](#hint-signal)
    - [Ownership of replicas and conflict handling](#ownership-of-replicas-and-conflict-handling)
    - [API Changes](#api-changes)
    - [Hint API options](#hint-api-options)
    - [Implementation Guidance](#implementation-guidance)
  - [Failure Handling](#failure-handling)
  - [Test Plan](#test-plan)
  - [Benchmark Plan](#benchmark-plan)
- [Scalability](#scalability)
- [Alternatives (Optional)](#alternatives-optional)
<!-- /toc -->

## Summary

`SandboxWarmPool` sizes are static unless an HPA or KEDA `ScaledObject` drives the `/scale`
subresource, and both react to claims that have *already* happened. Any burst shorter than the
time it takes the pool to replenish is therefore served partly by cold starts. This KEP proposes
an opt-in way to size a warm pool *ahead* of demand from two signals:

1. a short-horizon **forecast** of claim arrivals per pool, computed from claims the controller
   already observes, and
2. **demand hints**: short-lived, TTL-bounded declarations from agents or orchestrators of the
   form "I will need N sandboxes from pool P in T seconds".

The target is `clamp(forecast(leadTime) + Σ active hints, min, max)`. The KEP lists API options
(fields on `SandboxWarmPool` vs. a separate object in `extensions.agents.x-k8s.io`) and hint API
options without choosing one; a prototype exists out of tree to produce data for that decision.

## Motivation

Agent workloads are bursty in ways that are known in advance by somebody: an RL trainer starts a
step with G rollouts, an eval harness starts a shard of 500 tasks, a planner agent is about to fan
out to 20 tool-calling sub-agents. Today the only way to turn that knowledge into warm capacity is
for the client to patch `spec.replicas` itself (as `examples/agent-sandbox-rl` does), which:

- requires `update` on `sandboxwarmpools/scale`, i.e. the right to resize a shared pool,
- does not compose across clients (two clients overwrite each other's replica counts),
- has no expiry (a crashed client leaves the pool inflated), and
- conflicts with HPA/KEDA when they also target the pool.

### The reactive-lag argument

With a rate-based HPA/KEDA trigger on `agent_sandbox_claim_creation_total`, the earliest moment
new warm capacity can exist after a burst starts at `t0` is

```
t0 + scrape interval (15-30s) + rate() window fill (≥ 1 scrape, typically 1m window)
   + HPA/KEDA sync period (15s) + replenishment time T
```

For a burst of `B` claims over `D` seconds against a pool of `P` warm sandboxes, roughly
`max(0, B − P − refilled_during(D))` claims go cold whenever `D` is shorter than that lag. The
`examples/keda-scale-to-zero` scaler already encodes the workaround: size the buffer to
`rate × T` *after* the rate is observed. That works for sustained load, not for step-shaped
demand. A hint that arrives `H ≥ T` seconds before the burst removes the lag entirely, and a
forecast removes it for demand that is periodic or trending.

### Goals

- Opt-in, per-pool predictive sizing; default behavior of `SandboxWarmPool` unchanged.
- A hint mechanism usable from all three SDKs, namespaced and RBAC-scoped so an agent can hint
  without being able to resize the pool directly.
- Hints expire on their own; a crashed hinter never leaves a pool inflated past its TTL.
- Never fight another owner of `spec.replicas` (HPA, KEDA, the claim-aware standby policy of
  #1916/#1979, or a human): detect and surface a status condition instead.
- Reproducible benchmark showing p95 claim latency, cold-start rate, and idle sandbox-minutes
  against static, HPA and KEDA baselines on the same traces.

### Non-Goals

- Node-level capacity. `CapacityBuffer` (`autoscaling.x-k8s.io`) already provisions standby nodes
  for a `SandboxWarmPool` via `scalableRef`; this KEP sizes pods and composes with it.
- ML forecasting in v0.1. A simple model (EWMA / Holt) ships first; anything heavier must beat it
  on the benchmark.
- Admission control, quotas or cross-pool placement.
- Scaling individual `Sandbox` objects (see #1466).

## Proposal

### User Stories (Optional)

- **RL trainer.** Before each step, the trainer creates one hint `{pool: swe-py, replicas: 64,
  neededAt: now+20s, ttl: 2m}`. By the time rollouts claim, 64 sandboxes are warm. If the trainer
  crashes, the extra capacity drains after the TTL.
- **Agent platform.** A planner agent that decides to fan out creates a hint for 10 sandboxes; it
  has `create` on hints in its namespace only.
- **Operator.** A pool with diurnal traffic gets predictive sizing with `min: 5, max: 200` and no
  hints at all; pool size follows demand with less idle capacity than a static pool sized for peak.

### High-Level Design

#### Sizing rule

Per pool, every reconcile (driven by claim/hint events plus a short periodic requeue):

```
expected = Σ_{h=1..L} max(0, forecast(h))          # claim arrivals over leadTime, L buckets
fc       = ceil(expected + z·sqrt(expected))       # Poisson-style headroom, z configurable
hints    = Σ replicas of hints active now          # neededAt − leadTime ≤ now < neededAt + ttl
raw      = clamp(fc + hints, min, max)
target   = max(raw over the last scaleDownStabilization)   # scale up now, scale down slowly
```

`leadTime` is the pool's replenishment time (default 30s, configurable; auto-estimation from
`agent_sandbox_creation_latency_ms` is future work).

#### Forecast signal

The controller already watches `SandboxClaim`, and every claim names its pool in
`spec.warmPoolRef.name`. Arrivals are counted per pool into fixed buckets (default 5s) from
`metadata.creationTimestamp`, so no Prometheus dependency is introduced and the history is
rebuilt from the informer's initial list after a restart. The model is Holt's linear
(double exponential) smoothing over the bucket series; EWMA is the special case `β = 0`.
Seasonality (Holt-Winters) is deferred until a trace shows it pays for its state.

#### Hint signal

A hint is `{pool, replicas, neededAt?, ttl}`. It is active from `neededAt − leadTime` (or creation,
whichever is later) until `neededAt + ttl`, after which it no longer counts and is garbage
collected. Hints add to the forecast; claims made because of a hint also feed the forecaster,
which is intentional (the next similar burst is then partly forecast) and bounded by `max`.

#### Ownership of replicas and conflict handling

Exactly one writer may own `spec.replicas`. The predictive sizer:

- does not write if any `HorizontalPodAutoscaler` in the namespace has `scaleTargetRef` pointing
  at the pool (this also covers KEDA, which materialises an HPA named `keda-hpa-*`); it sets a
  `ScaleTargetConflict` condition naming the HPA instead,
- does not write if more than one predictive policy targets the same pool; the oldest wins,
  the others get `ScaleTargetConflict`,
- writes only `spec.replicas`, only when the value changes, with optimistic concurrency
  (`resourceVersion` precondition) so a concurrent writer causes a re-read, not a lost update.
  An upstream implementation should write through `/scale`, so its RBAC can be narrowed to
  `sandboxwarmpools/scale`; the prototype patches the object because the controller-runtime
  fake client used by the existing test harness does not implement `/scale` for CRDs.

If API option A below is chosen, there is no second writer at all: the effective pool target
becomes `max(spec.replicas, predictive target)` computed inside the warm pool controller, and
`spec.replicas` stays with the user/HPA. This is also how it would compose with the
`replenishmentCapacity` standby policy proposed in #1979 (apply that policy to the effective
target).

#### API Changes

Not decided; this is the main question for maintainers.

| | Option A: fields on `SandboxWarmPool` | Option B: new object in `extensions.agents.x-k8s.io` | Option C: external metric only |
|---|---|---|---|
| Shape | `spec.lookahead: {min, max, leadTime, forecast: {...}}`; `status.lookaheadReplicas`, conditions | `SandboxWarmPoolLookahead` with `poolRef`, same knobs; writes `/scale` | A component exports `desired = forecast + hints` as an external metric; HPA/KEDA write `/scale` |
| Owner of replicas | none added (effective target computed in pool controller) | the new object (must detect HPA/KEDA) | HPA/KEDA |
| Touches core pool controller | yes | no | no |
| Hint latency | one reconcile | one reconcile | scrape + HPA sync (≥ 15–45s) |
| Composes with #1979 | naturally (same target computation) | via `/scale` only | via `/scale` only |
| Risk | widens `SandboxWarmPool` API | another `/scale` writer to coordinate | adapter dependency; lag partly returns |

#### Hint API options

| | H1: hint CR (`SandboxDemandHint`) | H2: endpoint on controller or router | H3: early `SandboxClaim` |
|---|---|---|---|
| Auth | RBAC on the resource, namespaced | separate authn/z needed | existing claim RBAC |
| Restart-safe | yes (stored in etcd) | no, unless persisted | yes |
| Expiry | `ttl` + controller GC | in-memory TTL | n/a; binds a sandbox immediately |
| Cost per hint | one create (+ one delete) | one request | a whole sandbox |
| Visible / auditable | `kubectl get` | logs/metrics only | yes |
| SDK work | create a CR | new client | none |

H3 defeats the purpose (it consumes the capacity it asks for). H2 is cheaper per hint but needs a
new authenticated surface and state that survives leader failover. The prototype uses H1.

#### Implementation Guidance

- Reuse the existing `SandboxClaim` informer and the `.spec.warmPoolRef.name` field index for
  arrival counting.
- Keep the forecaster a pure function of the bucket history so reconcile is stateless and
  restart-safe.
- Hint GC is a separate tiny reconciler that requeues each hint to its expiry time.
- Metrics (bounded cardinality, per pool): forecast target, hint target, final target, active
  hint count.

### Failure Handling

| Failure | Behavior |
|---|---|
| Controller restart / leader failover | History rebuilt from listed claims; hints are objects. Claims created and deleted while the controller was down are missed, which biases the forecast low until new arrivals are seen; `min` is the floor. |
| No arrivals observed yet | Forecast is 0, target is `max(min, hints)`. |
| Pool not found / deleted | `PoolNotFound` condition, no writes, retry on pool events. |
| Conflicting replicas owner | `ScaleTargetConflict` condition, no writes. |
| API write errors | Retried with backoff; `status.conditions` reflects the last error. |
| Hint flood | Bounded by `max`; per-namespace object quota applies to hint objects. |

### Test Plan

- Unit: forecaster (EWMA/Holt numerics, empty/irregular histories, horizon sums), sizing rule
  (clamp, stabilization, hint windows), arrival recorder (dedup, pruning, concurrency, `-race`).
- Reconciler tests with the controller-runtime fake client, following the existing harness in
  `extensions/controllers`: target written via `/scale`, HPA conflict, duplicate policy,
  missing pool, hint expiry, restart (state rebuilt from claims).
- E2E on kind: policy drives a real pool; hint warms a pool before claims arrive.

### Benchmark Plan

Same harness for every strategy: a trace generator/replayer creates `SandboxClaim`s at recorded
offsets (and hints when the strategy uses them), holds each claim for its recorded duration,
and samples the pool once per second.

- Traces (seeded, reproducible): steady Poisson; periodic bursts shorter than replenishment;
  RL-style step bursts with a hint lead; random flash crowds without hints.
- Strategies: static (small), static (sized for peak), HPA on claim rate, KEDA Prometheus
  scaler, lookahead forecast only, lookahead hints only, lookahead forecast + hints.
- Metrics: p50/p95/p99 claim-to-Ready latency, cold-start rate (from the
  `agents.x-k8s.io/launch-type` label on the bound Sandbox), idle sandbox-minutes (integral of
  unclaimed pool members).
- Cluster: kind (as used by `make deploy-kind`), 3 seeds per trace; report variance.

## Scalability

- Per pool: one bucket ring (default 30 min / 5s = 360 counters) and one Holt pass over it per
  reconcile, O(buckets). Negligible next to the pool reconcile itself.
- Extra API writes: at most one `/scale` update per pool per reconcile when the target changes;
  stabilization keeps this low. Hints cost one create and one delete each.
- The extra target never exceeds `max`, so the existing refill path (and its documented
  ~70–85 creates/s per pool ceiling) is unchanged.

## Alternatives (Optional)

- **Client-side `/scale` patching** (status quo in `agent-sandbox-rl`): no aggregation, no
  expiry, needs scale RBAC, fights HPA.
- **PredictKube (KEDA scaler)**: external SaaS model, black box; not appropriate as a project
  default.
- **Crane EHPA**: prediction-driven HPA; general purpose and heavy, no notion of hints.
- **Speculative prewarm from the token stream** (SpecBox, LibreChat): complementary; such a
  system is a natural *producer* of hints.
- **Cron-based scaling**: covers schedules, not ad-hoc bursts.
