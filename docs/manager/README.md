# Synergia Manager

Central coordinator for the distributed worker network. Exposes an **OpenAI-compatible chat completion API** so the Flow Engine can use it as a drop-in LLM backend, and a **WebSocket gateway** for worker daemons to connect and receive work units.

See [Architecture](../architecture.md) for the full design.

## Phase 1 — Proof of Concept

Minimal implementation: single worker, no trust system, no redundancy. The goal is to validate the end-to-end path: Flow Engine → Cluster Manager → Worker → local LLM → result back.

### Scope

- OpenAI-compatible `/v1/chat/completions` endpoint (blocking — waits for worker result)
- WebSocket gateway (`/ws/worker`) for worker connections
- In-memory work queue (no persistence — single process, no restarts)
- Single worker support (first connected worker gets all work)
- Basic health endpoint (`/healthz`)
- Configuration via environment variables
- **Worker consent API** (`/v1/consent`): gate work unit dispatch on accepted data collection consent
- **Worker config API** (`/v1/worker-config`): store and retrieve worker preferred role
- **Role-model mapping** (`/v1/roles`, `/v1/admin/roles`): manager-controlled mapping of roles to LLM models with VRAM eligibility thresholds; the `tester` role is always present and always eligible regardless of hardware
- **Branding API** (`/v1/branding/style.css`, `/v1/admin/branding/css`): customisable CSS served to clients
- **Model storage** (filesystem + S3 via MinIO): model file listing and download with Range support
- **Synergia API** (`/v1/workers`, `/v1/work-units`, `/v1/stats`): cluster management endpoints
- **Error reporting API** (`/v1/errors`): receives and stores error reports from workers (with version, stack trace, timestamp)
- **Latency monitoring**: per-role adaptive payload-size bucketing with p50/p95/p99 percentiles, 48h rolling window, persisted to DB
- **Batch queue**: OpenAI-compatible asynchronous batch API (`/v1/batches`) — enqueue requests when no worker is immediately available; background processor dispatches in FIFO order when a worker becomes available. Supports create, retrieve, list, and cancel operations.
- **Development mode** (`--development`): batch requests are processed sequentially with random 1-5s delays between each, making it easier to observe and debug the queue lifecycle
- **429 rejection**: live `/v1/chat/completions` returns HTTP 429 when no worker is in `available` state (paused, idle, or processing); callers should retry or use the batch endpoint
- **Administration port**: separate listener for admin-only endpoints (dashboard, latency, config), enabling k8s service/ingress segregation
- **In-memory cache** (`internal/manager/cache/`): background-refreshed stats (1 s interval) and GitHub release tags for the admin dashboard; avoids per-request DB queries and GitHub API calls
- **Admin dashboard**: JS-polled (2 s) HTML page on the admin port showing connected workers, role distribution, today's payloads, and recent client errors
- **SQLite + PostgreSQL** storage (GORM): worker registry, work unit history, consent, config, client errors, latency samples
- **TLS** with optional HTTP→HTTPS redirect server
- **Result signing** received (signature is passed through the protocol but not yet persisted in DB)
- **Model update push**: when an admin updates a role-model mapping, the manager computes the expected `llmHash`, pushes a `model_update` message to connected workers for that role, and tracks their `sync_status` (`synced` / `out-of-sync`)
- **Dual-status model**: each worker has a `status` (client-reported) and a `sync_status` (manager-derived from LLM hash comparison). Only workers with both `available` + `synced` receive work dispatch
- **Binary auto-update push**: admin sets target client version via `POST /v1/admin/version`; manager resolves platform-specific download URLs from GitHub releases, computes SHA256 per artifact, pushes `binary_update` to outdated workers. Supports `all` or `percentage`-based rollout modes
- **Binary download proxy** (`GET /v1/binary/download`): fetches and caches release binaries from GitHub, serves to workers behind firewalls that cannot reach GitHub directly
- **Backend auto-update push**: admin sets target backend version and name via `POST /v1/admin/backend`; manager resolves download URL from a registry of backend URL templates, pushes `backend_update` to workers. Tags are fetched from GitHub via `/v1/admin/backend/tags`
- **Backend download proxy** (`GET /v1/backend/download`): fetches and caches full backend release archives from upstream, serves to workers behind firewalls that cannot reach GitHub directly
- **Backend registry** (`internal/manager/backend/`): central package defining supported backend names (constants), download URL templates with platform/arch placeholders, and GitHub tag retrieval
- **Admin configuration UI** (`/admin/config` on admin port): unified page showing target client version, backend name/version (with dropdown and refresh), role-model matrix, and worker overview (fingerprint, OS/arch, version, sync status)
- **Platform-aware worker registry**: stores `os` and `arch` (from `X-Worker-OS`/`X-Worker-Arch` headers) in the workers table for binary artifact resolution

