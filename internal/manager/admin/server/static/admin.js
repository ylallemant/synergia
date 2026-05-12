// admin.js — shared utilities for all admin dashboard pages.
// Expects the following globals defined inline before this script loads:
//   const API_KEY         = "...";   // from {{.APIKey}}
//   const MANAGER_VERSION = "...";   // from {{.ManagerVersion}}
//   const headers         = {Authorization, Content-Type};

// ── Nav panel ────────────────────────────────────────────────────────────────
function openNav() {
  document.getElementById("navPanel").classList.add("open");
  document.getElementById("navOverlay").classList.add("show");
}
function closeNav() {
  document.getElementById("navPanel").classList.remove("open");
  document.getElementById("navOverlay").classList.remove("show");
}
document.getElementById("navToggle").addEventListener("click", openNav);
document.getElementById("navClose").addEventListener("click", closeNav);
document.getElementById("navOverlay").addEventListener("click", closeNav);

// ── Status message helper ────────────────────────────────────────────────────
function showStatus(el, msg, ok) {
  el.textContent = msg;
  el.className = "status-msg " + (ok ? "ok" : "err");
  setTimeout(() => { el.textContent = ""; el.className = "status-msg"; }, 5000);
}

// ── Version state (sessionStorage) ──────────────────────────────────────────
// State shape:
//   { manager: {version, latest},
//     client:  {target, latest, synced, outdated},
//     backend: {version, latest, synced, outdated},
//     fetchedAt: ms }
//
// sessionStorage keeps the state alive while the admin navigates between pages
// in the same tab. Writing via setVersionState() fires the 'storage' event in
// OTHER tabs so they also get an instant UI refresh.

const VS_KEY = "synergia_admin_vs";
const VS_TTL = 30000; // 30 s

function getVersionState() {
  try { return JSON.parse(sessionStorage.getItem(VS_KEY)); } catch(e) { return null; }
}

function setVersionState(s) {
  s.fetchedAt = Date.now();
  try { sessionStorage.setItem(VS_KEY, JSON.stringify(s)); } catch(e) {}
  // storage event does not fire in the writing tab — apply directly
  applyVersionState(s);
}

// Listen for state written by other tabs / pages in the same origin
window.addEventListener("storage", e => {
  if (e.key === VS_KEY && e.newValue) {
    try { applyVersionState(JSON.parse(e.newValue)); } catch(e) {}
  }
});

// ── Fetch & store ─────────────────────────────────────────────────────────────
async function refreshVersionState() {
  try {
    const resp = await fetch("/v1/admin/versions/status", {headers});
    if (!resp.ok) return;
    const d = await resp.json();
    setVersionState({
      manager: {
        version:  d.manager_version  || "",
        latest:   d.manager_latest   || "",
      },
      client: {
        target:   d.client_target    || "",
        latest:   d.client_latest    || "",
        synced:   d.client_synced    ?? 0,
        outdated: d.client_outdated  ?? 0,
      },
      backend: {
        version:  d.backend_version  || "",
        latest:   d.backend_latest   || "",
        synced:   d.backend_synced   ?? 0,
        outdated: d.backend_outdated ?? 0,
      },
    });
  } catch(e) {}
}

// ── Apply state to DOM ────────────────────────────────────────────────────────
function applyVersionState(s) {
  if (!s) return;
  _updateManagerHint(s.manager  || {});
  _updateClientSection(s.client  || {});
  _updateBackendSection(s.backend || {});
}

function _updateManagerHint(m) {
  const el = document.getElementById("navManagerVersion");
  if (!el) return;
  const v = m.version || MANAGER_VERSION || "";
  if (m.latest && v && m.latest !== v
      && !v.startsWith("dev") && !v.startsWith("0.0.0")) {
    el.innerHTML = "v" + v
      + " <span style=\"color:#ca8a04;font-size:0.8em\">(" + m.latest + " available)</span>";
  } else if (v) {
    el.textContent = "v" + v;
  }
}

function _updateClientSection(c) {
  // Pre-select stored target in the dropdown (if already populated with options)
  const sel = document.getElementById("ver_target");
  if (sel && c.target) {
    for (const opt of sel.options) opt.selected = (opt.value === c.target);
  }

  // Newer-version hint
  const hint = document.getElementById("verNewerHint");
  if (hint) {
    if (c.target && c.latest && c.latest !== c.target) {
      hint.textContent = "Newer version available: " + c.latest;
      hint.style.display = "";
    } else {
      hint.style.display = "none";
    }
  }

  // Sync stat cards — workers that have / don't have the configured target.
  const noTarget = document.getElementById("versionSyncNoTarget");
  const grid     = document.getElementById("versionSyncGrid");
  if (!noTarget || !grid) return;
  if (c.target) {
    grid.style.display     = "";
    noTarget.style.display = "none";
    _setText("verSynced",   c.synced);
    _setText("verOutdated", c.outdated);
  } else {
    grid.style.display     = "none";
    noTarget.style.display = "";
    noTarget.textContent   =
      "No target version configured — workers cannot auto-update. Set one above and push.";
  }
}

function _updateBackendSection(b) {
  // Newer-version hint
  const hint = document.getElementById("beNewerHint");
  if (hint) {
    if (b.version && b.latest && b.latest !== b.version) {
      hint.textContent = "Newer version available: " + b.latest;
      hint.style.display = "";
    } else {
      hint.style.display = "none";
    }
  }

  // Sync stat cards — workers that have / don't have the configured version.
  const noTarget = document.getElementById("backendSyncNoTarget");
  const grid     = document.getElementById("backendSyncGrid");
  if (!noTarget || !grid) return;
  if (b.version) {
    grid.style.display     = "";
    noTarget.style.display = "none";
    _setText("beSynced",   b.synced);
    _setText("beOutdated", b.outdated);
  } else {
    grid.style.display     = "none";
    noTarget.style.display = "";
  }
}

function _setText(id, val) {
  const el = document.getElementById(id);
  if (el) el.textContent = val;
}

// ── Bootstrap ─────────────────────────────────────────────────────────────────
(function init() {
  // Apply cached state immediately — no network flash
  const cached = getVersionState();
  if (cached) applyVersionState(cached);
  // Fetch fresh if stale or missing
  if (!cached || Date.now() - cached.fetchedAt > VS_TTL) refreshVersionState();
})();

setInterval(refreshVersionState, VS_TTL);
