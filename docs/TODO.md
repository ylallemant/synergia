# Synergia — TODO

## Phase 1 — remaining

| done | item | description | status |
|---|---|---|---|
| [ ] | Worker rejection re-routing | When a worker returns a hard rejection (busy / GPU contention), the gateway re-queues the work unit and re-routes to the next available worker transparently — the caller never sees the rejection as an error. Currently propagated as HTTP 500. | not started |
| [ ] | Result signature verification | Manager verifies the Ed25519 signature on every `TypeResult` message against the worker's stored public key; persists signature to `work_units` table | not started |
| [ ] | Binary rollback on reconnect failure | After a self-update, if the new binary fails to reconnect to the manager within 60 s, restore the `.bak` binary and restart | not started |

## Phase 2 — trust & redundancy

| done | item | description | status |
|---|---|---|---|
| [ ] | Trust scoring | Increment/decrement `trust_score` in DB on result consensus match, canary pass/fail, and connection stability; expose in worker overview | scaffolded (field + API exist, scoring logic absent) |
| [ ] | Redundant processing | Send each work unit to N workers (N configurable per role), compare results; majority-vote or semantic similarity check | not started |
| [ ] | Canary work units | Inject known-good test cases at random intervals; fail workers who return wrong answers | not started |
| [ ] | mTLS | Issue per-worker TLS client certificates signed by a cluster CA; require them on the WebSocket endpoint | not started |

## Phase 2 — security hardening

| done | item | description | status |
|---|---|---|---|
| [x] | Challenge-response worker handshake | TOFU challenge/nonce/Ed25519-signature exchange on first connect; fingerprint→key mapping persisted to DB | done (both TOFU and key-auth modes coexist) |
| [ ] | Eliminate pre-shared worker key | Make challenge-response the only auth mode; remove `CLUSTER_WORKER_KEY` / key-auth fallback | partial (TOFU works, pre-shared key still supported) |
| [ ] | Named API keys | Replace single `CLUSTER_API_KEY` with a table of named keys with scopes (consumer, admin, worker); admin UI for key management | not started |
| [ ] | Manager public key publishing | Manager publishes its Ed25519 public key at a well-known endpoint; workers pin it on first connect (SSH-style TOFU) and verify on reconnect | not started |
| [ ] | Sign manager→worker push messages | Manager signs `model_update`, `binary_update`, `backend_update`, and `work_unit` messages with its private key; workers verify before acting | not started |
| [ ] | Verify worker result signatures | Manager verifies Ed25519 signature on every `TypeResult` against the worker's registered public key before accepting the result | not started |
| [ ] | Sign work units before dispatch | Manager signs work units so workers can verify origin and detect replay | not started |
| [ ] | Signed consent + config payloads | Worker signs consent acceptance and config updates; manager stores and verifies | not started |
| [ ] | Operator-signed release artifact manifest | Operator signs a manifest of all release binary hashes with an offline key; workers verify before installing | not started |
| [ ] | Container hardening | Read-only root filesystem, dropped Linux capabilities, RuntimeDefault seccomp profile, non-root user, no `--privileged` | not started |

## Phase 3 — production

| done | item | description | status |
|---|---|---|---|
| [ ] | Installer packages | `.dmg` (macOS) and `.msi` (Windows) wrapping the client binary with auto-start setup | not started |
| [ ] | Contributor agreement text | Add a short data-processing notice above the download button and in the client consent text; volunteers only ever process public data (private data is routed to k8s GPU nodes by the Flow Engine, never reaching Synergia) | not started |
| [ ] | Community leaderboard page | Public `/community` page with live cluster stats and contributor rankings; nickname already stored via client config — purely a UI work item | not started |
| [ ] | API proxy (`manager-api-proxy`) | New hardened binary: minimal read-only Linux image, no DB, no FS cache, no admin surface — the only internet-facing component. Handles worker WebSocket connections; applies atomic worker events (status, completions, heartbeats) to its own in-memory state; exchanges only **batches** with the manager (never atomic events). Manager stays on the internal network, never exposed to the internet. Multiple proxies scale independently behind a load balancer. | not started |
| [ ] | Horizontal cluster manager scaling | Shared PostgreSQL backend. Manager maintains unified in-memory state (built from DB on startup); live changes update memory first, async goroutines batch-flush to DB for eventual consistency; periodic full-reconciliation goroutine prevents drift (k8s informer pattern). Work unit dispatch uses atomic DB claim (`UPDATE ... WHERE status='pending'`) for cross-instance coordination. Stats and live API endpoints served from in-memory state only. With proxies in place, the manager never handles direct worker connections — only proxy batch syncs. | not started |

## Phase 4 — advanced

| done | item | description | status |
|---|---|---|---|
| [ ] | Encrypted work units | Encrypt prompts with worker's public key so the manager cannot read content (requires TEE or homomorphic approach) | not started |
| [ ] | Mobile support | Client daemon for high-end phones with NPUs (iOS Neural Engine, Android NNAPI) | not started |
| [ ] | P2P model distribution | BitTorrent-style model file sharing between workers; manager acts as tracker | not started |
