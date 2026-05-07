# Synergia Client

Worker daemon that connects to the Cluster Manager via WebSocket, receives LLM work units, runs inference locally using `llama-server`, and returns results.

See [Architecture](../architecture.md) for the full design.

## Phase 1 — Proof of Concept

Minimal implementation: connects to a single cluster manager, shells out to a local `llama-server` instance for inference. Includes local web dashboard, system tray, and auto-start.

### Scope

- WebSocket client connecting to cluster manager (`/ws/worker`)
- **Worker identity**: generates an Ed25519 keypair on first run, stores it encrypted locally, derives a fingerprint ID for all communication
- Forwards work units to a local `llama-server` OpenAI-compatible endpoint
- Returns results back over WebSocket
- Signs result payloads with the worker's private key
- Heartbeat keepalive (every 30s)
- Automatic reconnection with exponential backoff
- Configuration via CLI flags and/or environment variables
- Reports model name + quantisation + fingerprint at connection time
- **GPU activity monitoring**: detects external GPU usage (gaming, rendering) via platform APIs (NVML, Metal, ROCm SMI) and transitions to `idle` state, informing the manager that the worker is unavailable. Resumes when GPU contention ends
- **LLM hash verification**: computes a deterministic hash (`SHA256(role + ":" + SHA256(model_file))`) from the actual model file on disk and the assigned role. Reports hash to the manager on connect and after model updates, enabling the manager to verify model integrity without trusting the worker's self-report
- **Model update handling**: receives `model_update` pushes from the manager when a role-model mapping changes. Downloads the new model file, verifies its SHA256 hash, computes the new LLM hash, and reports back. Transitions through `updating` → `available` status during the process
- **LLM health monitoring**: periodic background health checks against the local `llama-server` endpoint with reachability state exposed to the dashboard
- **Data collection consent**: interactive or auto-approve; syncs consent state with the cluster manager (work units are gated on consent)
- **Worker configuration**: processing preferences (max model size, quant, context, preferred role) stored locally and synced to manager
- **Local web dashboard** (`http://127.0.0.1:9876`): connection status, consent banner, config form (role auto-syncs on change — no save button), hardware info review, live statistics, auto-start toggle
- **Auto-start**: register/unregister the client to start on OS login (macOS LaunchAgent, Linux systemd user service); status read directly from OS — no config file
- **System tray**: macOS/Linux tray icon showing connection state (🟢/🔵/🟡/⚫/🔴) with Pause/Resume/Quit menu
- **Hardware info collection**: OS, CPU, GPU, VRAM, RAM — reported to manager after consent
- **Branding**: fetches custom CSS from cluster manager with periodic refresh and disk cache
- **Error reporting**: catches errors and panics during work unit processing, deduplicates by message hash (1-hour cooldown), reports to manager via `POST /v1/errors` with version and stack trace
- **Client version**: reports version to manager via `X-Worker-Version` header on WebSocket connection
- **Platform reporting**: reports `X-Worker-OS` and `X-Worker-Arch` headers (`runtime.GOOS`/`runtime.GOARCH`) on WebSocket connect, enabling the manager to resolve platform-specific binary artifacts
- **Binary auto-update**: receives `binary_update` push from manager, downloads new binary (GitHub releases with manager proxy fallback), verifies SHA256, self-replaces with atomic rename (Unix) or helper shim (Windows), restarts. Previous binary kept as `.bak` for rollback if reconnect fails within 60s
- **Backend auto-update**: receives `backend_update` push from manager, downloads the full release archive (tar.gz/zip) from upstream or manager fallback, extracts all files (binaries, shared libraries, symlinks) into the backend directory, restarts `llama-server` with the new binary
- **Windows update helper**: separate `synergia-updater.exe` handles locked-file replacement on Windows. Downloaded from the same release on first need; version kept in sync with client

### Out of Scope (Phase 2+)

- Embedded `llama.cpp` (Phase 1 shells out to existing `llama-server`)
- Multi-model support (Phase 1 = one model loaded at a time)
- mTLS certificates (Phase 1 uses TLS + shared key auth)

## Project Structure

