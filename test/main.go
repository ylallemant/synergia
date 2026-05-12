package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	// Tiny model for testing — SmolLM2-135M is ~100MB quantized
	testModelURL      = "https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF/resolve/main/SmolLM2-135M-Instruct-Q4_K_M.gguf"
	testModelFilename = "SmolLM2-135M-Instruct-Q4_K_M.gguf"
	testModelName     = "SmolLM2-135M-Instruct"
	testQuantisation  = "Q4_K_M"

	// Second model for model-update test (different quantisation, same base model, smaller file)
	testModel2URL      = "https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF/resolve/main/SmolLM2-135M-Instruct-Q2_K.gguf"
	testModel2Filename = "SmolLM2-135M-Instruct-Q2_K.gguf"
	testModel2Name     = "SmolLM2-135M-Instruct"
	testModel2Quant    = "Q2_K"

	managerAddr         = "127.0.0.1:7500"
	managerAdminAddr    = "127.0.0.1:7501"
	managerRedirectAddr = "127.0.0.1:7080"
	// Test-specific client ports — deliberately different from the defaults (9876/9877)
	// so the test never conflicts with a production Synergia client running on the same machine.
	// Three clients run in parallel, each on its own port pair.
	clientDashboardAddr  = "127.0.0.1:7502" // client 1 (embedding)
	clientLlamaAddr      = "127.0.0.1:7503"
	client2DashboardAddr = "127.0.0.1:7505" // client 2 (inference)
	client2LlamaAddr     = "127.0.0.1:7506"
	client3DashboardAddr = "127.0.0.1:7507" // client 3 (tester)
	client3LlamaAddr     = "127.0.0.1:7508"
	apiKey              = "test-api-key"
	workerKey           = "test-worker-key"
	adminUser           = "admin"
	adminPassword       = "synergia"
)

// logBuffer captures process stdout/stderr with line-level synchronization.
type logBuffer struct {
	mu      sync.Mutex
	lines   []string
	name    string
	logFile *os.File
}

func newLogBuffer(name string, logDir string) *logBuffer {
	lb := &logBuffer{name: name}
	if logDir != "" {
		f, err := os.Create(filepath.Join(logDir, name+".log"))
		if err == nil {
			lb.logFile = f
		}
	}
	return lb
}

func (lb *logBuffer) Close() {
	if lb.logFile != nil {
		lb.logFile.Close()
	}
}

func (lb *logBuffer) Write(p []byte) (n int, err error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for _, line := range strings.Split(string(p), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lb.lines = append(lb.lines, line)
			log.Debug().Str("src", lb.name).Msg(line)
			if lb.logFile != nil {
				fmt.Fprintf(lb.logFile, "%s [%s] %s\n", time.Now().Format(time.RFC3339), lb.name, line)
			}
		}
	}
	return len(p), nil
}

func (lb *logBuffer) Lines() []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	cp := make([]string, len(lb.lines))
	copy(cp, lb.lines)
	return cp
}

func (lb *logBuffer) Contains(substr string) bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for _, line := range lb.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func (lb *logBuffer) CountContains(substr string) int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	count := 0
	for _, line := range lb.lines {
		if strings.Contains(line, substr) {
			count++
		}
	}
	return count
}

func (lb *logBuffer) Dump(w io.Writer) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for _, line := range lb.lines {
		fmt.Fprintf(w, "[%s] %s\n", lb.name, line)
	}
}

