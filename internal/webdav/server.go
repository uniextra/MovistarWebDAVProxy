package webdav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
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
	lockSys     *resilientLockSystem
	running     atomic.Bool
	authCfg     config.AuthConfig
	rateLimiter *config.AuthRateLimiter
	ntlmAuth    *config.NTLMAuthenticator
	connIDs     map[net.Conn]string // conexión TCP -> id, para ligar la auth NTLM a la conexión real
	connMu      sync.Mutex
	activeWG    sync.WaitGroup
}

// connSeq genera un identificador único por cada conexión TCP aceptada.
var connSeq atomic.Uint64

type connIDKey struct{}

// WithConnID guarda el identificador de conexión en el contexto de la petición.
func WithConnID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, connIDKey{}, id)
}

func connIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(connIDKey{}).(string)
	return id
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
		connIDs:     make(map[net.Conn]string),
		ntlmAuth:    config.NewNTLMAuthenticator(),
	}
	lockSys := newResilientLockSystem()
	s.lockSys = lockSys
	h := &webdav.Handler{
		FileSystem: fs,
		LockSystem: lockSys,
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

func isWindowsClient(r *http.Request) bool {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	return strings.Contains(ua, "microsoft-webdav-miniredir") ||
		strings.Contains(ua, "davclnt") ||
		strings.Contains(ua, "windows") ||
		strings.Contains(ua, "winhttp")
}

func (s *Server) shouldOfferNTLM(r *http.Request) bool {
	if s.authCfg.Password == "" {
		return false
	}
	ntlmEnv := strings.ToLower(strings.TrimSpace(os.Getenv("WEBDAV_NTLM")))
	if ntlmEnv == "false" || ntlmEnv == "0" || ntlmEnv == "no" || ntlmEnv == "disabled" {
		return false
	}
	if ntlmEnv == "true" || ntlmEnv == "always" || ntlmEnv == "1" {
		return true
	}
	// "auto" (por defecto): solo a clientes Windows nativos o que ya envíen NTLM
	return isWindowsClient(r) || config.IsNTLMHeader(r.Header.Get("Authorization"))
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

// ConnContext asigna un identificador único a cada conexión TCP entrante y lo guarda
// en el contexto, de modo que la autenticación NTLM quede ligada a la conexión real y
// no a su par IP:puerto (reutilizable). Se pasa a http.Server.ConnContext.
func (s *Server) ConnContext(ctx context.Context, c net.Conn) context.Context {
	id := strconv.FormatUint(connSeq.Add(1), 10)
	s.connMu.Lock()
	s.connIDs[c] = id
	s.connMu.Unlock()
	return WithConnID(ctx, id)
}

// ConnState invalida la autenticación NTLM de una conexión cuando se cierra, para que
// su identidad no pueda heredarse. Se pasa a http.Server.ConnState.
func (s *Server) ConnState(c net.Conn, state http.ConnState) {
	if state != http.StateClosed && state != http.StateHijacked {
		return
	}
	s.connMu.Lock()
	id := s.connIDs[c]
	delete(s.connIDs, c)
	s.connMu.Unlock()
	if id != "" {
		s.ntlmAuth.InvalidateConn(id)
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

	// 1. Validación Basic Auth / NTLMv2 y rate-limit por IP (salvo OPTIONS sin credenciales para discovery)
	if s.authCfg.IsEnabled() {
		authHeader := r.Header.Get("Authorization")
		// Identidad de la conexión TCP real (no el par IP:puerto) y si la petición
		// llega a través de un proxy inverso de confianza. La autenticación NTLM es
		// por conexión; tras un proxy que agrupa conexiones upstream no puede confiarse.
		connKey := connIDFromContext(r.Context())
		viaTrustedProxy := config.RemoteIsTrustedProxy(r)
		if config.IsNTLMHeader(authHeader) {
			if s.rateLimiter.IsBlocked(clientIP) {
				logger.Log("[WebDAV] 🚫 IP bloqueada temporalmente por exceso de fallos de autenticación: %s", clientIP)
				http.Error(w, "Too Many Authentication Failures", http.StatusTooManyRequests)
				return
			}
			challenge, isType1, ntlmUser, ok := s.ntlmAuth.HandleHandshake(connKey, clientIP, authHeader, s.authCfg)
			if isType1 {
				if r.Body != nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64*1024))
				}
				w.Header().Set("WWW-Authenticate", challenge)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			if !ok {
				if r.Body != nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64*1024))
				}
				if ntlmUser != "" {
					s.rateLimiter.RecordFailure(clientIP)
					logger.Log("[WebDAV] 🔒 Intento de acceso NTLM fallido desde %s (usuario: %s)", clientIP, logger.Mask(ntlmUser))
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
				if s.shouldOfferNTLM(r) {
					w.Header().Add("WWW-Authenticate", "NTLM")
				}
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			s.rateLimiter.Reset(clientIP)
		} else {
			rawUser, cleanUser, pass, hasAuth := config.ParseBasicAuthHeader(authHeader)
			connAuthed := false
			if !hasAuth && !viaTrustedProxy && connKey != "" && s.ntlmAuth != nil {
				_, connAuthed = s.ntlmAuth.IsConnAuthenticated(connKey)
			}
			if !connAuthed && (method != "OPTIONS" || hasAuth) {
				if s.rateLimiter.IsBlocked(clientIP) {
					logger.Log("[WebDAV] 🚫 IP bloqueada temporalmente por exceso de fallos de autenticación: %s", clientIP)
					http.Error(w, "Too Many Authentication Failures", http.StatusTooManyRequests)
					return
				}
				if !hasAuth || !s.authCfg.Validate(cleanUser, pass) {
					if r.Body != nil {
						_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64*1024))
					}
					if hasAuth && (rawUser != "" || pass != "") {
						s.rateLimiter.RecordFailure(clientIP)
						logger.Log("[WebDAV] 🔒 Intento de acceso fallido desde %s (usuario: %s, %s)", clientIP, logger.Mask(rawUser), config.DescribeCredentialShape(rawUser, pass))
					}
					w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
					if s.shouldOfferNTLM(r) {
						w.Header().Add("WWW-Authenticate", "NTLM")
					}
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				s.rateLimiter.Reset(clientIP)
			}
		}
	}

	// Eliminar la cabecera Authorization tras validarla para que x/net/webdav la procese limpiamente
	r.Header.Del("Authorization")

	// Log de operaciones WebDAV
	if method == "OPTIONS" || method == "PUT" || method == "GET" || method == "DELETE" || method == "MOVE" ||
		method == "MKCOL" || method == "PROPFIND" || method == "HEAD" || method == "LOCK" || method == "UNLOCK" {
		logger.Log("[WebDAV] ⚡ %s -> %s %s", clientIP, method, urlPath)
	}

	// SAPI rechaza .tmp (issue #13). No simular persistencia en RAM, tampoco
	// para la sonda de conexión de Duplicati: el cliente debe ver el fallo.
	unsupportedTemp := method == http.MethodPut && isUnsupportedTempPath(urlPath)
	if method == "COPY" || method == "MOVE" {
		// x/net/webdav puede borrar el destino con Overwrite antes de llamar
		// a OpenFile/Rename: rechazar aquí, antes de cualquier efecto remoto.
		if dst, err := url.Parse(r.Header.Get("Destination")); err == nil && (dst.Host == "" || dst.Host == r.Host) {
			unsupportedTemp = isUnsupportedTempPath(dst.Path)
		}
	}
	if unsupportedTemp {
		http.Error(w, "SAPI does not support .tmp files; nothing was stored (including Duplicati connection probes)", http.StatusUnsupportedMediaType)
		return
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
		if r.Context().Err() != nil && s.lockSys != nil {
			s.lockSys.unlockPath(time.Now(), urlPath)
		}
		return
	}

	// 3. Manejo de DELETE: auto-liberar bloqueos huérfanos cuando se recibe un DELETE
	// incondicional (sin cabecera If o sin el token del bloqueo) sobre un recurso bloqueado.
	// Clientes como Windows WebClient envían un DELETE incondicional tras cancelaciones o
	// timeouts dejando el bloqueo colgado.
	if method == "DELETE" {
		if s.lockSys != nil && !s.lockSys.hasTokenInIf(urlPath, r.Header.Get("If")) {
			if s.lockSys.unlockPath(time.Now(), urlPath) {
				logger.Log("[WebDAV] 🔓 Liberado bloqueo huérfano antes de DELETE sobre '%s'", urlPath)
			}
		}
		s.handler.ServeHTTP(w, r)
		// RFC 4918 §7.4: eliminar un recurso debe eliminar todos sus bloqueos
		if s.lockSys != nil {
			s.lockSys.unlockPath(time.Now(), urlPath)
		}
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

		// Vigilar que el cuerpo llegue entero: si O2 responde 200/206 con la longitud
		// anunciada pero no entrega ni un byte (p. ej. un archivo recién subido que aún
		// no se sirve), convertir ese cuerpo vacío en un 5xx completo en vez de un
		// 200/206 truncado que el cliente tomaría por bueno. Un cuerpo corto (truncado
		// a mitad de la transmisión) se deja como está: el cliente ya lo detecta con
		// 'unexpected EOF', igual que hasta ahora.
		g := &getBodyGuard{ResponseWriter: w, isHEAD: method == "HEAD"}
		s.handler.ServeHTTP(g, r)
		g.finish()
		return
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

// getBodyGuard envuelve el ResponseWriter de un GET/HEAD y retrasa el envío de la
// cabecera hasta el primer byte del cuerpo. Así, si el handler anuncia 200/206 con
// longitud > 0 pero acaba sin escribir ni un byte (O2 devuelve el contenido vacío de
// un archivo que aún no sirve), se sustituye por un 5xx completo en vez de entregar un
// 200/206 truncado que el cliente tomaría por bueno. Un cuerpo corto (algún byte y
// luego EOF) no se toca: ya se detecta como 'unexpected EOF'. HEAD y las respuestas de
// longitud 0 legítimas (archivo vacío) tampoco se alteran.
type getBodyGuard struct {
	http.ResponseWriter
	isHEAD       bool
	code         int
	wroteHeader  bool
	flushed      bool
	bytesWritten int64
}

func (g *getBodyGuard) WriteHeader(code int) {
	if g.flushed {
		return
	}
	g.code = code
	g.wroteHeader = true
}

func (g *getBodyGuard) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !g.flushed {
		g.flushReal()
	}
	n, err := g.ResponseWriter.Write(p)
	g.bytesWritten += int64(n)
	return n, err
}