```
cmd/synergia-client/ + internal/client/
├── go.mod
├── README.md
├── Dockerfile
├── cmd/
│   └── synergia-client/
│       └── main.go                  # Entrypoint, CLI flags, tray + sync orchestration
└── internal/
    ├── config/
    │   └── config.go                # CLI flag + env configuration
    ├── identity/
    │   └── identity.go              # Keypair generation, encrypted storage, fingerprint derivation
    ├── connection/
    │   └── websocket.go             # WSS client, reconnect logic, heartbeat, TLS config
    ├── worker/
    │   └── worker.go                # Work unit processing loop
    ├── llm/
    │   └── client.go                # HTTP client to local llama-server (/v1/chat/completions)
    ├── consent/
    │   └── consent.go               # Consent state management (local + sync to manager)
    ├── workerconfig/
    │   └── config.go                # Worker config preferences (local + sync to manager)
    ├── autostart/
    │   ├── autostart.go              # Autostart manager (shared struct)
    │   ├── autostart_darwin.go       # macOS LaunchAgent plist
    │   ├── autostart_linux.go        # Linux systemd user service
    │   ├── autostart_windows.go      # Windows Registry Run key
    │   └── autostart_other.go        # Fallback (unsupported)
    ├── errorreporter/
    │   └── reporter.go              # Error catching, dedup, and async reporting to manager
    ├── version/
    │   └── version.go               # Client version (set via ldflags or hardcoded)
    ├── branding/
    │   └── branding.go              # Fetch and cache cluster branding CSS
    ├── hwinfo/
    │   └── hwinfo.go                # Hardware info collection (OS, CPU, GPU, RAM)
    ├── gpu/
    │   ├── monitor.go               # GPU activity monitor (state machine + compatibility check)
    │   ├── prober_darwin.go          # macOS GPU prober (IOKit + process detection)
    │   ├── prober_linux.go           # Linux GPU prober (nvidia-smi, rocm-smi)
    │   ├── prober_windows.go         # Windows GPU prober (nvidia-smi)
    │   └── prober_other.go           # Fallback (noop)
    ├── backend/
    │   └── manager.go               # Backend binary management (download, extract, verify, restart)
    ├── server/
    │   ├── server.go                # Local dashboard HTTP server (:9876)
    │   └── static/                  # Embedded HTML + CSS for dashboard
    ├── protocol/
    │   └── messages.go              # Work unit / result message types (JSON)
    ├── status/
    │   └── status.go                # Aggregated status provider (atomic counters)
    └── tray/
        ├── tray.go                  # System tray integration (fyne.io/systray)
        └── icons.go                 # Generated tray icons (5 connection states)
```

## Configuration

| Flag / Env | Default | Description |
|---|---|---|
| `--manager-url` / `CLUSTER_MANAGER_URL` | `wss://localhost:7500/ws/worker` | WebSocket URL of the cluster manager |
| `--worker-key` / `CLUSTER_WORKER_KEY` | (required) | Shared secret for WebSocket auth |
| `--llm-url` / `WORKER_LLM_URL` | `http://localhost:8080` | Local `llama-server` endpoint |
| `--model` / `WORKER_MODEL` | (required) | Model name to report (e.g., `mistral-small-3.2-24b-instruct-2506`) |
| `--quantisation` / `WORKER_QUANTISATION` | `Q4_K_M` | Quantisation level to report |
| `--role` / `WORKER_ROLE` | `tester` | Worker role (`tester`, `embedding`, `inference`, `ingestion`) |
| `--model-file` / `WORKER_MODEL_FILE` | (empty) | Path to the GGUF model file (for LLM hash verification) |
| `--data-dir` / `CLUSTER_CLIENT_DATA_DIR` | `~/.synergia/worker/` | Directory for identity keystore and local state |
| `--auto-approve` / `CLUSTER_CLIENT_AUTO_APPROVE` | `false` | Automatically accept data collection consent on startup (for tests/CI) |
| `--insecure` / `CLUSTER_INSECURE` | `false` | Connect without TLS (`ws://` instead of `wss://`). Logs a warning on startup. |
| `--tls-ca-cert` / `TLS_CA_CERT` | (empty) | Path to CA certificate (PEM) for verifying the manager's TLS cert (e.g., for self-signed certs) |

### TLS (default)

TLS is the default transport. The client connects via `wss://` and validates the manager's certificate against the system trust store (or a custom CA when `--tls-ca-cert` is specified).

When running in insecure mode (`--insecure`), the client logs:
```
WARN  TLS disabled — running in insecure mode (traffic is unencrypted)
```

## GPU Platform Support

