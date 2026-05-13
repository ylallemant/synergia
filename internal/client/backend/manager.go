package backend

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/client/proc"
	"github.com/ylallemant/synergia/internal/protocol"
)

// LlamaParams holds llama-server startup parameters pushed by the manager.
type LlamaParams struct {
	ContextSize    int
	ParallelSlots  int
	GPULayers      int
	EndpointType   string // "chat" (default) or "embeddings"
	FlashAttention bool
}

// DefaultLlamaParams returns conservative defaults suitable for first launch.
func DefaultLlamaParams() LlamaParams {
	return LlamaParams{
		ContextSize:   4096,
		ParallelSlots: 1,
		GPULayers:     -1, // all layers on GPU
		EndpointType:  "chat",
	}
}

// BuildArgs constructs the llama-server command-line argument slice.
func BuildArgs(port, modelPath string, p LlamaParams) []string {
	args := []string{
		"--port", port,
		"--model", modelPath,
		"--ctx-size", strconv.Itoa(p.ContextSize),
		"--parallel", strconv.Itoa(p.ParallelSlots),
		"--n-gpu-layers", strconv.Itoa(p.GPULayers),
	}
	if p.FlashAttention {
		args = append(args, "--flash-attn")
	}
	if p.EndpointType == "embeddings" {
		args = append(args, "--embeddings")
	}
	return args
}

// Manager handles downloading, installing, and running the llama-server backend binary.
type Manager struct {
	workerKey      string
	managerHTTPURL string
	dataDir        string
	currentHash    string // SHA256 of the currently installed backend binary
	binaryPath     string // path to the installed backend binary

	procMu    sync.Mutex
	proc      *exec.Cmd
	lastPort  string
	lastModel string
	lastParams LlamaParams
}

// New creates a new backend Manager.
func New(workerKey, managerHTTPURL, dataDir string) *Manager {
	binDir := filepath.Join(dataDir, "backend")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		log.Warn().Err(err).Msg("failed to create backend dir")
	}

	binaryName := "llama-server"
	if runtime.GOOS == "windows" {
		binaryName = "llama-server.exe"
	}

	m := &Manager{
		workerKey:      workerKey,
		managerHTTPURL: managerHTTPURL,
		dataDir:        dataDir,
		binaryPath:     filepath.Join(binDir, binaryName),
	}

	// Compute hash of existing binary if present
	if hash, err := hashFile(m.binaryPath); err == nil {
		m.currentHash = hash
		log.Info().Str("hash", hash[:16]+"...").Str("path", m.binaryPath).Msg("existing backend binary found")
	}

	return m
}

// Hash returns the SHA256 of the currently installed backend binary.
func (m *Manager) Hash() string {
	return m.currentHash
}

// BinaryPath returns the path to the backend binary.
func (m *Manager) BinaryPath() string {
	return m.binaryPath
}

// IsInstalled returns true if a backend binary exists.
func (m *Manager) IsInstalled() bool {
	_, err := os.Stat(m.binaryPath)
	return err == nil
}

