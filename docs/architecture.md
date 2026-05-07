# Distributed Worker Network — Synergia

> Inspired by SETI@home / BOINC: a volunteer-driven compute mesh where community members donate idle GPU time for document ingestion and inference workloads.

## Motivation

LLM inference (especially for ingestion) is embarrassingly parallel — each document is independent. The current architecture processes documents sequentially on a single GPU node. A distributed worker network would:

1. **Eliminate GPU costs** for the central operator (no Scaleway GPU node needed)
2. **Scale horizontally** with community size — more users = faster ingestion
3. **Reduce single-point-of-failure** — if one worker dies, the work unit is reassigned
4. **Democratise participation** — anyone with a GPU can contribute to the knowledge base

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────┐
│              Cluster Manager (central server)                │
│                                                              │
│  ┌────────────┐  ┌────────────┐  ┌──────────────────────┐    │
│  │ Work Queue │  │  Result    │  │ Trust & Reputation   │    │
│  │ (postgres) │  │  Validator │  │ Engine               │    │
│  └─────┬──────┘  └─────┬──────┘  └──────────────────────┘    │
│        │               │                                     │
│  ┌─────▼───────────────▼──────────────────────────────────┐  │
│  │              WebSocket Gateway (WSS)                   │  │
│  │         wss://<manager-host>/ws/worker                 │  │
│  └────────────────────────┬───────────────────────────────┘  │
└───────────────────────────│──────────────────────────────────┘
                            │
            ┌───────────────┼───────────────────┐
            │ (outbound WSS)│                   │
       ┌────▼────┐    ┌─────▼───┐         ┌─────▼───┐
       │ Worker  │    │ Worker  │         │ Worker  │
       │ Daemon  │    │ Daemon  │   ...   │ Daemon  │
       │         │    │         │         │         │
       │ WebUI   │    │ WebUI   │         │ WebUI   │
       │ :9876   │    │ :9876   │         │ :9876   │
       │         │    │         │         │         │
       │ LLM     │    │ LLM     │         │ LLM     │
       │ (local) │    │ (local) │         │ (local) │
       └─────────┘    └─────────┘         └─────────┘
```

### Components

| Component | Description | Details |
|---|---|---|
| **Cluster Manager** | Central coordinator — OpenAI-compatible API, WebSocket gateway, work queue, admin dashboard | [docs/manager/README.md](../manager/README.md) |
| **Cluster Client** | Worker daemon — connects to manager, runs local LLM inference, local dashboard | [docs/client/README.md](../client/README.md) |
| **Integration Test** | End-to-end test harness for the full pipeline | [docs/test/README.md](../test/README.md) |

## Communication Protocol

### Why WebSocket (Reverse Tunnel)

Workers sit behind NATs, firewalls, and dynamic IPs. They cannot expose a port. The solution is the same pattern used by VS Code Remote, Tailscale, and Cloudflare Tunnel:

- Worker **initiates** an outbound WSS connection to the cluster manager
- Cluster manager **pushes** work units down the existing connection
- Worker **uploads** results back through the same channel

No router configuration. No port forwarding. Works from corporate networks, home routers, mobile hotspots.

### Message Flow

Workers connect via `wss://<manager>/ws/worker` with authentication headers (fingerprint, public key, model info). The manager pushes work units as JSON messages; the worker returns results signed with its Ed25519 private key. A heartbeat mechanism maintains liveness.