### Out of Scope (Phase 2+)

- Trust scoring / redundancy / canaries
- mTLS (Phase 1 uses TLS + shared API key auth)
- Persistent work queue (Postgres-backed in-flight survival)
- Multiple simultaneous workers
- Result signature storage and verification (signatures are received but not yet persisted or validated)

## Project Structure

```
cmd/synergia-manager/ + internal/manager/
├── go.mod
├── README.md
├── Dockerfile
├── cmd/
│   └── synergia-manager/
│       └── main.go                  # Entrypoint
└── internal/
    ├── config/
    │   └── config.go                # Env-based configuration
    ├── api/
    │   ├── backend.go               # Backend version management + download proxy + tags API
    │   ├── batch.go                 # OpenAI-compatible /v1/batches (create, retrieve, list, cancel)
    │   ├── branding.go              # Branding CSS API (served to worker dashboards)
    │   ├── completions.go           # OpenAI-compatible /v1/chat/completions handler
    │   ├── consent.go               # Consent + worker config API
    │   ├── synergia.go             # Synergia API (models, workers, work-units, stats)
    │   ├── errors.go                # Client error reporting API (POST + GET /v1/errors)
    │   ├── latency.go               # Latency monitoring admin API (GET /v1/latency, config)
    │   ├── models_download.go       # Model file listing + download endpoint
    │   └── roles.go                 # Role-model mapping + eligibility API
    ├── backend/
    │   └── backend.go               # Backend registry: names, URL templates, GitHub tag fetching
    ├── gateway/
    │   └── websocket.go             # WebSocket upgrade + worker session management
    ├── latency/
    │   └── monitor.go               # Latency recording, hourly aggregation, bucket computation
    ├── models/
    │   └── store.go                 # Model storage abstraction (filesystem + S3)
    ├── queue/
    │   └── queue.go                 # In-memory work unit queue + dispatch
    ├── cache/
    │   ├── cache.go                 # In-memory cache (dashboard stats + tag lists, background refresh)
    │   └── github.go                # GitHub Releases API client (client + backend tags)
    ├── server/
    │   ├── server.go                # Admin dashboard HTTP server (admin port)
    │   └── static/                  # Embedded HTML template + CSS for admin dashboard
    ├── store/
    │   ├── models.go                # GORM models (Worker, WorkUnit, WorkerConfig, RoleModel)
    │   └── store.go                 # Database init, migrations, CRUD operations
    └── protocol/
        └── messages.go              # Work unit / result message types (JSON)
```

## Configuration

