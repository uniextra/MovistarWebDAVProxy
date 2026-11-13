package webdav

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/net/webdav"
)

// Dos peticiones simultáneas sobre la misma ruta: la segunda espera al bloqueo
// implícito de la primera en vez de devolver 423.
func TestImplicitLockWaitsForConcurrentRequest(t *testing.T) {
	ls := newResilientLockSystem()
	implicit := webdav.LockDetails{Root: "/datos/carpeta", Duration: -1, ZeroDepth: true}

	tok1, err := ls.Create(time.Now(), implicit)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = ls.Unlock(time.Now(), tok1)
	}()
	start := time.Now()
	tok2, err := ls.Create(time.Now(), implicit)
	if err != nil {
		t.Fatalf("la segunda petición debería esperar y obtener el bloqueo: %v", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("no esperó al primer bloqueo (%v)", d)
	}
	_ = ls.Unlock(time.Now(), tok2)
}

// Un bloqueo explícito de cliente (LOCK con propietario) no espera: falla enseguida.
func TestExplicitLockConflictFailsFast(t *testing.T) {
	ls := newResilientLockSystem()
	held := webdav.LockDetails{Root: "/doc.txt", Duration: time.Minute, OwnerXML: "<owner>win</owner>"}
	if _, err := ls.Create(time.Now(), held); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := ls.Create(time.Now(), webdav.LockDetails{Root: "/doc.txt", Duration: time.Minute, OwnerXML: "<owner>otro</owner>"})
	if !errors.Is(err, webdav.ErrLocked) {
		t.Fatalf("esperado ErrLocked, obtenido %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("un bloqueo explícito no debe esperar (%v)", d)
	}
}
