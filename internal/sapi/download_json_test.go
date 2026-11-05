package sapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsSAPIErrorJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "SAPI MED-1017 error",
			input:    `{"error": {"code": "MED-1017", "message": "File is being validated"}}`,
			expected: true,
		},
		{
			name:     "SAPI AUTH-1001 error",
			input:    `{"error": {"code": "AUTH-1001", "message": "Token expired"}}`,
			expected: true,
		},
		{
			name:     "SAPI string error",
			input:    `{"error": "unauthorized"}`,
			expected: true,
		},
		{
			name:     "SAPI error_code string",
			input:    `{"error_code": "INVALID_TOKEN", "error_description": "Expired"}`,
			expected: true,
		},
		{
			name:     "SAPI status error",
			input:    `{"status": "error", "message": "fail"}`,
			expected: true,
		},
		{
			name:     "SAPI status FAIL uppercase",
			input:    `{"status": "FAIL"}`,
			expected: true,
		},
		{
			name:     "Home Assistant backup metadata JSON",
			input:    `{"version": 2, "date": "2026-11-04T12:00:00.000Z", "name": "Backup HA", "type": "full", "folders": ["share"]}`,
			expected: false,
		},
		{
			name:     "Generic config JSON",
			input:    `{"host": "192.168.1.1", "port": 8080, "debug": true}`,
			expected: false,
		},
		{
			name:     "JSON array",
			input:    `[{"id": 1}, {"id": 2}]`,
			expected: false,
		},
		{
			name:     "JSON with UTF-8 BOM",
			input:    "\xef\xbb\xbf" + `{"version": 1, "app": "test"}`,
			expected: false,
		},
		{
			name:     "JSON with null error",
			input:    `{"data": "content", "error": null}`,
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := isSAPIErrorJSON([]byte(tc.input))
			if actual != tc.expected {
				t.Errorf("isSAPIErrorJSON(%s) = %v; want %v", tc.input, actual, tc.expected)
			}
		})
	}
}

func TestIsDefiniteBinaryFile(t *testing.T) {
	binaries := []string{
		"backup.tar", "file.tar.gz", "archive.zip", "test.7z", "image.iso",
		"photo.jpg", "photo.PNG", "video.mp4", "song.mp3", "doc.pdf",
		"nested/path/to/backup.tar", "C:\\backups\\archive.zip",
	}
	for _, b := range binaries {
		if !isDefiniteBinaryFile(b) {
			t.Errorf("expected isDefiniteBinaryFile(%q) to be true", b)
		}
	}

	nonBinaries := []string{
		"01M6FZYZWHM8C7CNW60WNHD2XZ.metadata.json", "config.json", "app.js",
		"script.ts", "data.geojson", "readme.txt", "settings.json", "",
	}
	for _, nb := range nonBinaries {
		if isDefiniteBinaryFile(nb) {
			t.Errorf("expected isDefiniteBinaryFile(%q) to be false", nb)
		}
	}
}

func TestDownloadFileStreamRange_LegitimateJSON(t *testing.T) {
	haMetadataContent := `{"version": 2, "date": "2026-11-04T12:00:00.000Z", "name": "Backup 2026-11-04", "type": "full", "folders": ["share", "ssl"]}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(haMetadataContent))
	}))
	defer ts.Close()

	client := &Client{
		httpClient: ts.Client(),
	}

	item := MediaItem{
		ID:   "media123",
		Name: "01M6FZYZWHM8C7CNW60WNHD2XZ.metadata.json",
		URL:  ts.URL + "/download",
		Size: int64(len(haMetadataContent)),
	}

	rc, err := client.DownloadFileStreamRange(item, 0)
	if err != nil {
		t.Fatalf("DownloadFileStreamRange returned unexpected error: %v", err)
	}
	defer rc.Close()

	bodyBytes, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("io.ReadAll returned error: %v", err)
	}

	if string(bodyBytes) != haMetadataContent {
		t.Fatalf("got body %q; want %q", string(bodyBytes), haMetadataContent)
	}
}

func TestDownloadFileStreamRange_SAPIErrorJSON(t *testing.T) {
	sapiError := `{"error": {"code": "MED-1017", "message": "File is being validated"}}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sapiError))
	}))
	defer ts.Close()

	client := &Client{
		httpClient: ts.Client(),
	}

	item := MediaItem{
		ID:   "media123",
		Name: "test.json",
		URL:  ts.URL + "/download",
	}

	rc, err := client.DownloadFileStreamRange(item, 0)
	if err == nil {
		if rc != nil {
			rc.Close()
		}
		t.Fatalf("expected error for SAPI error response, got nil")
	}

	if !strings.Contains(err.Error(), "MED-1017") {
		t.Fatalf("expected error message to contain 'MED-1017', got: %v", err)
	}
}

func TestDownloadFileStreamRange_BinaryFileReceivingJSON(t *testing.T) {
	arbitraryJSON := `{"arbitrary": "object"}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(arbitraryJSON))
	}))
	defer ts.Close()

	client := &Client{
		httpClient: ts.Client(),
	}

	item := MediaItem{
		ID:   "media123",
		Name: "backup.tar",
		URL:  ts.URL + "/download",
	}

	rc, err := client.DownloadFileStreamRange(item, 0)
	if err == nil {
		if rc != nil {
			rc.Close()
		}
		t.Fatalf("expected error when downloading binary file receiving JSON, got nil")
	}

	if !strings.Contains(err.Error(), "cloud returned error JSON instead of file") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
