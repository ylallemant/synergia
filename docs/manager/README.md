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
- **Worker nickname**: stores optional display name from worker config sync, used on community leaderboard
- **Download page** (`GET /download` on API port): public HTML page with OS/arch auto-detection, serves pre-built client binaries with the manager URL patched in at download time (sentinel replacement)
- **Binary patching** (`GET /download/:os/:arch`): reads the generic client binary, replaces the `$$SYNERGIA_MANAGER_URL$$` sentinel with the manager's own WSS URL, streams the patched binary to the user
- **Install script** (`GET /install`): platform-aware shell script generated per-request with the manager's URL, downloads the correct binary and configures auto-start
- **Community stats page** (`GET /community` on API port): live public dashboard showing cluster health, compute power, workload, and contributor leaderboard (no auth required, aggregate data only)
- **Community stats API** (`GET /v1/community/stats`): public endpoint returning aggregate cluster metrics (workers online, TFLOPS, work units, leaderboard)

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
    ├── api/                             # Worker/client-facing handlers only
    │   ├── backend.go               # Backend download proxy
    │   ├── batch.go                 # OpenAI-compatible /v1/batches (create, retrieve, list, cancel)
    │   ├── branding.go              # Branding CSS API (served to worker dashboards)
    │   ├── community.go             # Community stats API (public, no auth)
    │   ├── completions.go           # OpenAI-compatible /v1/chat/completions handler
    │   ├── consent.go               # Consent + worker config API
    │   ├── download.go              # Client binary download with sentinel patching + install script
    │   ├── synergia.go              # Synergia API (models, workers, work-units, stats)
    │   ├── errors.go                # Client error reporting API (POST + GET /v1/errors)
    │   ├── models_download.go       # Model file listing + download endpoint
    │   └── roles.go                 # Role eligibility API (worker-facing)
    ├── admin/
    │   ├── api/                     # Admin-only handlers
    │   │   ├── backend.go           # Backend version management + tags API
    │   │   ├── branding.go          # Branding admin API
    │   │   ├── latency.go           # Latency monitoring admin API (GET /v1/latency, config)
    │   │   ├── oidc.go              # OIDC config read/save API
    │   │   ├── roles.go             # Role-model mapping admin API
    │   │   └── version.go           # Binary auto-update admin API
    │   ├── auth/                    # Session auth + OIDC flow
    │   └── server/                  # Admin HTTP server
    │       ├── server.go            # Admin listener (port 7501), login/logout routes
    │       └── static/              # Embedded HTML (dashboard + OIDC settings page)
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
    ├── public/
    │   └── static/                  # Embedded HTML for public pages (download, community)
    ├── store/
    │   ├── models.go                # GORM models (Worker, WorkUnit, WorkerConfig, RoleModel)
    │   ├── oidc.go                  # OidcConfig model (persists OIDC config set via admin UI)
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
| `CLUSTER_ADMIN_USER` | `admin` | Username for the admin dashboard login |
| `CLUSTER_ADMIN_PASSWORD` | `synergia` | Password for the admin dashboard login |
| `CLUSTER_OIDC_ENABLED` | `false` | Enable OIDC/SSO for admin login |
| `CLUSTER_OIDC_CLIENT_ID` | (empty) | OIDC client ID |
| `CLUSTER_OIDC_CLIENT_SECRET` | (empty) | OIDC client secret |
| `CLUSTER_OIDC_PROVIDER_URL` | (empty) | OIDC provider issuer URL |
| `CLUSTER_OIDC_REDIRECT_URL` | `http://localhost:7501/auth/oidc/callback` | OIDC redirect URL |

### Administration Port

The manager starts a **second HTTP listener** on `CLUSTER_ADMIN_ADDR` (default `:7501`) serving administration-only endpoints. This enables Kubernetes deployments to expose the main API and the admin API via separate Services and Ingresses with independent access rules (e.g., admin behind VPN / internal-only ingress).

