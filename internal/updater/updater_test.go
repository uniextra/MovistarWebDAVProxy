package updater

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestIsNewerVersion(t *testing.T) {
	tests := []struct {
		current string
		latest  string
		want    bool
	}{
		{"v2.6.12", "v2.6.13", true},
		{"v2.6.12", "v2.7.0", true},
		{"v2.6.12", "v3.0.0", true},
		{"v2.6.12", "v2.6.12", false},
		{"v2.6.12", "v2.6.11", false},
		{"v2.6.12-pr61", "v2.6.12", false},
		{"v2.6.12-pr61", "v2.6.13", true},
		{"v2.6.12", "v2.6.13-rc1", false},
		{"v2.6.12", "v2.6.13-beta.1", false},
		{"dev", "v2.6.13", false},
		{"v2.6.12", "invalid", false},
	}

	for _, tc := range tests {
		got := IsNewerVersion(tc.current, tc.latest)
		if got != tc.want {
			t.Errorf("IsNewerVersion(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestCheckOnce_NotifiesTelegramOncePerRelease(t *testing.T) {
	var mu sync.Mutex
	latestTag := "v2.6.13"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tag := latestTag
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tag_name":   tag,
			"html_url":   DefaultReleasesPageURL + "/tag/" + tag,
			"draft":      false,
			"prerelease": false,
		})
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), stateFileName)
	var notifications []string

	newTestChecker := func() *Checker {
		c := NewChecker()
		c.apiURL = server.URL
		c.currentVersion = func() string { return "v2.6.12" }
		c.stateFilePath = func() string { return statePath }
		c.notifyTelegram = func(latestVer, currentVer, releaseURL string) error {
			notifications = append(notifications, latestVer)
			return nil
		}
		return c
	}

	checker1 := newTestChecker()
	for i := 0; i < 3; i++ {
		st := checker1.CheckOnce(context.Background())
		if !st.UpdateAvailable || st.LatestVersion != "v2.6.13" {
			t.Fatalf("iteration %d: unexpected status %+v", i, st)
		}
	}

	if len(notifications) != 1 || notifications[0] != "v2.6.13" {
		t.Fatalf("expected exactly 1 notification for v2.6.13, got %v", notifications)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("expected persisted state file: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "v2.6.13" {
		t.Fatalf("unexpected persisted release tag: %q", string(raw))
	}

	// New Checker instance (simulating restart) must not re-notify for v2.6.13.
	checker2 := newTestChecker()
	st2 := checker2.CheckOnce(context.Background())
	if !st2.UpdateAvailable || st2.LatestVersion != "v2.6.13" {
		t.Fatalf("unexpected status after restart: %+v", st2)
	}
	if len(notifications) != 1 {
		t.Fatalf("expected no duplicate notification after restart, got %v", notifications)
	}

	// Newer release v2.6.14 must notify once.
	mu.Lock()
	latestTag = "v2.6.14"
	mu.Unlock()

	st3 := checker2.CheckOnce(context.Background())
	if !st3.UpdateAvailable || st3.LatestVersion != "v2.6.14" {
		t.Fatalf("unexpected status for v2.6.14: %+v", st3)
	}
	st4 := checker2.CheckOnce(context.Background())
	if !st4.UpdateAvailable || st4.LatestVersion != "v2.6.14" {
		t.Fatalf("unexpected status on repeat check for v2.6.14: %+v", st4)
	}
	if len(notifications) != 2 || notifications[1] != "v2.6.14" {
		t.Fatalf("expected 1 new notification for v2.6.14, got %v", notifications)
	}
}
