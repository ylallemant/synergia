# Synergia — Security Assessment

Attack surface analysis specific to the Synergia architecture, with a prioritised testing checklist and tool recommendations. All testing must be performed against infrastructure you own or have explicit written permission to test.

---

## Attack Surface Map

### Admin server (port 7501)

| Vector | Severity | Status | Notes |
|---|---|---|---|
| Login brute force | Medium | Open | No rate limiting or account lockout on `POST /login` |
| XSS via rendered worker data | Medium | Open | Worker-submitted content (error messages, role names) rendered in admin UI — unescaped `<script>` tags in error reports reach the errors table |
| SSRF via OIDC provider URL | High | Open | Admin sets `provider_url`, manager connects to it at startup — reachable internal endpoints, cloud metadata APIs (AWS IMDS, GCP metadata) |
| SSRF via backend download URL | High | Open | `POST /v1/admin/backend` sets a URL the manager will fetch and cache — same internal-network reach as above |
| Model filename path traversal | Medium | Open | Filename stored in DB and used to build filesystem paths; `../../../etc/passwd`-style values are not sanitised |
| CSRF on state-changing API calls | Low–Medium | Partial | `SameSite=Lax` on session cookie mitigates most cases but does not cover all cross-origin scenarios |
| Session fixation | Low | Likely OK | Session ID is generated server-side after credential verification; worth confirming in testing |
| Operator key in env vars | Medium | Open | `CLUSTER_API_KEY`, session secret readable from `/proc/self/environ` if container is compromised |

### Main API (port 7500)

| Vector | Severity | Status | Notes |
|---|---|---|---|
| API key brute force | Medium | Open | Single shared key, no rate limiting or IP blocking on `/v1/chat/completions` |
| Large-payload DoS | High | Open | No `Content-Length` or body size limit — a 100 MB `messages[]` payload is forwarded directly to llama-server |
| Request flooding | High | Open | No per-IP or per-key rate limiting; can saturate the queue and deny service to legitimate callers |
| Prompt injection | Medium | Inherent | Malicious API callers craft payloads designed to manipulate LLM output or extract system prompt content; mitigated partially by chain-of-custody signatures (TODO) |
| Result tampering | Medium | Open | Workers sign every result with Ed25519, but the manager **does not yet verify** the signatures before returning them to the caller (see TODO in manager README) |

### Worker authentication (WebSocket gateway)

| Vector | Severity | Status | Notes |
|---|---|---|---|
| TOFU race condition | Medium | Open | First connection from a fingerprint is registered unconditionally; an attacker who connects before the real worker can register a different public key for that fingerprint |
| Malicious worker registration | Medium | Open | No pre-enrollment or allowlist; anyone who can reach `:7500/ws/worker` can register as a worker and receive work units |
| Key mismatch detection not persisted | Low | Open | In-memory `knownKeys` map is cleared on manager restart — a key mismatch detected before a restart is not remembered afterward |
| Worker submitting malformed results | Low | Mitigated | Malformed JSON is rejected; oversized results are unbounded (no max result size) |

### Binary and backend distribution

| Vector | Severity | Status | Notes |
|---|---|---|---|
| Manager-provided SHA256 not independently signed | High | Open | Until the operator-signed manifest TODO is implemented, a compromised manager can serve a malicious binary with a matching hash it computed itself |
| Download proxy as SSRF relay | Medium | Open | `/v1/backend/download` proxies a URL from DB config — if an attacker can write to that config they can reach internal services |
| Model file integrity | Medium | Partial | Model file hash is verified on the client after download; but the expected hash comes from the manager — compromised manager can poison the expected value |

### Container and infrastructure

| Vector | Severity | Status | Notes |
|---|---|---|---|
| Unrestricted syscalls | Medium | TODO | No seccomp profile; see container hardening TODO in `docs/manager/README.md` |
| Writable root filesystem | Medium | TODO | Binary and config dirs are currently writable; attacker with code execution can persist malware |
| Secrets in environment variables | Medium | Open | Env vars are readable from `/proc/self/environ` within the container; prefer mounted secret files |
| Alpine shell available | Low | Partial | `curl` removed; shell (`/bin/sh`) and busybox remain — full shell access if container is compromised |