The admin port serves:
- **Login page** (`/login`) — username/password form (default: `admin` / `synergia`, configurable via `CLUSTER_ADMIN_USER` / `CLUSTER_ADMIN_PASSWORD`)
- **Logout** (`/logout`) — ends the session
- **Admin dashboard** (`/`) — auto-refreshing HTML page showing worker counts, role distribution, today's payload stats, and recent client errors; includes a burger menu for navigation
- **OIDC/SSO settings** (`/admin/oidc`) — configure OIDC provider (Authentik, Keycloak, etc.); settings are persisted to DB and take effect on next startup
- **Latency API** (`/v1/latency`, `/v1/latency/config`) — latency matrix queries and configuration

The admin port inherits the same TLS settings as the main port. Browser access uses session-based login (24 h cookie). Admin API endpoints also accept `Authorization: Bearer <CLUSTER_API_KEY>` for programmatic access.

### Public Pages (on API port)

The main API port also serves public pages accessible **without authentication**:

#### Download Page (`GET /download`)

A public page for distributing pre-configured client binaries:
- Detects visitor's OS and architecture via `navigator.userAgentData` (with User-Agent fallback)
- Shows a primary download button for the detected platform (e.g. "Download for macOS Apple Silicon")
- "Show all platforms" expander for other OS/arch combinations
- "Copy install command" button for CLI users

#### Binary Download (`GET /download/:os/:arch`)

Serves the client binary with the manager URL patched in:
1. Reads the pre-built generic binary from the configured artifact directory
2. Finds the `$$SYNERGIA_MANAGER_URL$$` sentinel (256 bytes, null-padded)
3. Replaces it with `wss://<request-host>/ws/worker` (null-padded to same length)
4. Streams the patched binary as a download

Valid OS values: `darwin`, `linux`, `windows`. Valid arch values: `amd64`, `arm64`.

#### Install Script (`GET /install`)

A platform-aware shell script generated per-request:
- Detects OS and arch from the request headers or query params
- Downloads the correct patched binary from the same manager
- Writes a config file with the manager URL and worker key (if provided via `?key=` query param)
- Sets up auto-start (macOS LaunchAgent / Linux systemd user service)

```bash
curl -sSL https://cluster.example.com:7500/install | sh
curl -sSL https://cluster.example.com:7500/install?key=<worker-key> | sh
```

#### Community Stats Page (`GET /community`)

A live public dashboard showing cluster health and contributor rankings:

| Section | Content |
|---|---|
| **Cluster Overview** | Workers online, total registered, cluster uptime |
| **Compute Power** | Total TFLOPS (FP16), aggregate VRAM, estimated tokens/sec |
| **Workload** | Requests today, avg latency (p50), batch queue depth |
| **Live Activity** | Requests/min sparkline, workers processing vs idle |
| **Contributions** | Total work units (all time), total tokens generated |
| **Hardware** | GPU breakdown (Apple Silicon / NVIDIA / AMD / Intel) |
| **Leaderboard** | Top contributors by work units (nickname + compute time) |

Data sourced from `GET /v1/community/stats` (public, no auth). Uses JS polling (2s interval). Only aggregate data is exposed — no fingerprints, no IPs.

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

Served on the dedicated admin listener. Authenticated via session cookie (browser) or `Authorization: Bearer <CLUSTER_API_KEY>` (programmatic).

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

### OIDC Configuration Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/admin/oidc` | Read current OIDC configuration (from DB or env vars) |
| `PUT` | `/v1/admin/oidc` | Save OIDC configuration to DB (takes effect on next startup) |

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

## TODO / Roadmap

