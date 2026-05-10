package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
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

// TestPatchSentinelAdHocSignatureValid verifies that after patching, the Mach-O
// ad-hoc code signature is still valid — i.e. all page hashes match the actual
// binary content. Without recomputeAdHocSignature, macOS kills the patched
// binary on darwin/arm64 because Go's embedded signature hashes every page from
// offset 0 to __LINKEDIT.
func TestPatchSentinelAdHocSignatureValid(t *testing.T) {
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

	patched, err := patchSentinel(data, "wss://example.com/ws/worker")
	if err != nil {
		t.Fatalf("patchSentinel failed: %v", err)
	}

	mismatches := countSignatureMismatches(t, patched)
	if mismatches > 0 {
		t.Errorf("ad-hoc signature has %d page hash mismatch(es) after patching — macOS would kill this binary", mismatches)
	}
}

// countSignatureMismatches parses the Mach-O ad-hoc code signature and returns
// the number of code pages whose stored hash does not match the actual content.
// Returns 0 if the binary has no Mach-O signature (non-darwin or not a Mach-O).
func countSignatureMismatches(t *testing.T, data []byte) int {
	t.Helper()
	if len(data) < 32 {
		return 0
	}
	if binary.LittleEndian.Uint32(data[0:]) != 0xfeedfacf {
		return 0 // not a 64-bit little-endian Mach-O
	}

	ncmds := binary.LittleEndian.Uint32(data[16:])
	off := uint32(32)
	var csOff, csSize uint32
	for i := uint32(0); i < ncmds; i++ {
		cmd := binary.LittleEndian.Uint32(data[off:])
		cmdsize := binary.LittleEndian.Uint32(data[off+4:])
		if cmd == 0x1d {
			csOff = binary.LittleEndian.Uint32(data[off+8:])
			csSize = binary.LittleEndian.Uint32(data[off+12:])
		}
		off += cmdsize
	}
	if csOff == 0 || int(csOff+csSize) > len(data) {
		return 0 // no code signature
	}

	cs := data[csOff : csOff+csSize]
	if binary.BigEndian.Uint32(cs[0:]) != 0xfade0cc0 {
		return 0
	}
	count := binary.BigEndian.Uint32(cs[8:])

	var cdOff uint32
	for i := uint32(0); i < count; i++ {
		if binary.BigEndian.Uint32(cs[12+i*8:]) == 0 {
			cdOff = binary.BigEndian.Uint32(cs[16+i*8:])
			break
		}
	}
	if cdOff == 0 {
		return 0
	}

	cd := cs[cdOff:]
	hashOffset := binary.BigEndian.Uint32(cd[16:])
	nCode := binary.BigEndian.Uint32(cd[28:])
	codeLimit := binary.BigEndian.Uint32(cd[32:])
	hashSize := uint32(cd[36])
	pageSize := uint32(1) << cd[39]

	mismatches := 0
	for i := uint32(0); i < nCode; i++ {
		start := i * pageSize
		end := start + pageSize
		if end > codeLimit {
			end = codeLimit
		}
		if int(end) > len(data) {
			break
		}
		h := sha256.Sum256(data[start:end])
		slotBase := cdOff + hashOffset + i*hashSize
		if int(slotBase)+int(hashSize) > len(cs) {
			break
		}
		if !bytes.Equal(cs[slotBase:slotBase+hashSize], h[:hashSize]) {
			mismatches++
		}
	}
	return mismatches
}