| Env Var | Default | Description |
|---|---|---|
| `CLUSTER_LISTEN_ADDR` | `:7500` | HTTPS + WebSocket listen address |
| `CLUSTER_API_KEY` | (required) | API key for the OpenAI-compatible endpoint (Flow Engine authenticates with this) |
| `CLUSTER_WORKER_KEY` | (required) | Shared secret workers use to authenticate the WebSocket handshake |
| `CLUSTER_TIMEOUT` | `120s` | Max time to wait for a worker result before returning 504 |
| `CLUSTER_INSECURE` | `false` | Set to `true` to disable TLS (serve plain HTTP). Logs a warning on startup. |
| `TLS_CERT_FILE` | (required unless insecure) | Path to TLS certificate (PEM) |
| `TLS_KEY_FILE` | (required unless insecure) | Path to TLS private key (PEM) |
| `CLUSTER_HTTP_REDIRECT_ADDR` | (empty) | When set and TLS is enabled, starts an HTTP listener on this address that redirects all requests to HTTPS with 301 |
| `CLUSTER_DB_DSN` | (empty) | PostgreSQL DSN. When set, uses PostgreSQL instead of SQLite (e.g., `host=localhost user=synergia password=secret dbname=cluster port=5432 sslmode=disable`) |
| `CLUSTER_DB_PATH` | `synergia-manager.db` | SQLite database path (used only when `CLUSTER_DB_DSN` is not set) |
| `CLUSTER_MODEL_BACKEND` | `filesystem` | Model storage backend: `filesystem` or `s3` |
| `CLUSTER_MODEL_PATH` | `./models` | Local directory for model files (filesystem backend) |
| `CLUSTER_MODEL_S3_ENDPOINT` | (required for s3) | S3-compatible endpoint URL (e.g., `s3.fr-par.scw.cloud`) |
| `CLUSTER_MODEL_S3_BUCKET` | `synergia-models` | S3 bucket name |
| `CLUSTER_MODEL_S3_REGION` | `us-east-1` | S3 region |
| `CLUSTER_MODEL_S3_KEY` | (required for s3) | S3 access key |
| `CLUSTER_MODEL_S3_SECRET` | (required for s3) | S3 secret key |
| `CLUSTER_MODEL_S3_SSL` | `true` | Use HTTPS for S3 connections |
| `CLUSTER_TEST_SETUP` | `false` | When `true`, seeds role-model mappings with minimal test thresholds (512 MB VRAM, SmolLM2-135M model) instead of production values. Note: the `tester` role is always seeded regardless of this flag |
| `CLUSTER_ADMIN_ADDR` | `:7501` | Administration listener address (separate from the main API port) |
| `CLUSTER_LATENCY_BUCKETS` | `4` | Number of payload-size buckets for the latency matrix |
| `CLUSTER_LATENCY_WINDOW_HOURS` | `48` | Rolling window (in hours) for payload statistics and sample retention |
| `CLUSTER_DEVELOPMENT` | `false` | When `true`, batch requests are processed sequentially with random 1–5 s delays (useful for testing/debugging) |

### Administration Port

The manager starts a **second HTTP listener** on `CLUSTER_ADMIN_ADDR` (default `:7501`) serving administration-only endpoints. This enables Kubernetes deployments to expose the main API and the admin API via separate Services and Ingresses with independent access rules (e.g., admin behind VPN / internal-only ingress).

The admin port serves:
- **Admin dashboard** (`/`) — auto-refreshing HTML page showing worker counts, role distribution, today's payload stats, and recent client errors
- **Latency API** (`/v1/latency`, `/v1/latency/config`) — latency matrix queries and configuration

The admin port inherits the same TLS settings as the main port. Authentication uses `CLUSTER_API_KEY` or `CLUSTER_WORKER_KEY` (query param `?key=` also supported for browser access).

### TLS (default)

TLS is **required** by default. The manager will fail to start without `TLS_CERT_FILE` and `TLS_KEY_FILE` unless `CLUSTER_INSECURE=true` is set.

When `CLUSTER_HTTP_REDIRECT_ADDR` is configured, the manager starts a secondary HTTP server that returns `301 Moved Permanently` to the HTTPS equivalent of any request. This ensures clients accidentally connecting via HTTP are redirected.

When running in insecure mode, the manager logs:
```
WARN  TLS disabled — running in insecure mode (traffic is unencrypted)
```

## Protocol (Phase 1 — simplified)

