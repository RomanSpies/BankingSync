package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"bankingsync/store"
)

const dockerHubTagsURL = "https://hub.docker.com/v2/repositories/romanspies/bankingsync/tags/?page_size=25&ordering=last_updated"

var versionPattern = regexp.MustCompile(`^1\.\d+\.\d+\.\d+$`)

// checkForUpdate queries Docker Hub for the latest version tag and stores the
// result in the database. It sends an email notification the first time a new
// version is detected.
func checkForUpdate(ctx context.Context, st *store.Store) {
	tracer := otel.Tracer("bankingsync")
	ctx, span := tracer.Start(ctx, "update.check")
	defer span.End()

	latest, err := fetchLatestVersion(ctx)
	if err != nil {
		log.Printf("Update check failed: %v", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetAttributes(
		attribute.String("latest_version", latest),
		attribute.String("current_version", Version),
	)
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
		span.SetAttributes(attribute.Bool("notified", true))
		sendEmail(ctx,
			"BankingSync: update available",
			fmt.Sprintf("A new version of BankingSync is available.\n\nRunning: %s\nAvailable: %s\n\nUpdate with: docker compose pull && docker compose up -d\n", Version, latest),
		)
	}
}

// fetchLatestVersion queries Docker Hub and returns the highest version tag.
func fetchLatestVersion(ctx context.Context) (string, error) {
	tracer := otel.Tracer("bankingsync")
	ctx, span := tracer.Start(ctx, "update.fetch_dockerhub")
	defer span.End()

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dockerHubTagsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("GET tags: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
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
	span.SetAttributes(attribute.String("best_tag", best))
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