---

## Testing Tools

### Web / API

| Tool | Use case |
|---|---|
| **Burp Suite** | Intercept and modify HTTP and WebSocket frames; tamper with session cookies; replay work unit results with altered signatures |
| **OWASP ZAP** | Automated active scan of admin UI and API; identifies missing rate limits, reflected XSS, insecure headers |
| **ffuf** | Fuzz API endpoints and parameters; path traversal payloads in `filename` fields; endpoint discovery |
| **hydra** | Credential brute force against `POST /login`; measures attempts/second to quantify the missing lockout risk |

### WebSocket

| Tool | Use case |
|---|---|
| **websocat** | Raw WebSocket client for manual handshake testing; TOFU race condition PoC; injecting malformed protocol messages |
| **Burp Suite Pro** | WebSocket frame interception, replay, and mutation |

### SSRF

| Tool | Use case |
|---|---|
| **interactsh** (projectdiscovery) | Out-of-band callback server; confirms SSRF on OIDC provider URL and backend download URL without needing a visible response |
| **ssrfmap** | Automated SSRF payload generation against the URL fields |

### TLS / Network

| Tool | Use case |
|---|---|
| **testssl.sh** | TLS version, cipher suite quality, certificate chain validation on both ports |
| **mitmproxy** | Verify workers reject a MITM certificate once manager key pinning is implemented; inspect raw WebSocket frames |
| **nmap** | Service detection; verify only expected ports are exposed; check TLS handshake details |

### Container / Infrastructure

| Tool | Use case |
|---|---|
| **trivy** | CVE scan of the alpine container image: `trivy image synergia-manager:latest` |
| **docker-bench-security** | CIS Docker Benchmark against running container and host configuration |
| **falco** | Runtime monitoring; alerts on unexpected syscalls, file writes to restricted paths, outbound connections from the container |
| **grype** | Alternative CVE scanner with SBOM support |

### Go code fuzzing

| Tool | Use case |
|---|---|
| **go-fuzz / dvyukov** | Fuzz JSON parsers in API handlers; especially model filename, OIDC callback path, and WebSocket message parsing |
| **RESTler** | Stateful REST API fuzzer; learns from API responses and generates multi-step attack sequences |

---

## Prioritised Testing Checklist

Work through these in order — they represent the highest-risk / lowest-effort findings.

### 1 — SSRF via backend download URL

**Target:** `POST /v1/admin/backend` on port 7501 (requires admin session or API key)

```bash
# After logging in, set the download URL to the AWS metadata endpoint
curl -sk -b admin-cookie.txt \
  -X POST https://localhost:7501/v1/admin/backend \
  -H "Content-Type: application/json" \
  -d '{"name":"llama.cpp","version":"test","download_url":"http://169.254.169.254/latest/meta-data/","sha256":""}'

# Then trigger a worker download request (or check manager logs for the fetch attempt)
```

**Expected (safe) result:** connection refused or unreachable — manager should not be able to reach cloud metadata.

**Vulnerable result:** manager logs show a successful fetch or returns metadata content.

---

### 2 — SSRF via OIDC provider URL

**Target:** `PUT /v1/admin/oidc` on port 7501

```bash
curl -sk -b admin-cookie.txt \
  -X PUT https://localhost:7501/v1/admin/oidc \
  -H "Content-Type: application/json" \
  -d '{"enabled":true,"provider_url":"http://169.254.169.254/latest/meta-data/","client_id":"x","client_secret":"x","redirect_url":"https://localhost:7501/auth/oidc/callback"}'
# Then restart the manager and observe startup logs
```

---

### 3 — XSS in errors table

**Target:** Worker-submitted error messages rendered in the admin dashboard errors table.

```bash
# As an authenticated worker (or using CLUSTER_WORKER_KEY), submit an error with a script payload
curl -sk -H "Authorization: Bearer test-worker-key" \
  -X POST https://localhost:7500/v1/errors \
  -H "Content-Type: application/json" \
  -d '{"fingerprint":"test","version":"0.1.0","error":"<img src=x onerror=alert(document.cookie)>","stack":""}'
# Then load the admin dashboard errors section and observe whether the JS executes
```

