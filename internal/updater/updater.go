package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/notifier"
)

const (
	DefaultReleasesAPIURL  = "https://api.github.com/repos/uniextra/MovistarWebDAVProxy/releases/latest"
	DefaultReleasesPageURL = "https://github.com/uniextra/MovistarWebDAVProxy/releases"
	DefaultCheckInterval   = time.Hour
	stateFileName          = ".last_notified_release"
)

// Status describes the current release check state exposed to the WebUI.
type Status struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
	CheckedAt       string `json:"checked_at,omitempty"`
}

// Checker periodically checks GitHub Releases and sends a single Telegram alert per new release.
type Checker struct {
	mu             sync.RWMutex
	apiURL         string
	client         *http.Client
	redirectClient *http.Client
	currentVersion func() string
	stateFilePath  func() string
	notifyTelegram func(latestVer, currentVer, releaseURL string) error

	latestVersion   string
	releaseURL      string
	checkedAt       time.Time
	lastNotifiedTag string
	lastLoggedTag   string
}

// NewChecker creates a release Checker using the default GitHub Releases endpoints and Telegram notifier.
func NewChecker() *Checker {
	return &Checker{
		apiURL: DefaultReleasesAPIURL,
		client: &http.Client{Timeout: 15 * time.Second},
		redirectClient: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		currentVersion: func() string { return config.Version },
		stateFilePath:  func() string { return filepath.Join(config.TokensDir, stateFileName) },
		notifyTelegram: sendTelegramUpdateNotification,
	}
}

// NewCheckerWithOptions creates a Checker with custom endpoints and callbacks.
func NewCheckerWithOptions(apiURL, statePath string, currentVersion func() string, notifyTelegram func(latestVer, currentVer, releaseURL string) error) *Checker {
	c := NewChecker()
	if strings.TrimSpace(apiURL) != "" {
		c.apiURL = apiURL
	}
	if statePath != "" {
		c.stateFilePath = func() string { return statePath }
	}
	if currentVersion != nil {
		c.currentVersion = currentVersion
	}
	if notifyTelegram != nil {
		c.notifyTelegram = notifyTelegram
	}
	return c
}

func sendTelegramUpdateNotification(latestVer, currentVer, releaseURL string) error {
	if notifier.Default == nil || !notifier.Default.IsConfigured() {
		return fmt.Errorf("telegram not configured")
	}
	msg := fmt.Sprintf(
		"🆕 *Nueva versión disponible: %s*\n\n"+
			"Hay una nueva release de *Movistar Cloud WebDAV Proxy* disponible en GitHub.\n\n"+
			"• Versión actual: `%s`\n"+
			"• Nueva versión: `%s`\n"+
			"• Release: %s",
		latestVer, currentVer, latestVer, releaseURL,
	)
	return notifier.Send(msg)
}

// Start executes an immediate check and then polls GitHub Releases on the given interval until ctx is cancelled.
func (c *Checker) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	c.CheckOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CheckOnce(ctx)
		}
	}
}

// Status returns a snapshot of the latest version check result.
func (c *Checker) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	curVer := c.getCurrentVersion()
	st := Status{
		CurrentVersion: curVer,
		LatestVersion:  c.latestVersion,
		ReleaseURL:     c.releaseURL,
	}
	if !c.checkedAt.IsZero() {
		st.CheckedAt = c.checkedAt.UTC().Format(time.RFC3339)
	}
	if c.latestVersion != "" {
		st.UpdateAvailable = IsNewerVersion(curVer, c.latestVersion)
	}
	return st
}

// CheckOnce checks GitHub for the latest stable release and triggers notifications if a newer release is found.
func (c *Checker) CheckOnce(ctx context.Context) Status {
	tag, htmlURL, err := c.fetchLatestRelease(ctx)
	if err != nil {
		return c.Status()
	}

	curVer := c.getCurrentVersion()
	updateAvailable := IsNewerVersion(curVer, tag)

	c.mu.Lock()
	c.latestVersion = tag
	c.releaseURL = htmlURL
	c.checkedAt = time.Now()

	shouldLog := false
	shouldNotify := false
	statePath := ""
	if c.stateFilePath != nil {
		statePath = c.stateFilePath()
	}

	if updateAvailable {
		if c.lastLoggedTag != tag {
			c.lastLoggedTag = tag
			shouldLog = true
		}
		if c.lastNotifiedTag == "" && statePath != "" {
			c.lastNotifiedTag = readNotifiedTag(statePath)
		}
		if c.lastNotifiedTag != tag {
			shouldNotify = true
		}
	}
	notifyFn := c.notifyTelegram
	c.mu.Unlock()

	if shouldLog {
		logger.Log("[Update] 🆕 Nueva versión disponible en GitHub: %s (actual: %s) -> %s", tag, curVer, htmlURL)
	}

	if shouldNotify && notifyFn != nil {
		if err := notifyFn(tag, curVer, htmlURL); err == nil {
			c.mu.Lock()
			c.lastNotifiedTag = tag
			c.mu.Unlock()
			if statePath != "" {
				writeNotifiedTag(statePath, tag)
			}
		}
	}

	return c.Status()
}

