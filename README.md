# Synergia

Community-powered distributed LLM inference cluster.

A BOINC-inspired volunteer GPU compute mesh that turns idle GPUs into a shared inference network. Participants run a lightweight client daemon that connects to a central manager, receives LLM work units, and returns results — all behind a standard OpenAI-compatible API.

> [!NOTE]
> **Synergia** (Greek: *collective work*) is the distributed compute subsystem of [Sophia](https://github.com/ylallemant/sophia), an LLM-wiki platform whose name means *wisdom/knowledge*. The pairing reads as "wisdom and knowledge through cooperation" — Sophia defines the goal, Synergia provides the means.

## Architecture

```
┌─────────────────┐        OpenAI API          ┌──────────────────┐
│  Flow Engine /  │ ──────────────────────────▶│                  │
│  Any LLM Client │  POST /v1/chat/completions │  synergia-manager│
│                 │  POST /v1/batches          │                  │
└─────────────────┘                            └────────┬─────────┘
                                                        │ WSS /ws/worker
                               ┌────────────────────────┼─────────────────────┐
                               │                        │                     │
                        ┌──────▼──────┐         ┌───────▼─────┐       ┌───────▼─────┐
                        │   Worker A  │         │   Worker B  │       │   Worker N  │
                        │ (GPU + LLM) │         │ (GPU + LLM) │       │ (GPU + LLM) │
                        └─────────────┘         └─────────────┘       └─────────────┘
```

**Manager** — central coordinator that queues work, tracks worker state, and serves an OpenAI-compatible API to consumers.

**Client** — worker daemon running on volunteer machines. Connects outbound via WebSocket (firewall-friendly), receives inference requests, runs them on a local `llama-server`, and returns results.

## Components

| Binary | Description |
|---|---|
| [`synergia-manager`](docs/manager/README.md) | Central coordinator + OpenAI-compatible API gateway |
| [`synergia-client`](docs/client/README.md) | Worker daemon (connects to manager, runs local LLM inference) |

## Quick Start

### Manager

```bash
go run ./cmd/synergia-manager --development
```

Environment variables: `CLUSTER_LISTEN_ADDR`, `CLUSTER_API_KEY`, `CLUSTER_WORKER_KEY`, `CLUSTER_DB_PATH`, etc.

### Client

```bash
go run ./cmd/synergia-client \
  --manager-url wss://manager-host:7500/ws/worker \
  --worker-key <shared-key> \
  --llm-url http://127.0.0.1:8080 \
  --model SmolLM2-135M-Instruct \
  --quantisation Q4_K_M \
  --role embedding \
  --model-file /path/to/model.gguf
```

### Integration Test

```bash
cd test && go run .
```

Downloads test models automatically on first run, starts manager + client + llama-server, runs the full validation suite.

## Features

- **OpenAI-compatible API** — drop-in replacement for any LLM consumer
- **WebSocket gateway** — workers connect outbound (NAT/firewall-friendly)
- **Dual-status model** — client_status (worker-reported) + sync_status (manager-derived)
- **Model update push** — admin updates role→model mapping, workers auto-download
- **LLM hash verification** — SHA256(role + ":" + SHA256(model_file)) ensures model integrity
- **GPU activity monitoring** — detects external GPU usage, pauses inference
- **Batch queue** — async batch API for when no workers are immediately available
- **Latency monitoring** — per-role adaptive bucketing with p50/p95/p99 percentiles
- **Worker consent** — interactive data collection consent, gated work dispatch
- **Auto-start** — macOS LaunchAgent, Linux systemd user service
- **System tray** — connection state icon with Pause/Resume/Quit menu
- **Multi-platform** — Linux/macOS/Windows, amd64/arm64

## GPU Platform Support

| Platform | GPU | Detection Method | Driver Version |
|---|---|---|---|
| macOS | Apple Metal | `system_profiler` + `ioreg` | macOS version |
| Linux | NVIDIA | `nvidia-smi` | `nvidia-smi` |
| Linux | AMD ROCm | `rocm-smi` | `rocm-smi` |
| Linux | Intel Arc | `intel_gpu_top` | `intel_gpu_top` |
| Linux | Moore Threads | `mthreads-gmi` | `mthreads-gmi` |
| Windows | NVIDIA | `nvidia-smi` | `nvidia-smi` |
| Windows | Intel/AMD (WDDM) | `typeperf` GPU counters | N/A |
| Windows | Moore Threads | `mthreads-gmi` | `mthreads-gmi` |

## Building

```bash
# Client (all platforms)
GOOS=linux GOARCH=amd64 go build -o synergia-client ./cmd/synergia-client

# Manager
GOOS=linux GOARCH=amd64 go build -o synergia-manager ./cmd/synergia-manager

# Docker (manager)
docker build -f deploy/Dockerfile -t synergia-manager .
```

## Project Structure

```
cmd/
  synergia-client/     Client entrypoint
  synergia-manager/    Manager entrypoint
internal/
  client/              Client internal packages
  manager/             Manager internal packages
test/                  Integration test suite
deploy/                Dockerfile
docs/                  Architecture documentation
```

## License

MIT