**Safe result:** the payload is rendered as escaped HTML text.

**Vulnerable result:** browser executes `alert(document.cookie)`.

---

### 4 — Login brute force (no rate limiting)

```bash
# Measure attempts per second — no lockout should mean hundreds/second
hydra -l admin -P /usr/share/wordlists/rockyou.txt \
  -s 7501 -S -t 4 \
  https-form-post "/login:username=^USER^&password=^PASS^:invalid credentials"
```

**Remediation:** add `golang.org/x/time/rate` per-IP rate limiting to `LoginHandler` (e.g., 5 attempts / 30 s).

---

### 5 — Large-payload DoS

```bash
# Generate a ~10 MB payload and send it to the completions endpoint
python3 -c "
import json, requests
payload = {'model':'test','messages':[{'role':'user','content':'A'*10_000_000}]}
r = requests.post('https://localhost:7500/v1/chat/completions',
  json=payload, headers={'Authorization':'Bearer test-api-key'},
  verify=False, timeout=30)
print(r.status_code, len(r.content))
"
```

**Expected (safe) result:** 413 Request Entity Too Large or connection closed immediately.

**Vulnerable result:** request forwarded to llama-server, causing OOM or queue saturation.

---

### 6 — TOFU race condition

Requires two WebSocket clients sharing a fingerprint but holding different keypairs:

```bash
# Start two clients simultaneously with the same --data-dir
# (same identity keypair) but connect to different manager instances — or
# simulate by deleting manager's workers table between attempts.
# Tool: websocat + a custom Go test client (see test/main.go patterns)
```

---

### 7 — Path traversal in model filename

```bash
# Submit a role with a traversal filename via the admin API
curl -sk -b admin-cookie.txt \
  -X POST https://localhost:7501/v1/admin/roles \
  -H "Content-Type: application/json" \
  -d '{"role":"test","model":"test","quantisation":"Q4","filename":"../../../etc/passwd","min_vram_mb":1}'

# Then trigger a worker model download request for that role and observe
# whether the manager serves /etc/passwd content
```

---

---

## Architecture: Split-Image Deployment

### Concept

The current manager is a single binary that handles everything: worker WebSocket connections, the public completions API, binary distribution, the admin UI, and all DB/filesystem writes. Splitting the public-facing surface into dedicated **proxy images** that sit in front of a non-exposed manager reduces attack surface and enables independent hardening.

```
Internet
  │
  ├─► [worker-proxy pods]  (:7500/ws/worker, :7500/v1/*)   ← exposed, hardened
  │         │ HTTP/WS forward (internal network only)
  └─► [api-proxy pods]     (:7500/v1/chat/completions, …)  ← exposed, hardened
              │
        [manager pod]  (admin :7501, DB, FS, cache)         ← never exposed
```

The proxies hold no state and need no filesystem writes, which lets them run on a far more restrictive image:

| Property | Proxy image | Manager image |
|---|---|---|
| Base image | `gcr.io/distroless/static` or `scratch` | Alpine (current) |
| Filesystem | read-only | writable (SQLite, model cache) |
| Shell | none | `/bin/sh` (busybox) |
| Seccomp | strict allow-list | default |
| Capabilities | none | none |
| SUID binaries | none | none |
| Network exposure | yes (LoadBalancer) | ClusterIP only |

In a Kubernetes deployment, enabling the proxy tier is optional — controlled by a Helm values flag. Without it, the manager service is exposed directly as today.

### Security benefit

If a vulnerability in WebSocket frame parsing or the completions endpoint is exploited, the attacker lands in a distroless container with a read-only filesystem, no shell, and no network path to the internet. Lateral movement to the manager requires pivoting through the internal network with no tooling available. The manager — which holds the DB, the admin session secrets, and the model cache — never faces the internet.

### Scalability benefit

