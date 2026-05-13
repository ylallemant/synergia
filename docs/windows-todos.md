# Windows TODOs

Items that are known to need attention on Windows but are deferred — either
because they require an environment we don't have locally (CI, signed releases)
or because they're production concerns rather than active bugs. Each entry
notes the suspected failure mode and a starting point for the work.

The items below complement the ones already fixed (tray PNG→ICO wrapper,
sidecar auto-download, backend-update file-in-use, hwinfo collection, VRAM
registry path, TLS pool, Job Object cleanup, multi-size ICO, freePort,
binary self-update e2e). See `docs/architecture/client/README.md` for the
broader Windows feature matrix.

---

## 1. ~~End-to-end binary self-update flow~~ — VALIDATED

The full GitHub → sidecar → swap → restart → reconnect cycle has been
exercised end to end on Windows by the dedicated `--upgrade-test` mode:

```
go run ./test --upgrade-test --from 0.0.28 --to 0.0.29
```

The mode:

1. Builds a "stale" `synergia-client.exe` with the `--from` version stamped in via `-ldflags`.
2. Brings up a single-client cluster against a real GitHub release.
3. Triggers the update via `/v1/admin/version`.
4. Asserts: `binary_update` received → sidecar fetched (if missing) → binary downloaded → swap → restart → manager observes a fresh connection → post-upgrade client logs `cluster client starting … version=<to>`.

The first round of this test surfaced **two real bugs** (now fixed):

- **`updater.Apply` raced its own download cleanup** — an unconditional
  `defer os.Remove(tmpPath)` deleted the downloaded `.exe` before the
  asynchronous Windows sidecar could rename it into place. Worked on Unix
  by coincidence (sync `os.Rename`). Now uses a `cleanupTmp bool` guard
  that only fires on error paths.

- **Sidecar restart drops the parent's argv** — `synergia-updater.exe`
  exec's the replaced client with no arguments, so any CLI flag the
  parent had (`--manager-url`, `--data-dir`, `--llm-url`, …) is lost. The
  new client falls back to defaults — including `~/.synergia/worker/` as
  the data-dir, which may hold a `worker-state.yaml` from an unrelated
  install pointing at a different cluster. This is by design (the sidecar
  is stateless on purpose), but it means **configuration must come from
  env vars, `worker-state.yaml`, or the sentinel-patched manager URL —
  never from CLI flags** if it has to survive an upgrade. The test now
  passes all client config via env vars (`CLUSTER_MANAGER_URL`,
  `WORKER_LLM_URL`, `CLUSTER_CLIENT_DATA_DIR`, etc.) which inherit
  cleanly through `parent → sidecar → new client`.

### Still worth a manual run before public release

Edge cases the automated test doesn't cover:

- Path with spaces (`C:\Program Files\Synergia\…`) — both the rename and the autostart registry value need to handle quoting.
- Anti-virus quarantining the freshly-downloaded `.exe` between download and `os.Rename`.
- The 60 s reconnect-or-rollback timer (described in `docs/architecture/README.md`) — no automated test for the rollback path yet.
- Sidecar version-skew across major-version jumps (the v0.x → v0.x+1 contract is stable; multi-version skips are not exercised).

---

## 2. Auto-start on Windows

**What we have**
[`internal/client/autostart/autostart_windows.go`](../internal/client/autostart/autostart_windows.go) writes
the current executable path to `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
when the user toggles "Start on login" in the dashboard.

**What's unverified**
The whole flow. The integration test never exercises auto-start at all.

**Things that could break**
- Path with spaces (`C:\Program Files\Synergia\…`) — the registry value must
  be quoted, otherwise `cmd.exe` resolves `Program` as the command.
- Moving the binary after enabling — the registered path becomes stale; the
  dashboard's "Start on login" toggle status comes from the registry, so it
  will display "enabled" while pointing nowhere.
- Logon-time race: tray icon vs Windows Explorer initialisation. The
  notification area sometimes drops icons registered before Explorer is ready.

**How to verify**
A short manual test on a clean Windows user account: enable, sign out,
sign back in, confirm the worker connected and the tray shows. Then move
the binary and repeat to confirm the dashboard reflects the broken state.

---

## 3. CGO build in CI

**What we have**
[`.github/workflows/release.yml`](../.github/workflows/release.yml) builds with
`CGO_ENABLED=1` for `windows/amd64` and `CGO_ENABLED=0` for `windows/arm64`.
The CGO path on `windows-latest` runners depends on the bundled MinGW
toolchain. `fyne.io/systray` needs CGO on Windows; building with
`-tags nosystray` is the only way to skip it.

**What's unverified**
That the release workflow actually produces a working amd64 `.exe` from a
clean checkout, and that the resulting binary starts on a machine without
the MinGW DLLs installed (i.e. the build must be statically linked enough
to run on a stock Windows install).

**Things that could break**
- The runner image stops shipping `mingw-w64`; CGO build fails.
- The linker dynamically links a runtime DLL the target machine doesn't have.

**How to verify**
Tag a test release, let the workflow run, download `synergia-client-windows-amd64.exe`
from the release assets onto a vanilla Windows VM, and run it. Check
`Dependencies.exe` (or `dumpbin /dependents`) for any non-standard DLL imports.

---

## 4. Code signing & SmartScreen

**Why it matters in production**
Unsigned `synergia-client.exe` triggers Windows SmartScreen on first run:
the "Windows protected your PC — Unrecognized app" dialog with a "Don't run"
default. Volunteers will refuse to click through this on a community-supplied
binary, which directly impacts adoption.

**What needs to happen**
- Acquire a code-signing certificate. Three realistic options:
  - **OV (Organization Validation)**: ~€200/yr, accumulates reputation slowly.
    SmartScreen warning persists for weeks/months until enough installs.
  - **EV (Extended Validation)**: ~€300–500/yr, hardware-token-bound,
    immediate SmartScreen reputation from the first signed binary.
  - **Azure Trusted Signing**: subscription-based, no token, modern flow.
- Sign `synergia-client*.exe` and `synergia-updater*.exe` in the release
  workflow using `signtool sign /tr http://timestamp.digicert.com /td sha256 /fd sha256 …`
  — see the existing `docs/apple-developer-id.md` for an analogous macOS
  workflow.
- Document the signing setup similar to the existing
  `docs/apple-developer-id.md`.

**Out of scope for now**
The current Phase 1 milestones don't require signing. Revisit when moving
toward public distribution / Phase 3.

---

## 5. Pre-existing nosystray stub mismatch

When building with `-tags nosystray`, `cmd/synergia-client/main.go` fails to
compile because `tray_stub.go`'s `UpdateStatus` signature
`(_ bool, _ gpu.State, _ bool, _ bool)` doesn't match the real handler's
`(string, string)` (the `status.ChangeHandler` interface).

This isn't Windows-specific but only shows up if someone tries to build
without the systray, which currently no CI path does.

**Fix:** align `tray_stub.go::UpdateStatus` with the
`(prev, current string)` signature used by the real tray.