### Worker → Manager (WebSocket)

```
GET /ws/worker
Headers:
  Authorization: Bearer <CLUSTER_WORKER_KEY>
  X-Worker-Fingerprint: a1b2c3d4...  (SHA256 hex of worker's Ed25519 public key)
  X-Worker-Public-Key: base64(ed25519_public_key)
  X-Worker-Model: mistral-small-3.2-24b-instruct-2506
  X-Worker-Quantisation: Q4_K_M
  X-Worker-Version: 0.1.0-dev
```

The manager registers the fingerprint ↔ public key mapping on first connection. On subsequent connections, it verifies the public key matches the known fingerprint. Mismatch = reject connection.

### Message flow (JSON over WebSocket)

```jsonc
// Manager → Worker: work unit
{
  "type": "work_unit",
  "id": "wu-abc123",
  "model": "mistral-small-3.2-24b-instruct-2506",
  "params": {
    "temperature": 0,
    "max_tokens": 2048,
    "response_format": { "type": "json_schema", "json_schema": {} }
  },
  "messages": [
    { "role": "system", "content": "..." },
    { "role": "user", "content": "..." }
  ]
}

// Worker → Manager: result
{
  "type": "result",
  "id": "wu-abc123",
  "fingerprint": "a1b2c3d4...",
  "output": {
    "choices": [{ "message": { "role": "assistant", "content": "..." } }]
  },
  "processing_time_ms": 4200,
  "signature": "base64(ed25519_sign(private_key, canonical(id + output + processing_time_ms)))"
}

// Worker → Manager: error
{
  "type": "error",
  "id": "wu-abc123",
  "error": "model not loaded"
}

// Bidirectional: heartbeat
{ "type": "heartbeat" }
```

## How it integrates

```
Flow Engine                    Cluster Manager                Worker Daemon
    │                                │                              │
    │── POST /v1/chat/completions ──▶│                              │
    │                                │── WSS work_unit ────────────▶│
    │                                │                              │── run llama.cpp
    │                                │◀── WSS result ──────────────│
    │◀── 200 OK (OpenAI response) ───│                              │
```

The Flow Engine just sees another LLM API endpoint:

```env
LLM_BASE_URL=https://localhost:7500
LLM_API_KEY=<CLUSTER_API_KEY>
LLM_MODEL=mistral-small-3.2-24b-instruct-2506
```

## Worker Status Model

Each worker has **two independent statuses** stored in the same `workers` table row:

### Client Status (`status` column)

Reported by the worker itself over WebSocket. Represents the worker's operational state:

| Value | Meaning |
|---|---|
| `available` | Ready to accept work units |
| `processing` | Currently executing a work unit |
| `updating` | Downloading/verifying a model file after a `model_update` push |
| `paused` | Manually paused by the user (system tray) |
| `idle` | GPU reclaimed by another process (GPU monitoring detected activity) |
| `withdrawn` | Consent revoked — worker will not accept work |
| `offline` | Disconnected (set by manager when WebSocket closes) |

### Manager Status (`sync_status` column)

Derived by the manager by comparing the worker's reported `llm_hash` against the **expected llmHash** for the worker's role (from the central role-model mapping):

| Value | Condition | Dashboard |
|---|---|---|
| `synced` | `worker.llm_hash == expected_llm_hash_for_role` | available |
| `out-of-sync` | `worker.llm_hash != expected_llm_hash_for_role` | unavailable |

The expected hash is computed as `SHA256(role + ":" + SHA256(model_file_bytes))` from the model file in the manager's model store. When a role-model mapping is updated, the manager recomputes the expected hash and pushes a `model_update` to connected workers.

### Aggregated Status (for dispatch and dashboard)

The **aggregated status** determines whether a worker is eligible for work dispatch and how it appears in the admin dashboard:

```
aggregated = (client_status == "available") AND (sync_status == "synced")
```

