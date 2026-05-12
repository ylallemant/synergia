# Integration Test

The `test` package is a self-contained integration test binary. It compiles and runs the real `synergia-manager` and `synergia-client` binaries from source, exercises the full protocol end to end, and verifies the results.

## Prerequisites

- Go toolchain
- `llama-server` in `PATH` — install via `brew install llama.cpp` (macOS) or build from source
- Internet access for first run — downloads ~100 MB of test models from HuggingFace and (in full mode) ~16 MB of llama.cpp releases from GitHub
- `sqlite3` CLI — used internally to query the manager DB

## Modes

### Default — full integration test

```
go run ./test
```

Runs all 22 test steps sequentially. Exits with a non-zero code and dumps relevant logs on any failure. Keeps the last 3 run directories under `test/runs/`.

### `--keep-alive` — test + live services

```
go run ./test --keep-alive
```

Runs the full test suite (skipping the longer destructive steps — model update, PANIC trigger, consent withdrawal, real llama.cpp download). After all tests pass, keeps the manager and client alive and starts a background payload sender (live completions every 1–4 s, batch requests every 10–20 s). Services stop on `Ctrl-C` or when the system tray is used to quit the client.

Useful for manual exploration of the admin dashboard and client dashboard after a successful test run:

| Interface | URL |
|---|---|
| Client Dashboard | `http://127.0.0.1:9876/static/index.html` |
| Admin Dashboard | `https://127.0.0.1:7501/login` (admin / synergia) |
| Manager API | `https://127.0.0.1:7500` |

### `--run` — services only, no tests

```
go run ./test --run
```

Starts the manager and client in the same configuration as the test (fresh data directory, local binary distribution server, `--development` mode) but does not run any assertions. Blocks until `Ctrl-C`. Intended for manual testing and debugging against a clean local stack.

### `--send` — payload sender against a live endpoint

```
go run ./test --send --endpoint https://synergia.example.com/ [--key API_KEY] [--model MODEL_NAME]
```

Sends live chat completions and batch requests continuously to an already-running Synergia cluster. Does not start any local services. Useful for load testing a staging or production deployment.

| Flag | Default |
|---|---|
| `--endpoint` | required — base URL of the cluster |
| `--key` | `CLUSTER_API_KEY` env var |
| `--model` | `SmolLM2-135M-Instruct` |

## Test Steps

| Step | What it tests |
|---|---|
| 0 | Generate self-signed TLS CA + server certificate used for the test manager |
| 1 | Download two quantisations of SmolLM2-135M from HuggingFace (cached across runs in `test/testdata/models/`) |
| 2 | Package the system `llama-server` binary (+ sibling `.dylib` / `.so` files) as a `tar.gz` and serve it from an in-process HTTP server |
| 3 | *(informational)* — llama-server will be started by the client after binary/model push |
| 4 | Start `synergia-manager` with `--development`, TLS, bearer auth, and the local binary distribution URL |
| 5 | Verify the model listing endpoint returns the expected test model |
| 6 | Start `synergia-client` with a clean data directory (no cached binary, no cached model) |
| 7 | Verify the client registers with the manager via key-auth (Bearer token) |
| 7a | Wait for the client's `InitialSync` bootstrap to complete — the client detects a fresh install, signals the manager, and the manager pushes `BackendUpdate` + `ModelUpdate`; the client downloads the binary, installs it, downloads the model, and starts llama-server on port 9877 |
| 7b | Run a parallel TOFU (Trust On First Use) handshake test — a second manager (no worker key) and a second client authenticate via challenge/nonce/signature exchange |
| 8 | Verify the worker appears in `/v1/workers` |
| 9 | Send three chat completion requests at increasing payload sizes (small ~150 B, medium ~1 KB, large ~5 KB) |
| 10 | Verify work unit completion and result delivery in logs |
| 11 | Verify `/v1/stats` shows completed work |
| 12 | Push a model update via admin API → verify the client downloads the new model, reports its file hash, the manager marks the worker as `synced`, and the worker returns to `available`; then revert the role and verify again *(skipped with `--keep-alive`)* |
| 13 | Send an `ERROR` trigger payload and verify the error is reported to the manager |
| 14 | Send a `PAUSE` trigger → verify the worker goes offline → verify live requests return 429 → submit a batch request → unpause → verify the batch completes *(skipped with `--keep-alive`)* |
| 15 | Submit 3 batch requests and verify all complete *(skipped with `--keep-alive`)* |
| 16 | Send a `PANIC` trigger and verify the client recovers and reports to the manager *(skipped with `--keep-alive`)* |
| 17 | Withdraw consent via client API → verify 429 → queue a batch request → re-accept consent → verify live completions work again and the queued batch completes *(skipped with `--keep-alive`)* |
| 18 | Verify error reports (ERROR + PANIC triggers) are stored in the manager DB |
| 19 | Verify latency samples are recorded in the admin API and DB |
| 20 | Post a target client version via admin API and verify the worker receives a `binary_update` push |
| 21 | Download two real llama.cpp releases from GitHub (b9049 → b9050), verify the client installs and restarts llama-server with each *(skipped with `--keep-alive`; downloads ~16 MB per step)* |
| 22 | Write collected API responses to `test/runs/<timestamp>/data/` |

## Run Output

Each run creates a timestamped directory:

```
test/runs/2026-05-12_08-27-43/
  logs/
    test-run.log          # test binary own output
    cluster-manager.log   # manager process stdout/stderr
    cluster-client.log    # client process stdout/stderr
    tofu-manager.log      # TOFU test manager (step 7b)
    tofu-client.log       # TOFU test client (step 7b)
  data/
    cluster-manager.db    # SQLite DB
    completion-response.json
    stats.json
    workers.json
    latency.json
    client-errors.json
    client-data/          # client's working directory (downloaded binary, model)
```

The last 3 runs are kept; older ones are deleted automatically.
