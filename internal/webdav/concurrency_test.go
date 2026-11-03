package webdav

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDecoupledConcurrencyLimits(t *testing.T) {
	// 1. Verificar capacidades de semáforos
	if cap(uploadSem) != 4 {
		t.Fatalf("expected uploadSem cap 4, got %d", cap(uploadSem))
	}
	if cap(downloadSem) != 4 {
		t.Fatalf("expected downloadSem cap 4, got %d", cap(downloadSem))
	}
	if cap(globalTransferSem) != 5 {
		t.Fatalf("expected globalTransferSem cap 5, got %d", cap(globalTransferSem))
	}
	if cap(uploadBufferSem) != 4 {
		t.Fatalf("expected uploadBufferSem cap 4, got %d", cap(uploadBufferSem))
	}

	// 2. Verificar que el techo conjunto bloquea el 6º hilo
	var acquired atomic.Int32
	var releaseChan = make(chan struct{})
	var wg sync.WaitGroup

	// Intentar lanzar 6 operaciones que adquieren globalTransferSem
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case globalTransferSem <- struct{}{}:
				acquired.Add(1)
				<-releaseChan
				<-globalTransferSem
			case <-time.After(100 * time.Millisecond):
				// Esperado para el 6º hilo: debe bloquearse
			}
		}()
	}

	// Esperar brevemente a que intenten adquirir
	time.Sleep(50 * time.Millisecond)

	currentAcquired := acquired.Load()
	if currentAcquired != 5 {
		t.Errorf("expected exactly 5 acquired slots before timeout, got %d", currentAcquired)
	}

	// Liberar los 5 retenidos
	close(releaseChan)
	wg.Wait()
}