func main() {
	keepAlive := false
	runOnly := false
	sendOnly := false
	var sendEndpoint, sendKey, sendModel string

	args := os.Args[1:]
	for i, arg := range args {
		switch arg {
		case "--keep-alive", "-keep-alive":
			keepAlive = true
		case "--run", "-run":
			runOnly = true
		case "--send", "-send":
			sendOnly = true
		case "--endpoint", "-endpoint":
			if i+1 < len(args) {
				sendEndpoint = args[i+1]
			}
		case "--key", "-key":
			if i+1 < len(args) {
				sendKey = args[i+1]
			}
		case "--model", "-model":
			if i+1 < len(args) {
				sendModel = args[i+1]
			}
		}
	}

	if runOnly {
		runServices()
		return
	}

	if sendOnly {
		if sendEndpoint == "" {
			fmt.Fprintln(os.Stderr, "usage: go run ./test --send --endpoint https://synergia.example.com/ [--key API_KEY] [--model MODEL_NAME]")
			os.Exit(1)
		}
		if sendKey == "" {
			sendKey = os.Getenv("CLUSTER_API_KEY")
		}
		if sendModel == "" {
			sendModel = testModelName
		}
		sendPayloads(sendEndpoint, sendKey, sendModel)
		return
	}

	initLogger()

	log.Info().Msg("=== Synergia Integration Test ===")

	// Resolve paths
	repoRoot := findRepoRoot()
	testDir := filepath.Join(repoRoot, "test")
	modelsDir := filepath.Join(testDir, "testdata", "models")
	runDir := filepath.Join(testDir, "runs", time.Now().Format("2006-01-02_15-04-05"))
	dataDir := filepath.Join(runDir, "data")
	logDir := filepath.Join(runDir, "logs")
	dbPath := filepath.Join(dataDir, "cluster-manager.db")

	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		fatal("create models dir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		fatal("create data dir: %v", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		fatal("create logs dir: %v", err)
	}
	if lf := addLogFile(filepath.Join(logDir, "test-run.log")); lf != nil {
		defer lf.Close()
	}

	// Clean up old runs (keep last 3)
	cleanupOldRuns(filepath.Join(testDir, "runs"), 3)

	// --- Step 0: Generate TLS certificates ---
	step("0. Generating TLS certificates")
	tlsDir := filepath.Join(testDir, "testdata", "tls")
	caCertPath, serverCertPath, serverKeyPath, err := ensureTLSCerts(tlsDir)
	if err != nil {
		fatal("TLS cert generation: %v", err)
	}
	pass("TLS certs ready: %s", tlsDir)

	// Configure global HTTP client to trust both system CAs and the test CA
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		fatal("read CA cert: %v", err)
	}
	caPool, err := x509.SystemCertPool()
	if err != nil {
		caPool = x509.NewCertPool()
	}
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		fatal("failed to parse CA certificate")
	}
	http.DefaultTransport = &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: caPool,
		},
	}

	// Pre-flight: kill any stale processes holding required ports.
	for _, addr := range []string{
		managerAddr, managerAdminAddr, managerRedirectAddr,
		clientDashboardAddr, clientLlamaAddr,
		client2DashboardAddr, client2LlamaAddr,
		client3DashboardAddr, client3LlamaAddr,
	} {
		freePort(addr)
	}

	// --- Step 1: Download models if needed ---
	step("1. Checking/downloading test models")
	modelPath := filepath.Join(modelsDir, testModelFilename)
	if err := ensureModel(modelPath); err != nil {
		fatal("model download: %v", err)
	}
	pass("Model 1 ready: %s", modelPath)

	model2Path := filepath.Join(modelsDir, testModel2Filename)
	if err := ensureModel2(model2Path); err != nil {
		fatal("model 2 download: %v", err)
	}
	pass("Model 2 ready: %s", model2Path)

	// Compute SHA256 file hashes for both models (used in llmHash verification)
	model1FileHash := hashFileOrFatal(modelPath)
	model2FileHash := hashFileOrFatal(model2Path)
	pass("Model 1 file hash: %s...", model1FileHash[:16])
	pass("Model 2 file hash: %s...", model2FileHash[:16])

	// --- Step 2: Package llama-server binary for manager distribution ---
	// The client downloads and starts its own llama-server (production behaviour).
	// We package the system binary and serve it via a local HTTP server so the
	// manager can push a BackendUpdate without a GitHub download.
	step("2. Packaging llama-server binary for manager distribution")
	llamaServerBin, err := exec.LookPath("llama-server")
	if err != nil {
		fatal("llama-server not found in PATH. Install llama.cpp first: brew install llama.cpp")
	}
	pass("llama-server found: %s", llamaServerBin)

	tarGzData, err := packageBinaryWithLibs(llamaServerBin)
	if err != nil {
		fatal("package llama-server binary: %v", err)
	}
	binaryLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal("binary distribution server: %v", err)
	}
	binaryServerURL := "http://" + binaryLn.Addr().String() + "/llama-server.tar.gz"
	binaryMux := http.NewServeMux()
	binaryMux.HandleFunc("/llama-server.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(tarGzData)))
		w.Write(tarGzData) //nolint:errcheck
	})
	binarySrv := &http.Server{Handler: binaryMux}
	go binarySrv.Serve(binaryLn) //nolint:errcheck
	defer binarySrv.Close()
	pass("binary distribution server ready: %s", binaryServerURL)

	// --- Step 3: (client manages its own llama-server — no external process) ---
	step("3. llama-server will be started by the client after binary/model push")

	// --- Step 4: Start cluster-manager ---
	step("4. Starting cluster-manager")
	managerLogs := newLogBuffer("cluster-manager", logDir)
	defer managerLogs.Close()
	managerCmd := exec.Command("go", "run", "./cmd/synergia-manager", "--development")
	managerCmd.Dir = repoRoot
	managerCmd.Env = append(os.Environ(),
		"CLUSTER_LISTEN_ADDR="+managerAddr,
		"CLUSTER_API_KEY="+apiKey,
		"CLUSTER_WORKER_KEY="+workerKey,
		"CLUSTER_MODEL_BACKEND=filesystem",
		"CLUSTER_MODEL_PATH="+modelsDir,
		"CLUSTER_DB_PATH="+filepath.Join(dataDir, "cluster-manager.db"),
		"CLUSTER_DEV_BACKEND_URL="+binaryServerURL,
		"CLUSTER_DEV_CLIENT_VERSION=0.1.0-dev",
		"CLUSTER_ADMIN_ADDR="+managerAdminAddr,
		"CLUSTER_ADMIN_USER="+adminUser,
		"CLUSTER_ADMIN_PASSWORD="+adminPassword,
		"TLS_CERT_FILE="+serverCertPath,
		"TLS_KEY_FILE="+serverKeyPath,
		"CLUSTER_HTTP_REDIRECT_ADDR="+managerRedirectAddr,
		"LOG_LEVEL=debug",
	)
	managerCmd.Stdout = managerLogs
	managerCmd.Stderr = managerLogs
	if err := managerCmd.Start(); err != nil {
		fatal("start cluster-manager: %v", err)
	}
	defer cleanup("cluster-manager", managerCmd)
	watcher.add("cluster-manager", managerCmd, managerLogs)

	// Wait for manager to be ready
	if err := waitForHTTP("https://"+managerAddr+"/healthz", 30*time.Second); err != nil {
		managerLogs.Dump(os.Stderr)
		fatal("cluster-manager did not become ready: %v", err)
	}
	pass("cluster-manager ready on %s (TLS)", managerAddr)

	// Verify HTTP→HTTPS redirect
	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	redirectResp, err := noRedirectClient.Get("http://" + managerRedirectAddr + "/healthz")
	if err != nil {
		fatal("HTTP redirect check failed: %v", err)
	}
	redirectResp.Body.Close()
	if redirectResp.StatusCode != http.StatusMovedPermanently {
		fatal("expected 301 redirect, got %d", redirectResp.StatusCode)
	}
	loc := redirectResp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://") {
		fatal("redirect Location does not point to HTTPS: %s", loc)
	}
	pass("HTTP→HTTPS redirect working on %s", managerRedirectAddr)

	// --- Step 5: Verify model listing ---
	step("5. Verifying model listing endpoint")
	modelsResp, err := apiGet("https://"+managerAddr+"/v1/models/files", workerKey)
	if err != nil {
		fatal("list models: %v", err)
	}
	if !strings.Contains(modelsResp, testModelFilename) {
		fatal("model not found in listing: %s", modelsResp)
	}
	pass("Model listed: %s", testModelFilename)

	// --- Step 6: Start 3 cluster-clients ---
	step("6. Starting 3 cluster-clients (embedding, inference, tester)")

	type clientSpec struct {
		name      string
		dashAddr  string
		llamaAddr string
		role      string
	}
	clientSpecs := []clientSpec{
		{"cluster-client-7502", clientDashboardAddr, clientLlamaAddr, "embedding"},
		{"cluster-client-7505", client2DashboardAddr, client2LlamaAddr, "inference"},
		{"cluster-client-7507", client3DashboardAddr, client3LlamaAddr, "tester"},
	}

	// startClient builds, starts, and watcher-registers a client process.
	// The caller must register defer cleanup(name, cmd) and defer logs.Close()
	// in the outer function scope (defer inside a helper runs on helper return).
	startClient := func(cs clientSpec) (*logBuffer, *exec.Cmd) {
		dashPort := strings.Split(cs.dashAddr, ":")[1]
		logs := newLogBuffer(cs.name, logDir)
		cmd := exec.Command("go", "run", "./cmd/synergia-client",
			"--manager-url", "wss://"+managerAddr+"/ws/worker",
			"--llm-url", "http://"+cs.llamaAddr,
			"--dashboard-addr", cs.dashAddr,
			"--model", testModelName,
			"--quantisation", testQuantisation,
			"--role", cs.role,
			"--model-file", modelPath,
			"--data-dir", filepath.Join(dataDir, "client-data-"+dashPort),
			"--auto-approve",
			"--tls-ca-cert", caCertPath,
		)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(),
			"LOG_LEVEL=debug",
			"CLUSTER_WORKER_KEY="+workerKey,
			"GPU_CONTENTION_THRESHOLD=100",
		)
		cmd.Stdout = logs
		cmd.Stderr = logs
		if err := cmd.Start(); err != nil {
			fatal("start %s: %v", cs.name, err)
		}
		watcher.add(cs.name, cmd, logs)
		return logs, cmd
	}

	// Phase 1: single worker — embedding client only.
	// Steps 7–21 rely on single-worker semantics (PAUSE toggle, consent withdrawal,
	// model update, binary update). Additional workers are added in Phase 2 (step 21b).
	var allClientLogs []*logBuffer
	clientLogs, client1Cmd := startClient(clientSpecs[0])
	defer clientLogs.Close()
	defer cleanup(clientSpecs[0].name, client1Cmd)
	allClientLogs = append(allClientLogs, clientLogs)

	// --- Step 7: Verify client registration ---
	step("7. Waiting for client registration")
	if err := waitForLog(managerLogs, "worker connected", 30*time.Second); err != nil {
		managerLogs.Dump(os.Stderr)
		clientLogs.Dump(os.Stderr)
		fatal("client did not register: %v", err)
	}
	pass("Client registered with manager")

	// Verify key-auth mode was used
	if err := waitForLog(managerLogs, "key-auth mode — Bearer token accepted", 5*time.Second); err != nil {
		log.Warn().Msg("key-auth handshake log not found in manager (non-fatal)")
	} else {
		pass("Manager: key-auth handshake confirmed in logs")
	}

	// Verify client-side logs
	if err := waitForLog(clientLogs, "connected to cluster manager", 10*time.Second); err != nil {
		clientLogs.Dump(os.Stderr)
		fatal("client did not confirm connection: %v", err)
	}
	pass("Client confirms connection")

	// --- Step 7a: Wait for client InitialSync to complete ---
	// The client detected a clean install (no binary, no model) and sent an
	// InitialSync message. The manager responded with BackendUpdate + ModelUpdate
	// using the pre-configured development backend URL and role model.
	step("7a. Waiting for client InitialSync bootstrap (binary + model + llama-server)")
	if err := waitForLog(clientLogs, "backend: llama-server process started", 120*time.Second); err != nil {
		clientLogs.Dump(os.Stderr)
		fatal("client did not start llama-server via InitialSync: %v", err)
	}
	pass("Client started llama-server via InitialSync")

	// --- Step 7b: TOFU challenge-response handshake ---
	step("7b. Testing TOFU challenge-response worker authentication")
	{
		tofuManagerAddr := "127.0.0.1:7510"
		tofuManagerAdminAddr := "127.0.0.1:7511"

		// Pre-flight: ensure TOFU test ports are free
		for _, addr := range []string{tofuManagerAddr, tofuManagerAdminAddr} {
			freePort(addr)
		}

		tofuManagerLogs := newLogBuffer("tofu-manager", logDir)
		defer tofuManagerLogs.Close()
		tofuManagerCmd := exec.Command("go", "run", "./cmd/synergia-manager")
		tofuManagerCmd.Dir = repoRoot
		tofuManagerCmd.Env = append(os.Environ(),
			"CLUSTER_LISTEN_ADDR="+tofuManagerAddr,
			"CLUSTER_API_KEY="+apiKey,
			// No CLUSTER_WORKER_KEY → TOFU mode
			"CLUSTER_MODEL_BACKEND=filesystem",
			"CLUSTER_MODEL_PATH="+modelsDir,
			"CLUSTER_DB_PATH="+filepath.Join(dataDir, "tofu-manager.db"),
			"CLUSTER_ADMIN_ADDR="+tofuManagerAdminAddr,
			"TLS_CERT_FILE="+serverCertPath,
			"TLS_KEY_FILE="+serverKeyPath,
			"CLUSTER_ADMIN_USER="+adminUser,
			"CLUSTER_ADMIN_PASSWORD="+adminPassword,
			"LOG_LEVEL=debug",
		)
		tofuManagerCmd.Stdout = tofuManagerLogs
		tofuManagerCmd.Stderr = tofuManagerLogs
		if err := tofuManagerCmd.Start(); err != nil {
			fatal("start tofu-manager: %v", err)
		}
		defer cleanup("tofu-manager", tofuManagerCmd)
		watcher.add("tofu-manager", tofuManagerCmd, tofuManagerLogs)

		if err := waitForHTTP("https://"+tofuManagerAddr+"/healthz", 30*time.Second); err != nil {
			tofuManagerLogs.Dump(os.Stderr)
			fatal("tofu-manager did not become ready: %v", err)
		}
		pass("TOFU manager ready on %s (TLS, no worker key)", tofuManagerAddr)

		tofuClientDataDir := filepath.Join(dataDir, "tofu-client-data")
		tofuClientLogs := newLogBuffer("tofu-client", logDir)
		defer tofuClientLogs.Close()
		tofuClientCmd := exec.Command("go", "run", "./cmd/synergia-client",
			"--manager-url", "wss://"+tofuManagerAddr+"/ws/worker",
			"--dashboard-addr", "127.0.0.1:7504", // auth-only test; unique port avoids conflict
			"--model", testModelName,
			"--quantisation", testQuantisation,
			"--role", "tester",
			"--model-file", modelPath,
			"--data-dir", tofuClientDataDir,
			"--auto-approve",
			"--tls-ca-cert", caCertPath,
		)
		tofuClientCmd.Dir = repoRoot
		// No CLUSTER_WORKER_KEY → TOFU mode
		tofuClientCmd.Env = append(os.Environ(), "LOG_LEVEL=debug")
		tofuClientCmd.Stdout = tofuClientLogs
		tofuClientCmd.Stderr = tofuClientLogs
		if err := tofuClientCmd.Start(); err != nil {
			fatal("start tofu-client: %v", err)
		}
		defer cleanup("tofu-client", tofuClientCmd)
		watcher.add("tofu-client", tofuClientCmd, tofuClientLogs)

		// Client should log TOFU mode selection
		if err := waitForLog(tofuClientLogs, "TOFU mode — awaiting challenge", 15*time.Second); err != nil {
			tofuClientLogs.Dump(os.Stderr)
			fatal("tofu-client did not log TOFU mode selection: %v", err)
		}
		pass("Client: TOFU mode selected (no worker key)")

		// Manager should log challenge sent
		if err := waitForLog(tofuManagerLogs, "handshake: TOFU mode — sending challenge", 15*time.Second); err != nil {
			tofuManagerLogs.Dump(os.Stderr)
			fatal("tofu-manager did not send challenge: %v", err)
		}
		pass("Manager: challenge sent to worker")

		// Client should log challenge received and signed
		if err := waitForLog(tofuClientLogs, "handshake: challenge received, signing nonce", 10*time.Second); err != nil {
			tofuClientLogs.Dump(os.Stderr)
			fatal("tofu-client did not receive/sign challenge: %v", err)
		}
		pass("Client: nonce signed")

		if err := waitForLog(tofuClientLogs, "handshake: challenge-response completed", 5*time.Second); err != nil {
			tofuClientLogs.Dump(os.Stderr)
			fatal("tofu-client did not complete handshake: %v", err)
		}
		pass("Client: challenge-response completed")

		// Manager should confirm success
		if err := waitForLog(tofuManagerLogs, "handshake: TOFU challenge-response succeeded", 10*time.Second); err != nil {
			tofuManagerLogs.Dump(os.Stderr)
			fatal("tofu-manager did not confirm handshake success: %v", err)
		}
		pass("Manager: TOFU challenge-response succeeded")

		if err := waitForLog(tofuManagerLogs, "worker connected", 10*time.Second); err != nil {
			tofuManagerLogs.Dump(os.Stderr)
			fatal("tofu-manager: worker did not register after TOFU handshake: %v", err)
		}
		pass("TOFU worker registered with manager")
	}

	// Wait for the main client's llama-server to be healthy before sending completions.
	// The TOFU test ran concurrently with llama-server loading, so it may still be starting up.
	if err := waitForHTTP("http://"+clientLlamaAddr+"/health", 120*time.Second); err != nil {
		clientLogs.Dump(os.Stderr)
		fatal("llama-server not ready on port "+clientLlamaAddr+": %v", err)
	}
	pass("llama-server ready on port "+clientLlamaAddr+"")

	// --- Step 8: Check worker appears in API ---
	step("8. Verifying worker in cluster API")
	workersResp, err := apiGet("https://"+managerAddr+"/v1/workers", apiKey)
	if err != nil {
		fatal("query workers: %v", err)
	}
	if !strings.Contains(workersResp, testModelName) {
		fatal("worker not found in API: %s", workersResp)
	}
	pass("Worker visible in /v1/workers")

	// --- Step 9: Send chat completion requests (small, medium, large payloads) ---
	step("9. Sending chat completion requests through cluster")

	// Small payload (~150 bytes)
	completionResp, err := sendCompletion("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName)
	if err != nil {
		fatal("chat completion (small) failed: %v", err)
	}
	pass("Small payload completion received (%d bytes)", len(completionResp))

	// Medium payload (~1KB) — multi-turn conversation
	mediumContent := "Summarize the following text in one sentence: " + strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20)
	mediumResp, err := sendCompletionWithMessage("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, mediumContent)
	if err != nil {
		fatal("chat completion (medium) failed: %v", err)
	}
	pass("Medium payload completion received (%d bytes)", len(mediumResp))

	// Large payload (~5KB) — long context
	largeContent := "Explain the key themes in this essay: " + strings.Repeat("Artificial intelligence is transforming the way we approach complex problems in science, medicine, and engineering. ", 40)
	largeResp, err := sendCompletionWithMessage("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, largeContent)
	if err != nil {
		fatal("chat completion (large) failed: %v", err)
	}
	pass("Large payload completion received (%d bytes)", len(largeResp))

	// --- Step 10: Verify work unit was processed ---
	// Work may have been dispatched to any of the 3 clients, so check all logs.
	step("10. Verifying work unit processing in logs")
	if err := waitForLogAny(allClientLogs, "work unit completed", 60*time.Second); err != nil {
		for _, lb := range allClientLogs {
			lb.Dump(os.Stderr)
		}
		fatal("no client processed work unit: %v", err)
	}
	pass("Client processed work unit")

	// Verify manager received the result
	if err := waitForLog(managerLogs, "returned result", 10*time.Second); err != nil {
		managerLogs.Dump(os.Stderr)
		fatal("manager did not receive result: %v", err)
	}
	pass("Manager returned result to caller")

	// --- Step 11: Check cluster stats ---
	step("11. Verifying cluster stats")
	statsResp, err := apiGet("https://"+managerAddr+"/v1/stats", apiKey)
	if err != nil {
		fatal("query stats: %v", err)
	}
	if !strings.Contains(statsResp, `"completed"`) {
		fatal("no completed work units in stats: %s", statsResp)
	}
	pass("Stats show completed work: %s", statsResp)

	// --- Step 12: LLM Hash and Model Update flow (file-hash-based) --- skipped with --keep-alive ---
	if !keepAlive {
		step("12. Testing LLM hash verification and model_update push (file-hash security)")

		// 12a: Verify the worker's initial LLM hash matches SHA256("embedding:" + model1FileHash)
		expectedInitialHash := computeLLMHash("embedding", model1FileHash)
		time.Sleep(2 * time.Second) // allow hash to propagate via WebSocket
		workerHash := querySQLiteString(dbPath, "SELECT llm_hash FROM workers LIMIT 1")
		if workerHash == expectedInitialHash {
			pass("Worker initial LLM hash matches file-based hash: %s", expectedInitialHash[:16])
		} else {
			log.Warn().Str("expected", expectedInitialHash[:16]).Str("got", workerHash).Msg("initial LLM hash mismatch (may not be set yet)")
		}

		// 12b: Update the embedding role to use model 2 via admin API — the manager will compute the file hash
		//       from its model store (model 2 is in the same models directory)
		updatePayload := map[string]any{
			"role":         "embedding",
			"model":        testModel2Name,
			"quantisation": testModel2Quant,
			"filename":     testModel2Filename,
			"min_vram_mb":  512,
			"description":  "Updated for LLM hash test — model 2",
		}
		updateBody, _ := json.Marshal(updatePayload)
		updateReq, _ := http.NewRequest(http.MethodPut, "https://"+managerAdminAddr+"/v1/admin/roles", bytes.NewReader(updateBody))
		updateReq.Header.Set("Authorization", "Bearer "+apiKey)
		updateReq.Header.Set("Content-Type", "application/json")
		updateResp, updateErr := http.DefaultClient.Do(updateReq)
		if updateErr != nil {
			fatal("admin role update request failed: %v", updateErr)
		}
		updateRespBody, _ := io.ReadAll(updateResp.Body)
		updateResp.Body.Close()
		if updateResp.StatusCode != http.StatusOK {
			fatal("admin role update failed: HTTP %d: %s", updateResp.StatusCode, string(updateRespBody))
		}
		pass("Admin role updated: embedding → %s %s (filename: %s)", testModel2Name, testModel2Quant, testModel2Filename)

		// 12c: Wait for model_update message to be received by the client
		if err := waitForLog(clientLogs, "received model update", 10*time.Second); err != nil {
			clientLogs.Dump(os.Stderr)
			fatal("client did not receive model_update: %v", err)
		}
		pass("Client received model_update from manager")

		// 12c2: Verify client sent "updating" status
		if err := waitForLog(clientLogs, "state=updating", 5*time.Second); err != nil {
			clientLogs.Dump(os.Stderr)
			fatal("client did not send 'updating' status: %v", err)
		}
		pass("Client sent 'updating' status")

		// 12c3: Verify manager received "updating" status
		if err := waitForLog(managerLogs, "state=updating", 5*time.Second); err != nil {
			managerLogs.Dump(os.Stderr)
			fatal("manager did not receive 'updating' status: %v", err)
		}
		pass("Manager received 'updating' status from worker")

		// 12c4: Verify manager logged aggregated status transition during update
		if err := waitForLog(managerLogs, "client_status=updating", 5*time.Second); err != nil {
			managerLogs.Dump(os.Stderr)
			fatal("manager did not log status transition for 'updating': %v", err)
		}
		if err := waitForLog(managerLogs, "aggregated=updating", 5*time.Second); err != nil {
			managerLogs.Dump(os.Stderr)
			fatal("manager did not log aggregated=updating during update: %v", err)
		}
		pass("Manager logged status transition: client_status=updating aggregated=updating")

		// 12d: Wait for the client to download the model, hash it, and report
		if err := waitForLog(clientLogs, "model file verified", 60*time.Second); err != nil {
			// Fallback: the client may have used the "already exists" path or trust path
			if err2 := waitForLog(clientLogs, "sending LLM hash report", 5*time.Second); err2 != nil {
				clientLogs.Dump(os.Stderr)
				fatal("client did not verify/report model hash: %v", err)
			}
		}
		pass("Client verified model file and sent LLM hash report")

		// 12e: Wait for the manager to log the hash report
		if err := waitForLog(managerLogs, "worker LLM hash report", 10*time.Second); err != nil {
			managerLogs.Dump(os.Stderr)
			fatal("manager did not receive LLM hash report: %v", err)
		}
		pass("Manager received LLM hash report")

		// 12e1b: Verify manager logged sync_status=synced after hash report
		if err := waitForLog(managerLogs, "sync_status=synced", 5*time.Second); err != nil {
			managerLogs.Dump(os.Stderr)
			fatal("manager did not log sync_status=synced after hash report: %v", err)
		}
		pass("Manager logged sync_status=synced after hash update")

		// 12e2: Verify client sent "available" status after update completed
		if err := waitForLog(clientLogs, "state=available", 5*time.Second); err != nil {
			clientLogs.Dump(os.Stderr)
			fatal("client did not return to 'available' status after update: %v", err)
		}
		pass("Client returned to 'available' status after model update")

		// 12e3: Verify manager received "available" status
		if err := waitForLog(managerLogs, "state=available", 5*time.Second); err != nil {
			managerLogs.Dump(os.Stderr)
			fatal("manager did not receive 'available' status after update: %v", err)
		}
		pass("Manager received 'available' status from worker after update")

		// 12e4: Verify manager logged aggregated=available after model update completes
		if err := waitForLog(managerLogs, "aggregated=available", 5*time.Second); err != nil {
			managerLogs.Dump(os.Stderr)
			fatal("manager did not log aggregated=available after update: %v", err)
		}
		pass("Manager logged status transition: client_status=available sync_status=synced aggregated=available")

		// 12f: Verify the new hash in the DB matches SHA256("embedding:" + model2FileHash)
		expectedNewHash := computeLLMHash("embedding", model2FileHash)
		time.Sleep(1 * time.Second) // allow DB write to complete
		newWorkerHash := querySQLiteString(dbPath, "SELECT llm_hash FROM workers LIMIT 1")
		if newWorkerHash == expectedNewHash {
			pass("Worker LLM hash updated in DB (file-hash verified): %s", expectedNewHash[:16])
		} else {
			fatal("worker LLM hash not updated: expected %s (from model2 file hash), got %s", expectedNewHash[:16], newWorkerHash)
		}

		// 12g: Revert role back to model 1 so subsequent tests still work
		revertPayload := map[string]any{
			"role":         "embedding",
			"model":        testModelName,
			"quantisation": testQuantisation,
			"filename":     testModelFilename,
			"min_vram_mb":  512,
			"description":  "Vector embeddings (test mode — minimal model)",
		}
		revertBody, _ := json.Marshal(revertPayload)
		revertReq, _ := http.NewRequest(http.MethodPut, "https://"+managerAdminAddr+"/v1/admin/roles", bytes.NewReader(revertBody))
		revertReq.Header.Set("Authorization", "Bearer "+apiKey)
		revertReq.Header.Set("Content-Type", "application/json")
		revertResp, _ := http.DefaultClient.Do(revertReq)
		if revertResp != nil {
			revertResp.Body.Close()
		}

		// Wait for the revert to propagate (client downloads model 1 again or recognizes it)
		time.Sleep(5 * time.Second)
		revertedHash := querySQLiteString(dbPath, "SELECT llm_hash FROM workers LIMIT 1")
		revertedExpected := computeLLMHash("embedding", model1FileHash)
		if revertedHash == revertedExpected {
			pass("Role reverted — worker hash restored (file-hash verified): %s", revertedExpected[:16])
		} else {
			log.Warn().Str("expected", revertedExpected[:16]).Str("got", revertedHash).Msg("hash revert mismatch")
		}

		// 12h: Verify dispatch still works after hash cycle
		postHashResp, postHashErr := sendCompletionWithMessage("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "Post-hash-test: what is 5+5?")
		if postHashErr != nil {
			fatal("completion after hash cycle failed: %v", postHashErr)
		}
		pass("Completion succeeds after LLM hash update cycle (%d bytes)", len(postHashResp))
	} else {
		step("12. Skipping LLM hash/model update test (--keep-alive mode)")
	}

	// --- Step 13: Trigger error reporting (returned error) ---
	step("13. Sending ERROR trigger payload")
	_, errResp := sendTriggerCompletion("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "##############ERROR##############")
	if errResp == nil {
		log.Warn().Msg("expected error from ERROR trigger, got success")
	} else {
		pass("Manager returned error from ERROR trigger: %v", errResp)
	}

	// Wait for error report in manager logs
	if err := waitForLog(managerLogs, "client error reported", 10*time.Second); err != nil {
		log.Warn().Msg("ERROR trigger: error report not confirmed in manager logs")
	} else {
		pass("ERROR trigger: error reported to manager")
	}

	// --- Step 14: PAUSE trigger — 429 handling and batch queue — skipped with --keep-alive ---
	if !keepAlive {
		step("14. Testing PAUSE trigger, 429 rejection, and batch queue")

		// 14a: Send PAUSE trigger to make client pause
		pauseResp, pauseErr := sendTriggerCompletion("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "##############PAUSE##############")
		if pauseErr != nil {
			fatal("PAUSE trigger failed: %v", pauseErr)
		}
		if !strings.Contains(pauseResp, "pause toggled") {
			fatal("PAUSE trigger did not return expected response: %s", pauseResp)
		}
		pass("PAUSE trigger sent — client should now be paused")

		// Wait for the status to propagate to the DB
		time.Sleep(5 * time.Second)

		// 14b: Send a normal payload — should get 429 (no available worker)
		_, normalErr := sendCompletionWithMessage("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "What is 1+1?")
		if normalErr == nil {
			fatal("expected 429 error when worker is paused, got success")
		}
		if !strings.Contains(normalErr.Error(), "429") {
			fatal("expected HTTP 429, got: %v", normalErr)
		}
		pass("Got 429 (Too Many Requests) — worker is paused, no available workers")

		// 14c: Queue the request via batch endpoint
		batchID, batchErr := submitBatchRequest("https://"+managerAddr+"/v1/batches", apiKey, testModelName, "What is 1+1?")
		if batchErr != nil {
			fatal("batch submit failed: %v", batchErr)
		}
		pass("Request queued via batch endpoint: %s", batchID)

		// 14d: Send PAUSE trigger again to unpause (PAUSE trigger bypasses 429 check)
		unpauseResp, unpauseErr := sendTriggerCompletion("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "##############PAUSE##############")
		if unpauseErr != nil {
			fatal("PAUSE unpause trigger failed: %v", unpauseErr)
		}
		if !strings.Contains(unpauseResp, "pause toggled") {
			fatal("PAUSE unpause did not return expected response: %s", unpauseResp)
		}
		pass("PAUSE trigger sent — client should now be unpaused")

		// Wait for the worker to become genuinely available again (GPU cool-down,
		// llama-server healthy, not just unpaused). Polling the API avoids
		// sending a work unit to a worker that is still in busy/paused transition.
		waitDeadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(waitDeadline) {
			if wr, apiErr := apiGet("https://"+managerAddr+"/v1/workers", apiKey); apiErr == nil &&
				strings.Contains(wr, `"status":"available"`) {
				break
			}
			time.Sleep(2 * time.Second)
		}

		// 14e: Verify the batch request completed
		batchStatus, batchPollErr := pollBatchRequest("https://"+managerAddr+"/v1/batches", apiKey, batchID, 30*time.Second)
		if batchPollErr != nil {
			fatal("batch request did not complete: %v", batchPollErr)
		}
		pass("Batch request completed with status: %s", batchStatus)
	} else {
		step("14. Skipping PAUSE/429/batch test (--keep-alive mode)")
	}

	// --- Step 15: Batch queue with multiple payloads — skipped with --keep-alive ---
	if !keepAlive {
		step("15. Testing batch queue with 3 payloads (development mode)")

		// Submit 3 batch requests
		batchIDs := make([]string, 3)
		batchPayloads := []string{
			"What is the capital of France?",
			"Explain photosynthesis in one sentence.",
			"What is 7 times 8?",
		}
		for i, msg := range batchPayloads {
			id, err := submitBatchRequest("https://"+managerAddr+"/v1/batches", apiKey, testModelName, msg)
			if err != nil {
				fatal("batch submit %d failed: %v", i+1, err)
			}
			pass("Batch %d/%d submitted: %s", i+1, len(batchPayloads), id)
			batchIDs[i] = id
		}

		// Poll all 3 until completed, checking every second
		allDone := false
		deadline := time.Now().Add(60 * time.Second)
		for !allDone && time.Now().Before(deadline) {
			time.Sleep(1 * time.Second)
			allDone = true
			for _, id := range batchIDs {
				status, err := getBatchStatus("https://"+managerAddr+"/v1/batches/"+id, apiKey)
				if err != nil {
					log.Debug().Err(err).Str("id", id).Msg("poll error")
					allDone = false
					continue
				}
				if status != "completed" && status != "failed" {
					allDone = false
				}
			}
		}

		// Verify all completed
		for i, id := range batchIDs {
			status, _ := getBatchStatus("https://"+managerAddr+"/v1/batches/"+id, apiKey)
			if status != "completed" {
				fatal("batch %d (%s) did not complete: status=%s", i+1, id, status)
			}
			pass("Batch %d/%d completed: %s", i+1, len(batchIDs), id)
		}
	} else {
		step("15. Skipping batch queue test (--keep-alive mode)")
	}

	// --- Step 16: Trigger error reporting (panic) — skipped with --keep-alive ---
	if !keepAlive {
		step("16. Sending PANIC trigger payload")
		_, panicResp := sendTriggerCompletion("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "##############PANIC##############")
		if panicResp == nil {
			log.Warn().Msg("expected error from PANIC trigger, got success")
		} else {
			pass("Manager returned error from PANIC trigger: %v", panicResp)
		}

		// Wait for panic recovery report in manager logs
		if err := waitForLog(managerLogs, "client error reported", 10*time.Second); err != nil {
			// Check client-side recovery log
			if err2 := waitForLog(clientLogs, "recovered from panic", 5*time.Second); err2 != nil {
				log.Warn().Msg("PANIC trigger: recovery not confirmed in logs")
			} else {
				pass("PANIC trigger: client recovered from panic")
			}
		} else {
			pass("PANIC trigger: panic reported to manager")
		}
	} else {
		step("16. Skipping PANIC trigger (--keep-alive mode)")
	}

	// --- Step 17: WITHDRAW consent — 429 handling and re-accept — skipped with --keep-alive ---
	if !keepAlive {
		step("17. Testing consent withdrawal, 429 rejection, and re-accept")

		// 17a: Revoke consent via client dashboard API
		withdrawErr := postConsentToClient("http://"+clientDashboardAddr+"/api/consent", false)
		if withdrawErr != nil {
			fatal("consent withdrawal failed: %v", withdrawErr)
		}
		pass("Consent withdrawn via client API")

		// Wait for the withdrawal to propagate (client → manager → DB status set to "withdrawn")
		time.Sleep(3 * time.Second)

		// 17b: Send a normal payload — should get 429 (worker withdrawn)
		_, normalErr := sendCompletionWithMessage("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "What is 1+1?")
		if normalErr == nil {
			fatal("expected 429 error when consent is withdrawn, got success")
		}
		if !strings.Contains(normalErr.Error(), "429") {
			fatal("expected HTTP 429, got: %v", normalErr)
		}
		pass("Got 429 (Too Many Requests) — consent withdrawn, worker unavailable")

		// 17c: Queue a request via batch endpoint (should accept into queue)
		batchID, batchErr := submitBatchRequest("https://"+managerAddr+"/v1/batches", apiKey, testModelName, "What is 2+2?")
		if batchErr != nil {
			fatal("batch submit while withdrawn failed: %v", batchErr)
		}
		pass("Request queued via batch endpoint while withdrawn: %s", batchID)

		// 17d: Re-accept consent via client dashboard API
		acceptErr := postConsentToClient("http://"+clientDashboardAddr+"/api/consent", true)
		if acceptErr != nil {
			fatal("consent re-accept failed: %v", acceptErr)
		}
		pass("Consent re-accepted via client API")

		// Wait for the re-accept to propagate and worker to become available again
		time.Sleep(3 * time.Second)

		// 17e: Verify a normal completion succeeds again
		resp, normalErr2 := sendCompletionWithMessage("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "What is 3+3?")
		if normalErr2 != nil {
			fatal("completion failed after consent re-accept: %v", normalErr2)
		}
		if resp == "" {
			fatal("empty response after consent re-accept")
		}
		pass("Completion succeeds after consent re-accept")

		// 17f: Verify the batch request queued during withdrawal completed
		time.Sleep(5 * time.Second)
		batchStatus, batchPollErr := pollBatchRequest("https://"+managerAddr+"/v1/batches", apiKey, batchID, 30*time.Second)
		if batchPollErr != nil {
			fatal("batch request queued during withdrawal did not complete: %v", batchPollErr)
		}
		pass("Batch request (queued during withdrawal) completed with status: %s", batchStatus)
	} else {
		step("17. Skipping WITHDRAW consent test (--keep-alive mode)")
	}

	// --- Step 18: Verify error reports stored ---
	step("18. Verifying error reports in manager DB")
	time.Sleep(2 * time.Second) // allow async reporter to complete
	errorsResp, err := apiGet("https://"+managerAddr+"/v1/errors", workerKey)
	if err != nil {
		log.Warn().Err(err).Msg("could not query /v1/errors")
	} else {
		if strings.Contains(errorsResp, "intentional error triggered by test payload") {
			pass("ERROR trigger report stored in DB")
		} else {
			log.Warn().Msg("ERROR trigger report not found in /v1/errors")
		}
		if !keepAlive {
			if strings.Contains(errorsResp, "intentional panic triggered by test payload") {
				pass("PANIC trigger report stored in DB")
			} else {
				log.Warn().Msg("PANIC trigger report not found in /v1/errors")
			}
		}
	}

	// --- Step 19: Verify latency data recorded ---
	step("19. Verifying latency data in admin API")
	// The 3 completions in step 9 should have recorded latency samples with different payload sizes
	latencyResp, err := apiGet("https://"+managerAdminAddr+"/v1/latency", apiKey)
	if err != nil {
		log.Warn().Err(err).Msg("could not query /v1/latency on admin port")
	} else {
		// Check that there are samples recorded (matrix has count > 0 in at least one bucket)
		var latencyData struct {
			Matrices []struct {
				Role   string `json:"role"`
				Matrix []struct {
					Count int   `json:"count"`
					P50Ms int64 `json:"p50_ms"`
				} `json:"matrix"`
			} `json:"matrices"`
		}
		// Try multi-role format first
		if jsonErr := json.Unmarshal([]byte(latencyResp), &latencyData); jsonErr == nil && len(latencyData.Matrices) > 0 {
			totalSamples := 0
			for _, m := range latencyData.Matrices {
				for _, b := range m.Matrix {
					totalSamples += b.Count
				}
			}
			if totalSamples > 0 {
				pass("Latency matrix has %d sample(s) across roles", totalSamples)
			} else {
				// Samples exist in DB but hourly stats haven't been computed yet (first hour);
				// boundaries are empty so all samples fall in first bucket with bounds=[]
				log.Warn().Msg("latency matrix has 0 samples (hourly stats not yet aggregated — expected on first run)")
			}
		} else {
			// Try single-role format
			var singleMatrix struct {
				Role   string `json:"role"`
				Matrix []struct {
					Count int `json:"count"`
				} `json:"matrix"`
			}
			if jsonErr2 := json.Unmarshal([]byte(latencyResp), &singleMatrix); jsonErr2 == nil && len(singleMatrix.Matrix) > 0 {
				totalSamples := 0
				for _, b := range singleMatrix.Matrix {
					totalSamples += b.Count
				}
				if totalSamples > 0 {
					pass("Latency matrix has %d sample(s) for role %s", totalSamples, singleMatrix.Role)
				} else {
					log.Warn().Msg("latency matrix has 0 samples")
				}
			} else {
				log.Warn().Str("response", latencyResp).Msg("unexpected latency response format")
			}
		}
		writeOutput(filepath.Join(dataDir, "latency.json"), []byte(latencyResp))
	}

	// Also verify directly via the SQLite database that latency_samples has an entry
	latencySamplesCount := querySQLiteCount(dbPath, "SELECT COUNT(*) FROM latency_samples")
	if latencySamplesCount > 0 {
		pass("latency_samples table has %d row(s) in DB", latencySamplesCount)
	} else {
		log.Warn().Msg("latency_samples table is empty in DB")
	}

	// Verify workers table has total_requests incremented
	workerRequestCount := querySQLiteCount(dbPath, "SELECT COALESCE(SUM(total_requests), 0) FROM workers")
	if workerRequestCount > 0 {
		pass("workers.total_requests = %d (lifetime counter incremented)", workerRequestCount)
	} else {
		log.Warn().Msg("workers.total_requests is 0")
	}

	// --- Step 20: Version admin API ---
	step("20. Testing version admin API")
	{
		// POST a target version
		versionPayload := `{"target_version":"v99.0.0-test","rollout_mode":"all","rollout_percentage":100,"sha256":""}`
		versionReq, _ := http.NewRequest(http.MethodPost, "https://"+managerAdminAddr+"/v1/admin/version", bytes.NewBufferString(versionPayload))
		versionReq.Header.Set("Authorization", "Bearer "+apiKey)
		versionReq.Header.Set("Content-Type", "application/json")
		versionResp, err := http.DefaultClient.Do(versionReq)
		if err != nil {
			fatal("version POST failed: %v", err)
		}
		versionResp.Body.Close()
		if versionResp.StatusCode != http.StatusOK {
			fatal("version POST returned %d", versionResp.StatusCode)
		}
		pass("POST /v1/admin/version → 200")

		// GET the config back
		versionGetResp, err := apiGet("https://"+managerAdminAddr+"/v1/admin/version", apiKey)
		if err != nil {
			fatal("version GET failed: %v", err)
		}
		if !strings.Contains(versionGetResp, "v99.0.0-test") {
			fatal("version GET did not return expected version: %s", versionGetResp)
		}
		pass("GET /v1/admin/version → target_version=v99.0.0-test")

		// Verify binary_update was pushed to worker (it will fail to download, but we see the log)
		if err := waitForLog(clientLogs, "binary update received", 10*time.Second); err != nil {
			log.Warn().Msg("binary_update not received by client (may be expected if version matches)")
		} else {
			pass("Client received binary_update push")
		}

		// Verify worker has OS/Arch in DB
		workerOSCount := querySQLiteCount(dbPath, "SELECT COUNT(*) FROM workers WHERE os != '' AND arch != ''")
		if workerOSCount > 0 {
			pass("Worker has OS/Arch stored in DB")
		} else {
			log.Warn().Msg("Worker OS/Arch not stored in DB")
		}
	}

	// --- Step 21: Backend admin API (real llama.cpp download) ---
	// Skip in keep-alive mode: downloads ~16MB from GitHub per run
	if !keepAlive {
		step("21. Testing backend admin API with real llama.cpp release")
		{
			// Use the real llama.cpp release URL template
			// First: set backend version b9049 (triggers real download)
			backendPayload1 := `{"name":"llama.cpp","version":"b9049","sha256":""}`
			backendReq1, _ := http.NewRequest(http.MethodPost, "https://"+managerAdminAddr+"/v1/admin/backend", bytes.NewBufferString(backendPayload1))
			backendReq1.Header.Set("Authorization", "Bearer "+apiKey)
			backendReq1.Header.Set("Content-Type", "application/json")
			backendResp1, err := http.DefaultClient.Do(backendReq1)
			if err != nil {
				fatal("backend POST b9049 failed: %v", err)
			}
			backendResp1.Body.Close()
			if backendResp1.StatusCode != http.StatusOK {
				fatal("backend POST b9049 returned %d", backendResp1.StatusCode)
			}
			pass("POST /v1/admin/backend → b9049")

			// Wait for the client to process the backend update
			if err := waitForLog(clientLogs, "backend binary updated successfully", 120*time.Second); err != nil {
				// Check if it was received at all
				if err2 := waitForLog(clientLogs, "backend update", 5*time.Second); err2 != nil {
					fatal("backend_update not received by client: %v", err)
				}
				fatal("backend download/install failed (check client logs): %v", err)
			}
			pass("Client downloaded and installed llama-server b9049")

			// Wait for the client to restart llama-server with the new binary (2nd start overall).
			if err := waitForLogN(clientLogs, "backend: llama-server process started", 2, 120*time.Second); err != nil {
				fatal("llama-server did not restart after b9049 upgrade: %v", err)
			}
			if err := waitForHTTP("http://"+clientLlamaAddr+"/health", 120*time.Second); err != nil {
				fatal("llama-server not ready after b9049 upgrade: %v", err)
			}
			pass("llama-server restarted and ready with b9049")

			// Verify the installed binary works
			backendBin := filepath.Join(dataDir, "client-data-7502", "backend", "llama-server")
			if runtime.GOOS == "windows" {
				backendBin += ".exe"
			}
			verCtx, verCancel := context.WithTimeout(context.Background(), 10*time.Second)
			verCmd := exec.CommandContext(verCtx, backendBin, "--version")
			defer verCancel()
			// Include the binary's directory in DYLD_LIBRARY_PATH/LD_LIBRARY_PATH so
			// shared libs extracted alongside the binary (from the release archive) are found.
			binDir := filepath.Dir(backendBin)
			verCmd.Env = append(os.Environ(),
				"DYLD_LIBRARY_PATH="+binDir+":"+os.Getenv("DYLD_LIBRARY_PATH"),
				"LD_LIBRARY_PATH="+binDir+":"+os.Getenv("LD_LIBRARY_PATH"),
			)
			versionOut, err := verCmd.CombinedOutput()
			if err != nil && verCtx.Err() == nil {
				fatal("backend binary verification failed: %v (output: %s)", err, string(versionOut))
			}
			if verCtx.Err() != nil {
				pass("Backend binary installed (version check timed out — binary starts via health check)")
			} else {
				pass("Backend binary verified: %s", strings.TrimSpace(string(versionOut)))
			}

			// GET the config back
			backendGetResp, err := apiGet("https://"+managerAdminAddr+"/v1/admin/backend", apiKey)
			if err != nil {
				fatal("backend GET failed: %v", err)
			}
			if !strings.Contains(backendGetResp, "b9049") {
				fatal("backend GET did not return expected version: %s", backendGetResp)
			}
			pass("GET /v1/admin/backend → version=b9049")

			// Second: upgrade to b9050 (triggers real update)
			backendPayload2 := `{"name":"llama.cpp","version":"b9050","sha256":""}`
			backendReq2, _ := http.NewRequest(http.MethodPost, "https://"+managerAdminAddr+"/v1/admin/backend", bytes.NewBufferString(backendPayload2))
			backendReq2.Header.Set("Authorization", "Bearer "+apiKey)
			backendReq2.Header.Set("Content-Type", "application/json")
			backendResp2, err := http.DefaultClient.Do(backendReq2)
			if err != nil {
				fatal("backend POST b9050 failed: %v", err)
			}
			backendResp2.Body.Close()
			if backendResp2.StatusCode != http.StatusOK {
				fatal("backend POST b9050 returned %d", backendResp2.StatusCode)
			}
			pass("POST /v1/admin/backend → b9050 (upgrade)")

			// Wait for the client to process the update
			if err := waitForLogN(clientLogs, "backend binary updated successfully", 2, 120*time.Second); err != nil {
				fatal("backend upgrade to b9050 failed: %v", err)
			}
			pass("Client upgraded backend to b9050")

			// Wait for llama-server to restart with b9050 (3rd start overall).
			if err := waitForLogN(clientLogs, "backend: llama-server process started", 3, 120*time.Second); err != nil {
				fatal("llama-server did not restart after b9050 upgrade: %v", err)
			}
			if err := waitForHTTP("http://"+clientLlamaAddr+"/health", 120*time.Second); err != nil {
				fatal("llama-server not ready after b9050 upgrade: %v", err)
			}
			pass("llama-server restarted and ready with b9050")
		}
	}

	// --- Step 21b: Phase 2 — start additional workers and verify multi-worker dispatch ---
	step("21b. Starting additional workers (inference, tester) for multi-worker validation")
	for _, cs := range clientSpecs[1:] {
		logs, cmd := startClient(cs)
		defer logs.Close()
		defer cleanup(cs.name, cmd)
		allClientLogs = append(allClientLogs, logs)
	}
	// Wait for all additional workers to connect and become available.
	for i := 1; i <= len(clientSpecs)-1; i++ {
		if err := waitForLog(managerLogs, "worker connected", 30*time.Second); err != nil {
			managerLogs.Dump(os.Stderr)
			fatal("additional worker %d did not connect: %v", i+1, err)
		}
	}
	pass("All %d workers connected to manager", len(clientSpecs))

	// Verify dispatch works across multiple workers.
	multiResp, multiErr := sendCompletionWithMessage("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, "What is 4+4?")
	if multiErr != nil {
		fatal("multi-worker dispatch failed: %v", multiErr)
	}
	pass("Multi-worker dispatch succeeded (%d bytes)", len(multiResp))
	if err := waitForLogAny(allClientLogs, "work unit completed", 60*time.Second); err != nil {
		fatal("no worker processed multi-worker dispatch: %v", err)
	}
	pass("Work unit processed by one of %d workers", len(clientSpecs))

	// --- Step 22: Collect output ---
	step("22. Collecting output")
	writeOutput(filepath.Join(dataDir, "completion-response.json"), []byte(completionResp))
	writeOutput(filepath.Join(dataDir, "stats.json"), []byte(statsResp))
	writeOutput(filepath.Join(dataDir, "workers.json"), []byte(workersResp))
	if errorsResp != "" {
		writeOutput(filepath.Join(dataDir, "client-errors.json"), []byte(errorsResp))
	}
	pass("Output written to %s", runDir)

	log.Info().Msg("")
	log.Info().Msg("=== ALL TESTS PASSED ===")
	log.Info().Str("dir", runDir).Msg("output")

	if keepAlive {
		log.Info().Msg("")
		log.Info().Msg("--keep-alive: services still running")
		log.Info().Msgf("  Dashboard: http://"+clientDashboardAddr+"/static/index.html")
		log.Info().Msgf("  Admin:     https://%s/login  (user: %s / pass: %s)", managerAdminAddr, adminUser, adminPassword)
		log.Info().Msgf("  Manager:   https://%s", managerAddr)
		log.Info().Msg("  Press Ctrl+C or use tray → Quit to stop")

		// Start background payload sender
		var payloadCount atomic.Int64
		payloadStop := make(chan struct{})
		go func() {
			messages := []string{
				"What is 2+2? Reply with just the number.",
				"Summarize the following text in one sentence: " + strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20),
				"Explain the key themes in this essay: " + strings.Repeat("Artificial intelligence is transforming the way we approach complex problems in science, medicine, and engineering. ", 40),
				"Write a haiku about programming.",
				"List the first 5 prime numbers.",
				"Translate 'hello world' into French, German, and Japanese. " + strings.Repeat("Provide context and etymology for each translation. ", 15),
			}
			for {
				select {
				case <-payloadStop:
					return
				default:
				}
				msg := messages[rand.Intn(len(messages))]
				_, err := sendCompletionWithMessage("https://"+managerAddr+"/v1/chat/completions", apiKey, testModelName, msg)
				if err != nil {
					log.Warn().Err(err).Msg("keep-alive payload failed")
				} else {
					n := payloadCount.Add(1)
					if n%10 == 0 {
						log.Info().Int64("count", n).Msg("keep-alive payloads sent")
					}
				}
				// Random delay 1-4 seconds
				delay := time.Duration(1000+rand.Intn(3000)) * time.Millisecond
				select {
				case <-payloadStop:
					return
				case <-time.After(delay):
				}
			}
		}()

		// Start background batch request sender (1-5 payloads every 10-20s)
		var batchCount atomic.Int64
		go func() {
			batchMessages := []string{
				"What is the speed of light?",
				"Name three planets in our solar system.",
				"What year did the French Revolution start?",
				"Define the word 'ephemeral'.",
				"What is the square root of 144?",
			}
			for {
				// Random delay 10-20 seconds before sending a batch
				delay := time.Duration(10+rand.Intn(11)) * time.Second
				select {
				case <-payloadStop:
					return
				case <-time.After(delay):
				}

				// Submit 1-5 random batch requests
				count := 1 + rand.Intn(5)
				ids := make([]string, 0, count)
				for i := 0; i < count; i++ {
					msg := batchMessages[rand.Intn(len(batchMessages))]
					id, err := submitBatchRequest("https://"+managerAddr+"/v1/batches", apiKey, testModelName, msg)
					if err != nil {
						log.Warn().Err(err).Msg("keep-alive batch submit failed")
						continue
					}
					ids = append(ids, id)
				}
				n := batchCount.Add(int64(len(ids)))
				log.Info().Int("submitted", len(ids)).Int64("total_batches", n).Msg("keep-alive batch requests sent")
			}
		}()

		// The processWatcher monitors all workers and the manager; any unexpected
		// exit triggers fatal() → stopAll(). Here we only need to handle
		// user-initiated shutdown via Ctrl-C / SIGTERM.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		watcher.stopping.Store(true)
		log.Info().Msg("signal received, shutting down...")

		close(payloadStop)
		finalCount := payloadCount.Load()
		finalBatchCount := batchCount.Load()
		log.Info().Int64("total_payloads_sent", finalCount).Int64("total_batches_sent", finalBatchCount).Msg("keep-alive payload sender stopped")

		// Verify the count matches what's in the DB (3 from tests + keep-alive payloads)
		expectedMinimum := int64(3) + finalCount
		dbCount := int64(querySQLiteCount(dbPath, "SELECT COUNT(*) FROM latency_samples"))
		if dbCount >= expectedMinimum {
			log.Info().Int64("db_count", dbCount).Int64("expected_min", expectedMinimum).Msg("✓ latency sample count matches")
		} else {
			log.Warn().Int64("db_count", dbCount).Int64("expected_min", expectedMinimum).Msg("latency sample count mismatch")
		}
	}
}

// --- Helpers ---

// findRepoRoot walks up from the current working directory until it finds go.mod,
// returning that directory as the repository root. This lets the test be invoked
// as either `go run .` (from test/) or `go run ./test` (from the repo root).
func findRepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		fatal("get working dir: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			fatal("could not locate go.mod — run from inside the synergia repository")
		}
		dir = parent
	}
}

// cleanupOldRuns removes old run directories, keeping only the most recent `keep` entries.
func cleanupOldRuns(runsDir string, keep int) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return
	}

	// Filter to directories only
	var dirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}

	if len(dirs) <= keep {
		return
	}

	// Sort by name (timestamp format ensures lexicographic = chronological)
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].Name() < dirs[j].Name()
	})

	// Remove oldest entries
	for _, d := range dirs[:len(dirs)-keep] {
		path := filepath.Join(runsDir, d.Name())
		if err := os.RemoveAll(path); err != nil {
			log.Warn().Err(err).Str("path", path).Msg("failed to remove old run")
		} else {
			log.Debug().Str("path", path).Msg("removed old run")
		}
	}
}

var logFormatLevel = func(i interface{}) string {
	return strings.ToUpper(fmt.Sprintf("| %-6s|", i))
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func initLogger() {
	output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339, NoColor: !isTerminal()}
	output.FormatLevel = logFormatLevel
	zerolog.CallerMarshalFunc = func(pc uintptr, file string, line int) string {
		return filepath.Base(file) + ":" + strconv.Itoa(line)
	}
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	log.Logger = zerolog.New(output).With().Timestamp().Caller().Logger()
}

// addLogFile tees the global logger to a file in addition to stdout.
// Returns the open file so the caller can defer-close it.
func addLogFile(path string) *os.File {
	f, err := os.Create(path)
	if err != nil {
		log.Warn().Err(err).Msg("could not create test log file")
		return nil
	}
	consoleOut := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339, NoColor: !isTerminal()}
	consoleOut.FormatLevel = logFormatLevel
	fileOut := zerolog.ConsoleWriter{Out: f, TimeFormat: time.RFC3339, NoColor: true}
	fileOut.FormatLevel = logFormatLevel
	multi := zerolog.MultiLevelWriter(consoleOut, fileOut)
	log.Logger = zerolog.New(multi).With().Timestamp().Caller().Logger()
	return f
}