| Client Status | Sync Status | Aggregated | Dashboard Category |
|---|---|---|---|
| `available` | `synced` | **available** | Ready |
| `available` | `out-of-sync` | unavailable | Unavailable |
| `processing` | `synced` | processing | Processing |
| `processing` | `out-of-sync` | unavailable | Unavailable |
| `updating` | any | unavailable | Unavailable |
| `paused` | any | unavailable | Unavailable |
| `idle` | any | unavailable | Unavailable |
| `withdrawn` | any | unavailable | Unavailable |
| `offline` | any | offline | Offline |

**Key rule**: Only `available` + `synced` workers receive work units. All other combinations result in HTTP 429 for live requests (callers should use the batch endpoint).

### Status Change Logging

On every status transition, the manager emits debug log entries:

```
DEBUG worker status transition  fingerprint=<fp> client_status=<new> sync_status=<current> aggregated=<result>
```

This enables integration tests to verify the full status lifecycle (e.g., `available` → `updating` → `available` during a model update).

## Synergia API

The cluster manager exposes an **OpenAI-compatible API** plus cluster-specific endpoints. All endpoints require authentication via `Authorization: Bearer <CLUSTER_API_KEY>` or `X-API-Key: <CLUSTER_API_KEY>`.

### OpenAI-Compatible Endpoints (used by Flow Engine)

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/chat/completions` | Chat completion — dispatches to worker, returns OpenAI-format response. Returns **429** if no worker is available (paused/idle/processing). |
| `POST` | `/v1/batches` | Submit a request to the batch queue (returns 202 with `{ "id": "...", "status": "pending" }`) |
| `GET` | `/v1/batches?id=<request_id>` | Poll a batch request status (pending → processing → completed/failed) |
| `GET` | `/v1/batches` | List recent batch requests |
| `GET` | `/v1/models` | List available models (based on online workers) |

### Batch Queue

The batch endpoint provides **Scaleway-style** asynchronous request processing:

1. Client submits a request via `POST /v1/batches` — immediately receives a request ID
2. The request is stored in the database with status `pending`
3. A background processor picks up pending requests in FIFO order when a worker becomes available
4. Client polls `GET /v1/batches?id=<id>` to check status and retrieve the result

This is useful when the live endpoint returns 429 (all workers busy/paused). Instead of retrying, callers can submit to the batch queue and poll for completion.

### Cluster Management Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/workers` | List all registered workers (fingerprint, model, trust score, status) |
| `GET` | `/v1/work-units` | List recent work unit history (last 100, with status and timing) |
| `GET` | `/v1/stats` | Aggregate cluster stats (worker count, work unit totals by status) |
| `GET` | `/healthz` | Health check (no auth required) — reports worker readiness |

### Worker Consent & Configuration Endpoints (authenticated with worker key)

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/consent` | Get consent state for the authenticated worker |
| `POST` | `/v1/consent` | Accept or revoke data collection consent |
| `GET` | `/v1/worker-config` | Get configuration preferences for the authenticated worker |
| `POST` | `/v1/worker-config` | Store preferred role (requires prior consent) |
| `GET` | `/v1/roles?fingerprint=<fp>` | List all roles with eligibility computed from worker VRAM (tester is always eligible) |

### Role Administration Endpoints (authenticated with API key)

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/admin/roles` | List all role-model mappings |
| `POST` | `/v1/admin/roles` | Create or update a role-model mapping |

Workers authenticate these requests with `Authorization: Bearer <CLUSTER_WORKER_KEY>` and `X-Worker-Fingerprint` header.

**Consent enforcement**: The WebSocket gateway will not dispatch work units to workers that have not accepted consent. Workers must POST consent acceptance before they receive any payloads.

### Error Reporting Endpoints (authenticated with worker key)

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/errors` | Submit an error report (fingerprint, version, error message, stack trace) |
| `GET` | `/v1/errors` | List stored error reports (most recent 100) |

Workers authenticate with `Authorization: Bearer <CLUSTER_WORKER_KEY>`.

### Model Download Endpoints (authenticated with worker key)

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/models/files` | List available model files (name, quantisation, size) |
| `GET` | `/v1/models/download/{filename}` | Download a model file. Supports `Range` header for resumable downloads |