func (c *Checker) getCurrentVersion() string {
	if c.currentVersion != nil {
		if v := strings.TrimSpace(c.currentVersion()); v != "" {
			return v
		}
	}
	return config.Version
}

type githubReleaseResponse struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

func (c *Checker) fetchLatestRelease(ctx context.Context) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "MovistarWebDAVProxy/"+c.getCurrentVersion())

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", "", err
		}
		var rel githubReleaseResponse
		if err := json.Unmarshal(body, &rel); err != nil {
			return "", "", err
		}
		if rel.Draft || rel.Prerelease {
			return "", "", fmt.Errorf("latest release is draft or prerelease")
		}
		tag := strings.TrimSpace(rel.TagName)
		if tag == "" {
			return "", "", fmt.Errorf("empty tag_name in release response")
		}
		htmlURL := strings.TrimSpace(rel.HTMLURL)
		if htmlURL == "" {
			htmlURL = DefaultReleasesPageURL + "/tag/" + tag
		}
		return tag, htmlURL, nil
	}

	// Fallback if GitHub API rate-limits unauthenticated requests (e.g. HTTP 403/429).
	if c.apiURL == DefaultReleasesAPIURL {
		return c.fetchLatestReleaseRedirect(ctx)
	}
	return "", "", fmt.Errorf("github releases API returned HTTP %d", resp.StatusCode)
}

func (c *Checker) fetchLatestReleaseRedirect(ctx context.Context) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultReleasesPageURL+"/latest", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "MovistarWebDAVProxy/"+c.getCurrentVersion())

	client := c.redirectClient
	if client == nil {
		client = &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("releases redirect returned HTTP %d", resp.StatusCode)
	}

	loc := strings.TrimSpace(resp.Header.Get("Location"))
	const marker = "/releases/tag/"
	idx := strings.Index(loc, marker)
	if idx < 0 {
		return "", "", fmt.Errorf("unexpected redirect location: %s", loc)
	}
	tag := strings.TrimSpace(loc[idx+len(marker):])
	tag = strings.TrimSuffix(tag, "/")
	if tag == "" {
		return "", "", fmt.Errorf("empty tag in redirect location")
	}
	htmlURL := DefaultReleasesPageURL + "/tag/" + tag
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		htmlURL = loc
	}
	return tag, htmlURL, nil
}

// IsNewerVersion returns true strictly when latest represents a higher stable vMAJOR.MINOR.PATCH than current.
func IsNewerVersion(current, latest string) bool {
	if hasPrereleaseSuffix(latest) {
		return false
	}
	curMajor, curMinor, curPatch, okCur := parseSemver(current)
	latMajor, latMinor, latPatch, okLat := parseSemver(latest)
	if !okCur || !okLat {
		return false
	}
	if latMajor != curMajor {
		return latMajor > curMajor
	}
	if latMinor != curMinor {
		return latMinor > curMinor
	}
	return latPatch > curPatch
}

func hasPrereleaseSuffix(v string) bool {
	trimmed := strings.TrimSpace(v)
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "v"), "V")
	if idx := strings.IndexByte(trimmed, '+'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return strings.Contains(trimmed, "-")
}

func parseSemver(v string) (int, int, int, bool) {
	trimmed := strings.TrimSpace(v)
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "v"), "V")
	if idx := strings.IndexByte(trimmed, '+'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if idx := strings.IndexByte(trimmed, '-'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	major, err1 := strconv.Atoi(stripTrailingSuffix(parts[0]))
	minor, err2 := strconv.Atoi(stripTrailingSuffix(parts[1]))
	patch, err3 := strconv.Atoi(stripTrailingSuffix(parts[2]))
	if err1 != nil || err2 != nil || err3 != nil || major < 0 || minor < 0 || patch < 0 {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

func stripTrailingSuffix(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return s[:i]
		}
	}
	return s
}

func readNotifiedTag(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeNotifiedTag(path, tag string) {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	_ = os.WriteFile(path, []byte(strings.TrimSpace(tag)+"\n"), 0o600)
}