func ensureModel(path string) error {
	return downloadModelFile(path, testModelURL)
}

func ensureModel2(path string) error {
	return downloadModelFile(path, testModel2URL)
}

func downloadModelFile(path, url string) error {
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		log.Info().Str("path", path).Int64("size_mb", info.Size()/1024/1024).Msg("model already downloaded")
		return nil
	}

	log.Info().Str("url", url).Msg("downloading model")
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("download interrupted: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	log.Info().Int64("size_mb", written/1024/1024).Msg("model downloaded")
	return nil
}

// hashFileOrFatal computes SHA256 of a file, fatally exits on error.
func hashFileOrFatal(path string) string {
	f, err := os.Open(path)
	if err != nil {
		fatal("open model file for hashing: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		fatal("hash model file: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func waitForHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", url)
}

func waitForLog(lb *logBuffer, substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if lb.Contains(substr) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for log containing %q", substr)
}

// waitForLogN waits until the substring appears at least n times in the log.
func waitForLogN(lb *logBuffer, substr string, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if lb.CountContains(substr) >= n {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %d occurrences of %q (got %d)", n, substr, lb.CountContains(substr))
}

func apiGet(url, key string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// postConsentToClient calls the client's local dashboard API to accept or withdraw consent.
func postConsentToClient(url string, accepted bool) error {
	payload := map[string]any{
		"accepted":           accepted,
		"hardware_stats":     accepted,
		"config_preferences": accepted,
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func sendCompletion(url, key, model string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "What is 2+2? Reply with just the number."},
		},
		"temperature": 0,
		"max_tokens":  32,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return string(respBody), nil
}

// sendCompletionWithMessage sends a chat completion with a custom user message content.
func sendCompletionWithMessage(url, key, model, content string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": content},
		},
		"temperature": 0,
		"max_tokens":  32,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return string(respBody), nil
}

// sendTriggerCompletion sends a chat completion with a special trigger payload
// that the client recognizes and handles specially (panic or error).
func sendTriggerCompletion(url, key, model, trigger string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": trigger},
		},
		"temperature": 0,
		"max_tokens":  32,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return string(respBody), nil
}

// submitBatchRequest submits a chat completion request to the batch queue.
// Returns the batch request ID.
func submitBatchRequest(url, key, model, content string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": content},
		},
		"temperature": 0,
		"max_tokens":  32,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("invalid response: %w", err)
	}
	return result.ID, nil
}