// Apply downloads and installs the backend binary from a BackendUpdate message.
// Returns true if the binary was updated (caller should restart llama-server).
func (m *Manager) Apply(bu *protocol.BackendUpdate) (bool, error) {
	if bu.SHA256 != "" && bu.SHA256 == m.currentHash {
		log.Info().Str("version", bu.Version).Msg("backend already at target version, skipping")
		return false, nil
	}

	log.Info().
		Str("version", bu.Version).
		Str("current_hash", truncHash(m.currentHash)).
		Str("target_hash", truncHash(bu.SHA256)).
		Msg("backend update — starting download")

	// Try primary URL first
	archivePath, err := m.download(bu.DownloadURL)
	if err != nil {
		log.Warn().Err(err).Msg("primary backend download failed, trying fallback")
		fallbackURL := m.buildFallbackURL(bu)
		archivePath, err = m.download(fallbackURL)
		if err != nil {
			return false, fmt.Errorf("both primary and fallback download failed: %w", err)
		}
	}
	defer os.Remove(archivePath)

	// Extract all files from the archive (binary + shared libraries)
	binDir := filepath.Join(m.dataDir, "backend")
	binaryName := "llama-server"
	if runtime.GOOS == "windows" {
		binaryName = "llama-server.exe"
	}

	// Stop any running llama-server before overwriting the binary and its
	// companion DLLs/shared libraries. On Windows this is required — the OS
	// holds an exclusive lock on files mapped by the running process, so
	// extraction would otherwise fail with "file in use" for ggml-base.dll
	// and friends. On macOS/Linux this is defensive (overwrite works while a
	// process holds the inode, but starting a new instance with mixed-version
	// libraries is unsafe). The caller restarts llama-server after Apply
	// succeeds.
	if m.IsRunning() {
		log.Info().Msg("stopping running llama-server before backend extraction")
		m.Stop()
	}

	binaryPath, err := m.extractBinary(archivePath, bu.DownloadURL, binDir, binaryName)
	if err != nil {
		return false, fmt.Errorf("extract backend binary: %w", err)
	}

	// Verify SHA256 of the extracted binary (if provided)
	if bu.SHA256 != "" {
		hash, err := hashFile(binaryPath)
		if err != nil {
			return false, fmt.Errorf("failed to hash downloaded backend: %w", err)
		}
		if hash != bu.SHA256 {
			return false, fmt.Errorf("SHA256 mismatch: expected %s, got %s", bu.SHA256, hash)
		}
	}

	// Make executable
	if runtime.GOOS != "windows" {
		if err := os.Chmod(binaryPath, 0755); err != nil {
			return false, fmt.Errorf("chmod: %w", err)
		}
	}

	// Update current hash
	if hash, err := hashFile(binaryPath); err == nil {
		m.currentHash = hash
	}

	log.Info().
		Str("version", bu.Version).
		Str("hash", truncHash(m.currentHash)).
		Msg("backend binary updated successfully")

	return true, nil
}

// Verify checks that the installed binary is executable by running --version.
func (m *Manager) Verify() error {
	if !m.IsInstalled() {
		return fmt.Errorf("backend binary not found at %s", m.binaryPath)
	}

	cmd := exec.Command(m.binaryPath, "--version")
	proc.HideWindow(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("backend binary verification failed: %w (output: %s)", err, string(output))
	}
	return nil
}

// Start stops any running instance then launches llama-server on the given port.
func (m *Manager) Start(port, modelPath string, p LlamaParams) error {
	m.procMu.Lock()
	defer m.procMu.Unlock()

	m.stopLocked()

	if !m.isInstalledLocked() {
		return fmt.Errorf("llama-server binary not found at %s", m.binaryPath)
	}

	cmd := exec.Command(m.binaryPath, BuildArgs(port, modelPath, p)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	proc.HideWindow(cmd)
	// Ensure the binary's own directory is in the dynamic-linker search path.
	// Real llama.cpp release archives place shared libs alongside the binary
	// (flat layout); Homebrew installs use @rpath/../lib. Including the binary
	// directory first covers the flat layout; the existing DYLD_LIBRARY_PATH /
	// LD_LIBRARY_PATH from the parent process covers the rest.
	binDir := filepath.Dir(m.binaryPath)
	cmd.Env = append(os.Environ(),
		"DYLD_LIBRARY_PATH="+binDir+":"+os.Getenv("DYLD_LIBRARY_PATH"),
		"LD_LIBRARY_PATH="+binDir+":"+os.Getenv("LD_LIBRARY_PATH"),
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start llama-server: %w", err)
	}

	m.proc = cmd
	m.lastPort = port
	m.lastModel = modelPath
	m.lastParams = p

	log.Info().
		Str("binary", m.binaryPath).
		Str("port", port).
		Str("model", filepath.Base(modelPath)).
		Int("ctx_size", p.ContextSize).
		Int("parallel_slots", p.ParallelSlots).
		Int("gpu_layers", p.GPULayers).
		Str("endpoint_type", p.EndpointType).
		Bool("flash_attn", p.FlashAttention).
		Msg("backend: llama-server process started")
	return nil
}

// Stop kills the running llama-server process if any.
func (m *Manager) Stop() {
	m.procMu.Lock()
	defer m.procMu.Unlock()
	m.stopLocked()
}

// Restart stops the current instance and starts a new one with the last stored params.
func (m *Manager) Restart() error {
	m.procMu.Lock()
	port := m.lastPort
	modelPath := m.lastModel
	params := m.lastParams
	m.procMu.Unlock()

	if port == "" || modelPath == "" {
		return fmt.Errorf("Restart called before any successful Start")
	}
	return m.Start(port, modelPath, params)
}

// IsRunning reports whether a llama-server process is currently running.
func (m *Manager) IsRunning() bool {
	m.procMu.Lock()
	defer m.procMu.Unlock()
	return m.proc != nil
}

// stopLocked kills the process. Caller must hold procMu.
func (m *Manager) stopLocked() {
	if m.proc == nil {
		return
	}
	_ = m.proc.Process.Kill()
	_ = m.proc.Wait()
	m.proc = nil
	log.Info().Msg("llama-server stopped")
}

// isInstalledLocked is like IsInstalled but assumes procMu is already held.
func (m *Manager) isInstalledLocked() bool {
	_, err := os.Stat(m.binaryPath)
	return err == nil
}

func (m *Manager) download(url string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Minute}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.workerKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	binDir := filepath.Join(m.dataDir, "backend")
	tmp, err := os.CreateTemp(binDir, "backend-archive-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write temp file: %w", err)
	}
	tmp.Close()

	return tmp.Name(), nil
}

