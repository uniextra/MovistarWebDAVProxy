package webdav

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/logger"
)

const (
	// DuplicatiTestProbeFileName es el nombre del archivo sonda utilizado por Duplicati para probar privilegios.
	DuplicatiTestProbeFileName = "duplicati-access-privileges-test.tmp"

	// duplicatiProbeTTL es el tiempo de vida máximo de un archivo de prueba en memoria antes de ser purgado (5 min).
	duplicatiProbeTTL = 5 * time.Minute

	// maxDuplicatiProbeMemory es el tamaño máximo en bytes permitido para el buffer en memoria de una sonda (10 MB).
	maxDuplicatiProbeMemory = 10 * 1024 * 1024
)

// isDuplicatiTestPath comprueba si una ruta o nombre de archivo corresponde al archivo sonda
// de privilegios de Duplicati (duplicati-access-privileges-test.tmp) o a archivos .tmp de control.
func isDuplicatiTestPath(rawPath string) bool {
	base := strings.ToLower(path.Base(rawPath))
	base = strings.TrimSpace(base)
	if base == DuplicatiTestProbeFileName {
		return true
	}
	if strings.HasPrefix(base, "duplicati") && strings.HasSuffix(base, ".tmp") {
		return true
	}
	if strings.HasSuffix(base, ".tmp") {
		return true
	}
	return false
}

// duplicatiTestEntry representa el archivo temporal de prueba almacenado en RAM.
type duplicatiTestEntry struct {
	content   []byte
	modTime   time.Time
	etag      string
	createdAt time.Time
}

// DuplicatiProbeStore gestiona el almacenamiento volátil en memoria y concurrente
// para absorber las pruebas de privilegios de Duplicati sin contactar con SAPI de Telefónica.
type DuplicatiProbeStore struct {
	mu      sync.RWMutex
	entries map[string]*duplicatiTestEntry
}

// globalDuplicatiStore es la instancia compartida en memoria para el proxy WebDAV.
var globalDuplicatiStore = NewDuplicatiProbeStore()

// NewDuplicatiProbeStore crea un nuevo almacén en memoria para pruebas de Duplicati.
func NewDuplicatiProbeStore() *DuplicatiProbeStore {
	return &DuplicatiProbeStore{
		entries: make(map[string]*duplicatiTestEntry),
	}
}

// normalizeProbePath normaliza la ruta para usarla como clave única case-insensitive.
func normalizeProbePath(rawPath string) string {
	clean := path.Clean("/" + rawPath)
	return strings.ToLower(clean)
}

// Put almacena el contenido y metadatos de una prueba temporal en memoria.
func (s *DuplicatiProbeStore) Put(rawPath string, content []byte, modTime time.Time) string {
	key := normalizeProbePath(rawPath)
	now := time.Now()
	if modTime.IsZero() {
		modTime = now
	}

	etag := fmt.Sprintf("\"%x-%x\"", modTime.UnixNano(), len(content))

	s.mu.Lock()
	defer s.mu.Unlock()

	// Limpieza oportunista de entradas expiradas
	for k, e := range s.entries {
		if now.Sub(e.createdAt) > duplicatiProbeTTL {
			delete(s.entries, k)
		}
	}

	s.entries[key] = &duplicatiTestEntry{
		content:   content,
		modTime:   modTime,
		etag:      etag,
		createdAt: now,
	}

	return etag
}

// Get recupera el contenido y metadatos de una prueba en memoria.
func (s *DuplicatiProbeStore) Get(rawPath string) (content []byte, modTime time.Time, etag string, ok bool) {
	key := normalizeProbePath(rawPath)

	s.mu.RLock()
	entry, found := s.entries[key]
	s.mu.RUnlock()

	if !found {
		return nil, time.Time{}, "", false
	}

	if time.Since(entry.createdAt) > duplicatiProbeTTL {
		s.Delete(rawPath)
		return nil, time.Time{}, "", false
	}

	return entry.content, entry.modTime, entry.etag, true
}

// Delete elimina una prueba temporal de la memoria.
func (s *DuplicatiProbeStore) Delete(rawPath string) {
	key := normalizeProbePath(rawPath)
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// DuplicatiProbeFileWriter implementa webdav.File actuando como buffer volátil en memoria
// para absorber escrituras de archivos de prueba de Duplicati sin subirlos a SAPI.
type DuplicatiProbeFileWriter struct {
	store   *DuplicatiProbeStore
	path    string
	name    string
	buf     *bytes.Buffer
	closed  bool
	modTime time.Time
}

func NewDuplicatiProbeFileWriter(store *DuplicatiProbeStore, path, name string) *DuplicatiProbeFileWriter {
	if store == nil {
		store = globalDuplicatiStore
	}
	return &DuplicatiProbeFileWriter{
		store: store,
		path:  path,
		name:  name,
		buf:   bytes.NewBuffer(nil),
	}
}

func (d *DuplicatiProbeFileWriter) Write(p []byte) (n int, err error) {
	if int64(d.buf.Len()+len(p)) > maxDuplicatiProbeMemory {
		return len(p), nil // Absorber sin exceder memoria máxima
	}
	return d.buf.Write(p)
}

func (d *DuplicatiProbeFileWriter) Close() error {
	if !d.closed {
		d.closed = true
		content := d.buf.Bytes()
		modTime := d.modTime
		if modTime.IsZero() {
			modTime = time.Now()
		}
		d.store.Put(d.path, content, modTime)
		logger.Log("[WebDAV] 🛡️ Interceptado test de Duplicati (PUT %s), simulando éxito en memoria.", d.path)
	}
	return nil
}

func (d *DuplicatiProbeFileWriter) Read(p []byte) (n int, err error) { return 0, io.EOF }
func (d *DuplicatiProbeFileWriter) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		return offset, nil
	case io.SeekCurrent, io.SeekEnd:
		return int64(d.buf.Len()) + offset, nil
	default:
		return 0, os.ErrInvalid
	}
}
func (d *DuplicatiProbeFileWriter) Readdir(count int) ([]os.FileInfo, error) { return nil, fs.ErrInvalid }
func (d *DuplicatiProbeFileWriter) Stat() (os.FileInfo, error) {
	return &VirtualFileInfo{name: d.name, size: int64(d.buf.Len()), mode: 0644, modTime: time.Now()}, nil
}