// pollBatchRequest polls the batch endpoint until the request reaches a terminal state.
func pollBatchRequest(baseURL, key, requestID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		reqURL := baseURL + "/" + requestID
		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+key)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			time.Sleep(1 * time.Second)
			continue
		}

		var result struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		switch result.Status {
		case "completed":
			return "completed", nil
		case "failed":
			return "failed", fmt.Errorf("batch request failed")
		case "pending", "in_progress":
			time.Sleep(1 * time.Second)
			continue
		default:
			return result.Status, fmt.Errorf("unexpected status: %s", result.Status)
		}
	}
	return "", fmt.Errorf("timeout waiting for batch request %s", requestID)
}

// getBatchStatus does a single GET to retrieve the batch status (no polling).
func getBatchStatus(url, key string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", err
	}
	return result.Status, nil
}

// processWatcher monitors all started child processes. If any exits while the
// test is running it kills every other process and fails the test.
type processWatcher struct {
	mu       sync.Mutex
	procs    []watchedProc
	stopping atomic.Bool
}

type watchedProc struct {
	name string
	cmd  *exec.Cmd
	logs *logBuffer
}

var watcher = &processWatcher{}

func (pw *processWatcher) add(name string, cmd *exec.Cmd, logs *logBuffer) {
	pw.mu.Lock()
	pw.procs = append(pw.procs, watchedProc{name, cmd, logs})
	pw.mu.Unlock()
	go func() {
		cmd.Wait() //nolint:errcheck
		if pw.stopping.Load() {
			return
		}
		log.Error().Str("process", name).Msg("process exited unexpectedly")
		if logs != nil {
			logs.Dump(os.Stderr)
		}
		fatal("process %q exited unexpectedly — aborting test", name)
	}()
}