For the full protocol specification (headers, message types, JSON schemas), see the [Cluster Manager README — Protocol section](../manager/README.md#protocol-phase-1--simplified).

## Integration with Flow Engine

The cluster manager exposes an **OpenAI-compatible API**. From the flow engine's perspective, it's just another LLM provider:

```
Current:    LLM Node → HTTP → Scaleway/GitHub Models API
Distributed: LLM Node → HTTP → Cluster Manager → WSS → Worker → local LLM
```

```env
LLM_BASE_URL=https://<manager-host>:7500
LLM_API_KEY=<internal-service-key>
LLM_MODEL=mistral-small-3.2-24b-instruct-2506
```

This means **zero changes to the flow engine** — it's completely transparent.

## Worker Identity & Security

Each worker generates a persistent **Ed25519 keypair** on first run. The fingerprint (`SHA256(public_key)`) is the worker's unique identity across all communication. Result payloads are signed with the private key, enabling non-repudiation.

For identity storage, encryption details, and key rotation semantics, see the [Cluster Client README — Worker Identity](../client/README.md#worker-identity-fingerprint).

## Data Collection Consent

Workers must **explicitly accept** data collection before receiving work units. Consent covers hardware statistics and configuration preferences. Without consent, the worker connects but remains idle.

See [Cluster Client README — Consent & Configuration](../client/README.md#consent--configuration).

## GPU Contention Detection

The worker daemon detects external GPU usage (gaming, rendering) via platform APIs (NVML, Metal, ROCm SMI) and transitions to `idle` state. The manager treats idle workers as unavailable for scheduling. When contention ends, the worker resumes automatically.

**No user impact**: GPU priority belongs to the user — the worker is a background scavenger of idle cycles only.

## Contribution Modes

### Hardware Tiers

| Tier | Min VRAM | Workload | Model |
|---|---|---|---|
| **Embeddings** | 4 GB | Vector embeddings only | `bge-m3` (Q4, ~2GB) |
| **Ingestion** | 8 GB | Metadata, chunking, entities | `mistral-small` (Q4, ~4.5GB) |
| **Full** | 16 GB | All above + inference queries | `mistral-small` (Q6/F16) or larger |

Role eligibility is managed by the cluster manager via role-model mappings with VRAM thresholds. See [Cluster Manager README — Role Administration](../manager/README.md#role-administration-endpoints-authenticated-with-api-key).

## Tamper Resistance (Phase 2+)

The fundamental challenge: **the worker controls the hardware**. They can return fabricated results, run cheaper models, or modify outputs. Defence layers (planned for Phase 2+):

| Layer | Mechanism | Status |
|---|---|---|
| Transport security | TLS + shared key auth (Phase 1), mTLS (Phase 2) | Phase 1 implemented |
| Result signing | Ed25519 signatures on every result | Implemented (received, not yet persisted or verified) |
| Redundant processing | Send each work unit to N workers, compare results | Phase 2 |
| Canary work units | Inject known-good test cases at random intervals | Phase 2 |
| Deterministic mode | `temperature=0` + fixed model for reproducible outputs | Phase 2 |
| Model attestation | Worker reports model file SHA256, server verifies | Phase 2 |

### Trust Score System (Phase 2+)

Each worker accumulates a trust score:

| Event | Score change |
|---|---|
| Result matches consensus | +1 |
| Canary passed | +5 |
| Result diverges from consensus | -10 |
| Canary failed | -50 |
| Connection stability (per day) | +1 |

Trust tiers:
- **Probation** (0-50): Triple redundancy, frequent canaries
- **Trusted** (50-200): Double redundancy, normal canaries
- **Veteran** (200+): Single processing for non-critical work, reduced canaries

## Privacy & Data Handling

### Document Classification

Not all documents should be sent to community workers:

| Classification | Distributed? | Reason |
|---|---|---|
| Public (scraped web, YouTube) | ✅ Yes | Already public |
| User-uploaded (personal) | ❌ No | Privacy risk |
| Internal (organisational) | ❌ No | Confidentiality |

### Technical Mitigations

- Work units contain only the **prompt text** — no metadata about source, bucket, etc.
- Results are **ephemeral** on the worker — cleared from memory after upload
- Client binary is **open source** for community verification

## Incentive Model

Why would someone donate their GPU?

- **Intrinsic**: Contributing to an open knowledge base, "my GPU does something useful while I sleep"
- **Gamification**: Leaderboard, badges, monthly stats
- **Tangible**: Priority query access, higher rate limits, contributor badge

## Scaling Properties

| Workers | Throughput (docs/hour) | Notes |
|---|---|---|
| 1 | ~30 | Single GPU, rate-limited by inference speed |
| 10 | ~250 | Some overhead from redundancy |
| 50 | ~1,000 | Approaching ingestion pipeline bottleneck (S3, DB) |
| 200 | ~3,000 | Need to scale cluster manager horizontally |

## Implementation Phases

### Phase 1: Proof of Concept ✅

Single worker, no trust system, no redundancy. Validates the end-to-end path. **Implemented.**

- OpenAI-compatible API on the manager
- WebSocket gateway with work unit dispatch
- Worker daemon with local `llama-server` integration
- Ed25519 identity + result signing
- Consent-gated dispatch
- Local dashboard on both manager (admin) and client
- Batch queue for async processing
- Latency monitoring with adaptive bucketing
- GPU contention detection
- System tray integration (macOS/Linux)
- TLS with optional HTTP→HTTPS redirect
- Auto-start (LaunchAgent / systemd)
- Error reporting pipeline
- Full integration test suite

### Phase 2: Multi-Worker

- Trust scoring with redundancy
- Canary system
- Model auto-download from S3 (API exists, client doesn't pull yet)
- mTLS certificates
- Result signature verification

### Phase 3: Production

- Installer packages (.dmg, .msi)
- Public registration + contributor agreement
- Document classification (public vs private routing)
- Horizontal scaling of cluster manager

### Phase 4: Advanced

- Encrypted work units (if homomorphic or TEE becomes practical)
- Mobile support (high-end phones with NPUs)
- P2P model distribution (BitTorrent-style)

## Open Questions

1. **Model updates**: How to roll out new model versions across 200 workers without disrupting processing? Gradual rollout with version pinning per work unit?

2. **Heterogeneous hardware**: Different quantisations produce slightly different results. Is semantic comparison sufficient?

3. **Geographic distribution**: Prefer workers near the manager (lower latency) or distribute globally (follow-the-sun)?

4. **Abuse prevention**: What stops someone from registering fake workers to poison results? Rate-limit registrations? Require verification?

5. **Legal**: Data residency implications when a volunteer in Country X processes a document?

6. **LLM determinism**: How deterministic is `temperature=0` across different hardware (CUDA vs Metal vs ROCm)?

## Comparison with Alternatives

| Approach | Cost | Speed | Privacy | Complexity |
|---|---|---|---|---|
| Scaleway GPU (current) | €0.013/doc | Fast | Full control | Low |
| GitHub Models API | Free (rate-limited) | Fast | Data to Microsoft | Low |
| Distributed workers | Free (community) | Variable | Data to volunteers | High |
| Self-hosted LocalAI | Hardware cost | Fast | Full control | Medium |

The distributed approach is **complementary** — use it for public documents where privacy isn't critical, fall back to Scaleway/LocalAI for sensitive content.

## Related Projects & Prior Art

- **BOINC** — Original distributed computing framework (UC Berkeley). Battle-tested for 20+ years
- **Folding@home** — Protein folding, peak 2.4 exaFLOPS during COVID
- **Petals** — Distributed LLM inference (run parts of large models across multiple machines)
- **Hive** — Decentralised AI compute marketplace
- **Golem** — Generic distributed computing with economic incentives

The key difference: we don't need a blockchain or token economics. A trust-score + reputation system (like StackOverflow or Wikipedia) is simpler and more aligned with community values.
