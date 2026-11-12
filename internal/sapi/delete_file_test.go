package sapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDeleteFile_OfficialFormat(t *testing.T) {
	var capturedPath string
	var capturedQuery map[string][]string
	var capturedContentType string
	var capturedBody []byte
	var capturedMethod string

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedQuery = r.URL.Query()
		capturedContentType = r.Header.Get("Content-Type")

		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed reading body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":"Media soft deleted successfully"}`))
	})

	err := client.DeleteFile("7276344205", "sample.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedMethod != "POST" {
		t.Errorf("expected POST method, got %s", capturedMethod)
	}
	if capturedPath != "/sapi/media" {
		t.Errorf("expected path /sapi/media, got %s", capturedPath)
	}
	if len(capturedQuery["action"]) == 0 || capturedQuery["action"][0] != "softdelete" {
		t.Errorf("expected action=softdelete, got %v", capturedQuery["action"])
	}
	if len(capturedQuery["id"]) == 0 || capturedQuery["id"][0] != "7276344205" {
		t.Errorf("expected id=7276344205, got %v", capturedQuery["id"])
	}
	if !strings.Contains(capturedContentType, "application/x-www-form-urlencoded") {
		t.Errorf("expected application/x-www-form-urlencoded content type, got %s", capturedContentType)
	}
	if len(capturedBody) != 0 {
		t.Errorf("expected empty body, got %d bytes: %s", len(capturedBody), string(capturedBody))
	}
}

func TestDeleteFile_MED1022_Idempotent(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":{"code":"MED-1022","message":"File already soft deleted"}}`))
	})

	err := client.DeleteFile("7276344205", "test.cinfo")
	if err != nil {
		t.Fatalf("expected nil error on MED-1022, got %v", err)
	}
}

func TestDeleteFile_MED1017_Retry(t *testing.T) {
	orig := deleteRetryBackoff
	deleteRetryBackoff = func(attempt int) time.Duration { return 5 * time.Millisecond }
	defer func() { deleteRetryBackoff = orig }()

	attempts := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		if attempts == 1 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"error":{"code":"MED-1017","message":"Media in async validation"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":"Media soft deleted successfully"}`))
	})

	err := client.DeleteFile("7276344205", "test.mp4")
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestDeleteFile_404_NotFound_Idempotent(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"MED-1004","message":"Media not found"}}`))
	})

	err := client.DeleteFile("999999999", "missing.txt")
	if err != nil {
		t.Fatalf("expected nil error on 404 not found, got %v", err)
	}
}

func TestDeleteFile_403_Retry(t *testing.T) {
	orig := delete403Backoff
	delete403Backoff = func(attempt int) time.Duration { return 5 * time.Millisecond }
	defer func() { delete403Backoff = orig }()

	attempts := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`403 Forbidden`))
			return
		}
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":"Media soft deleted successfully"}`))
	})

	err := client.DeleteFile("7276344205")
	if err != nil {
		t.Fatalf("expected success after 403 retry, got %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestDeleteFile_APIError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":{"code":"MED-9999","message":"Internal error"}}`))
	})

	err := client.DeleteFile("7276344205")
	if err == nil {
		t.Fatalf("expected error on API error response, got nil")
	}
	if !strings.Contains(err.Error(), "MED-9999") {
		t.Errorf("expected error containing MED-9999, got %v", err)
	}
}

func TestDeleteFolder_OfficialFormat(t *testing.T) {
	var capturedPath string
	var capturedQuery map[string][]string
	var capturedContentType string
	var capturedBody []byte
	var capturedMethod string

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedQuery = r.URL.Query()
		capturedContentType = r.Header.Get("Content-Type")

		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed reading body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":"Folder soft deleted successfully"}`))
	})

	err := client.DeleteFolder(12345, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedMethod != "POST" {
		t.Errorf("expected POST method, got %s", capturedMethod)
	}
	if capturedPath != "/sapi/media/folder" {
		t.Errorf("expected path /sapi/media/folder, got %s", capturedPath)
	}
	if len(capturedQuery["action"]) == 0 || capturedQuery["action"][0] != "softdelete" {
		t.Errorf("expected action=softdelete, got %v", capturedQuery["action"])
	}
	if len(capturedQuery["id"]) == 0 || capturedQuery["id"][0] != "12345" {
		t.Errorf("expected id=12345, got %v", capturedQuery["id"])
	}
	if !strings.Contains(capturedContentType, "application/x-www-form-urlencoded") {
		t.Errorf("expected application/x-www-form-urlencoded content type, got %s", capturedContentType)
	}
	if len(capturedBody) != 0 {
		t.Errorf("expected empty body, got %d bytes: %s", len(capturedBody), string(capturedBody))
	}
}

func TestDeleteFolder_Permanent(t *testing.T) {
	var capturedQuery map[string][]string

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":"Folder deleted permanently"}`))
	})

	err := client.DeleteFolder(12345, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(capturedQuery["action"]) == 0 || capturedQuery["action"][0] != "delete" {
		t.Errorf("expected action=delete, got %v", capturedQuery["action"])
	}
}

func TestDeleteFolder_404_NotFound(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"MED-1004","message":"Folder not found"}}`))
	})

	err := client.DeleteFolder(99999, false)
	if err != nil {
		t.Fatalf("expected nil error on folder 404, got %v", err)
	}
}

func TestDeleteFolder_MED1022(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":{"code":"MED-1022","message":"Folder already soft deleted"}}`))
	})

	err := client.DeleteFolder(12345, false)
	if err != nil {
		t.Fatalf("expected nil error on folder MED-1022, got %v", err)
	}
}

func TestDeleteFolder_APIError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":{"code":"MED-9999","message":"Folder error"}}`))
	})

	err := client.DeleteFolder(12345, false)
	if err == nil {
		t.Fatalf("expected error on folder API error, got nil")
	}
	if !strings.Contains(err.Error(), "MED-9999") {
		t.Errorf("expected error containing MED-9999, got %v", err)
	}
}