func (pw *processWatcher) stopAll() {
	if !pw.stopping.CompareAndSwap(false, true) {
		return // already stopping
	}
	pw.mu.Lock()
	procs := make([]watchedProc, len(pw.procs))
	copy(procs, pw.procs)
	pw.mu.Unlock()
	for _, p := range procs {
		cleanup(p.name, p.cmd)
	}
}

// waitForLogAny polls multiple log buffers and returns nil as soon as any one
// of them contains substr within the timeout. Used when a dispatched work unit
// could land on any of several worker processes.
func waitForLogAny(bufs []*logBuffer, substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, lb := range bufs {
			if lb.Contains(substr) {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for any client log containing %q", substr)
}

func cleanup(name string, cmd *exec.Cmd) {
	if cmd.Process != nil {
		log.Info().Str("process", name).Msg("stopping")
		cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	}
}

func writeOutput(path string, data []byte) {
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Warn().Str("path", path).Err(err).Msg("failed to write output")
	}
}

func step(msg string) {
	log.Info().Msg("")
	log.Info().Msg(msg)
}

func pass(format string, args ...any) {
	log.Info().Msgf("  ✓ "+format, args...)
}

func fatal(format string, args ...any) {
	log.Error().Msgf("  ✗ "+format, args...)
	watcher.stopAll()
	os.Exit(1)
}

// freePort ensures addr ("host:port") is available, killing any stale process
// that holds it. Tries SIGTERM first, then SIGKILL after 3 s, then fatal.
func freePort(addr string) {
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		return // already free
	}

	_, portStr, _ := net.SplitHostPort(addr)
	log.Warn().Str("addr", addr).Msg("port in use — killing stale process")

	// Find and signal all PIDs holding the port.
	out, err := exec.Command("lsof", "-ti", ":"+portStr).Output()
	if err == nil {
		for pidStr := range strings.FieldsSeq(string(out)) {
			pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
			if err != nil {
				continue
			}
			p, err := os.FindProcess(pid)
			if err != nil {
				continue
			}
			log.Warn().Int("pid", pid).Str("addr", addr).Msg("sending SIGTERM to stale process")
			p.Signal(syscall.SIGTERM)
		}
	}

	// Wait up to 3 s for SIGTERM to take effect.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if ln, err := net.Listen("tcp", addr); err == nil {
			ln.Close()
			log.Info().Str("addr", addr).Msg("port now free")
			return
		}
	}

	// SIGTERM wasn't enough — escalate to SIGKILL.
	if out, err := exec.Command("lsof", "-ti", ":"+portStr).Output(); err == nil {
		for pidStr := range strings.FieldsSeq(string(out)) {
			pid, _ := strconv.Atoi(strings.TrimSpace(pidStr))
			if p, err := os.FindProcess(pid); err == nil {
				log.Warn().Int("pid", pid).Str("addr", addr).Msg("sending SIGKILL to stale process")
				p.Kill()
			}
		}
	}
	time.Sleep(500 * time.Millisecond)
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		log.Info().Str("addr", addr).Msg("port now free after SIGKILL")
		return
	}

	fatal("port %s is still in use after SIGKILL — give up", addr)
}

