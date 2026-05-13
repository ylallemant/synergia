# Synergia — Documentation

## Architecture

| Document | What it covers |
|---|---|
| [Architecture overview](architecture/README.md) | System design, components, protocol, trust model, role system |
| [Manager](architecture/manager/README.md) | `synergia-manager` — API reference, admin endpoints, configuration, deployment |
| [Client](architecture/client/README.md) | `synergia-client` — worker lifecycle, identity, consent, configuration |
| [Integration test](architecture/test/README.md) | Test harness design, phases, port layout, run output |
| [API Proxy *(Phase 3)*](architecture/proxy/README.md) | Hardened internet-facing edge node — security hardening and horizontal scaling via batch sync |

## API & Integration

| Document | What it covers |
|---|---|
| [OpenAI-compatible API](openai-api.md) | HTTP endpoints, request/response schemas, streaming, batch |

## Roadmap

| Document | What it covers |
|---|---|
| [TODO](TODO.md) | Phase-by-phase backlog with completion status |

## Operations & Security

| Document | What it covers |
|---|---|
| [Security assessment](security.md) | Attack surface analysis, threat model, prioritised testing checklist |
| [Apple Developer ID](apple-developer-id.md) | Code signing and notarization for macOS client distribution |
| [Windows TODOs](windows-todos.md) | Deferred Windows-specific items (binary self-update, autostart, CGO build, code signing) |
