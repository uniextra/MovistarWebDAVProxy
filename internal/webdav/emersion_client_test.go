package webdav

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	emersion "github.com/emersion/go-webdav"
	"movistar-webdav-proxy/internal/config"
)

func TestEmersionWebDAVClient_Compatibility(t *testing.T) {
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{AuthDisabled: true})
	defer cleanup()

	ts := httptest.NewServer(srv)
	defer ts.Close()

	client, err := emersion.NewClient(nil, ts.URL)
	if err != nil {
		t.Fatalf("emersion.NewClient failed: %v", err)
	}

	ctx := context.Background()

	// 1. Stat raíz (PROPFIND Depth: 0)
	rootFi, err := client.Stat(ctx, "/")
	if err != nil {
		t.Fatalf("client.Stat(/) failed: %v", err)
	}
	if !rootFi.IsDir {
		t.Errorf("expected root to be dir, got isDir=%v", rootFi.IsDir)
	}

	// 2. Stat archivo existente (/hello.txt)
	fileFi, err := client.Stat(ctx, "/hello.txt")
	if err != nil {
		t.Fatalf("client.Stat(/hello.txt) failed: %v", err)
	}
	if fileFi.IsDir || fileFi.Size != 11 {
		t.Errorf("unexpected file stat: isDir=%v size=%d", fileFi.IsDir, fileFi.Size)
	}

	// 3. Descarga GET (/hello.txt)
	rc, err := client.Open(ctx, "/hello.txt")
	if err != nil {
		t.Fatalf("client.Open(/hello.txt) failed: %v", err)
	}
	content, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(content) != "hello world" {
		t.Fatalf("unexpected content: %q (err: %v)", string(content), err)
	}

	// 4. Subida PUT (/test_emersion.bin)
	payload := []byte("emersion webdav client interoperability test payload")
	wc, err := client.Create(ctx, "/test_emersion.bin")
	if err != nil {
		t.Fatalf("client.Create(/test_emersion.bin) failed: %v", err)
	}
	if _, err := wc.Write(payload); err != nil {
		t.Fatalf("wc.Write failed: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("wc.Close failed: %v", err)
	}

	// 5. Listado ReadDir (PROPFIND Depth: 1)
	items, err := client.ReadDir(ctx, "/", false)
	if err != nil {
		t.Fatalf("client.ReadDir(/) failed: %v", err)
	}
	if len(items) == 0 {
		t.Errorf("expected items in root listing, got 0")
	}

	// 6. Borrado DELETE de archivo (/hello.txt)
	if err := client.RemoveAll(ctx, "/hello.txt"); err != nil {
		t.Errorf("client.RemoveAll(/hello.txt) failed: %v", err)
	}

	// 7. Borrado DELETE de carpeta (/Backups)
	if err := client.RemoveAll(ctx, "/Backups"); err != nil {
		t.Errorf("client.RemoveAll(/Backups) failed: %v", err)
	}
}