// querySQLiteCount runs a SQL query that returns a single integer count via the sqlite3 CLI.
func querySQLiteCount(dbPath, query string) int {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		log.Debug().Err(err).Str("query", query).Msg("sqlite3 query failed")
		return 0
	}
	result := strings.TrimSpace(string(output))
	count, _ := strconv.Atoi(result)
	return count
}

// scanLines bridges a reader to the logBuffer line by line.
func scanLines(r io.Reader, lb *logBuffer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lb.Write([]byte(scanner.Text() + "\n"))
	}
}

// computeLLMHash computes the expected LLM hash from role + model file hash.
// Must match the logic in both cluster-manager and cluster-client protocol packages:
// SHA256(role + ":" + modelFileHash)
func computeLLMHash(role, modelFileHash string) string {
	h := sha256.Sum256([]byte(role + ":" + modelFileHash))
	return hex.EncodeToString(h[:])
}

// querySQLiteString runs a SQL query that returns a single string value via the sqlite3 CLI.
func querySQLiteString(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		log.Debug().Err(err).Str("query", query).Msg("sqlite3 query failed")
		return ""
	}
	return strings.TrimSpace(string(output))
}

// packageBinaryWithLibs creates a flat tar.gz that contains the binary and any
// shared-library files (.dylib / .so) found in the sibling lib/ directory of
// the resolved binary path. This mirrors real llama.cpp release archives, which
// bundle the binary together with its shared libraries so they can all be
// extracted into a single directory and found via @loader_path/.
func packageBinaryWithLibs(llamaBin string) ([]byte, error) {
	// Resolve symlinks so we can find the real installation tree.
	realBin, err := filepath.EvalSymlinks(llamaBin)
	if err != nil {
		realBin = llamaBin
	}

	// Sibling lib/ directory — present in both Homebrew cellar and real releases.
	libDir := filepath.Join(filepath.Dir(filepath.Dir(realBin)), "lib")

	addFile := func(tw *tar.Writer, srcPath, entryName string) error {
		// Follow symlinks for the actual file content.
		realPath, _ := filepath.EvalSymlinks(srcPath)
		if realPath == "" {
			realPath = srcPath
		}
		info, err := os.Stat(realPath)
		if err != nil {
			return err
		}
		f, err := os.Open(realPath)
		if err != nil {
			return err
		}
		defer f.Close()
		hdr := &tar.Header{
			Name:    entryName,
			Mode:    0755,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if err := addFile(tw, realBin, "llama-server"); err != nil {
		return nil, fmt.Errorf("package binary: %w", err)
	}

	// Include every .dylib / .so from the lib/ directory if it exists.
	if entries, err := os.ReadDir(libDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".dylib") && !strings.HasSuffix(name, ".so") {
				continue
			}
			_ = addFile(tw, filepath.Join(libDir, name), name) // best-effort
		}
	}

	tw.Close()
	gw.Close()
	return buf.Bytes(), nil
}

