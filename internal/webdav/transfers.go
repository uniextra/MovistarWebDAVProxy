package webdav

import (
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Transferencias en curso del proxy, para el panel: subidas a la nube y descargas
// (GET) servidas a los clientes. Las descargas de un mismo archivo se agrupan (un
// cliente puede leerlo por rangos) y, al terminar, siguen visibles un rato marcadas
// como inactivas: el cliente suele tener lo leído en su búfer.

// TransferInfo es una transferencia tal como la muestra el panel.
type TransferInfo struct {
	Kind        string  `json:"kind"` // upload | download
	Path        string  `json:"path"`
	Size        int64   `json:"size"`
	Bytes       int64   `json:"bytes"` // subidas: enviados; descargas: posición alcanzada en el archivo
	Percentage  int     `json:"percentage"`
	Speed       float64 `json:"speed"` // bytes/s en los últimos segundos
	ETA         *int64  `json:"eta,omitempty"`
	Idle        bool    `json:"idle,omitempty"`
	IdleSeconds int64   `json:"idle_seconds,omitempty"`
}

type transferSample struct {
	at    time.Time
	total int64 // bytes transferidos acumulados
}

type transfer struct {
	kind, path string
	size       int64
	position   int64 // descargas: hasta dónde se ha leído; subidas: bytes enviados
	moved      int64 // bytes transferidos acumulados (para la velocidad)
	active     int   // peticiones en curso
	lastActive time.Time
	samples    []transferSample
}

// TransferTracker lleva la cuenta de las transferencias en curso.
type TransferTracker struct {
	mu    sync.Mutex
	items map[string]*transfer
	keep  time.Duration // cuánto siguen visibles las descargas terminadas
	now   func() time.Time
}

// NewTransferTracker crea un registro; las descargas terminadas se muestran keep más.
func NewTransferTracker(keep time.Duration) *TransferTracker {
	return &TransferTracker{items: map[string]*transfer{}, keep: keep, now: time.Now}
}

// Transfers registra las transferencias de este proceso.
var Transfers = NewTransferTracker(5 * time.Minute)

// TransferHandle es una petición en curso dentro de una transferencia.
type TransferHandle struct {
	t   *TransferTracker
	key string
}

// Start registra una subida o una petición de descarga. offset es la posición de
// inicio dentro del archivo (rango de un GET); size, el tamaño total si se conoce.
func (t *TransferTracker) Start(kind, path string, size, offset int64) *TransferHandle {
	key := kind + "\x00" + path
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.items[key]
	if tr == nil || kind == "upload" {
		tr = &transfer{kind: kind, path: path}
		t.items[key] = tr
	}
	if size > 0 {
		tr.size = size
	}
	if kind == "download" {
		tr.position = offset
	}
	tr.active++
	tr.lastActive = t.now()
	return &TransferHandle{t: t, key: key}
}

// Add suma n bytes transferidos.
func (h *TransferHandle) Add(n int64) {
	if h == nil || n <= 0 {
		return
	}
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	tr := h.t.items[h.key]
	if tr == nil {
		return
	}
	now := h.t.now()
	tr.position += n
	tr.moved += n
	tr.lastActive = now
	if len(tr.samples) == 0 || now.Sub(tr.samples[len(tr.samples)-1].at) >= 500*time.Millisecond {
		tr.samples = append(tr.samples, transferSample{at: now, total: tr.moved})
		if len(tr.samples) > 40 {
			tr.samples = tr.samples[len(tr.samples)-40:]
		}
	}
}

// Reset vuelve la posición a 0 (una subida que se reintenta desde el principio).
func (h *TransferHandle) Reset() {
	if h == nil {
		return
	}
	h.t.mu.Lock()
	if tr := h.t.items[h.key]; tr != nil {
		tr.position = 0
	}
	h.t.mu.Unlock()
}

// Done cierra la petición. Las subidas desaparecen; las descargas quedan inactivas.
func (h *TransferHandle) Done() {
	if h == nil {
		return
	}
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	tr := h.t.items[h.key]
	if tr == nil {
		return
	}
	tr.active--
	tr.lastActive = h.t.now()
	if tr.active <= 0 && tr.kind == "upload" {
		delete(h.t.items, h.key)
	}
}

// Snapshot devuelve las transferencias activas y las descargas recientes.
func (t *TransferTracker) Snapshot() []TransferInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	out := []TransferInfo{}
	for key, tr := range t.items {
		idleFor := now.Sub(tr.lastActive)
		if tr.active <= 0 && idleFor > t.keep {
			delete(t.items, key)
			continue
		}
		info := TransferInfo{Kind: tr.kind, Path: tr.path, Size: tr.size, Bytes: tr.position}
		if tr.size > 0 {
			info.Percentage = int(tr.position * 100 / tr.size)
			if info.Percentage > 100 {
				info.Percentage = 100
			}
		}
		// Velocidad de los últimos ~5 s; cero si lleva parada más de 3 s.
		if n := len(tr.samples); n > 0 && idleFor < 3*time.Second {
			first := tr.samples[0]
			for _, s := range tr.samples {
				if now.Sub(s.at) <= 5*time.Second {
					first = s
					break
				}
			}
			if dt := now.Sub(first.at).Seconds(); dt >= 1 {
				info.Speed = float64(tr.moved-first.total) / dt
			}
		}
		if tr.kind == "upload" && info.Speed > 0 && tr.size > tr.position {
			eta := int64(float64(tr.size-tr.position) / info.Speed)
			info.ETA = &eta
		}
		if tr.active <= 0 || idleFor >= 30*time.Second {
			info.Idle, info.IdleSeconds = true, int64(idleFor.Seconds())
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "upload"
		}
		if out[i].Idle != out[j].Idle {
			return !out[i].Idle
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// countingReader cuenta lo que se lee de una subida.
type countingReader struct {
	r io.Reader
	h *TransferHandle
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Add(int64(n))
	return n, err
}

// countingReadSeeker conserva el Seek del original (las subidas se reintentan
// rebobinando el cuerpo).
type countingReadSeeker struct {
	countingReader
	s io.Seeker
}

func (c *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	pos, err := c.s.Seek(offset, whence)
	if err == nil && pos == 0 {
		c.h.Reset()
	}
	return pos, err
}

// trackUpload envuelve el cuerpo de una subida a la nube para contar su avance.
func trackUpload(path string, size int64, r io.Reader) (io.Reader, *TransferHandle) {
	h := Transfers.Start("upload", path, size, 0)
	if rs, ok := r.(io.ReadSeeker); ok {
		return &countingReadSeeker{countingReader: countingReader{r: rs, h: h}, s: rs}, h
	}
	return &countingReader{r: r, h: h}, h
}

// transferWriter cuenta lo que se envía en un GET. El tamaño total y la posición
// de inicio salen de Content-Range (206) o Content-Length (200).
type transferWriter struct {
	http.ResponseWriter
	path    string
	h       *TransferHandle
	started bool
}

func (t *transferWriter) WriteHeader(code int) {
	if !t.started && (code == http.StatusOK || code == http.StatusPartialContent) {
		t.started = true
		size, offset := int64(0), int64(0)
		if cr := t.Header().Get("Content-Range"); cr != "" {
			// "bytes 100-199/1000"
			if sp := strings.Fields(cr); len(sp) == 2 {
				if i := strings.Index(sp[1], "/"); i > 0 {
					size, _ = strconv.ParseInt(sp[1][i+1:], 10, 64)
					if j := strings.Index(sp[1][:i], "-"); j > 0 {
						offset, _ = strconv.ParseInt(sp[1][:j], 10, 64)
					}
				}
			}
		} else if cl := t.Header().Get("Content-Length"); cl != "" {
			size, _ = strconv.ParseInt(cl, 10, 64)
		}
		t.h = Transfers.Start("download", t.path, size, offset)
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *transferWriter) Write(p []byte) (int, error) {
	if !t.started {
		t.WriteHeader(http.StatusOK)
	}
	n, err := t.ResponseWriter.Write(p)
	t.h.Add(int64(n))
	return n, err
}

func (t *transferWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *transferWriter) done() { t.h.Done() }