The items below are the manager-side counterparts of the security roadmap defined in [docs/client/README.md](../client/README.md#todo--roadmap). They are listed in the same order and require coordinated changes on both sides.

---

### Challenge-Response Worker Handshake (replaces `CLUSTER_WORKER_KEY`)

**Current state**: the manager authenticates workers by comparing `Authorization: Bearer <CLUSTER_WORKER_KEY>` on WebSocket upgrade. This shared secret must be pre-distributed.

**Target**: on WebSocket upgrade, before accepting the connection, the manager:

1. Generates a 32-byte random challenge and sends it to the connecting worker
2. Receives back the worker's signature, long-term Ed25519 public key, and fingerprint
3. Verifies `SHA256(pubKey) == fingerprint` and `Ed25519.Verify(pubKey, challenge, signature)`
4. **First connection** (TOFU): registers the `fingerprint → public_key` mapping in the `workers` table; assigns trust score 0
5. **Subsequent connections**: verifies the public key matches the registered one for that fingerprint; rejects on mismatch

Once in place, `CLUSTER_WORKER_KEY` becomes optional — kept as a fallback for deployments that prefer an allowlist model, but no longer required.

**Manager-side work**:
- `internal/manager/gateway/websocket.go` — send challenge before upgrade, verify signed response, TOFU registration
- `internal/manager/store/store.go` — ensure `public_key` column is set on first TOFU registration
- `internal/manager/config/config.go` — make `WorkerKey` optional (currently fatal if missing)
- Remove `Authorization: Bearer <CLUSTER_WORKER_KEY>` checks from all worker-authenticated HTTP handlers: `branding`, `consent`, `errors`, `models_download`, `roles` (worker path), `version`, `backend` download proxy

---

### Named API Keys (replaces single `CLUSTER_API_KEY`)

**Current state**: a single `CLUSTER_API_KEY` gates all inference API access. It is configured at startup and cannot be rotated without a restart.

**Target**: admin-managed named API keys stored in the database.

- New `api_keys` table: `id`, `name` (label), `key_hash` (bcrypt/Argon2 of the raw key), `created_at`, `expires_at` (nullable), `last_used_at`
- Admin dashboard page (`/admin/api-keys`): create key (raw value shown once), list active keys, revoke individual keys
- New admin API: `GET/POST/DELETE /v1/admin/api-keys`
- Auth middleware updated: on each inference request, look up the presented Bearer token hash in the `api_keys` table instead of comparing to a single env var
- `CLUSTER_API_KEY` kept as a legacy fallback during migration, deprecated

---

### Publish Manager Public Key for Worker Pinning

**Current state**: workers have no way to verify they are connected to the legitimate manager (only the manager verifies workers).

**Target**: the manager has a long-term Ed25519 keypair. Its public key is advertised during the WebSocket handshake (alongside the challenge) and also served at a well-known endpoint:

```
GET /.well-known/synergia-pubkey
→ { "public_key": "base64(ed25519_pubkey)", "version": "1" }
```

Workers store this key on first connect (`manager.pub` in their data directory) and verify it on every subsequent connection. The manager's private key is loaded from a key file at startup (similar to TLS cert/key) or auto-generated and persisted.

**Manager-side work**:
- Key generation / loading at startup (`internal/manager/admin/auth/` or a new `identity/` package)
- Include public key in WebSocket upgrade response headers
- Register `GET /.well-known/synergia-pubkey` on the main API port (no auth required)

---

### Sign Manager → Worker Push Messages

**Current state**: `model_update`, `binary_update`, and `backend_update` messages are sent without a signature — workers trust them implicitly because they arrive on an authenticated WebSocket.

**Target**: every push message is signed with the manager's long-term Ed25519 private key before dispatch:

```go
payload = canonical(type + role + filename + expected_hash + unix_timestamp)
message.Signature = base64(Ed25519.Sign(manager_privkey, payload))
message.Timestamp = unix_timestamp   // included in signature; workers reject if stale > 30s
```

Workers verify the signature against the pinned manager public key before applying any update. A failed verification is logged as an anomaly and reported to the manager via `POST /v1/errors`.

**Manager-side work**:
- `internal/manager/gateway/websocket.go` — sign all outgoing push messages
- Add `signature` and `timestamp` fields to push message types in `internal/manager/protocol/messages.go`

---

### Verify Worker Result Signatures

**Current state**: workers already sign every result payload with their Ed25519 private key and include the signature in the WebSocket message. The manager receives it but **does not verify it**.

**Target**: on every result message, the manager verifies:

```go
Ed25519.Verify(
  worker.PublicKey,                            // from workers table (set during TOFU)
  canonical(result.ID + result.Output + result.ProcessingTimeMs),
  result.Signature,
)
```

A failed verification rejects the result (returns an error to the caller), increments a `signature_failures` counter on the worker record, and logs the anomaly. Repeated failures can trigger automatic trust score reduction.

**Manager-side work**:
- `internal/manager/gateway/websocket.go` — verify signature on result receipt
- `internal/manager/store/store.go` — add `signature_failures` column to `workers` table

---

### Sign Work Units Before Dispatch

**Current state**: work units are dispatched to workers without a manager signature — a MITM with WebSocket access could inject arbitrary work units.

**Target**: before dispatching a work unit, the manager signs it:

```go
payload = canonical(wu.ID + wu.Messages + wu.Params + unix_timestamp)
wu.ManagerSignature = base64(Ed25519.Sign(manager_privkey, payload))
```

Workers verify the signature against the pinned manager public key before forwarding the unit to `llama-server`. Unsigned or invalidly-signed units are rejected and reported.

Combined with result signing, both signatures are stored in the `work_units` table (`manager_signature`, `worker_signature`). The API response to the caller optionally includes both, enabling end-to-end audit without trusting the manager's word.

**Manager-side work**:
- `internal/manager/gateway/websocket.go` — sign work units before dispatch
- `internal/manager/store/store.go` — add `manager_signature`, `worker_signature` columns to `work_units`
- `internal/manager/api/completions.go`, `batch.go` — optionally surface signatures in API response headers

---

### Verify Signed Consent and Config Payloads

**Current state**: `POST /v1/consent` and `POST /v1/worker-config` are authenticated only by `CLUSTER_WORKER_KEY`. Once that is replaced by challenge-response (see above), the WebSocket session proves worker identity — but HTTP endpoints need a separate mechanism.

**Target**: workers include an `X-Worker-Signature` header on consent and config sync requests:

```
X-Worker-Signature: base64(Ed25519.Sign(worker_privkey, canonical(method + path + body + unix_timestamp)))
```

The manager verifies the signature against the worker's registered public key (from the TOFU registration) before storing the record. This provides:
- **Non-repudiation of consent**: the stored `worker_consents` row proves the holder of that private key accepted the terms
- **Config integrity**: role preference changes are attributable to the authenticated worker, not a process that merely knows the fingerprint

**Manager-side work**:
- `internal/manager/api/consent.go` — verify `X-Worker-Signature` on POST
- `internal/manager/api/roles.go` (worker path) — verify on config sync
- `internal/manager/store/store.go` — store raw signature in `worker_consents` table (`consent_signature` column)

---

### Operator-Signed Release Artifact Manifest

**Current state**: when the manager proxies or serves binary/backend updates, it provides the SHA256 hash to workers. Workers trust that hash because it came from the manager — a compromised manager can serve a malicious binary with a matching hash.

**Target**: the cluster operator maintains an offline Ed25519 signing key. At release time, a manifest is signed:

```json
// manifest.json
{ "version": "v1.2.3", "artifacts": [
    { "os": "linux", "arch": "amd64", "sha256": "abc..." },
    { "os": "darwin", "arch": "arm64", "sha256": "def..." }
] }
// manifest.sig = Ed25519.Sign(operator_offline_privkey, manifest.json)
```

Both files ship alongside each release (e.g., as GitHub release assets). The manager:
1. Fetches manifest + signature when caching a new release
2. Verifies the signature against the operator's public key (configured via `CLUSTER_OPERATOR_PUBKEY` env var or file)
3. Serves the verified manifest to workers alongside cached binaries

Workers pin the operator's public key at install time (patched into the binary as a sentinel, alongside the manager URL). The manager becomes a **verified transport** — it cannot forge artifact hashes without the operator's offline private key.

**Manager-side work**:
- `internal/manager/cache/github.go` — fetch and verify manifest + signature when caching releases
- `internal/manager/config/config.go` — add `CLUSTER_OPERATOR_PUBKEY` (path or base64 inline)
- `internal/manager/api/backend.go`, `version.go` — serve manifest alongside download proxy responses

---

### Container and Deployment Hardening

Security improvements for the container image and Kubernetes deployment. The items below complement the protocol-level TODOs above.

#### Already done

| What | How |
|---|---|
| Statically linked binary | `CGO_ENABLED=0` — no libc dependency |
| Stripped binary | `-ldflags="-s -w"` — no debug symbols or DWARF |
| Distroless final image | `gcr.io/distroless/static-debian12:nonroot` — no shell, no curl, no package manager, CA certs included |
| Non-root process | Distroless `:nonroot` runs as uid 65532 by default |
| Ports declared | `EXPOSE 7500 7501` |

#### TODO: read-only root filesystem

The manager only writes to the database path and the backend cache directory. The rest of the filesystem can be immutable at runtime.

Kubernetes:
```yaml
securityContext:
  readOnlyRootFilesystem: true
volumeMounts:
  - name: data     # SQLite DB + cache
    mountPath: /data
  - name: tmp
    mountPath: /tmp  # tmpfs emptyDir
```

Set `CLUSTER_DB_PATH=/data/synergia-manager.db` and `CLUSTER_CACHE_DIR=/data/cache` to redirect all writes to the mounted volume.

#### TODO: secret injection — never bake secrets into the image

`CLUSTER_API_KEY`, `CLUSTER_WORKER_KEY`, TLS private key (`TLS_KEY_FILE`), and the future operator signing key must not appear in:
- `docker run -e` (visible in `docker inspect`)
- `docker-compose.yml` committed to git
- Kubernetes `Deployment` env blocks in plaintext

Preferred approach: Kubernetes `Secret` → `envFrom` or mounted file reference:
```yaml
envFrom:
  - secretRef:
      name: synergia-manager-secrets
```
Or mount secrets as files and point env vars to the paths. For production, integrate with a secrets manager (Vault, AWS Secrets Manager, Doppler) rather than native Kubernetes secrets.

#### TODO: drop all Linux capabilities

```yaml
securityContext:
  capabilities:
    drop: ["ALL"]
```

The manager binds to ports 7500 and 7501 (above 1024 — `CAP_NET_BIND_SERVICE` not needed). No capabilities are required at all.

#### TODO: image signing (Cosign / Sigstore)

Sign each released image with the operator's offline key:
```bash
cosign sign --key operator.key ghcr.io/ylallemant/synergia-manager:v1.2.3
```

Enforce in Kubernetes via Kyverno or OPA Gatekeeper so only signed images can run. Uses the same operator key infrastructure as the operator-signed release artifacts TODO — one offline key signs both container images and binary manifests.

#### TODO: network segmentation — admin port internal only

The admin port (7501) must not be internet-facing. Enforce at the infrastructure layer, not just by convention:

**Docker Compose**:
```yaml
ports:
  - "0.0.0.0:7500:7500"    # public
  - "127.0.0.1:7501:7501"  # loopback only
```

**Kubernetes**: expose port 7500 via a public `LoadBalancer`/`Ingress`; expose port 7501 via a separate `ClusterIP` service with a `NetworkPolicy` restricting ingress to ops pods or a VPN-gated ingress only.

#### TODO: enable RuntimeDefault seccomp profile

```yaml
securityContext:
  seccompProfile:
    type: RuntimeDefault
```

Kubernetes `RuntimeDefault` (available since 1.19, opt-in) blocks the most dangerous syscalls (`ptrace`, `mount`, `pivot_root`, `setuid`, etc.) with no per-application profiling required. A one-line change with meaningful blast-radius reduction if a vulnerability leads to RCE.

## Build & Run

```bash
cd tools/synergia-manager
go run ./cmd/synergia-manager
```