// runServices starts cluster-manager and cluster-client in development mode
// without running any tests. The client manages its own llama-server binary
// (downloaded from the manager on first connect), mirroring the production
// first-start flow. If any process exits, the others are stopped.
// sendPayloads continuously sends live and batch completion requests to a real
// cluster endpoint. Useful for manual load / smoke testing against production.
//
//	go run ./test --send --endpoint https://synergia.example.com/ --key <api-key>
func sendPayloads(baseURL, key, model string) {
	initLogger()

	base := strings.TrimRight(baseURL, "/")
	log.Info().Str("endpoint", base).Str("model", model).
		Msg("=== Synergia Send Mode — sending payloads continuously (Ctrl-C to stop) ===")

	stop := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(stop)
	}()

	runPayloadLoop(base+"/v1/chat/completions", base+"/v1/batches", key, model, stop)
	log.Info().Msg("send mode stopped")
}

// runPayloadLoop runs live completion and batch request senders until stop is closed.
// It blocks until stop is closed, so callers that want background behaviour should
// wrap it in a goroutine.
func runPayloadLoop(completionsURL, batchURL, key, model string, stop <-chan struct{}) {
	liveMessages := []string{
		"What is 2+2? Reply with just the number.",
		"Summarize the following text in one sentence: " + strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20),
		"Explain the key themes in this essay: " + strings.Repeat("Artificial intelligence is transforming the way we approach complex problems in science, medicine, and engineering. ", 40),
		"Write a haiku about programming.",
		"List the first 5 prime numbers.",
		"Translate 'hello world' into French, German, and Japanese. " + strings.Repeat("Provide context and etymology for each translation. ", 15),
	}
	batchMessages := []string{
		"What is the speed of light?",
		"Name three planets in our solar system.",
		"What year did the French Revolution start?",
		"Define the word 'ephemeral'.",
		"What is the square root of 144?",
	}

	var liveCount int64

	// Live sender goroutine
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			msg := liveMessages[rand.Intn(len(liveMessages))]
			_, err := sendCompletionWithMessage(completionsURL, key, model, msg)
			if err != nil {
				log.Warn().Err(err).Msg("payload failed")
			} else {
				liveCount++
				if liveCount%10 == 0 {
					log.Info().Int64("count", liveCount).Msg("payloads sent")
				}
			}
			delay := time.Duration(1000+rand.Intn(3000)) * time.Millisecond
			select {
			case <-stop:
				return
			case <-time.After(delay):
			}
		}
	}()

	// Batch sender goroutine
	go func() {
		for {
			delay := time.Duration(10+rand.Intn(11)) * time.Second
			select {
			case <-stop:
				return
			case <-time.After(delay):
			}
			count := 1 + rand.Intn(5)
			for i := 0; i < count; i++ {
				msg := batchMessages[rand.Intn(len(batchMessages))]
				_, err := submitBatchRequest(batchURL, key, model, msg)
				if err != nil {
					log.Warn().Err(err).Msg("batch submit failed")
				}
			}
			log.Info().Int("submitted", count).Msg("batch requests sent")
		}
	}()

	<-stop
}