The client monitors GPU utilization to detect contention from other processes (gaming, rendering). When utilization exceeds the threshold, the worker transitions to `idle` and stops accepting work.

| OS | GPU Vendor | Driver / Tool | Detection Method |
|---|---|---|---|
| **macOS** | Apple Silicon / Intel | Metal (ioreg) | `ioreg -r -c IOAccelerator` GPU utilization keys + process detection |
| **Linux** | NVIDIA | `nvidia-smi` (NVML) | `--query-gpu=utilization.gpu` per-GPU max |
| **Linux** | AMD | `rocm-smi` (ROCm) | `--showuse --json` GPU utilization percentage |
| **Linux** | Intel | `intel_gpu_top` (i915) | `-J -s 500 -l 1` engine busy percentage |
| **Linux** | Moore Threads | `mthreads-gmi` (MUSA) | `--query-gpu=utilization.gpu` |
| **Windows** | NVIDIA | `nvidia-smi` (NVML) | `--query-gpu=utilization.gpu` per-GPU max |
| **Windows** | Intel / AMD / any | WDDM 2.0+ (typeperf) | `\GPU Engine(*engtype_3D)\Utilization Percentage` |
| **Windows** | Moore Threads | `mthreads-gmi` (MUSA) | `--query-gpu=utilization.gpu` |

### Driver Version Detection

The client also reports the GPU driver name and version to the cluster manager as part of hardware info:

| OS | GPU Vendor | How version is detected |
|---|---|---|
| macOS | Apple | `system_profiler SPDisplaysDataType` Metal family |
| Linux | NVIDIA | `nvidia-smi --query-gpu=driver_version` |
| Linux | AMD | `rocm-smi --showdriverversion` |
| Linux | Intel | `modinfo -F version i915` (fallback: kernel version) |
| Linux | Moore Threads | `mthreads-gmi --query-gpu=driver_version` |
| Windows | NVIDIA | `nvidia-smi --query-gpu=driver_version` |
| Windows | Moore Threads | `mthreads-gmi --query-gpu=driver_version` |
| Windows | Intel / AMD / any | PowerShell `Get-CimInstance Win32_VideoController` |

If no GPU monitoring tool is found but Vulkan is available, the client logs a warning explaining which tool to install. On unsupported platforms (e.g., FreeBSD), GPU monitoring is disabled with a noop prober.

## LLM Hash & Model Update

### LLM Hash

The LLM hash is a deterministic fingerprint of the worker's current model configuration:

```
llmHash = SHA256(role + ":" + SHA256(model_file_bytes))
```

This binds the worker's identity to both its assigned **role** and the exact **model file** on disk. The manager uses this to verify that a worker has the correct model loaded without needing to inspect the file itself.

The hash is:
- Computed on startup from `--model-file` and `--role`
- Sent to the manager via `X-Worker-LLM-Hash` header on WebSocket connect
- Included in every `status` message (so the manager can re-verify on each state change)
- Recomputed and reported after a model update

### Model Update Flow

When the cluster manager admin changes a role-model mapping, the manager pushes a `model_update` message to all connected workers assigned to that role:

```
Manager                          Worker
   │                                │
   │── model_update ───────────────▶│  (role, model, quantisation, filename, expected_hash)
   │                                │── status: "updating"
   │◀── status: updating ───────────│
   │                                │── download model file from /v1/models/download/{filename}
   │                                │── SHA256(downloaded_file) → verify against expected_hash
   │                                │── llmHash = SHA256(role + ":" + file_hash)
   │◀── llm_hash_report ────────────│  (new llmHash)
   │                                │── status: "available"
   │◀── status: available ──────────│
   │                                │
```

During the update, the worker is in `updating` state and will not receive work units. If the download fails or the file hash doesn't match, the worker reports an error and returns to `available` (but remains `out-of-sync` until the model is corrected).

## Consent & Configuration

### Data Collection Consent

Before the cluster manager will dispatch work units to this worker, the user **must accept** the collection and centralisation of:

- **Hardware statistics** — OS + version, GPU + VRAM, CPU + core count, RAM
- **Configuration preferences** — preferred role

This consent is tied to the worker's cryptographic fingerprint. The consent state is:
1. Stored locally in `<data-dir>/consent.json`
2. Synced to the cluster manager via `POST /v1/consent`

Without consent, the worker connects and stays online but receives **no work units**.

### Auto-Approve Flag

