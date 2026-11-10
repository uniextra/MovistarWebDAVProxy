package webdav

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/sapi"
)

func TestSingleflightGroup_Deduplication(t *testing.T) {
	var g singleflightGroup
	var calls atomic.Int32

	var wg sync.WaitGroup
	results := make([]string, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			val, err := g.Do("test_key", func() (interface{}, error) {
				calls.Add(1)
				time.Sleep(50 * time.Millisecond)
				return "deduped_result", nil
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			results[idx] = val.(string)
		}(i)
	}

	wg.Wait()

	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 call, got %d", calls.Load())
	}
	for i, r := range results {
		if r != "deduped_result" {
			t.Errorf("result[%d] = %s, expected 'deduped_result'", i, r)
		}
	}
}

func TestMovistarFS_StaleCacheFallbackOnRateLimit(t *testing.T) {
	mfs := NewMovistarFS(nil)
	mfs.cacheTTL = 10 * time.Millisecond

	// Poblar caché
	folderID := int64(12345)
	initialMedia := []sapi.MediaItem{
		{ID: "m1", Name: "backup.db", Size: 1024},
	}
	mfs.mediaCache[folderID] = newCachedMedia(initialMedia)

	// Esperar a que expire el TTL
	time.Sleep(20 * time.Millisecond)

	// Simular fallo en singleflight / llamada a SAPI simulada
	// Al fallar Do o la función, getMedia debe recurrir a la caché existente
	key := "media:12345"
	_, _ = mfs.sf.Do(key, func() (interface{}, error) {
		return nil, errors.New("HTTP 403: CloudFront WAF block")
	})

	// findMedia debe encontrar el archivo en la caché previa (stale cache) sin error
	item, ok, err := mfs.findMedia(folderID, "backup.db")
	if err != nil {
		t.Fatalf("expected nil error using stale cache, got %v", err)
	}
	if !ok || item == nil {
		t.Fatalf("expected findMedia to succeed using stale cache, got found=%v", ok)
	}
	if item.Name != "backup.db" {
		t.Errorf("expected item.Name = 'backup.db', got %s", item.Name)
	}
}
