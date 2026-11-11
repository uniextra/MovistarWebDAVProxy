package sapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDeleteFile_OfficialFormat(t *testing.T) {
	var capturedPath string
	var capturedQuery map[string][]string
	var capturedContentType string
	var capturedBody map[string]interface{}

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.Query()
		capturedContentType = r.Header.Get("Content-Type")

		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &capturedBody)

		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":"Files soft deleted successfully"}`))
	})

	err := client.DeleteFile("7276344205", "sample.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedPath != "/sapi/media/file" {
		t.Errorf("expected path /sapi/media/file, got %s", capturedPath)
	}
	if capturedQuery["action"][0] != "delete" {
		t.Errorf("expected action=delete, got %v", capturedQuery["action"])
	}
	if capturedQuery["softdelete"][0] != "true" {
		t.Errorf("expected softdelete=true, got %v", capturedQuery["softdelete"])
	}
	if !strings.Contains(capturedContentType, "application/json") {
		t.Errorf("expected application/json content type, got %s", capturedContentType)
	}

	data, ok := capturedBody["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected data object in body, got %v", capturedBody)
	}
	files, ok := data["files"].([]interface{})
	if !ok || len(files) != 1 {
		t.Fatalf("expected files array with 1 item, got %v", data["files"])
	}
	// Verify ID is numeric 7276344205
	if files[0] != float64(7276344205) && files[0] != int64(7276344205) {
		t.Errorf("expected file id 7276344205, got %v", files[0])
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
		w.Write([]byte(`{"success":"Files soft deleted successfully"}`))
	})

	err := client.DeleteFile("7276344205", "test.mp4")
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestDeleteFile_PictureKind(t *testing.T) {
	var capturedPath string
	var capturedBody map[string]interface{}

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &capturedBody)

		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":"Pictures soft deleted successfully"}`))
	})

	err := client.DeleteFile("12345", "photo.jpg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedPath != "/sapi/media/picture" {
		t.Errorf("expected path /sapi/media/picture, got %s", capturedPath)
	}
	data := capturedBody["data"].(map[string]interface{})
	if _, ok := data["pictures"]; !ok {
		t.Errorf("expected pictures key in body data, got %v", data)
	}
}
