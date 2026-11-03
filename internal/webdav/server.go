package webdav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/webdav"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
)

var (
	emptyQuotaRegex = regexp.MustCompile(`(?i)<[dD]:quota-(available|used)-bytes(\s*/>|>\s*</[dD]:quota-(available|used)-bytes>)`)
	empty404Regex   = regexp.MustCompile(`(?i)<[dD]:propstat>\s*<[dD]:prop>\s*</[dD]:prop>\s*<[dD]:status>HTTP/1\.1 404 Not Found</[dD]:status>\s*</[dD]:propstat>`)
	re200Upper      = regexp.MustCompile(`(?i)</D:prop>\s*<D:status>HTTP/1\.1 200 OK</D:status>`)
	re200Lower      = regexp.MustCompile(`(?i)</d:prop>\s*<d:status>HTTP/1\.1 200 OK</d:status>`)
	reRespUpper     = regexp.MustCompile(`(?i)</D:response>`)
	reRespLower     = regexp.MustCompile(`(?i)</d:response>`)
)

type Server struct {
	handler     *webdav.Handler
	fs          *MovistarFS
	running     atomic.Bool
	authCfg     config.AuthConfig
	rateLimiter *config.AuthRateLimiter
	activeWG    sync.WaitGroup
}

func NewServer(client *sapi.Client) *Server {
	return NewServerWithAuth(client, config.LoadWebDAVAuthConfig())
}