The API proxy tier (completions, batch) is stateless HTTP and scales horizontally with no special coordination. For WebSocket connections, each worker proxy pod maintains its own set of live connections; sticky sessions (client/worker pinned to one pod by the load balancer) allow horizontal scale-out without changing the gateway protocol. A message bus (Redis pub/sub, NATS) between proxy pods and the manager would remove the sticky-session requirement and support millions of concurrent WebSocket connections, but that is a significant protocol refactor.

### Implementation challenges

1. **WebSocket state is in-process** — `gateway.PushModelUpdate` iterates over live connections held in the manager's memory. A proxy that only forwards frames cannot receive push messages on behalf of the manager. This is resolved without a message bus: each proxy pod exposes a lightweight internal endpoint that the manager calls when it needs to push an update:

   ```
   POST /internal/push/model-update   (body: ModelUpdate JSON)
   ```

   The manager calls this endpoint on every known proxy pod; each pod fans out the message to its locally-connected workers. The manager discovers live proxy pods via a k8s headless service (all pod IPs via DNS) or by having proxy pods register themselves on startup. Workers that miss a push (e.g., mid-reconnect) receive the current model config on their next successful handshake, which is the same behaviour as today. No external broker required.

2. **TOFU challenge-response** — the manager generates a nonce and must verify the signed response from the same worker. Rather than replicating nonce state across proxy pods, the manager can own the full handshake via two lightweight internal endpoints:

   ```
   POST /internal/challenge/issue?fingerprint=<fp>
       → manager generates nonce, stores {fingerprint → nonce + expiry} in-memory (TTL ~30 s), returns {nonce}

   POST /internal/challenge/verify
       body: {fingerprint, nonce, signature, pubkey}
       → manager looks up stored nonce, verifies Ed25519 signature, returns {ok: true/false}
   ```

   The proxy calls `issue` when the WebSocket upgrades, forwards the `Challenge` message to the worker, receives the `SignedResponse`, then calls `verify` before allowing the connection. All cryptographic logic stays in the manager; the proxy is stateless with respect to authentication. This works with or without sticky sessions and requires no external broker.

3. **Binary patching endpoint** (`/download/*`) — currently served by the manager since it reads from the local cache directory. The proxy tier could serve pre-patched binaries from an object store (S3, GCS) instead, removing this dependency on the manager's filesystem.

4. **Admin UI** (`localhost:7501`) — already separated on a different port and never intended to be exposed. No change needed.

### Phased roadmap

| Phase | Scope | Code changes |
|---|---|---|
| **1 — Security isolation** | Single proxy pod per role (worker-proxy, api-proxy); sticky sessions; ClusterIP manager | Thin reverse-proxy binary (or nginx/Envoy config); Helm chart toggle; no gateway changes |
| **2 — Horizontal scale** | Multi-pod worker proxy; manager fans out pushes via internal proxy endpoints; manager-owned challenge endpoints | Proxy internal API (`/internal/push/*`, `/internal/challenge/*`); manager pod-discovery (headless service or self-registration); client reconnect handling |
| **3 — Binary distribution** | Pre-patch binaries uploaded to object store at release time; proxy serves directly | CI step to patch + upload; manager download handler becomes optional |

---

## Relationship to Security TODOs

The items in this file complement the implementation-level TODOs documented in:

- [`docs/manager/README.md` — Container and Deployment Hardening](manager/README.md#container-and-deployment-hardening) — seccomp, read-only filesystem, secret injection, image signing
- [`docs/manager/README.md` — Protocol TODOs](manager/README.md#challenge-response-worker-handshake-replaces-cluster_worker_key) — challenge-response, result signature verification, signed pushes, operator-signed artifacts
- [`docs/client/README.md` — TODO / Roadmap](client/README.md#todo--roadmap) — pre-shared key elimination, signed local config, manager key pinning, work unit provenance

Fix priority based on exploitability × impact:

1. SSRF (backend URL + OIDC URL) — High / trivially exploitable by any admin account
2. Result signature verification — High / any connected worker can tamper with results
3. Login rate limiting — Medium / prerequisite for brute-force resistance
4. Payload size limits — Medium / unauthenticated DoS vector
5. XSS in error messages — Medium / requires a compromised worker
6. Operator-signed binary manifests — High impact / complex to exploit remotely
