package main

import (
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.26.14.2", "1.26.14.1", true},
		{"1.26.15.1", "1.26.14.99", true},
		{"1.27.1.1", "1.26.52.99", true},
		{"1.26.14.1", "1.26.14.1", false},
		{"1.26.14.1", "1.26.14.2", false},
		{"1.25.1.1", "1.26.1.1", false},
	}
	for _, tt := range tests {
		if got := isNewer(tt.a, tt.b); got != tt.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	got := parseVersion("1.26.14.42")
	want := [4]int{1, 26, 14, 42}
	if got != want {
		t.Errorf("parseVersion: got %v, want %v", got, want)
	}
}

func TestParseVersion_dev(t *testing.T) {
	got := parseVersion("dev")
	want := [4]int{0, 0, 0, 0}
	if got != want {
		t.Errorf("parseVersion(dev): got %v, want %v", got, want)
	}
}

func TestFetchLatestVersion_live(t *testing.T) {
	// Live test against Docker Hub public API.
	// The repo exists at hub.docker.com/r/romanspies/bankingsync.
	// This test verifies that the API call succeeds and the response is parsed
	// correctly. The result may be empty (no version tags yet) or a valid
	// version string.
	latest, err := fetchLatestVersion()
	if err != nil {
		t.Fatalf("fetchLatestVersion: %v", err)
	}
	if latest != "" && !versionPattern.MatchString(latest) {
		t.Errorf("got %q, want empty or version matching 1.x.x.x", latest)
	}
	t.Logf("latest version from Docker Hub: %q", latest)
}

func TestVersionPattern(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{"1.26.14.42", true},
		{"1.0.0.1", true},
		{"latest", false},
		{"7834c71fb976b0f1e3b43c92ba80096e53a68984", false},
		{"v1.0.0", false},
		{"1.26.14", false},
	}
	for _, tt := range tests {
		if got := versionPattern.MatchString(tt.tag); got != tt.want {
			t.Errorf("versionPattern.Match(%q) = %v, want %v", tt.tag, got, tt.want)
		}
	}
}