// handleDuplicatiProbe intercepta peticiones WebDAV dirigidas a archivos de prueba de Duplicati.
// Devuelve true si la petición fue interceptada y respondida, o false en caso contrario.
func (s *Server) handleDuplicatiProbe(w http.ResponseWriter, r *http.Request, rawPath string) bool {
	if !isDuplicatiTestPath(rawPath) {
		return false
	}

	cleanPath := path.Clean("/" + rawPath)
	method := r.Method
	store := s.getDuplicatiStore()

	switch method {
	case http.MethodPut:
		logger.Log("[WebDAV] 🛡️ Interceptado test de Duplicati (%s %s), simulando éxito en memoria.", method, cleanPath)

		// Conservar fecha original si envían X-OC-Mtime
		r = withOCMtime(w, r)
		modTime := ocMtimeFromContext(r.Context())
		if modTime.IsZero() {
			modTime = time.Now()
		}

		// Consumir el cuerpo de la petición evitando fugas de memoria o conexiones abiertas
		var buf bytes.Buffer
		if r.Body != nil {
			_, _ = io.Copy(&buf, io.LimitReader(r.Body, maxDuplicatiProbeMemory))
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}

		content := buf.Bytes()
		etag := store.Put(cleanPath, content, modTime)

		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated) // HTTP 201 Created según RFC 4918 §9.7.1
		return true

	case http.MethodGet, http.MethodHead:
		logger.Log("[WebDAV] 🛡️ Interceptado test de Duplicati (%s %s), simulando éxito en memoria.", method, cleanPath)

		content, modTime, etag, ok := store.Get(cleanPath)
		if !ok {
			// Si Duplicati hace GET/HEAD sin PUT previo, proporcionar contenido dummy coherente
			content = []byte("duplicati-access-test-ok\n")
			modTime = time.Now()
			etag = fmt.Sprintf("\"%x-%x\"", modTime.UnixNano(), len(content))
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)

		if method == http.MethodGet {
			_, _ = w.Write(content)
		}
		return true

	case http.MethodDelete:
		logger.Log("[WebDAV] 🛡️ Interceptado test de Duplicati (%s %s), simulando éxito en memoria.", method, cleanPath)

		store.Delete(cleanPath)
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}

		w.WriteHeader(http.StatusNoContent) // HTTP 204 No Content
		return true

	case "PROPFIND":
		logger.Log("[WebDAV] 🛡️ Interceptado test de Duplicati (%s %s), simulando éxito en memoria.", method, cleanPath)

		content, modTime, etag, ok := store.Get(cleanPath)
		if !ok {
			content = []byte("duplicati-access-test-ok\n")
			modTime = time.Now()
			etag = fmt.Sprintf("\"%x-%x\"", modTime.UnixNano(), len(content))
		}

		fileName := path.Base(cleanPath)
		lastMod := modTime.UTC().Format(http.TimeFormat)
		contentLen := len(content)

		xmlResp := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>%s</D:href>
    <D:propstat>
      <D:prop>
        <D:displayname>%s</D:displayname>
        <D:getcontentlength>%d</D:getcontentlength>
        <D:getlastmodified>%s</D:getlastmodified>
        <D:getetag>%s</D:getetag>
        <D:getcontenttype>application/octet-stream</D:getcontenttype>
        <D:resourcetype/>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`, cleanPath, fileName, contentLen, lastMod, etag)

		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(xmlResp)))
		w.WriteHeader(http.StatusMultiStatus) // HTTP 207 Multi-Status
		_, _ = io.WriteString(w, xmlResp)
		return true

	case "OPTIONS":
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, POST, PUT, DELETE, TRACE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK")
		w.Header().Set("DAV", "1, 2")
		w.Header().Set("MS-Author-Via", "DAV")
		w.WriteHeader(http.StatusOK)
		return true

	case "LOCK":
		logger.Log("[WebDAV] 🛡️ Interceptado test de Duplicati (%s %s), simulando éxito en memoria.", method, cleanPath)
		xmlLock := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:prop xmlns:D="DAV:">
  <D:lockdiscovery>
    <D:activelock>
      <D:locktype><D:write/></D:locktype>
      <D:lockscope><D:exclusive/></D:lockscope>
      <D:depth>0</D:depth>
      <D:timeout>Second-3600</D:timeout>
      <D:locktoken><D:href>urn:uuid:duplicati-probe-lock</D:href></D:locktoken>
      <D:lockroot><D:href>%s</D:href></D:lockroot>
    </D:activelock>
  </D:lockdiscovery>
</D:prop>`, cleanPath)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Lock-Token", "<urn:uuid:duplicati-probe-lock>")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, xmlLock)
		return true

	case "UNLOCK":
		logger.Log("[WebDAV] 🛡️ Interceptado test de Duplicati (%s %s), simulando éxito en memoria.", method, cleanPath)
		w.WriteHeader(http.StatusNoContent)
		return true

	default:
		return false
	}
}