Workers authenticate model download requests with `Authorization: Bearer <CLUSTER_WORKER_KEY>`.

## Administration API (port 7501)

Served on the dedicated admin listener. Authenticated with `CLUSTER_API_KEY`.

### Backend Administration Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/admin/backend` | Get current backend version config (name, version, download URL) |
| `POST` | `/v1/admin/backend` | Set target backend version — pushes `backend_update` to workers |
| `GET` | `/v1/admin/backend/tags?name=llama.cpp&limit=20` | Fetch recent release tags from GitHub for the named backend |
| `GET` | `/v1/admin/backend/names` | List all supported backend names |
| `GET` | `/v1/backend/download?version=b9049&os=darwin&arch=arm64` | Backend download proxy (worker-authenticated) — caches and serves full release archives |

#### POST /v1/admin/backend

```json
{
  "name": "llama.cpp",
  "version": "b9049"
}
```

The `download_url` field is optional — if omitted, the manager resolves it from the backend's URL template using the version and the worker's OS/arch. If provided explicitly, it overrides the template.

The response includes the resolved configuration and a `fallback_url` for each connected worker.

### Latency Monitoring Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/latency` | Latency matrix for all roles (bucket bounds + p50/p95/p99 per bucket) |
| `GET` | `/v1/latency?role=inference` | Latency matrix filtered to a single role |
| `GET` | `/v1/latency/config` | Current latency monitoring configuration (bucket count, window hours) |
| `POST` | `/v1/latency/config` | Update latency monitoring configuration |

### Example: Query latency matrix

```bash
curl -H "Authorization: Bearer <CLUSTER_API_KEY>" http://localhost:7501/v1/latency?role=inference
```

```json
{
  "role": "inference",
  "window_hours": 48,
  "bucket_count": 4,
  "bounds": [2048, 4096, 6144],
  "matrix": [
    { "range": "0-2048", "count": 142, "p50_ms": 1200, "p95_ms": 3400, "p99_ms": 5100 },
    { "range": "2048-4096", "count": 87, "p50_ms": 2800, "p95_ms": 6200, "p99_ms": 8900 },
    { "range": "4096-6144", "count": 34, "p50_ms": 5100, "p95_ms": 9800, "p99_ms": 12400 },
    { "range": "6144+", "count": 11, "p50_ms": 8200, "p95_ms": 14500, "p99_ms": 18700 }
  ]
}
```

### Example: Update latency config

```bash
curl -X POST -H "Authorization: Bearer <CLUSTER_API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"bucket_count": 6, "window_hours": 24}' \
  http://localhost:7501/v1/latency/config
```

## Latency Monitoring

The manager tracks per-worker processing latency correlated with payload size (byte length of serialized messages JSON). This data is aggregated per **role** into adaptive latency matrices.

### How it works

1. **Record** — On each completed work unit, the manager stores a latency sample: `(fingerprint, role, payload_bytes, latency_ms, timestamp)`. It also increments per-worker lifetime counters (`total_requests`, `total_latency_ms`).

2. **Hourly aggregation** — A background goroutine runs every hour:
   - Computes `min`, `max`, and `mean` payload size from that hour's samples (per role)
   - Stores the result in `latency_hourly_stats`
   - Deletes samples and hourly stats older than the configured window (default 48h)

3. **Adaptive bucket boundaries** — On read (cached, refreshed after each hourly tick):
   - `avg_min` = average of all `min_payload_bytes` in the window (per role)
   - `avg_max` = average of all `max_payload_bytes` in the window (per role)
   - Divide the `[avg_min, avg_max]` interval into N equal parts (configurable, default 4)
   - `bounds[i] = avg_min + (avg_max - avg_min) / N * (i + 1)` for i in 0..N-2

