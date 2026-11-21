package webdav

import (
	"fmt"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

// Las subidas masivas nunca ocupan el último hueco y ceden el turno a las normales.
func TestUploadGate_PriorityAndReserve(t *testing.T) {
	g := newUploadGate(3)
	if !g.tryAcquire(true) || !g.tryAcquire(true) {
		t.Fatal("las masivas deben poder usar 2 de 3 huecos")
	}
	if g.tryAcquire(true) {
		t.Fatal("una masiva no debe ocupar el último hueco")
	}
	if !g.tryAcquire(false) {
		t.Fatal("una subida normal debe tener el hueco reservado")
	}
	// Con todo ocupado esperan una normal y una masiva: al liberar, entra la normal.
	order := make(chan string, 2)
	go func() { g.acquire(true); order <- "masiva" }()
	time.Sleep(20 * time.Millisecond)
	go func() { g.acquire(false); order <- "normal" }()
	time.Sleep(20 * time.Millisecond)
	g.release()
	if got := <-order; got != "normal" {
		t.Fatalf("primero debía entrar la normal, entró %s", got)
	}
}

// Sin hueco libre la subida queda en cola (RAM) y, al liberarse un hueco, pasa a
// subirse en flujo con lo que ya tenía guardado.
func TestQueuedUpload_PromotesToStream(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // subida en flujo del modo web (sin el envío de la app oficial)
	t.Setenv("WEBDAV_UPLOAD_SPOOL", "false")
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	t.Setenv("WEBDAV_QUEUE_RAM_MB", "64")
	oldCap := uploads.capacityNow()
	uploadGateSet(1)
	t.Cleanup(func() { uploadGateSet(oldCap) })

	var mu sync.Mutex
	var got []byte
	var calls int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/sapi/upload" {
			atomic.AddInt32(&calls, 1)
			_ = r.ParseMultipartForm(1 << 20)
			if f, _, err := r.FormFile("file"); err == nil {
				b, _ := io.ReadAll(f)
				mu.Lock()
				got = b
				mu.Unlock()
			}
			_, _ = io.WriteString(w, `{"data":{"id":"777","name":"t.bin"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"folders":[{"id":100,"name":"Root","parentid":0,"magic":true}]}}`)
	}))
	t.Cleanup(mock.Close)
	client, err := sapi.NewClient(config.AccountConfig{Provider: "movistar", Phone: "612345678", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	mfs := NewMovistarFS(client)

	if !uploads.tryAcquire(false) { // otro cliente ocupa el único hueco
		t.Fatal("no se pudo ocupar el hueco")
	}
	payload := bytes.Repeat([]byte{7}, 3<<20)
	w := NewWebDAVFileWriter(mfs, "/t.bin", "t.bin", 100, "")
	w.guard = &uploadGuard{expected: int64(len(payload))}
	if _, err := w.Write(payload[:2<<20]); err != nil {
		t.Fatal(err)
	}
	if !w.queued || w.streamOnly {
		t.Fatal("sin hueco libre la subida debe quedar en cola")
	}
	uploads.release() // queda libre el hueco
	if _, err := w.Write(payload[2<<20:]); err != nil {
		t.Fatal(err)
	}
	if !w.streamOnly {
		t.Fatal("con hueco libre debía pasar a subirse en flujo")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(got, payload) || calls != 1 {
		t.Fatalf("la nube recibió %d bytes en %d subidas, esperados %d en 1", len(got), calls, len(payload))
	}
	if queuedRAM.Load() != 0 {
		t.Errorf("la RAM de la cola debe quedar liberada, quedan %d", queuedRAM.Load())
	}
}

// La cuota que traiga la respuesta (1 PiB del rclone interno) se sustituye por la real.
func TestAnyQuotaRegex(t *testing.T) {
	in := `<D:prop><D:quota-available-bytes>1125899906842624</D:quota-available-bytes><D:quota-used-bytes>0</D:quota-used-bytes></D:prop>`
	if out := anyQuotaRegex.ReplaceAllString(in, ""); out != "<D:prop></D:prop>" {
		t.Fatalf("quedó: %s", out)
	}
}

// La caché de archivos pequeños leídos tiene tope: se descarta el contenido de lo
// más antiguo que ya está en la nube, nunca el de lo que aún no tiene ID.
func TestTrimSmallFileCache(t *testing.T) {
	t.Setenv("WEBDAV_SMALLFILE_CACHE_MB", "1")
	mfs := &MovistarFS{uploadCache: map[string]*MemoryFile{}}
	now := time.Now()
	for i := 0; i < 4; i++ {
		mfs.uploadCache[fmt.Sprintf("/a%d", i)] = &MemoryFile{ID: fmt.Sprintf("%d", i), Content: make([]byte, 400<<10), Size: 400 << 10, CachedAt: now.Add(time.Duration(i) * time.Second)}
	}
	mfs.uploadCache["/pendiente"] = &MemoryFile{Content: make([]byte, 400<<10), Size: 400 << 10, CachedAt: now.Add(-time.Minute)}
	mfs.trimSmallFileCacheLocked(now.Add(10 * time.Second))
	var total int64
	for _, m := range mfs.uploadCache {
		total += int64(len(m.Content))
	}
	if total > 1<<20+400<<10 { // el pendiente no cuenta para descartar
		t.Fatalf("no respetó el tope: %d", total)
	}
	if len(mfs.uploadCache["/pendiente"].Content) == 0 {
		t.Fatal("no se puede descartar lo que aún no está en la nube")
	}
	if len(mfs.uploadCache["/a3"].Content) == 0 {
		t.Fatal("debía conservar lo más reciente")
	}
}

// Aunque no haya memoria libre, siempre cabe el búfer de una subida.
func TestQueueRAMBudget_FloorOneBuffer(t *testing.T) {
	t.Setenv("WEBDAV_QUEUE_RAM_MB", "0")
	if got, want := queueRAMBudget(), int64(GetUploadRAMBufferMaxBytes()); got < want {
		t.Fatalf("presupuesto %d < un búfer %d", got, want)
	}
}

// Con escrituras pequeñas antes de encolarse (lo normal por SMB o el montaje), la
// reserva de RAM del búfer no se cuenta dos veces: al terminar queda en cero.
func TestQueuedUpload_NoRAMLeakWithSmallWrites(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "browser") // subida en flujo del modo web (sin el envío de la app oficial)
	t.Setenv("WEBDAV_UPLOAD_SPOOL", "false")
	t.Setenv("WEBDAV_BUFFER_MB", "1")
	t.Setenv("WEBDAV_QUEUE_RAM_MB", "64")
	oldCap := uploads.capacityNow()
	uploadGateSet(1)
	t.Cleanup(func() { uploadGateSet(oldCap) })
	start := queuedRAM.Load()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/sapi/upload" {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"data":{"id":"778","name":"u.bin"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"folders":[{"id":100,"name":"Root","parentid":0,"magic":true}]}}`)
	}))
	t.Cleanup(mock.Close)
	client, err := sapi.NewClient(config.AccountConfig{Provider: "movistar", Phone: "612345678", AccessToken: "a", RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURLs(mock.URL+"/sapi", mock.URL)
	mfs := NewMovistarFS(client)

	if !uploads.tryAcquire(false) {
		t.Fatal("no se pudo ocupar el hueco")
	}
	w := NewWebDAVFileWriter(mfs, "/u.bin", "u.bin", 100, "")
	w.guard = &uploadGuard{expected: 3 << 20}
	for i := 0; i < 6; i++ { // 6 × 128 KB en RAM antes de pasar del búfer inicial
		if _, err := w.Write(make([]byte, 128<<10)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if !w.queued {
		t.Fatal("sin hueco libre la subida debe quedar en cola")
	}
	uploads.release()
	if _, err := w.Write(make([]byte, 3<<20-6*(128<<10)-(1<<20))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := queuedRAM.Load() - start; got != 0 {
		t.Fatalf("la RAM de la cola debe quedar liberada; quedan %d bytes reservados", got)
	}
}
