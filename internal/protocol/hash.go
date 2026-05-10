package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// ComputeLLMHash computes the deterministic integrity hash for a worker's loaded model:
//
//	SHA256(role + ":" + modelFileHash)
//
// modelFileHash must be the hex-encoded SHA256 of the model file bytes.
// This hash is reported to the manager and used to verify model integrity.
func ComputeLLMHash(role, modelFileHash string) string {
	h := sha256.Sum256([]byte(role + ":" + modelFileHash))
	return hex.EncodeToString(h[:])
}

// HashFile computes the hex-encoded SHA256 hash of a file on disk.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
