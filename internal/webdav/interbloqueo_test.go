package webdav

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Reproduce el interbloqueo visto con restic -> rclone -> proxy:
// restic multiplexa varias subidas en UNA conexión HTTP/2 con control de flujo
// compartido. Si el proxy deja de leer el cuerpo de una subida B mientras espera
// turno, los datos de B llenan la ventana compartida y los datos que necesita la
// subida A (la que tiene el turno) ya no llegan: A espera a B y B espera a A,
// hasta que el cliente corta por timeout (5 min en rclone).
//
// Aquí el cuerpo de A entrega un primer trozo y luego solo continúa cuando el
// cuerpo de B se ha leído entero. Con el proxy bloqueando a B antes de leer, A
// nunca continúa y acaba "cortándose" (timeout del test).

const esperaMaxima = 5 * time.Second

var errCorteCliente = errors.New("el cliente cortó: no llegaron los datos (timeout)")

// cuerpoQueEsperaAOtro entrega primero (entero, en tantas lecturas como haga
// falta), avisa de que está esperando y solo entrega segundo cuando se cierra
// continuar (o falla tras esperaMaxima, como un cliente que corta por timeout).
type cuerpoQueEsperaAOtro struct {
	primero, segundo *bytes.Reader
	esperando        chan struct{}
	continuar        <-chan struct{}
	avisado          bool
}

func (c *cuerpoQueEsperaAOtro) Read(p []byte) (int, error) {
	if c.primero.Len() > 0 {
		return c.primero.Read(p)
	}
	if !c.avisado {
		c.avisado = true
		close(c.esperando) // el primer trozo ya está entero en el proxy
		select {
		case <-c.continuar:
		case <-time.After(esperaMaxima):
			return 0, errCorteCliente
		}
	}
	return c.segundo.Read(p) // io.EOF al terminar
}

func (c *cuerpoQueEsperaAOtro) Close() error { return nil }

// cuerpoQueAvisaAlTerminar entrega datos y cierra leido cuando el proxy lo ha leído entero.
type cuerpoQueAvisaAlTerminar struct {
	r     *bytes.Reader
	leido chan struct{}
	once  sync.Once
}

func (c *cuerpoQueAvisaAlTerminar) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		c.once.Do(func() { close(c.leido) })
	}
	return n, err
}

func (c *cuerpoQueAvisaAlTerminar) Close() error { return nil }

