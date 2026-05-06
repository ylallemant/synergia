# Synergia Integration Test

End-to-end integration test for the distributed worker pipeline: Manager → Client → llama-server → result.

## Prerequisites

- `llama-server` in PATH (install via `brew install llama.cpp`)
- Internet access (downloads ~150MB of test models on first run: SmolLM2-135M Q4_K_M + Q2_K)
- Go 1.23+

## Quick Run

```bash
cd test && go run .
```

## Flags

| Flag | Description |
|---|---|
| `--keep-alive` | After tests pass, keep all services running and wait for Ctrl+C. Skips the PAUSE trigger test (step 13), batch-of-3 test (step 14), PANIC trigger test (step 15), and consent withdrawal test (step 16) to keep the client alive. Sends background payloads and batch requests. |

### Interactive Mode

```bash
cd test && go run . --keep-alive
```

When `--keep-alive` is set, after all tests complete the test prints:
- Dashboard URL: `http://127.0.0.1:9876/static/index.html`
- Admin URL: `https://127.0.0.1:7501/?key=<api-key>`
- Manager URL: `https://127.0.0.1:7500`

Two background goroutines keep the cluster active:
1. **Payload sender** — sends random chat completion requests every 1–4 seconds
2. **Batch sender** — submits 1–5 batch requests every 10–20 seconds

The synergia-client's system tray icon (🟢/🟡/⚫/🔴) appears in the macOS menu bar during the test. Press Ctrl+C to shut down.

## What It Does

| Step | Action | Verifies |
|---|---|---|
| 0 | Generate TLS certificates (CA + localhost cert) if not cached | Self-signed cert for test TLS |
| 1 | Download SmolLM2-135M models (Q4_K_M + Q2_K) if not cached, compute file hashes | Model availability + hash computation |
| 2 | Check `llama-server` in PATH | Runtime dependency |
| 3 | Start `llama-server` with test model | Local inference |
| 4 | Start `synergia-manager` via `go run` (TLS + HTTP redirect + `CLUSTER_TEST_SETUP=true` + `--development`) | Manager serves with TLS; test roles seeded (512 MB threshold); batch development mode |
| — | Verify HTTP→HTTPS redirect on `:7080` | Redirect returns 301 to HTTPS |
| 5 | Verify `/v1/models/files` lists the model (HTTPS) | Model storage backend |
| 6 | Start `synergia-client` via `go run` (WSS + CA cert + `--role` + `--model-file`) | Client connects via WSS with LLM hash |
| 7 | Check manager logs for worker registration | WebSocket + identity |
| 8 | Check `/v1/workers` API shows the worker (HTTPS) | DB persistence |
| 9 | Send 3 chat completions: small (~150B), medium (~1KB), large (~5KB) payloads | End-to-end inference + latency bucketing |
| 10 | Verify client logs show work unit completion and manager logs show result returned | Work unit processing + result relay |
| 11 | Query `/v1/stats` and verify completed work units | Cluster stats API |
| 12 | LLM hash verification + model_update push cycle *(skipped with `--keep-alive`)* | File-hash-based model integrity, dual-status lifecycle |
| 13 | Send ERROR trigger payload (`##############ERROR##############`) | Error reporting: client detects trigger, reports error to manager via `POST /v1/errors` |
| 14 | PAUSE trigger + 429 + batch queue test *(skipped with `--keep-alive`)* | Pause/unpause, 429 rejection, batch queue processing |
| 15 | Submit 3 batch requests and poll until all complete *(skipped with `--keep-alive`)* | Development mode sequential batch processing, multi-request queue |
| 16 | Send PANIC trigger payload (`##############PANIC##############`) *(skipped with `--keep-alive`)* | Panic recovery: client panics, `defer recover()` catches it, reports with stack trace |
| 17 | Consent withdrawal + 429 + re-accept *(skipped with `--keep-alive`)* | Consent revocation via client API, 429 when withdrawn, batch queue during withdrawal, re-accept restores worker |
| 18 | Query `GET /v1/errors` and verify reports stored | Error persistence in DB |
| 19 | Query admin port `/v1/latency`, verify 3+ samples in matrix; check `latency_samples` and `workers.total_requests` in SQLite | Latency recording + adaptive bucketing |
| 20 | Collect output files | Final state capture |

### Step 12 Detail: LLM Hash Verification + Model Update Push

*(Skipped with `--keep-alive`)*

This step validates the full model integrity pipeline using file-based SHA256 hashes:

1. **Verify initial hash** — the worker's `llm_hash` in the DB matches `SHA256("embedding:" + SHA256(model1_file))`. This proves the worker computed the hash from the actual file on disk.
2. **Admin updates role** — change the `embedding` role from SmolLM2 Q4_K_M to SmolLM2 Q2_K via `POST /v1/admin/roles`
3. **Manager pushes `model_update`** — the worker receives the new model config (filename, expected file hash, expected llmHash)
4. **Worker transitions to `updating`** — logs `state=updating`, manager logs `client_status=updating aggregated=unavailable`
5. **Worker downloads and verifies** — downloads model file, computes SHA256, verifies against expected hash
6. **Worker reports new hash** — sends `llm_hash_report` with `SHA256("embedding:" + SHA256(model2_file))`
7. **Manager updates `sync_status`** — transitions from `out-of-sync` to `synced`, logs `sync_status=synced`
8. **Worker returns to `available`** — logs `state=available`, manager logs `aggregated=available`
9. **Completion succeeds** — send a chat completion to verify the worker is dispatch-eligible again
10. **Revert role** — restore the original model, verify the reverse update cycle completes

### Step 14 Detail: PAUSE + 429 + Batch Queue

*(Skipped with `--keep-alive`)*

This step validates the full pause/unpause lifecycle and the batch queue fallback:

1. Send `##############PAUSE##############` → client toggles to paused state
2. Wait 5 seconds for status to propagate to manager DB
3. Send a normal completion request → expect **HTTP 429** (no available worker)
4. Submit the same request via `POST /v1/batches` → receives batch ID (202 Accepted)
5. Send `##############PAUSE##############` again → client toggles back to available (PAUSE trigger bypasses 429 check)
6. Wait 5 seconds for batch processor to pick up the queued request
7. Poll `GET /v1/batches/{id}` → verify status becomes `completed`

### Step 15 Detail: Batch Queue with Multiple Payloads

*(Skipped with `--keep-alive`)*

Submits 3 batch requests simultaneously and polls every 1 second until all complete (60s timeout). In development mode, the manager processes them sequentially with random 1–5s delays between each, verifying the FIFO queue lifecycle end-to-end.

### Step 17 Detail: Consent Withdrawal and Re-Accept

*(Skipped with `--keep-alive`)*

This step validates that consent revocation properly gates work unit dispatch:

1. Revoke consent via `POST /api/consent` on the client's local dashboard API (port 9876)
2. Wait 3 seconds for the withdrawal to propagate (client → manager → DB status set to "withdrawn")
3. Send a normal completion request → expect **HTTP 429** (worker withdrawn)
4. Submit the same request via `POST /v1/batches` → receives batch ID (202 Accepted)
5. Re-accept consent via `POST /api/consent` on the client's local dashboard API
6. Wait 3 seconds for the re-accept to propagate
7. Send a normal completion → verify it succeeds
8. Poll the batch request queued during withdrawal → verify it completed

## Output

All logs and responses are saved to `test/runs/<timestamp>/`:

| File | Content |
|---|---|
| `logs/llama-server.log` | llama-server stdout/stderr |
| `logs/synergia-manager.log` | Manager logs |
| `logs/synergia-client.log` | Client logs (registration, processing) |
| `data/completion-response.json` | Raw OpenAI-format response from the cluster |
| `data/stats.json` | Final cluster stats |
| `data/workers.json` | Final worker listing |
| `data/client-errors.json` | Error reports stored in manager DB |
| `data/latency.json` | Latency matrix response from admin API |
| `data/synergia-manager.db` | SQLite DB with worker + work unit records |
| `data/client-data/` | Client identity files (keypair, fingerprint) |

Only the 3 most recent runs are kept; older runs are cleaned up automatically.

## Model Cache

The test model is downloaded once to `test/testdata/models/` and reused on subsequent runs. Delete the directory to force re-download.

## TLS Certificates

Self-signed CA and server certificates are generated once in `test/testdata/tls/` and reused on subsequent runs. Delete the directory to force regeneration.

## Configuration

All values are hardcoded for isolation — the test uses:
- Manager on `127.0.0.1:7500` (HTTPS)
- Admin port on `127.0.0.1:7501` (HTTPS, dashboard + latency/config endpoints)
- HTTP redirect on `127.0.0.1:7080` (redirects to HTTPS)
- llama-server on `127.0.0.1:8090`
- Client connects via `wss://127.0.0.1:7500/ws/worker`
- API key: `test-api-key`
- Worker key: `test-worker-key`
- TLS certificates: auto-generated in `testdata/tls/` (self-signed CA + localhost cert)
- `CLUSTER_TEST_SETUP=true`: seeds role-model mappings with minimal test thresholds (512 MB VRAM, SmolLM2-135M-Instruct) so any hardware can run all roles