For automated tests and CI environments, use `--auto-approve` to skip the interactive consent step:

```bash
go run ./cmd/synergia-client --auto-approve ...
```

This immediately sets consent to accepted on startup and syncs with the manager.

### Worker Configuration

After accepting consent, the worker can configure its preferred role:

| Field | Description |
|---|---|
| `preferred_role` | Preferred task type: `tester`, `embedding`, `inference`, or `ingestion` |

The **tester** role is always eligible regardless of hardware — it uses a minimal model (SmolLM2-135M) and allows any machine to participate in the cluster. If the worker's VRAM is insufficient for other roles, the client preselects "tester" automatically.

For other roles, eligibility is determined by the cluster manager based on the worker's reported VRAM and the manager-controlled role-model mapping. The worker can only select roles it is eligible for.

Configuration is stored locally in `<data-dir>/config.json` and synced to the manager via `POST /v1/worker-config`.

### Auto-Start on Login

The dashboard includes a "Start on login" toggle that registers/unregisters the client to start automatically when the user logs in to their OS.

**Status is read directly from the OS** — there is no config file or manager-side setting. The client queries the actual OS registration state each time the dashboard loads.

| Platform | Mechanism | Location |
|---|---|---|
| macOS | LaunchAgent plist | `~/Library/LaunchAgents/com.deepthink.synergia-client.plist` |
| Linux | systemd user service | `~/.config/systemd/user/synergia-client.service` |
| Windows | Registry Run key | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` |
| Other | Not supported | Toggle hidden in dashboard |

When enabled, the plist/service is created with `KeepAlive`/`Restart=on-failure` so the worker restarts automatically if it crashes.

The auto-start registration uses the **current binary path** and **current CLI arguments** — if you move the binary or change flags, disable and re-enable auto-start.

### Local Dashboard

The worker exposes a local web dashboard at `http://127.0.0.1:9876` showing:

- Connection status (with consent-aware indicators)
- **Consent banner** — must be accepted before the worker receives work
- **"Review Data Sent" button** — opens an overlay showing the exact JSON payload sent to the cluster manager, so the user can inspect what data is being shared
- **Configuration form** — editable preferences synced with the cluster manager
- Worker info (OS, GPU + VRAM, CPU + cores, RAM, model, quantisation, GPU state)
- Statistics (units processed, uptime)
- Identity (fingerprint)

### Hardware Info API

The dashboard exposes `GET /api/hardware-info` which returns the full payload that would be sent to the cluster manager:

```json
{
  "fingerprint": "a1b2c3...",
  "hardware": {
    "os": "darwin",
    "os_version": "15.4.1",
    "gpu": "Apple M2 Pro",
    "gpu_driver": "metal",
    "gpu_driver_version": "Metal GPUFamily Apple 9",
    "vram_mb": 32768,
    "cpu": "Apple M2 Pro",
    "cpu_cores": 12,
    "ram_mb": 32768
  },
  "config": {
    "preferred_role": "inference"
  }
}
```

### Storage Layout (updated)

```
~/.synergia/worker/
├── identity.enc          # AES-256-GCM encrypted Ed25519 private key
├── identity.pub          # Ed25519 public key (PEM)
├── fingerprint           # Plain text fingerprint (SHA256 hex of public key)
├── consent.json          # Consent state (accepted, hardware_stats, config_preferences)
└── config.json           # Worker configuration preferences
```

## Worker Identity (Fingerprint)

On **first run**, the client generates a persistent cryptographic identity:

1. **Generate Ed25519 keypair** — fast, small keys (32 bytes), suitable for signing
2. **Derive fingerprint** — `SHA256(public_key)` encoded as hex (64 chars). This is the worker's unique ID across all communication with the cluster.
3. **Encrypt and store** — the private key is encrypted at rest using AES-256-GCM with a key derived from a machine-local secret (hostname + OS user + MAC address, passed through Argon2id). Stored in `<data-dir>/identity.enc`.
4. **Public key export** — the public key is stored unencrypted in `<data-dir>/identity.pub` for easy inspection.

### Storage Layout

```
~/.synergia/worker/
├── identity.enc          # AES-256-GCM encrypted Ed25519 private key
├── identity.pub          # Ed25519 public key (PEM)
└── fingerprint           # Plain text fingerprint (SHA256 hex of public key)
```

### Usage in Protocol