func NewServerWithAuth(client *sapi.Client, authCfg config.AuthConfig) *Server {
	fs := NewMovistarFS(client)
	s := &Server{
		fs:          fs,
		authCfg:     authCfg,
		rateLimiter: config.NewAuthRateLimiter(10, time.Minute),
	}
	h := &webdav.Handler{
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, ErrUploadTooLarge) {
				logger.Log("[WebDAV] ❌ Error en %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}
	s.handler = h
	s.running.Store(true)
	return s
}

func (s *Server) InvalidateCache() {
	if s.fs != nil {
		s.fs.InvalidateCache(0)
	}
}

func (s *Server) IsRunning() bool {
	return s.running.Load()
}

func (s *Server) SetRunning(r bool) {
	s.running.Store(r)
}

// WaitActiveUploads espera a que finalicen las transferencias PUT activas antes de apagar.
func (s *Server) WaitActiveUploads(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.activeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.IsRunning() {
		http.Error(w, "WebDAV Server Paused", http.StatusServiceUnavailable)
		return
	}

	clientIP := config.ExtractClientIP(r)
	method := r.Method
	urlPath := r.URL.Path

	// Informar al cliente Windows que es un servidor WebDAV oficial.
	// Nota: Windows WebClient (Mini-Redirector) envía OPTIONS sin credenciales
	// durante el descubrimiento inicial de la unidad de red antes de enviar Basic Auth.
	if method == "OPTIONS" {
		w.Header().Set("MS-Author-Via", "DAV")
	}

	// 1. Validación Basic Auth y rate-limit por IP (salvo OPTIONS sin credenciales para discovery)
	if s.authCfg.IsEnabled() {
		rawUser, cleanUser, pass, hasAuth := config.ParseBasicAuthHeader(r.Header.Get("Authorization"))
		if method != "OPTIONS" || hasAuth {
			if s.rateLimiter.IsBlocked(clientIP) {
				logger.Log("[WebDAV] 🚫 IP bloqueada temporalmente por exceso de fallos de autenticación: %s", clientIP)
				http.Error(w, "Too Many Authentication Failures", http.StatusTooManyRequests)
				return
			}
			if !hasAuth || !s.authCfg.Validate(cleanUser, pass) {
				if hasAuth && (rawUser != "" || pass != "") {
					s.rateLimiter.RecordFailure(clientIP)
					logger.Log("[WebDAV] 🔒 Intento de acceso fallido desde %s (usuario: %s)", clientIP, logger.Mask(rawUser))
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			s.rateLimiter.Reset(clientIP)
		}
	}

	// Eliminar la cabecera Authorization tras validarla para que x/net/webdav la procese limpiamente
	r.Header.Del("Authorization")

	// Log de operaciones WebDAV
	if method == "OPTIONS" || method == "PUT" || method == "GET" || method == "DELETE" || method == "MOVE" ||
		method == "MKCOL" || method == "PROPFIND" || method == "HEAD" || method == "LOCK" || method == "UNLOCK" {
		logger.Log("[WebDAV] ⚡ %s -> %s %s", clientIP, method, urlPath)
	}

	// 2. Rechazar PUT a colecciones según RFC 4918 §9.7.1 y validar límite de subida
	if method == "PUT" {
		if strings.HasSuffix(urlPath, "/") {
			http.Error(w, "Cannot PUT to a collection", http.StatusMethodNotAllowed)
			return
		}
		if folder, _, isFolder, err := s.fs.resolvePath(urlPath); err == nil && isFolder && folder != nil {
			logger.Log("[WebDAV] ℹ️ Rechazado PUT sobre carpeta existente: '%s' (RFC 4918 405 Method Not Allowed)", urlPath)
			http.Error(w, "Cannot PUT to a collection", http.StatusMethodNotAllowed)
			return
		}
		maxBytes := GetMaxUploadBytes()
		if maxBytes > 0 && r.ContentLength > maxBytes {
			logger.Log("[WebDAV] ❌ Subida rechazada por superar límite (%d bytes > %d bytes): '%s'", r.ContentLength, maxBytes, urlPath)
			http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
			return
		}

		// Vigilar que el cuerpo llegue entero (ver uploadGuard en fs.go).
		guard := &uploadGuard{expected: r.ContentLength}
		if r.Body != nil {
			r.Body = &guardedBody{rc: r.Body, guard: guard}
		}
		r = r.WithContext(context.WithValue(r.Context(), uploadGuardKey{}, guard))

		var exceededFlag atomic.Bool
		if maxBytes > 0 && r.Body != nil {
			r.Body = &maxUploadReader{rc: r.Body, remaining: maxBytes, path: urlPath, exceededFlag: &exceededFlag}
		}
		s.activeWG.Add(1)
		defer s.activeWG.Done()

		// Conservar la fecha de modificación original si el cliente envía X-OC-Mtime
		r = withOCMtime(w, r)

		pw := &putStatusInterceptor{ResponseWriter: w, exceededFlag: &exceededFlag}
		s.handler.ServeHTTP(pw, r)
		return
	}

	// 4. Prevenir doble descarga en GET y sniffing en HEAD pre-configurando Content-Type
	if method == "GET" || method == "HEAD" {
		ext := filepath.Ext(urlPath)
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
	}

	// 5. Interceptar PROPFIND para inyectar cuotas RFC 4331 si las solicitan (Hyper Backup / Duplicati)
	if method == "PROPFIND" {
		rw := &responseInterceptor{
			ResponseWriter: w,
			body:           bytes.NewBuffer(nil),
			fs:             s.fs,
		}
		s.handler.ServeHTTP(rw, r)
		rw.flush()
		return
	}

	s.handler.ServeHTTP(w, r)
}

type maxUploadReader struct {
	rc           io.ReadCloser
	remaining    int64
	exceeded     bool
	path         string
	exceededFlag *atomic.Bool
}

func (m *maxUploadReader) Read(p []byte) (int, error) {
	if m.exceeded {
		return 0, ErrUploadTooLarge
	}
	n, err := m.rc.Read(p)
	if int64(n) > m.remaining {
		m.exceeded = true
		if m.exceededFlag != nil {
			m.exceededFlag.Store(true)
		}
		return n, nil
	}
	m.remaining -= int64(n)
	return n, err
}

func (m *maxUploadReader) Close() error {
	return m.rc.Close()
}

// putStatusInterceptor convierte el error de tamaño excedido durante streaming PUT en HTTP 413.
type putStatusInterceptor struct {
	http.ResponseWriter
	exceededFlag *atomic.Bool
	wroteHeader  bool
}

func (p *putStatusInterceptor) WriteHeader(statusCode int) {
	if p.exceededFlag != nil && p.exceededFlag.Load() && statusCode >= 400 {
		statusCode = http.StatusRequestEntityTooLarge
	}
	p.wroteHeader = true
	p.ResponseWriter.WriteHeader(statusCode)
}

type responseInterceptor struct {
	http.ResponseWriter
	body        *bytes.Buffer
	statusCode  int
	wroteHeader bool
	fs          *MovistarFS
}

func (r *responseInterceptor) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseInterceptor) Write(b []byte) (int, error) {
	return r.body.Write(b)
}

func (r *responseInterceptor) flush() {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}

	rawXML := r.body.String()

	// Si es una respuesta multistatus de WebDAV, inyectamos o corregimos cuotas RFC 4331
	if strings.Contains(rawXML, "<D:multistatus") || strings.Contains(rawXML, "<d:multistatus") {
		space := r.fs.GetStorageSpace()
		usedBytes := space.UsedBytes
		var availBytes int64 = 10 * 1024 * 1024 * 1024 * 1024 // 10 TB virtual para cuentas Unlimited
		if !space.NoLimit && space.FreeBytes > 0 {
			availBytes = space.FreeBytes
		} else if space.NoLimit {
			if availBytes > usedBytes {
				availBytes -= usedBytes
			}
		}

		prefix := "D"
		re200 := re200Upper
		reResp := reRespUpper
		if strings.Contains(rawXML, "<d:") {
			prefix = "d"
			re200 = re200Lower
			reResp = reRespLower
		}

		quotaXML := fmt.Sprintf("<%[1]s:quota-available-bytes>%[2]d</%[1]s:quota-available-bytes><%[1]s:quota-used-bytes>%[3]d</%[1]s:quota-used-bytes>", prefix, availBytes, usedBytes)

		// 1. Limpiar cualquier tag vacío de cuota
		rawXML = emptyQuotaRegex.ReplaceAllString(rawXML, "")

		// 2. Limpiar cualquier <D:propstat> 404 que haya quedado con <D:prop></D:prop> vacío
		rawXML = empty404Regex.ReplaceAllString(rawXML, "")

		// 3. Inyectar quotaXML
		has200OK := strings.Contains(strings.ToLower(rawXML), "http/1.1 200 ok")
		if has200OK {
			if re200.MatchString(rawXML) {
				rawXML = re200.ReplaceAllString(rawXML, quotaXML+"</"+prefix+":prop><"+prefix+":status>HTTP/1.1 200 OK</"+prefix+":status>")
			} else {
				// Fallback: reemplazar en el primer </D:prop>
				rawXML = strings.Replace(rawXML, "</"+prefix+":prop>", quotaXML+"</"+prefix+":prop>", 1)
			}
		} else {
			// Inyectar un bloque completo propstat 200 OK antes del primer </D:response>
			newPropstat := fmt.Sprintf("<%[1]s:propstat><%[1]s:prop>%[2]s</%[1]s:prop><%[1]s:status>HTTP/1.1 200 OK</%[1]s:status></%[1]s:propstat>", prefix, quotaXML)
			if loc := reResp.FindStringIndex(rawXML); loc != nil {
				rawXML = rawXML[:loc[0]] + newPropstat + rawXML[loc[0]:]
			}
		}
	}

	for k, v := range r.Header() {
		r.ResponseWriter.Header()[k] = v
	}
	r.ResponseWriter.Header().Set("Content-Length", fmt.Sprintf("%d", len(rawXML)))
	r.ResponseWriter.WriteHeader(r.statusCode)
	_, _ = io.WriteString(r.ResponseWriter, rawXML)
}

