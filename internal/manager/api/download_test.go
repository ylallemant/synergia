package api

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPatchSentinelOnBuiltClientBinary(t *testing.T) {
	rootDir := filepath.Join("..", "..", "..")
	outputPath := filepath.Join(t.TempDir(), "synergia-client-test")

	cmd := exec.Command("go", "build", "-o", outputPath, "./cmd/synergia-client")
	cmd.Dir = rootDir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build client binary: %v\n%s", err, string(out))
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read built client binary: %v", err)
	}

	if !bytes.Contains(data, []byte(sentinelValue)) {
		t.Fatal("built client binary does not contain the sentinel value")
	}

	patched, err := patchSentinel(data, "wss://example.com/ws/worker")
	if err != nil {
		t.Fatalf("patchSentinel failed on built client binary: %v", err)
	}

	if bytes.Contains(patched, []byte(sentinelValue)) {
		t.Fatal("patched binary still contains sentinel value")
	}
}
