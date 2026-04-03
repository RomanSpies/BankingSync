package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bankingsync/store"
)

const dockerHubTagsURL = "https://hub.docker.com/v2/repositories/romanspies/bankingsync/tags/?page_size=25&ordering=last_updated"

var versionPattern = regexp.MustCompile(`^1\.\d+\.\d+\.\d+$`)

// checkForUpdate queries Docker Hub for the latest version tag and stores the
// result in the database. It sends an email notification the first time a new
// version is detected.
func checkForUpdate(st *store.Store) {
	latest, err := fetchLatestVersion()
	if err != nil {
		log.Printf("Update check failed: %v", err)
		return
	}
	if latest == "" || latest == Version {
		return
	}
	if !isNewer(latest, Version) {
		return
	}

	notified, _ := st.GetSetting("update_notified_version")
	_ = st.SetSetting("update_available", latest)

	if notified != latest {
		log.Printf("New version available: %s (running %s)", latest, Version)
		_ = st.SetSetting("update_notified_version", latest)
		sendEmail(
			"BankingSync: update available",
			fmt.Sprintf("A new version of BankingSync is available.\n\nRunning: %s\nAvailable: %s\n\nUpdate with: docker compose pull && docker compose up -d\n", Version, latest),
		)
	}
}

// fetchLatestVersion queries Docker Hub and returns the highest version tag.
func fetchLatestVersion() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(dockerHubTagsURL)
	if err != nil {
		return "", fmt.Errorf("GET tags: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var data struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	var best string
	for _, tag := range data.Results {
		if versionPattern.MatchString(tag.Name) {
			if best == "" || isNewer(tag.Name, best) {
				best = tag.Name
			}
		}
	}
	return best, nil
}

// isNewer returns true if version a is newer than version b.
// Both must be in the format "1.x.x.x".
func isNewer(a, b string) bool {
	ap := parseVersion(a)
	bp := parseVersion(b)
	for i := 0; i < 4; i++ {
		if ap[i] != bp[i] {
			return ap[i] > bp[i]
		}
	}
	return false
}

// parseVersion splits "1.26.14.42" into [1, 26, 14, 42].
func parseVersion(v string) [4]int {
	var parts [4]int
	for i, s := range strings.SplitN(v, ".", 4) {
		if i < 4 {
			parts[i], _ = strconv.Atoi(s)
		}
	}
	return parts
}