4. **Percentile computation** — For each bucket, query samples within the bucket's byte range and window:
   - Sort by `latency_ms`
   - Compute p50, p95, p99 from the sorted result

### Per-worker lifetime counters

The `workers` table gets two additional columns:
- `total_requests` — total completed work units (monotonically increasing)
- `total_latency_ms` — sum of all processing times (monotonically increasing)

On each hour boundary, the hourly delta is implicitly captured by the samples. The lifetime counters enable computing all-time averages without retaining expired data.

### Global aggregation

The latency matrix is computed **per role** (one matrix per role: embedding, inference, ingestion). The `/v1/latency` endpoint without a `role` filter returns all role matrices.

### Example: Bucket boundary computation

Given 48 hourly stats for role `inference`:
- `avg_min` = 512 bytes
- `avg_max` = 8192 bytes
- `bucket_count` = 4
- `bucket_size` = (8192 - 512) / 4 = 1920
- Bounds: `[2432, 4352, 6272]`
- Buckets: `[512-2432]`, `[2432-4352]`, `[4352-6272]`, `[6272+]`

### Example: List available model files

```bash
curl -H "Authorization: Bearer <CLUSTER_WORKER_KEY>" https://localhost:7500/v1/models/files
```

```json
{
  "models": [
    {
      "name": "mistral-small-3.2-24b-instruct-2506",
      "quantisation": "Q4_K_M",
      "filename": "mistral-small-3.2-24b-instruct-2506-Q4_K_M.gguf",
      "size": 4831829012
    }
  ],
  "total": 1
}
```

### Example: Download a model (resumable)

```bash
# Full download
curl -H "Authorization: Bearer <CLUSTER_WORKER_KEY>" \
  -o model.gguf \
  https://localhost:7500/v1/models/download/mistral-small-3.2-24b-instruct-2506-Q4_K_M.gguf

# Resume from byte offset (e.g., after interruption)
curl -H "Authorization: Bearer <CLUSTER_WORKER_KEY>" \
  -H "Range: bytes=1048576-" \
  -o model.gguf.part \
  https://localhost:7500/v1/models/download/mistral-small-3.2-24b-instruct-2506-Q4_K_M.gguf
```

### Example: Query cluster stats

```bash
curl -H "Authorization: Bearer <CLUSTER_API_KEY>" https://localhost:7500/v1/stats
```

```json
{
  "workers": { "total": 3, "online": 1 },
  "work_units": { "total": 847, "completed": 831, "failed": 12, "timeout": 4 }
}
```

### Example: List available models

```bash
curl -H "Authorization: Bearer <CLUSTER_API_KEY>" https://localhost:7500/v1/models
```

```json
{
  "object": "list",
  "data": [
    { "id": "mistral-small-3.2-24b-instruct-2506", "object": "model", "owned_by": "synergia" }
  ]
}
```

## Database (GORM — SQLite or PostgreSQL)

The cluster manager persists state via GORM with two backend options:

- **SQLite** (default): Zero-config, single-file database. Good for single-node / development.
- **PostgreSQL**: Set `CLUSTER_DB_DSN` for production deployments with multiple manager replicas.

Auto-migrated on startup — tables are created/altered automatically.

### Tables

| Table | Purpose |
|---|---|
| `workers` | Registered workers: fingerprint, public key, model, trust score, status |
| `work_units` | Work unit history: unit ID, assigned worker, status, processing time, errors |
| `worker_consents` | Worker consent state: fingerprint, accepted flag, accepted_at, data categories, hardware info (OS, GPU, GPU driver/version, CPU, RAM) |
| `worker_configs` | Worker configuration preferences: model size limits, quantisation, GPU memory, role |
| `client_errors` | Error reports from workers: fingerprint, version, error message, stack trace, timestamp |
| `latency_samples` | Raw latency observations: fingerprint, role, payload bytes, latency ms, timestamp (48h retention) |
| `latency_hourly_stats` | Hourly payload size aggregates per role: min, max, mean bytes, count (48h retention) |