## Checking Test Results

### Quick Pass/Fail

- Look for `=== ALL TESTS PASSED ===` at the end of stdout
- Exit code 0 = pass, 1 = fail with `✗` error message

### Log Files (`runs/<timestamp>/logs/`)

Key log patterns to grep:

| Service | Pattern | Meaning |
|---|---|---|
| synergia-manager | `"worker connected"` | Client registered via WebSocket |
| synergia-manager | `"returned result"` | End-to-end completion succeeded |
| synergia-manager | `"consent updated"` | Consent sync received from client |
| synergia-manager | `"HTTP redirect server starting"` | HTTP→HTTPS redirect active |
| synergia-manager | `"redirecting HTTP to HTTPS"` | Redirect served (DEBUG) |
| synergia-client | `"connected to cluster manager"` | WSS connection established |
| synergia-client | `"auto-approve enabled"` | Consent auto-accepted |
| synergia-client | `"consent synced with manager"` | Consent POST succeeded (HTTPS) |
| synergia-client | `"work unit completed"` | Inference round-trip done |
| synergia-client | `"following redirect"` | HTTP→HTTPS redirect followed (DEBUG) |

### SQLite DB (`runs/<timestamp>/data/synergia-manager.db`)

```bash
DB=runs/<timestamp>/data/synergia-manager.db

# Workers table — registered workers with trust score and status
sqlite3 "$DB" ".mode column" ".headers on" "SELECT * FROM workers;"

# Work units — dispatched tasks with status and timing
sqlite3 "$DB" ".mode column" ".headers on" "SELECT * FROM work_units;"

# Consent table — includes hardware stats (OS, GPU, CPU, RAM)
sqlite3 "$DB" ".mode column" ".headers on" \
  "SELECT fingerprint, accepted, hw_os, hw_os_ver, hw_gpu, hw_v_ram_mb, hw_cpu, hw_cpu_cores, hw_ram_mb FROM worker_consents;"

# Worker config — processing preferences synced from client
sqlite3 "$DB" ".mode column" ".headers on" "SELECT * FROM worker_configs;"

# Client errors — errors reported by workers
sqlite3 "$DB" ".mode column" ".headers on" "SELECT * FROM client_errors;"
```

Note: GORM maps `HwVRAMMB` to column `hw_v_ram_mb` (splits on uppercase boundaries).

### Client Data (`runs/<timestamp>/data/client-data/`)

| File | Expected |
|---|---|
| `consent.json` | `{"accepted": true, "hardware_stats": true, "config_preferences": true}` |
| `fingerprint` | 64-char hex SHA256 of public key |
| `identity.enc` | Encrypted Ed25519 private key |
| `identity.pub` | PEM-encoded public key |

### JSON Output (`runs/<timestamp>/data/`)

| File | What to check |
|---|---|
| `completion-response.json` | Valid OpenAI-format response with `choices[0].message.content` |
| `stats.json` | `work_units.completed` should be ≥ 1 |
| `workers.json` | Worker entry with correct model name and `status: "online"` |

### Common Failures

| Error | Fix |
|---|---|
| `llama-server not found in PATH` | `brew install llama.cpp` |
| `port 7500 is already in use` | Port 7500 already in use — kill stale process |
| `port 7080 is already in use` | Port 7080 already in use (HTTP redirect) — kill stale process |
| `client did not register` | Check client logs for connection/auth/TLS errors |
| `chat completion failed` | Verify worker has consent (required for dispatch) |
| `worker has not accepted data collection terms` | `--auto-approve` flag missing from client args |

## Error Reporting Test

The test verifies the client's error reporting pipeline using special trigger payloads embedded in chat completion messages:

| Trigger | Payload | Behaviour |
|---|---|---|
| ERROR | `##############ERROR##############` | Client detects trigger, returns an intentional error, reports it to manager via `POST /v1/errors` |
| PANIC | `##############PANIC##############` | Client detects trigger, panics intentionally, `defer recover()` catches it, reports with full goroutine stack trace |

The PANIC trigger is **skipped in `--keep-alive` mode** because it exercises crash recovery — the client must remain alive for interactive use.

Both triggers verify:
1. Manager receives the error response from the worker (HTTP 500)
2. Error reporter sends the report asynchronously to `POST /v1/errors`
3. Reports are stored in `client_errors` table (confirmed via `GET /v1/errors`)
