package config

import (
	"testing"
)

func TestDefaultManagerURLSentinel(t *testing.T) {
	sentinel := defaultManagerURLSentinel()
	if sentinel != "$$SYNERGIA_MANAGER_URL$$" {
		t.Fatalf("expected sentinel %q, got %q", "$$SYNERGIA_MANAGER_URL$$", sentinel)
	}
	if len(DefaultManagerURL) != defaultManagerURLSentinelSize {
		t.Fatalf("expected slot size %d, got %d", defaultManagerURLSentinelSize, len(DefaultManagerURL))
	}
}

func TestResolveManagerURLWithUnpatchedDefault(t *testing.T) {
	orig := DefaultManagerURL
	defer func() { DefaultManagerURL = orig }()

	// Unpatched state: first 24 bytes are the sentinel, rest are zeros.
	DefaultManagerURL = [256]byte{
		'$', '$', 'S', 'Y', 'N', 'E', 'R', 'G', 'I', 'A',
		'_', 'M', 'A', 'N', 'A', 'G', 'E', 'R', '_', 'U', 'R', 'L', '$', '$',
	}
	if got := resolveManagerURL(); got != "" {
		t.Fatalf("expected empty URL for unpatched sentinel, got %q", got)
	}
}

func TestResolveManagerURLWithPatchedValue(t *testing.T) {
	orig := DefaultManagerURL
	defer func() { DefaultManagerURL = orig }()

	// Patched state: URL written at the start, rest null-padded.
	patched := "wss://example.com/ws/worker"
	var arr [256]byte
	copy(arr[:], patched)
	DefaultManagerURL = arr

	if got := resolveManagerURL(); got != patched {
		t.Fatalf("expected %q, got %q", patched, got)
	}
}