### Schema

```sql
-- workers
CREATE TABLE workers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fingerprint TEXT UNIQUE NOT NULL,
  public_key TEXT,
  llm_model TEXT,
  quantisation TEXT,
  trust_score INTEGER DEFAULT 0,
  last_seen_at DATETIME,
  status TEXT DEFAULT 'offline',
  created_at DATETIME,
  updated_at DATETIME,
  deleted_at DATETIME
);

-- workers additional columns (added by auto-migration)
--   total_requests INTEGER DEFAULT 0
--   total_latency_ms BIGINT DEFAULT 0

-- work_units
CREATE TABLE work_units (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  unit_id TEXT UNIQUE NOT NULL,
  worker_fingerprint TEXT,
  llm_model TEXT,
  status TEXT,
  processing_time_ms INTEGER,
  error_message TEXT,
  created_at DATETIME,
  completed_at DATETIME,
  updated_at DATETIME,
  deleted_at DATETIME
);

-- latency_samples (48h rolling retention)
CREATE TABLE latency_samples (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fingerprint TEXT NOT NULL,
  role TEXT NOT NULL,
  payload_bytes INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE INDEX idx_latency_samples_role_created ON latency_samples(role, created_at);
CREATE INDEX idx_latency_samples_role_payload ON latency_samples(role, payload_bytes);

-- latency_hourly_stats (48h rolling retention)
CREATE TABLE latency_hourly_stats (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  role TEXT NOT NULL,
  hour DATETIME NOT NULL,
  count INTEGER NOT NULL,
  min_payload_bytes INTEGER NOT NULL,
  max_payload_bytes INTEGER NOT NULL,
  mean_payload_bytes INTEGER NOT NULL,
  UNIQUE(role, hour)
);
```

## Model Storage

The cluster manager serves GGUF model files to workers so they can auto-download the required model on first connection. Two storage backends are supported:

### Filesystem (default)

Store model files in a local directory. Ideal for Kubernetes deployments with persistent volume mounts or single-node setups.

```bash
# Place model files in the configured directory
cp mistral-small-3.2-24b-instruct-2506-Q4_K_M.gguf ./models/

# Set env (default is ./models)
export CLUSTER_MODEL_BACKEND=filesystem
export CLUSTER_MODEL_PATH=/data/models
```

In Kubernetes, mount a PersistentVolumeClaim or hostPath:

```yaml
volumes:
  - name: models
    persistentVolumeClaim:
      claimName: synergia-models
containers:
  - name: synergia-manager
    env:
      - name: CLUSTER_MODEL_BACKEND
        value: filesystem
      - name: CLUSTER_MODEL_PATH
        value: /models
    volumeMounts:
      - name: models
        mountPath: /models
```

### S3-compatible storage

Store model files in any S3-compatible object storage (AWS S3, MinIO, Scaleway Object Storage, etc.). Supports resumable downloads via `Range` header.

```bash
export CLUSTER_MODEL_BACKEND=s3
export CLUSTER_MODEL_S3_ENDPOINT=s3.fr-par.scw.cloud
export CLUSTER_MODEL_S3_BUCKET=synergia-models
export CLUSTER_MODEL_S3_REGION=fr-par
export CLUSTER_MODEL_S3_KEY=SCWXXXXXXXXX
export CLUSTER_MODEL_S3_SECRET=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
export CLUSTER_MODEL_S3_SSL=true
```

### Filename convention

Model files should follow the naming pattern:

```
{model-name}-{quantisation}.gguf
```

Examples:
- `mistral-small-3.2-24b-instruct-2506-Q4_K_M.gguf`
- `bge-m3-Q4_K_M.gguf`

The manager parses the filename to extract model name and quantisation for the listing endpoint.

## Build & Run

```bash
cd tools/synergia-manager
go run ./cmd/synergia-manager
```