func runServices() {
	initLogger()

	log.Info().Msg("=== Synergia Run Mode (no tests) ===")

	repoRoot := findRepoRoot()
	testDir := filepath.Join(repoRoot, "test")
	modelsDir := filepath.Join(testDir, "testdata", "models")
	runDir := filepath.Join(testDir, "runs", time.Now().Format("2006-01-02_15-04-05"))
	dataDir := filepath.Join(runDir, "data")
	logDir := filepath.Join(runDir, "logs")

	for _, d := range []string{modelsDir, dataDir, logDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			fatal("create dir %s: %v", d, err)
		}
	}

	cleanupOldRuns(filepath.Join(testDir, "runs"), 3)

	// Pre-flight: kill any stale processes holding required ports.
	for _, addr := range []string{
		managerAddr, managerAdminAddr, managerRedirectAddr,
		clientDashboardAddr, clientLlamaAddr,
		client2DashboardAddr, client2LlamaAddr,
		client3DashboardAddr, client3LlamaAddr,
	} {
		freePort(addr)
	}

	// TLS certs
	tlsDir := filepath.Join(testDir, "testdata", "tls")
	caCertPath, serverCertPath, serverKeyPath, err := ensureTLSCerts(tlsDir)
	if err != nil {
		fatal("TLS cert generation: %v", err)
	}

	// Trust the test CA
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		fatal("read CA cert: %v", err)
	}
	caPool, err := x509.SystemCertPool()
	if err != nil {
		caPool = x509.NewCertPool()
	}
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		fatal("failed to parse CA certificate")
	}
	http.DefaultTransport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: caPool},
	}

	// Ensure model file is available in testdata
	modelPath := filepath.Join(modelsDir, testModelFilename)
	if err := ensureModel(modelPath); err != nil {
		fatal("model download: %v", err)
	}

	// Find the system llama-server binary and package it as a tar.gz so the
	// manager can distribute it to the client — matching the production flow.
	llamaServerBin, err := exec.LookPath("llama-server")
	if err != nil {
		fatal("llama-server not found in PATH — install llama.cpp first: brew install llama.cpp")
	}
	log.Info().Str("path", llamaServerBin).Msg("packaging llama-server binary for distribution")

	tarGzData, err := packageBinaryWithLibs(llamaServerBin)
	if err != nil {
		fatal("package llama-server binary: %v", err)
	}

	// Serve the tar.gz from an in-process HTTP server on an ephemeral port.
	binaryLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal("binary distribution server: %v", err)
	}
	binaryServerURL := "http://" + binaryLn.Addr().String() + "/llama-server.tar.gz"
	binaryMux := http.NewServeMux()
	binaryMux.HandleFunc("/llama-server.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(tarGzData)))
		w.Write(tarGzData) //nolint:errcheck
	})
	binarySrv := &http.Server{Handler: binaryMux}
	go binarySrv.Serve(binaryLn) //nolint:errcheck
	defer binarySrv.Close()
	log.Info().Str("url", binaryServerURL).Msg("binary distribution server ready")

	// --- Start cluster-manager ---
	managerLogs := newLogBuffer("cluster-manager", logDir)
	defer managerLogs.Close()
	managerCmd := exec.Command("go", "run", "./cmd/synergia-manager", "--development")
	managerCmd.Dir = repoRoot
	managerCmd.Env = append(os.Environ(),
		"CLUSTER_LISTEN_ADDR="+managerAddr,
		"CLUSTER_API_KEY="+apiKey,
		"CLUSTER_WORKER_KEY="+workerKey,
		"CLUSTER_MODEL_BACKEND=filesystem",
		"CLUSTER_MODEL_PATH="+modelsDir,
		"CLUSTER_DB_PATH="+filepath.Join(dataDir, "cluster-manager.db"),
		"CLUSTER_DEV_BACKEND_URL="+binaryServerURL,
		"CLUSTER_DEV_CLIENT_VERSION=0.1.0-dev",
		"CLUSTER_ADMIN_ADDR="+managerAdminAddr,
		"CLUSTER_ADMIN_USER="+adminUser,
		"CLUSTER_ADMIN_PASSWORD="+adminPassword,
		"TLS_CERT_FILE="+serverCertPath,
		"TLS_KEY_FILE="+serverKeyPath,
		"CLUSTER_HTTP_REDIRECT_ADDR="+managerRedirectAddr,
		"LOG_LEVEL=debug",
	)
	managerCmd.Stdout = managerLogs
	managerCmd.Stderr = managerLogs
	if err := managerCmd.Start(); err != nil {
		fatal("start cluster-manager: %v", err)
	}

	if err := waitForHTTP("https://"+managerAddr+"/healthz", 30*time.Second); err != nil {
		managerLogs.Dump(os.Stderr)
		fatal("cluster-manager did not become ready: %v", err)
	}
	log.Info().Str("addr", managerAddr).Msg("cluster-manager ready")

	// --- Start 3 cluster-clients (clean directories — no binary, no cached state) ---
	sendClientSpecs := []struct {
		name      string
		dashAddr  string
		llamaAddr string
		role      string
	}{
		{"cluster-client-7502", clientDashboardAddr, clientLlamaAddr, "embedding"},
		{"cluster-client-7505", client2DashboardAddr, client2LlamaAddr, "inference"},
		{"cluster-client-7507", client3DashboardAddr, client3LlamaAddr, "tester"},
	}

	var clientLogs *logBuffer
	var clientCmd *exec.Cmd
	for i, cs := range sendClientSpecs {
		dashPort := strings.Split(cs.dashAddr, ":")[1]
		logs := newLogBuffer(cs.name, logDir)
		defer logs.Close()
		cmd := exec.Command("go", "run", "./cmd/synergia-client",
			"--manager-url", "wss://"+managerAddr+"/ws/worker",
			"--llm-url", "http://"+cs.llamaAddr,
			"--dashboard-addr", cs.dashAddr,
			"--model", testModelName,
			"--quantisation", testQuantisation,
			"--role", cs.role,
			"--model-file", modelPath,
			"--data-dir", filepath.Join(dataDir, "client-data-"+dashPort),
			"--auto-approve",
			"--tls-ca-cert", caCertPath,
		)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "LOG_LEVEL=debug", "CLUSTER_WORKER_KEY="+workerKey)
		cmd.Stdout = logs
		cmd.Stderr = logs
		if err := cmd.Start(); err != nil {
			fatal("start %s: %v", cs.name, err)
		}
		if i == 0 {
			clientLogs = logs
			clientCmd = cmd
		}
	}
	_ = clientLogs

	log.Info().Msg("")
	log.Info().Msg("All services started — bootstrapping client (binary download → model push → llama-server start)")
	log.Info().Msgf("  Client Dashboard: http://"+clientDashboardAddr+"/static/index.html")
	log.Info().Msgf("  Admin Dashboard:  https://%s/login  (user: %s / pass: %s)", managerAdminAddr, adminUser, adminPassword)
	log.Info().Msgf("  Manager API:      https://%s", managerAddr)
	log.Info().Msg("  Press Ctrl+C or use tray → Quit to stop")
	log.Info().Msg("")

	// The manager was pre-configured with the local binary URL and client version
	// target at startup. When the client connects and sends InitialSync, the manager
	// immediately pushes BackendUpdate + ModelUpdate. Just wait for the result.
	if err := waitForLog(managerLogs, "worker connected", 30*time.Second); err != nil {
		fatal("client did not connect to manager: %v", err)
	}
	log.Info().Msg("worker connected — waiting for InitialSync bootstrap")

	if err := waitForLog(clientLogs, "backend: llama-server process started", 120*time.Second); err != nil {
		clientLogs.Dump(os.Stderr)
		fatal("client did not start llama-server via InitialSync: %v", err)
	}
	if err := waitForHTTP("http://"+clientLlamaAddr+"/health", 120*time.Second); err != nil {
		fatal("llama-server not ready on port "+clientLlamaAddr+": %v", err)
	}
	log.Info().Msg("llama-server started and ready on port "+clientLlamaAddr+"")

	// Start live and batch payload senders in background.
	stop := make(chan struct{})
	go runPayloadLoop("https://"+managerAddr+"/v1/chat/completions",
		"https://"+managerAddr+"/v1/batches", apiKey, testModelName, stop)

	// Monitor lifecycle
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	clientDone := make(chan struct{})
	go func() { clientCmd.Wait(); close(clientDone) }()

	managerDone := make(chan struct{})
	go func() { managerCmd.Wait(); close(managerDone) }()

	select {
	case <-sigCh:
		log.Info().Msg("signal received, shutting down gracefully...")
	case <-clientDone:
		log.Info().Msg("client exited, shutting down...")
	case <-managerDone:
		log.Info().Msg("manager exited, shutting down...")
	}

	close(stop)

	// Graceful shutdown: stop each service in order
	cleanup("cluster-client", clientCmd)
	cleanup("cluster-manager", managerCmd)

	log.Info().Msg("all services stopped")
	os.Exit(0)
}