- **Connection**: fingerprint is sent as `X-Worker-Fingerprint` header during WebSocket handshake
- **Result signing**: every result payload is signed with the private key. The manager can verify using the registered public key.
- **Identity persistence**: the same fingerprint survives restarts, reconnections, and IP changes. The cluster manager tracks workers by fingerprint, not by connection.

### Key Rotation

If the identity files are deleted, a new keypair is generated on next start — the worker appears as a new worker to the cluster (starts at trust score 0). This is intentional: identity loss = trust loss.

## Backend Management

The client manages the local `llama-server` backend binary lifecycle. When the cluster manager pushes a `backend_update` message (containing a version and download URL), the client:

1. **Downloads the archive** — tries the direct URL first (e.g. GitHub release), falls back to the manager's caching proxy (`/v1/backend/download?version=...&os=...&arch=...`)
2. **Extracts all files** — the archive (tar.gz on Unix, zip on Windows) is extracted flat into `<data-dir>/backend/`. All regular files and symlinks are preserved. This is critical because `llama-server` links against companion shared libraries (e.g. `libllama-common.0.dylib` on macOS)
3. **Verifies SHA256** — if the manager provides an expected hash, the extracted binary is validated
4. **Restarts llama-server** — the worker stops the running `llama-server` process and starts the new binary

### Archive Structure

llama.cpp release archives contain a subdirectory (e.g. `llama-b9049/`) with:
- `llama-server` (the main binary)
- Shared libraries (e.g. `libllama.0.0.9049.dylib`, `libggml-metal.0.11.0.dylib`)
- Symlinks (e.g. `libllama-common.0.dylib` → `libllama-common.0.0.9049.dylib`)

The extractor flattens this structure, placing all files as `basename` in the backend directory. Symlinks are recreated pointing to the target's basename.

### Storage Layout

```
<data-dir>/backend/
├── llama-server                    # Main binary
├── libllama-common.0.dylib         # Symlink → libllama-common.0.0.9049.dylib
├── libllama-common.0.0.9049.dylib  # Actual shared library
├── libllama.0.0.9049.dylib
├── libggml-metal.0.11.0.dylib
└── ...                             # Other libraries and binaries
```

### Platform Details

| OS | Archive Format | Library Extension | Loader |
|---|---|---|---|
| macOS | `.tar.gz` | `.dylib` | `@rpath` resolves to binary directory |
| Linux | `.tar.gz` | `.so` | `LD_LIBRARY_PATH` or `$ORIGIN` rpath |
| Windows | `.zip` | `.dll` | Same directory as executable |

## Prerequisites (Phase 1)

The worker daemon does **not** embed `llama.cpp`. You need a running `llama-server` instance:

```bash
# Start llama-server separately (example)
llama-server \
  --model ~/.synergia/models/mistral-small-3.2-24b-instruct-2506-Q4_K_M.gguf \
  --port 8080 \
  --ctx-size 8192 \
  --n-gpu-layers 99
```

The cluster client then connects to this local server and proxies work units to it.

### LLM Health Monitoring

The client continuously monitors the reachability of the configured `llama-server` endpoint (every 10 seconds via `GET /health`). If the server is unreachable:

- **System tray**: shows a yellow warning icon with tooltip "LLM Unreachable: <error>"
- **Dashboard API**: `/api/status` includes `"llm_reachable": false` and `"llm_error": "..."`
- **Logs**: logs a warning on transition from reachable → unreachable, and an info message when it recovers

The client does **not** exit or refuse to start when `llama-server` is unavailable — it starts normally, connects to the cluster manager, but will fail work units until the LLM server is available. This allows the user to start `llama-server` after the client is already running.

## Processing Loop

```
┌─────────────────────────────────────────────────┐
│                 Cluster Client                   │
│                                                 │
│  1. Connect WSS to cluster manager              │
│  2. Wait for work_unit message                  │
│  3. Forward messages[] to local llama-server    │
│     POST http://localhost:8080/v1/chat/completions
│  4. Read response                               │
│  5. Send result message back over WSS           │
│  6. Go to 2                                     │
│                                                 │
│  (heartbeat every 30s in background goroutine)  │
└─────────────────────────────────────────────────┘
```

## GPU Activity Monitoring

The worker daemon continuously monitors GPU usage to avoid impacting the user's primary workload (gaming, video editing, 3D rendering).

### Detection

