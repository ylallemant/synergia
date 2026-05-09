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

	defaultURL := buildDefaultManagerURL()
	if len(defaultURL) != defaultManagerURLSentinelSize {
		t.Fatalf("expected default URL length %d, got %d", defaultManagerURLSentinelSize, len(defaultURL))
	}
	if strings.TrimRight(defaultURL, "\x00") != sentinel {
		t.Fatalf("expected trimmed default URL to equal sentinel, got %q", strings.TrimRight(defaultURL, "\x00"))
	}
}

func TestResolveManagerURLWithUnpatchedDefault(t *testing.T) {
	orig := DefaultManagerURL
	DefaultManagerURL = buildDefaultManagerURL()
	defer func() { DefaultManagerURL = orig }()

	if got := resolveManagerURL(); got != "" {
		t.Fatalf("expected empty resolved URL for unpatched default, got %q", got)
	}
}

func TestResolveManagerURLWithPatchedValue(t *testing.T) {
	orig := DefaultManagerURL
	DefaultManagerURL = buildDefaultManagerURL()
	defer func() { DefaultManagerURL = orig }()

	patched := "wss://example.com/ws/worker"
	DefaultManagerURL = patched + strings.Repeat("\x00", defaultManagerURLSentinelSize-len(patched))

	if got := resolveManagerURL(); got != patched {
		t.Fatalf("expected resolved URL %q, got %q", patched, got)
	}
}
