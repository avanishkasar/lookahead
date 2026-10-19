# lookahead

Predictive prewarming for [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
`SandboxWarmPool`s: size a warm pool **ahead** of demand from a claim-arrival forecast and from
**demand hints** that agents send ("I will need N sandboxes from pool P in T seconds").

This is an out-of-tree prototype for an upstream proposal. The API group
`lookahead.avanishkasar.github.io/v1alpha1` exists only to collect data; the upstream API shape
is open — see [the KEP draft](docs/keps/NNNN-lookahead/README.md) and the
[issue draft](docs/issue-draft.md).

## How it works

```
target = clamp(ceil(E + z·√E) + Σ active hints, minReplicas, maxReplicas)
         E = Holt forecast of claim arrivals over leadTimeSeconds
pool.spec.replicas = max(target over the last scaleDownStabilizationSeconds)
```

- **LookaheadPolicy** (`lap`) targets one pool and owns its `spec.replicas`. If an HPA (or a KEDA
  ScaledObject's HPA) or an older policy targets the same pool, it writes nothing and sets
  `ScaleTargetConflict=True`.
- **DemandHint** (`dh`) counts from `neededAt − leadTime` until `neededAt + ttlSeconds`, then the
  controller deletes it. Agents need only `create` on `demandhints`, never access to the pool.
- Arrivals are counted from the `SandboxClaim` informer (`spec.warmPoolRef.name`), so there is no
  Prometheus dependency; history is rebuilt from existing claims after a restart.

## Use

```powershell
go generate ./...                  # CRDs, RBAC, deepcopy (after changing api/ or RBAC markers)
go test -race ./...
kubectl apply -k config/default    # needs image lookahead-controller:dev (see bench/setup.ps1)
kubectl apply -f config/samples/lookaheadpolicy.yaml
go run ./cmd/lookahead hint create -n default --pool python-sdk-warmpool --replicas 20 --in 30s --ttl 2m
```

## Layout

| Path | |
|---|---|
| `api/v1alpha1` | LookaheadPolicy, DemandHint |
| `internal/forecast` | Holt/EWMA, sizing, scale-down stabilizer (pure functions) |
| `internal/arrivals` | per-pool claim arrival counter |
| `internal/controller` | policy and hint-expiry reconcilers |
| `internal/trace`, `internal/replay` | trace generator, replayer, summary and report |
| `cmd/lookahead-controller` | controller manager |
| `cmd/lookahead` | CLI: `hint`, `trace generate`, `replay`, `report` |
| `bench/` | benchmark setup, baselines and runner ([bench/README.md](bench/README.md)) |
| `docs/` | KEP and issue drafts |