| Platform | Tool | Driver Name |
|---|---|---|
| **NVIDIA** (Linux/Windows) | `nvidia-smi` | `nvidia` |
| **AMD** (Linux) | `rocm-smi` | `amdgpu` |
| **AMD** (Windows) | Windows GPU performance counters (WDDM) | `amd` |
| **Intel** (Linux) | `intel_gpu_top` | `i915` |
| **Intel** (Windows) | Windows GPU performance counters (WDDM) | `intel` |
| **Moore Threads / MUSA** (Linux/Windows) | `mthreads-gmi` | `musa` |
| **macOS (Metal)** | `ioreg` IOAccelerator + process detection | `metal` |
| **Vulkan only** | `vulkaninfo` (detection only, no monitoring) | — |

The GPU prober also reports the **driver name** and **driver version** (e.g., `nvidia` / `535.129.03`), which are sent to the cluster manager via the consent hardware sync.

The worker tracks its own GPU utilization baseline. When total GPU utilization exceeds the worker's known load by a configurable threshold (default: 15%), or when a process from a known gaming/rendering category is detected, the worker transitions to `idle`.

### State Transitions

```
available ──[GPU contention detected]──▶ idle
    ▲                                       │
    └──────[contention resolved]────────────┘
```

- On transition to `idle`: sends `STATUS { state: "idle" }` to the cluster manager. If a work unit is in-progress, it is completed (not interrupted mid-inference) but no new units are accepted.
- On transition back to `available`: sends `STATUS { state: "available" }` to the cluster manager. The worker resumes accepting work units.

### Configuration

| Flag / Env | Default | Description |
|---|---|---|
| `--gpu-monitor-interval` / `GPU_MONITOR_INTERVAL` | `5s` | How often to poll GPU utilization |
| `--gpu-contention-threshold` / `GPU_CONTENTION_THRESHOLD` | `15` | Percentage above baseline that triggers idle state |
| `--gpu-resume-delay` / `GPU_RESUME_DELAY` | `30s` | How long contention must be absent before resuming |

## Error Handling (Phase 1)

| Scenario | Behaviour |
|---|---|
| `llama-server` unreachable | Send `error` message to manager, wait for next unit |
| `llama-server` returns 4xx/5xx | Send `error` message with status + body |
| WebSocket disconnected | Reconnect with exponential backoff (1s, 2s, 4s, ... max 60s) |
| Work unit timeout (no response in 120s) | Cancel request, send `error` message |

## Test Trigger Payloads

The worker recognises special message content as test triggers:

| Trigger | Content | Behaviour |
|---|---|---|
| **PAUSE** | `##############PAUSE##############` | Toggles pause state — if running, pauses and sends `STATUS { state: "paused" }`; if paused, resumes and sends `STATUS { state: "available" }`. Returns a result (not an error). Bypasses the paused-rejection check. |
| **ERROR** | `##############ERROR##############` | Reports an intentional error to the manager via the error reporter, returns an error response |
| **PANIC** | `##############PANIC##############` | Triggers `panic()` — the `defer recover()` catches it, reports with stack trace, returns error |

## Build & Run

```bash
# Start local llama-server first (separate terminal)
llama-server --model ~/.synergia/models/mistral-small-3.2.gguf --port 8080

# Run the cluster client
cd tools/synergia-client
go run ./cmd/synergia-client \
  --manager-url wss://localhost:7500/ws/worker \
  --worker-key my-secret \
  --llm-url http://localhost:8080 \
  --model mistral-small-3.2-24b-instruct-2506 \
  --quantisation Q4_K_M
```

## End-to-End Test (Phase 1)

```bash
# Terminal 1: llama-server
llama-server --model ~/.synergia/models/mistral-small-3.2.gguf --port 8080

# Terminal 2: cluster-manager
cd tools/cluster-manager && go run ./cmd/cluster-manager

# Terminal 3: synergia-client
cd tools/synergia-client && go run ./cmd/synergia-client --manager-url wss://localhost:7500/ws/worker --worker-key test --llm-url http://localhost:8080 --model mistral-small-3.2-24b-instruct-2506

# Terminal 4: send a request (like the flow engine would)
curl -X POST https://localhost:7500/v1/chat/completions \
  -H "Authorization: Bearer <CLUSTER_API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "mistral-small-3.2-24b-instruct-2506",
    "messages": [{"role": "user", "content": "Hello, what is 2+2?"}],
    "temperature": 0
  }'
```
