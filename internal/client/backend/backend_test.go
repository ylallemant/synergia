package backend

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ── BuildArgs ─────────────────────────────────────────────────────────────────

func hasFlag(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasSwitch(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestBuildArgs_BasicFlags(t *testing.T) {
	p := LlamaParams{ContextSize: 4096, ParallelSlots: 2, GPULayers: -1, EndpointType: "chat"}
	args := BuildArgs("9877", "/models/smol.gguf", p)

	for _, tc := range []struct{ flag, want string }{
		{"--port", "9877"},
		{"--model", "/models/smol.gguf"},
		{"--ctx-size", "4096"},
		{"--parallel", "2"},
		{"--n-gpu-layers", "-1"},
	} {
		if !hasFlag(args, tc.flag, tc.want) {
			t.Errorf("want %s %s in args %v", tc.flag, tc.want, args)
		}
	}
}

func TestBuildArgs_FlashAttention_AddsFlag(t *testing.T) {
	p := LlamaParams{ContextSize: 4096, ParallelSlots: 1, GPULayers: -1, FlashAttention: true}
	args := BuildArgs("9877", "/models/smol.gguf", p)
	if !hasSwitch(args, "--flash-attn") {
		t.Errorf("want --flash-attn in args %v", args)
	}
}

func TestBuildArgs_FlashAttention_AbsentByDefault(t *testing.T) {
	args := BuildArgs("9877", "/models/smol.gguf", DefaultLlamaParams())
	if hasSwitch(args, "--flash-attn") {
		t.Errorf("want no --flash-attn when FlashAttention=false; got %v", args)
	}
}

func TestBuildArgs_Embeddings_AddsFlag(t *testing.T) {
	p := LlamaParams{ContextSize: 4096, ParallelSlots: 1, GPULayers: -1, EndpointType: "embeddings"}
	args := BuildArgs("9877", "/models/smol.gguf", p)
	if !hasSwitch(args, "--embeddings") {
		t.Errorf("want --embeddings in args %v", args)
	}
}

func TestBuildArgs_Chat_NoEmbeddingsFlag(t *testing.T) {
	args := BuildArgs("9877", "/models/smol.gguf", DefaultLlamaParams())
	if hasSwitch(args, "--embeddings") {
		t.Errorf("want no --embeddings for chat endpoint; got %v", args)
	}
}

// ── DefaultLlamaParams ────────────────────────────────────────────────────────

func TestDefaultLlamaParams(t *testing.T) {
	p := DefaultLlamaParams()
	if p.ContextSize != 4096 {
		t.Errorf("want ContextSize=4096, got %d", p.ContextSize)
	}
	if p.ParallelSlots != 1 {
		t.Errorf("want ParallelSlots=1, got %d", p.ParallelSlots)
	}
	if p.GPULayers != -1 {
		t.Errorf("want GPULayers=-1, got %d", p.GPULayers)
	}
	if p.EndpointType != "chat" {
		t.Errorf("want EndpointType=chat, got %q", p.EndpointType)
	}
	if p.FlashAttention {
		t.Error("want FlashAttention=false by default")
	}
}

// ── Manager process lifecycle ─────────────────────────────────────────────────

func TestManager_IsRunning_InitiallyFalse(t *testing.T) {
	m := &Manager{dataDir: t.TempDir()}
	if m.IsRunning() {
		t.Error("want IsRunning=false for a new Manager")
	}
}

func TestManager_Stop_WhenNotRunning_IsNoop(t *testing.T) {
	m := &Manager{dataDir: t.TempDir()}
	m.Stop() // must not panic
}

func TestManager_Start_BinaryMissing_ReturnsError(t *testing.T) {
	m := &Manager{
		binaryPath: filepath.Join(t.TempDir(), "nonexistent-llama-server"),
		dataDir:    t.TempDir(),
	}
	err := m.Start("9877", "/models/smol.gguf", DefaultLlamaParams())
	if err == nil {
		t.Error("want error when binary is missing")
		m.Stop()
	}
}

func TestManager_Restart_BeforeStart_ReturnsError(t *testing.T) {
	m := &Manager{dataDir: t.TempDir()}
	if err := m.Restart(); err == nil {
		t.Error("want error from Restart before any Start")
	}
}

// fakeBinary writes a shell script that runs indefinitely and returns its path.
// Skips the test on Windows.
func fakeBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fakeBinary uses a shell script — not supported on Windows")
	}
	path := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 60\n"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestManager_Start_SetsIsRunning(t *testing.T) {
	bin := fakeBinary(t)
	m := &Manager{binaryPath: bin, dataDir: t.TempDir()}
	t.Cleanup(m.Stop)

	if err := m.Start("9877", "ignored", DefaultLlamaParams()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.IsRunning() {
		t.Error("want IsRunning=true after Start")
	}
}

func TestManager_Stop_ClearsIsRunning(t *testing.T) {
	bin := fakeBinary(t)
	m := &Manager{binaryPath: bin, dataDir: t.TempDir()}

	if err := m.Start("9877", "ignored", DefaultLlamaParams()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()
	if m.IsRunning() {
		t.Error("want IsRunning=false after Stop")
	}
}

func TestManager_Start_StopsExistingProcess(t *testing.T) {
	bin := fakeBinary(t)
	m := &Manager{binaryPath: bin, dataDir: t.TempDir()}
	t.Cleanup(m.Stop)

	if err := m.Start("9877", "first", DefaultLlamaParams()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	first := m.proc

	// Second Start must kill the first process and launch a new one.
	if err := m.Start("9877", "second", DefaultLlamaParams()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if m.proc == first {
		t.Error("want a new process handle after second Start")
	}
}

func TestManager_Restart_ReusesStoredParams(t *testing.T) {
	bin := fakeBinary(t)
	m := &Manager{binaryPath: bin, dataDir: t.TempDir()}
	t.Cleanup(m.Stop)

	p := LlamaParams{ContextSize: 2048, ParallelSlots: 4, GPULayers: 32, EndpointType: "chat"}
	if err := m.Start("9877", "model.gguf", p); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()

	if err := m.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !m.IsRunning() {
		t.Error("want IsRunning=true after Restart")
	}
	if m.lastParams.ContextSize != 2048 {
		t.Errorf("want stored ContextSize=2048, got %d", m.lastParams.ContextSize)
	}
}
