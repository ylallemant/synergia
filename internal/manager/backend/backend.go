package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Backend name constants.
const (
	LlamaCpp = "llama.cpp"
	// VLLM = "vllm" // future
)

// Names lists all supported backend names.
var Names = []string{LlamaCpp}

// DownloadURLTemplates maps backend name → download URL template.
// Supported placeholders: {version}, {os}, {platform}, {arch}, {ext}
var DownloadURLTemplates = map[string]string{
	LlamaCpp: "https://github.com/ggml-org/llama.cpp/releases/download/{version}/llama-{version}-bin-{platform}-{arch}.{ext}",
}

// releaseSources maps backend name → GitHub releases API URL.
var releaseSources = map[string]string{
	LlamaCpp: "https://api.github.com/repos/ggml-org/llama.cpp/releases",
}

// githubRelease represents a single release from the GitHub API.
type githubRelease struct {
	TagName string `json:"tag_name"`
}

// FetchTags retrieves the most recent release tags for the given backend.
func FetchTags(name string, limit int) ([]string, error) {
	url, ok := releaseSources[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend: %s", name)
	}

	if limit <= 0 {
		limit = 10
	}

	reqURL := fmt.Sprintf("%s?per_page=%d", url, limit)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}

	result := make([]string, 0, len(releases))
	for _, r := range releases {
		result = append(result, r.TagName)
	}
	return result, nil
}

// ExpandURL expands a download URL template for the given version, OS, and arch.
// Supported placeholders: {version}, {os}, {platform}, {arch}, {ext}.
func ExpandURL(tpl, version, goos, goarch string) string {
	platform := mapPlatform(goos)
	arch := mapArch(goarch)
	ext := mapExt(goos)
	url := tpl
	url = replaceAll(url, "{version}", version)
	url = replaceAll(url, "{os}", goos)
	url = replaceAll(url, "{platform}", platform)
	url = replaceAll(url, "{arch}", arch)
	url = replaceAll(url, "{ext}", ext)
	return url
}

func mapPlatform(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	case "linux":
		return "ubuntu"
	case "windows":
		return "win-cpu"
	default:
		return goos
	}
}

func mapArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	default:
		return goarch
	}
}

func mapExt(goos string) string {
	if goos == "windows" {
		return "zip"
	}
	return "tar.gz"
}

func replaceAll(s, old, new string) string {
	for {
		i := indexOf(s, old)
		if i < 0 {
			return s
		}
		s = s[:i] + new + s[i+len(old):]
	}
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// IsValid returns true if the given name is a known backend.
func IsValid(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}
