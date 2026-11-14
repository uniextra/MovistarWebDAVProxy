package cache

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Escrituras concurrentes desde varias conexiones del pool: con busy_timeout en la
// cadena de conexión esperan su turno en vez de fallar con SQLITE_BUSY.
func TestConcurrentWritesDoNotFailBusy(t *testing.T) {
	store, err := NewMetadataStore(filepath.Join(t.TempDir(), "meta test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				tx, err := store.db.Begin()
				if err != nil {
					errs <- err
					return
				}
				if _, err := tx.Exec("INSERT INTO t (v) VALUES (?)", fmt.Sprintf("%d-%d", g, i)); err != nil {
					_ = tx.Rollback()
					errs <- err
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("escritura concurrente falló: %v", err)
	}
}
