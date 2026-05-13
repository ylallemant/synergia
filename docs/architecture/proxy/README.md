# Synergia API Proxy (`manager-api-proxy`)

> **Phase 3** — not yet implemented.

See [Architecture](../README.md) for the full design.

## Motivation

The proxy serves a **dual role**: security hardening and horizontal scaling. Both goals are achieved by the same architectural decision — keeping the manager off the internet.

### Security

The cluster manager carries a large attack surface: SQLite/PostgreSQL database, filesystem model cache, admin dashboard, OIDC configuration, TLS private key, and application logic. Exposing it directly to the internet means any vulnerability in any of those layers can be exploited by a volunteer worker or external attacker.

The proxy is the **only** internet-facing component. It is deliberately minimal — no DB, no FS, no admin surface — so its attack surface is close to zero. Even a fully compromised proxy gives an attacker only the in-memory state of its current worker connections.

### Scaling

The proxy also decouples the high-frequency worker event path (heartbeats, status updates, completions — potentially thousands per second at scale) from the manager's business logic. Proxies absorb all the connection-handling load and deliver pre-aggregated batches to the manager, which only needs to process data at batch frequency. Adding proxy instances scales worker connection capacity without touching the manager at all.

The `manager-api-proxy` implements this with a **two-tier deployment**:

```
Internet
  │
  ├── workers (WSS) ──▶  [proxy]  [proxy]  [proxy]   ← hardened, internet-facing
  │                          │       │       │
  │                          └───────┴───────┘
  │                               (batch sync)
  │                                  │
Internal network only             [manager]           ← DB, cache, admin, never internet-facing
  │                                  │
  └── admin / Flow Engine ──────────▶│
```

The proxy is the **only** component that touches the public internet. The manager lives entirely on an internal network.

## What the Proxy Is

A minimal, purpose-built binary with no dependencies beyond:

- WebSocket server (worker connections)
- In-memory state store
- Batch sync client to the manager (internal network only)
- TLS termination

No database. No filesystem writes. No admin interface. No model cache. Deployed from a hardened, read-only, minimal Linux image (e.g. distroless or Alpine with dropped capabilities and a RuntimeDefault seccomp profile).

## In-Memory State

On startup the proxy fetches its initial state from the manager (one batch pull). After that it maintains the following entirely in memory:

| State | Source | Updated by |
|---|---|---|
| Connected workers (fingerprint → connection) | Manager batch | Worker connects / disconnects |
| Worker status per fingerprint | Manager batch | Worker status messages (atomic, local) |
| Pending work units to dispatch | Manager batch | Manager push batch |
| Completed work units (pending flush) | Local | Worker result messages (atomic, local) |
| Worker config + consent flags | Manager batch | Manager push batch |
| Role-model mapping | Manager batch | Manager push batch |

Worker-driven atomic events (heartbeats, status changes, completions) are applied **only to memory** — they never trigger a DB write or a manager call. Only batch syncs move data between proxy and manager.

## Batch Exchange Protocol

Proxies and the manager exchange **batches** on a configurable interval (e.g. every 2–5 s). No per-event RPC calls cross the proxy↔manager boundary.

### Proxy → Manager (outbound batch)

```json
{
  "proxy_id": "proxy-eu-1",
  "timestamp": "2026-05-13T10:00:00Z",
  "worker_snapshots": [
    { "fingerprint": "abc123", "status": "available", "llm_hash": "...", "backend_version": "local" }
  ],
  "completions": [
    { "unit_id": "wu-001", "result": "...", "signature": "...", "completed_at": "..." }
  ],
  "errors": [
    { "fingerprint": "abc123", "message": "...", "stack": "..." }
  ]
}
```

### Manager → Proxy (inbound batch)

```json
{
  "timestamp": "2026-05-13T10:00:00Z",
  "work_units": [
    { "id": "wu-002", "model": "SmolLM2-135M-Instruct", "messages": [...] }
  ],
  "role_model_map": { "embedding": { "filename": "bge-m3-Q4_K_M.gguf", "llm_hash": "..." } },
  "push_commands": [
    { "fingerprint": "abc123", "type": "model_update", "payload": { ... } }
  ],
  "revoked_fingerprints": []
}
```

`push_commands` carry model/binary/backend update instructions that the proxy delivers over the worker's open WebSocket connection.

## Work Unit Dispatch

The manager claims each work unit atomically from the DB (`UPDATE work_units SET status='dispatching', claimed_by=? WHERE status='pending'`) before including it in a proxy batch. Only one proxy receives any given unit. The proxy holds it in memory and dispatches to the first available matching worker.

If the proxy loses a work unit before completion (crash, restart), the manager's reconciliation goroutine detects the stale `dispatching` row after a timeout and re-queues it.

### Hard Rejection and Re-routing

Because the proxy's in-memory worker state is eventually consistent, it may dispatch to a worker that has just become busy (GPU contention, user activity). The worker's contract with its contributor is unconditional: if it cannot accept a unit right now, it returns a hard **rejection** immediately — no queuing, no partial processing.

The proxy (and the current non-proxy gateway) must treat a worker rejection as a **transparent internal event**, never as a caller-visible error:

1. Worker returns rejection (`TypeError` with a `busy` / `unavailable` reason)
2. Proxy marks the unit as undispatched and immediately re-routes to the next available worker on any connected proxy
3. If no worker is available, the unit is returned to the pending pool and the caller waits (same as a normal queue backpressure)
4. The caller only sees an error if the unit times out with no worker ever accepting it

This keeps the contributor's promise — their resources are never held against their will — while making the eventual-consistency gap invisible to the system's consumers.

> **Current implementation gap**: the non-proxy gateway path currently propagates worker rejections as HTTP 500 to the caller. This needs to be fixed to re-queue and re-route before the proxy layer is built, since the behaviour must be identical on both paths.

## Scaling

Proxies are stateless between batches and horizontally scalable:

- Add or remove proxy instances without coordination
- Each proxy independently maintains its slice of connected workers
- The manager aggregates state from all proxies via batches
- A load balancer distributes worker WebSocket connections across proxies (any algorithm — sticky sessions not required because workers reconnect automatically)

## What Changes for Workers

Workers connect to the proxy's WebSocket endpoint instead of the manager's. From the worker's perspective the protocol is identical — the proxy speaks exactly the same WebSocket message format. The `--manager-url` flag / `worker-state.yaml` simply points at the proxy address.

## Deployment Profile

| Property | Value |
|---|---|
| Base image | Distroless or Alpine |
| Filesystem | Read-only root |
| Network | Public internet ingress (WSS port only); internal egress to manager |
| Linux capabilities | All dropped except `NET_BIND_SERVICE` |
| Seccomp | RuntimeDefault |
| Process | Single binary, no shell, no package manager |
| State | In-memory only — a restart loses buffered completions (recovered by manager reconciliation) |

## Relationship to Horizontal Manager Scaling

With proxies in place, the manager no longer handles direct worker connections. Multiple manager instances (if needed for the admin API or DB throughput) coordinate exclusively through the shared PostgreSQL backend — no cross-manager RPC required. The proxy layer absorbs all the connection-handling scale.
