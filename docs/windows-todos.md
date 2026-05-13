# Windows TODOs

Items that are known to need attention on Windows but are deferred — either
because they require an environment we don't have locally (CI, signed releases)
or because they're production concerns rather than active bugs. Each entry
notes the suspected failure mode and a starting point for the work.

The items below complement the ones already fixed (tray PNG→ICO wrapper,
sidecar auto-download, backend-update file-in-use, hwinfo collection, VRAM
registry path, TLS pool, Job Object cleanup, multi-size ICO, freePort).
See `docs/architecture/client/README.md` for the broader Windows feature
matrix.

---

## 1. End-to-end binary self-update flow

**What we have**
- [`cmd/synergia-updater/main.go`](../cmd/synergia-updater/main.go) — the sidecar that performs the locked-file swap.
- [`internal/client/updater/replace_windows.go`](../internal/client/updater/replace_windows.go) — calls the sidecar with `--pid/--src/--dst/--restart`.
- [`internal/client/updater/ensure_windows.go`](../internal/client/updater/ensure_windows.go) — fetches the sidecar from the GitHub release on first need, with a manager-proxy fallback.
- Integration test step 20 + the Windows-only sidecar download verification check.

**What's unverified**
The actual swap. The test pushes `v99.0.0-test` (a deliberately non-existent
version) so the binary download fails fast and only the *attempt* is asserted.
We have never round-tripped a real version bump end to end on Windows:

1. Client running.
2. Admin POSTs a real target version that exists on GitHub releases.
3. Client downloads new binary, downloads sidecar (if missing), invokes the sidecar.
4. Sidecar waits for the parent to exit, renames `.exe` → `.exe.bak`, moves new binary into place, starts it.
5. New process reconnects and the manager observes the new `X-Worker-Version`.

**Things that could break**
- Sidecar version-skew (auto-downloaded sidecar is the new version, but the running client expects the old contract).
- The 60 s reconnect-or-rollback timer (described in `docs/architecture/README.md`) — we have no automated test for the rollback path.
- Path quoting if the install location contains spaces (`Program Files`).
- Anti-virus quarantining the freshly-downloaded `.exe` between download and `os.Rename`.

**How to verify**
Build a real `v0.X.Y-test` release on GitHub with both `synergia-client-windows-amd64.exe`
and `synergia-updater-windows-amd64.exe`, then run a manual test on a clean
Windows VM pointing at it. Watch `client.log` and the registered binary path
through the cycle.

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
