# Draft issue for kubernetes-sigs/agent-sandbox (Feature Request template)

**Title:** SandboxWarmPool: size warm pools ahead of demand from a forecast and agent demand hints

---

### What would you like to be added?

An opt-in way to size a `SandboxWarmPool` *before* claims arrive, from two signals:

1. **Forecast:** a short-horizon forecast of claim arrivals per pool (Holt/EWMA over the claims
   the controller already watches; no Prometheus dependency).
2. **Demand hints:** short-lived, TTL-bounded declarations from agents or orchestrators:
   "I will need N sandboxes from pool P in T seconds". Hints are namespaced and RBAC-scoped, so an
   agent can hint without having `update` on `sandboxwarmpools/scale`, and they expire on their
   own if the hinter crashes.

Target = `clamp(forecast over leadTime + Σ active hints, min, max)`, scale up immediately, scale
down after a stabilization window. It never fights another owner of `spec.replicas`: if an
HPA/KEDA targets the pool it surfaces a `ScaleTargetConflict` condition instead of writing.

I'd like maintainer input on the API shape before writing a KEP PR:

- **A:** fields on `SandboxWarmPool` (`spec.lookahead`), effective target computed in the pool
  controller as `max(spec.replicas, lookahead target)`, so no second replicas writer;
- **B:** a separate object in `extensions.agents.x-k8s.io` that writes `/scale`, like an HPA;
- **C:** only export the desired size as a metric and let HPA/KEDA write.

And the hint API: a hint CR with TTL (my current choice) vs. an endpoint on the controller/router.

### Why is this needed?

HPA and KEDA (both have examples in this repo) scale on claim rate, so new warm capacity cannot
exist until scrape interval + `rate()` window + HPA sync + replenishment time after a burst starts.
Any burst shorter than that is partly served cold. The KEDA example's comment already sizes the
buffer as `rate × T` after the rate is observed; that helps sustained load, not step-shaped load.

Agent workloads are step-shaped and the steps are often known in advance (an RL step with G
rollouts, an eval shard, a planner fanning out). Today the only way to use that knowledge is to
patch `spec.replicas` from the client (as `examples/agent-sandbox-rl` does), which needs scale
RBAC, does not compose across clients, has no expiry, and conflicts with HPA/KEDA.

Evidence from an out-of-tree prototype on kind, same traces for all strategies:
see the benchmark report linked below (p95 claim latency, cold-start rate, idle sandbox-minutes
for static / HPA / KEDA / forecast / hints / forecast+hints).

Related: #1916 / #1979 (claim-aware standby target; this would feed the same target computation
under option A), #1466, #677. Prior art: PredictKube (KEDA, SaaS model), Crane EHPA, OpenAI
Agents API sandbox pre-warm, SpecBox (speculative sandbox start from the LLM token stream), which
would be a natural producer of hints.

Draft KEP: <link to fork branch>
Prototype + benchmark: <link to avanishkasar/lookahead>

I'm happy to implement this. /kind feature
