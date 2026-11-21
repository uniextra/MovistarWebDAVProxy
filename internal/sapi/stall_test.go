package sapi

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Si quien lee el cuerpo deja de pedir datos, se avisa; si es el origen el que tarda
// en dar datos, o el cuerpo ya se envió entero, no.
func TestStallReader(t *testing.T) {
	var fired int32
	s := newStallReader(strings.NewReader("hola"), 60*time.Millisecond, func() { atomic.StoreInt32(&fired, 1) })
	time.Sleep(150 * time.Millisecond) // nadie lee
	if !s.stop() || atomic.LoadInt32(&fired) != 1 {
		t.Fatal("debía detectar que no se lee")
	}

	atomic.StoreInt32(&fired, 0)
	pr, pw := io.Pipe()
	s = newStallReader(pr, 60*time.Millisecond, func() { atomic.StoreInt32(&fired, 1) })
	go func() { time.Sleep(150 * time.Millisecond); _, _ = pw.Write([]byte("x")); _ = pw.Close() }()
	_, _ = io.ReadAll(s) // lectura esperando al origen: no cuenta
	time.Sleep(150 * time.Millisecond) // cuerpo ya enviado: esperar la respuesta no cuenta
	if s.stop() || atomic.LoadInt32(&fired) != 0 {
		t.Fatal("no debía cortar: el retraso era del origen o de la respuesta")
	}
}
