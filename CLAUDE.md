# Synergia — Claude Code Instructions

## Session start

Read the following documents at the beginning of every session to build context before making any changes:

1. [`docs/architecture/README.md`](docs/architecture/README.md) — system design, components, protocol, trust model
2. [`docs/architecture/manager/README.md`](docs/architecture/manager/README.md) — manager API, configuration, deployment
3. [`docs/architecture/client/README.md`](docs/architecture/client/README.md) — client lifecycle, identity, consent

The documentation index is at [`docs/README.md`](docs/README.md).

## Project layout

```
cmd/
  synergia-manager/   — central coordinator binary
  synergia-client/    — worker daemon binary
internal/
  manager/            — manager packages (gateway, store, admin, api)
  client/             — client packages (worker, connection, tray, updater)
  protocol/           — shared WebSocket message types
docs/
  architecture/       — design docs (README + manager/ + client/ + test/)
  security.md
  openai-api.md
test/                 — integration test harness (go run ./test)
```

## Key conventions

- Always present a plan and wait for approval before editing files.
- Never add `Co-Authored-By` lines to git commits.
- Always recap changes in chat and get approval before opening the commit dialog.
- Default branch is `main`. Tests run via the pre-commit hook (`go test ./...`).
- Integration test: `go run ./test` — see [`test/README.md`](test/README.md).