// extractBinary extracts all files from the archive into destDir and returns the path to the target binary.
// Supports .tar.gz and .zip based on the download URL extension.
func (m *Manager) extractBinary(archivePath, downloadURL, destDir, binaryName string) (string, error) {
	if strings.HasSuffix(downloadURL, ".zip") {
		return m.extractFromZip(archivePath, destDir, binaryName)
	}
	// Default: tar.gz
	return m.extractFromTarGz(archivePath, destDir, binaryName)
}

func (m *Manager) extractFromTarGz(archivePath, destDir, binaryName string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip open: %w", err)
	}
	defer gzr.Close()

	var binaryPath string
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar read: %w", err)
		}

		base := filepath.Base(hdr.Name)
		// Security: prevent path traversal
		if strings.Contains(base, "..") {
			continue
		}

		outPath := filepath.Join(destDir, base)

		switch hdr.Typeflag {
		case tar.TypeReg:
			out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				return "", fmt.Errorf("create output %s: %w", base, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				os.Remove(outPath)
				return "", fmt.Errorf("extract %s: %w", base, err)
			}
			out.Close()
		case tar.TypeSymlink:
			// Recreate symlink; target must be a simple filename (no path traversal)
			linkTarget := filepath.Base(hdr.Linkname)
			os.Remove(outPath)
			if err := os.Symlink(linkTarget, outPath); err != nil {
				return "", fmt.Errorf("symlink %s -> %s: %w", base, linkTarget, err)
			}
		default:
			continue
		}

		if base == binaryName {
			binaryPath = outPath
			log.Info().Str("entry", hdr.Name).Str("dest", outPath).Msg("extracted backend binary from tar.gz")
		}
	}

	if binaryPath == "" {
		return "", fmt.Errorf("%s not found in archive", binaryName)
	}
	return binaryPath, nil
}

func (m *Manager) extractFromZip(archivePath, destDir, binaryName string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("zip open: %w", err)
	}
	defer r.Close()

	var binaryPath string
	for _, zf := range r.File {
		if zf.FileInfo().IsDir() {
			continue
		}

		base := filepath.Base(zf.Name)
		// Security: prevent path traversal
		if strings.Contains(base, "..") {
			continue
		}

		rc, err := zf.Open()
		if err != nil {
			return "", fmt.Errorf("zip entry open %s: %w", base, err)
		}
		outPath := filepath.Join(destDir, base)
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			rc.Close()
			return "", fmt.Errorf("create output %s: %w", base, err)
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			os.Remove(outPath)
			return "", fmt.Errorf("extract %s: %w", base, err)
		}
		out.Close()
		rc.Close()

		if base == binaryName {
			binaryPath = outPath
			log.Info().Str("entry", zf.Name).Str("dest", outPath).Msg("extracted backend binary from zip")
		}
	}

	if binaryPath == "" {
		return "", fmt.Errorf("%s not found in archive", binaryName)
	}
	return binaryPath, nil
}

func (m *Manager) buildFallbackURL(bu *protocol.BackendUpdate) string {
	return fmt.Sprintf("%s/v1/backend/download?version=%s&os=%s&arch=%s",
		m.managerHTTPURL, bu.Version, runtime.GOOS, runtime.GOARCH)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func truncHash(h string) string {
	if len(h) > 16 {
		return h[:16] + "..."
	}
	return h
}