// flushReal envía al cliente la cabecera capturada tal cual.
func (g *getBodyGuard) flushReal() {
	if g.flushed {
		return
	}
	g.flushed = true
	code := g.code
	if !g.wroteHeader {
		code = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(code)
}

// finish se llama tras volver el handler. Si la cabecera aún no se envió y resulta ser
// un 200/206 con longitud anunciada > 0 del que no salió ni un byte, se convierte en
// 503 completo; en cualquier otro caso se envía la cabecera capturada sin cambios.
func (g *getBodyGuard) finish() {
	if g.flushed || !g.wroteHeader {
		return
	}
	if !g.isHEAD && (g.code == http.StatusOK || g.code == http.StatusPartialContent) {
		expected, _ := strconv.ParseInt(g.Header().Get("Content-Length"), 10, 64)
		if expected > 0 && g.bytesWritten == 0 {
			const msg = "upstream devolvió un cuerpo vacío para un archivo no vacío"
			g.flushed = true
			h := g.Header()
			h.Del("Content-Range")
			h.Del("Etag")
			h.Del("Last-Modified")
			h.Set("Content-Length", strconv.Itoa(len(msg)))
			g.ResponseWriter.WriteHeader(http.StatusServiceUnavailable)
			_, _ = g.ResponseWriter.Write([]byte(msg))
			return
		}
	}
	g.flushReal()
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

// resilientLockSystem envuelve webdav.LockSystem para liberar automáticamente bloqueos
// huérfanos cuando un cliente (como Windows WebClient tras un timeout o cancelación)
// envía un DELETE o PUT sin cabecera If sobre un recurso cuyo bloqueo quedó colgado.
type resilientLockSystem struct {
	inner  webdav.LockSystem
	mu     sync.Mutex
	byPath map[string]map[string]struct{}
	byTok  map[string]string
}

func newResilientLockSystem() *resilientLockSystem {
	return &resilientLockSystem{
		inner:  webdav.NewMemLS(),
		byPath: make(map[string]map[string]struct{}),
		byTok:  make(map[string]string),
	}
}

func cleanLockPath(p string) string {
	return path.Clean("/" + strings.TrimSpace(p))
}

func (l *resilientLockSystem) Create(now time.Time, details webdav.LockDetails) (string, error) {
	tok, err := l.inner.Create(now, details)
	if err != nil {
		return "", err
	}
	cp := cleanLockPath(details.Root)
	l.mu.Lock()
	if l.byPath[cp] == nil {
		l.byPath[cp] = make(map[string]struct{})
	}
	l.byPath[cp][tok] = struct{}{}
	l.byTok[tok] = cp
	l.mu.Unlock()
	return tok, nil
}

func (l *resilientLockSystem) Refresh(now time.Time, token string, duration time.Duration) (webdav.LockDetails, error) {
	return l.inner.Refresh(now, token, duration)
}

func (l *resilientLockSystem) Unlock(now time.Time, token string) error {
	l.mu.Lock()
	if cp, ok := l.byTok[token]; ok {
		delete(l.byTok, token)
		if set := l.byPath[cp]; set != nil {
			delete(set, token)
			if len(set) == 0 {
				delete(l.byPath, cp)
			}
		}
	}
	l.mu.Unlock()
	return l.inner.Unlock(now, token)
}

func (l *resilientLockSystem) hasTokenInIf(p, ifHdr string) bool {
	if ifHdr == "" {
		return false
	}
	cp := cleanLockPath(p)
	l.mu.Lock()
	defer l.mu.Unlock()

	for curr := cp; curr != "" && curr != "." && curr != "/"; curr = path.Dir(curr) {
		for tok := range l.byPath[curr] {
			if strings.Contains(ifHdr, tok) {
				return true
			}
		}
	}
	for tok := range l.byPath["/"] {
		if strings.Contains(ifHdr, tok) {
			return true
		}
	}
	return false
}

func (l *resilientLockSystem) unlockPath(now time.Time, p string) bool {
	if p == "" {
		return false
	}
	cp := cleanLockPath(p)
	prefix := cp
	if prefix != "/" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	l.mu.Lock()
	var pathsToClean []string
	var tokens []string
	for k, set := range l.byPath {
		if k == cp || (prefix != "/" && strings.HasPrefix(k, prefix)) {
			pathsToClean = append(pathsToClean, k)
			for tok := range set {
				tokens = append(tokens, tok)
				delete(l.byTok, tok)
			}
		}
	}
	for _, k := range pathsToClean {
		delete(l.byPath, k)
	}
	l.mu.Unlock()

	unlocked := false
	for _, tok := range tokens {
		if err := l.inner.Unlock(now, tok); err == nil {
			unlocked = true
		}
	}
	return unlocked
}

func (l *resilientLockSystem) Confirm(now time.Time, name0, name1 string, conditions ...webdav.Condition) (func(), error) {
	release, err := l.inner.Confirm(now, name0, name1, conditions...)
	if errors.Is(err, webdav.ErrLocked) && len(conditions) == 0 {
		u0 := l.unlockPath(now, name0)
		u1 := l.unlockPath(now, name1)
		if u0 || u1 {
			logger.Log("[WebDAV] 🔓 Liberado bloqueo huérfano sobre '%s'", name0)
			return l.inner.Confirm(now, name0, name1, conditions...)
		}
	}
	return release, err
}
