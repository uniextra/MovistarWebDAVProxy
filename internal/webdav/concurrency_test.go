package webdav

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDecoupledConcurrencyLimits(t *testing.T) {
	// 1. Verificar capacidades de semáforos alineados a 1 (estricta serie como cliente oficial)
	if cap(uploadSem) != 1 {
		t.Fatalf("expected uploadSem cap 1, got %d", cap(uploadSem))
	}
	if cap(downloadSem) != 1 {
		t.Fatalf("expected downloadSem cap 1, got %d", cap(downloadSem))
	}
	if cap(globalTransferSem) != 1 {
		t.Fatalf("expected globalTransferSem cap 1, got %d", cap(globalTransferSem))
	}
	if cap(uploadBufferSem) != 1 {
		t.Fatalf("expected uploadBufferSem cap 1, got %d", cap(uploadBufferSem))
	}

	// 2. Verificar que el techo conjunto bloquea el 2º hilo
	var acquired atomic.Int32
	var releaseChan = make(chan struct{})
	var wg sync.WaitGroup

	// Intentar lanzar 2 operaciones que adquieren globalTransferSem
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case globalTransferSem <- struct{}{}:
				acquired.Add(1)
				<-releaseChan
				<-globalTransferSem
			case <-time.After(100 * time.Millisecond):
				// Esperado para el 2º hilo: debe bloquearse
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	if acquired.Load() != 1 {
		t.Fatalf("expected exactly 1 transfer acquired, got %d", acquired.Load())
	}
	close(releaseChan)
	wg.Wait()
}
