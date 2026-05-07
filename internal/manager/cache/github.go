package cache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const synergiaReleasesURL = "https://api.github.com/repos/ylallemant/synergia/releases"

func fetchSynergiaTags(limit int) ([]string, error) {
	reqURL := fmt.Sprintf("%s?per_page=%d", synergiaReleasesURL, limit)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}

	result := make([]string, 0, len(releases))
	for _, r := range releases {
		result = append(result, r.TagName)
	}
	return result, nil
}
