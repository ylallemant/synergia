package config

import (
	"strings"
	"testing"
)

func TestDefaultManagerURLSentinel(t *testing.T) {
	sentinel := defaultManagerURLSentinel()
	if sentinel != "$$SYNERGIA_MANAGER_URL$$" {
		t.Fatalf("expected sentinel %q, got %q", "$$SYNERGIA_MANAGER_URL$$", sentinel)
	}

	if len(DefaultManagerURL) != defaultManagerURLSentinelSize {
		t.Fatalf("expected default URL length %d, got %d", defaultManagerURLSentinelSize, len(DefaultManagerURL))
	}
	if strings.TrimRight(DefaultManagerURL, "\x00") != sentinel {
		t.Fatalf("expected trimmed default URL to equal sentinel, got %q", strings.TrimRight(DefaultManagerURL, "\x00"))
	}
}

func TestResolveManagerURLWithUnpatchedDefault(t *testing.T) {
	orig := DefaultManagerURL
	DefaultManagerURL = "$$SYNERGIA_MANAGER_URL$$" + strings.Repeat("\x00", defaultManagerURLSentinelSize-defaultManagerURLSentinelLen)
	defer func() { DefaultManagerURL = orig }()

	if got := resolveManagerURL(); got != "" {
		t.Fatalf("expected empty resolved URL for unpatched default, got %q", got)
	}
}

func TestResolveManagerURLWithPatchedValue(t *testing.T) {
	orig := DefaultManagerURL
	defer func() { DefaultManagerURL = orig }()

	patched := "wss://example.com/ws/worker"
	DefaultManagerURL = patched + strings.Repeat("\x00", defaultManagerURLSentinelSize-len(patched))

	if got := resolveManagerURL(); got != patched {
		t.Fatalf("expected resolved URL %q, got %q", patched, got)
	}
}