func trozo(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func probarInterbloqueo(t *testing.T, tamA, tamB int) {
	t.Helper()
	srv, subidas, cerrar := servidorQueCuentaSubidas(t)
	defer cerrar()

	leidoB := make(chan struct{})
	esperandoA := make(chan struct{})
	mitad := tamA / 2
	cuerpoA := &cuerpoQueEsperaAOtro{
		primero:   bytes.NewReader(trozo('A', mitad)),
		segundo:   bytes.NewReader(trozo('a', tamA-mitad)),
		esperando: esperandoA,
		continuar: leidoB,
	}
	cuerpoB := &cuerpoQueAvisaAlTerminar{r: bytes.NewReader(trozo('B', tamB)), leido: leidoB}

	inicio := time.Now()
	var wg sync.WaitGroup
	codigos := make([]int, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("PUT", "/a.bin", cuerpoA)
		req.ContentLength = int64(tamA)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		codigos[0] = rec.Code
	}()

	// B solo empieza cuando A ya tiene escrito su primer trozo (y con él el hueco/turno).
	select {
	case <-esperandoA:
	case <-time.After(esperaMaxima):
		t.Fatal("A no llegó a escribir su primer trozo")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("PUT", "/b.bin", cuerpoB)
		req.ContentLength = int64(tamB)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		codigos[1] = rec.Code
	}()

	wg.Wait()
	dur := time.Since(inicio)

	for i, nombre := range []string{"A", "B"} {
		if codigos[i] != http.StatusCreated && codigos[i] != http.StatusNoContent {
			t.Errorf("la subida %s respondió %d (interbloqueo: el proxy no leyó el cuerpo de B mientras A esperaba)", nombre, codigos[i])
		}
	}
	if n := subidas.Load(); n != 2 {
		t.Errorf("se esperaban 2 subidas a la nube, hubo %d", n)
	}
	if dur >= esperaMaxima {
		t.Errorf("tardó %v: se llegó al corte por timeout", dur)
	}
}

// Ficheros pequeños (buffer en RAM): A tiene el hueco de RAM y B no debe esperarlo para leer.
func TestInterbloqueo_SubidasPequenasEnRAM(t *testing.T) {
	t.Setenv("WEBDAV_TMPDIR", t.TempDir())
	probarInterbloqueo(t, 64*1024, 64*1024)
}

// A grande (subida en flujo, tiene el turno de subida) y B pequeño.
func TestInterbloqueo_FlujoGrandeYPequeno(t *testing.T) {
	t.Setenv("WEBDAV_TMPDIR", t.TempDir())
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	probarInterbloqueo(t, 3*1024*1024, 64*1024)
}

// A y B grandes: B no puede subir en flujo (turno ocupado) y debe ir entero al spool.
func TestInterbloqueo_DosFlujosGrandes(t *testing.T) {
	t.Setenv("WEBDAV_TMPDIR", t.TempDir())
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	probarInterbloqueo(t, 3*1024*1024, 2*1024*1024)
}

// Los mismos casos con la subida en flujo activada a propósito (WEBDAV_STREAM_UPLOAD=1):
// el turno de ingestión no debe bloquear a B aunque A suba en flujo.
func TestInterbloqueo_ConFlujoActivado(t *testing.T) {
	t.Setenv("WEBDAV_TMPDIR", t.TempDir())
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	t.Setenv("WEBDAV_STREAM_UPLOAD", "1")
	probarInterbloqueo(t, 3*1024*1024, 64*1024)
}

// Segundo interbloqueo (visto en vivo con el primer arreglo): una subida grande
// en flujo retiene el hueco de SAPI (SAPI_MAX_CONCURRENT=1) mientras su cuerpo
// sigue llegando; el cliente solo envía el resto cuando le responden a otra
// petición que también necesita SAPI. Por defecto (sin flujo) no debe bloquearse.
func TestInterbloqueo_HuecoSAPIRetenidoPorSubida(t *testing.T) {
	t.Setenv("WEBDAV_TMPDIR", t.TempDir())
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	t.Setenv("SAPI_MAX_CONCURRENT", "1")
	srv, subidas, cerrar := servidorQueCuentaSubidas(t)
	defer cerrar()

	otraTerminada := make(chan struct{})
	esperandoA := make(chan struct{})
	tamA := 3 * 1024 * 1024
	cuerpoA := &cuerpoQueEsperaAOtro{
		primero:   bytes.NewReader(trozo('A', tamA/2)),
		segundo:   bytes.NewReader(trozo('a', tamA-tamA/2)),
		esperando: esperandoA,
		continuar: otraTerminada,
	}

	inicio := time.Now()
	codigo := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("PUT", "/grande.bin", cuerpoA)
		req.ContentLength = int64(tamA)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		codigo <- rec.Code
	}()

	select {
	case <-esperandoA:
	case <-time.After(esperaMaxima):
		t.Fatal("A no llegó a escribir su primera mitad")
	}

	// Otra petición que necesita el hueco de SAPI (como un PROPFIND/MKCOL del cliente).
	errOtra := make(chan error, 1)
	go func() {
		_, err := srv.fs.client.GetStorageSpace()
		errOtra <- err
	}()
	select {
	case err := <-errOtra:
		if err != nil {
			t.Logf("GetStorageSpace devolvió error (no importa para el test): %v", err)
		}
		close(otraTerminada)
	case <-time.After(esperaMaxima):
		close(otraTerminada)
		t.Error("la otra petición a SAPI se quedó esperando el hueco que retiene la subida en curso (interbloqueo)")
	}

	if c := <-codigo; c != http.StatusCreated && c != http.StatusNoContent {
		t.Errorf("la subida respondió %d", c)
	}
	if n := subidas.Load(); n != 1 {
		t.Errorf("se esperaba 1 subida a la nube, hubo %d", n)
	}
	if d := time.Since(inicio); d >= esperaMaxima {
		t.Errorf("tardó %v: se llegó al corte por timeout", d)
	}
}
