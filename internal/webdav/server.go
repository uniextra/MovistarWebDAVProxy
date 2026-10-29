package webdav

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	"golang.org/x/net/webdav"

	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
)

var (
	emptyQuotaRegex = regexp.MustCompile(`(?i)<[dD]:quota-(available|used)-bytes(\s*/>|>\s*</[dD]:quota-(available|used)-bytes>)`)
	empty404Regex   = regexp.MustCompile(`(?i)<[dD]:propstat>\s*<[dD]:prop>\s*</[dD]:prop>\s*<[dD]:status>HTTP/1\.1 404 Not Found</[dD]:status>\s*</[dD]:propstat>`)
)

type Server struct {
	handler *webdav.Handler
	fs      *MovistarFS
	running atomic.Bool
}

func NewServer(client *sapi.Client) *Server {
	fs := NewMovistarFS(client)
	h := &webdav.Handler{
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				logger.Log("[WebDAV] ❌ Error en %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}
	s := &Server{handler: h, fs: fs}
	s.running.Store(true)
	return s
}

func (s *Server) IsRunning() bool {
	return s.running.Load()
}

func (s *Server) SetRunning(r bool) {
	s.running.Store(r)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.IsRunning() {
		http.Error(w, "WebDAV Server Paused", http.StatusServiceUnavailable)
		return
	}

	clientIP := r.RemoteAddr
	if colon := strings.LastIndex(clientIP, ":"); colon != -1 {
		clientIP = clientIP[:colon]
	}

	method := r.Method
	urlPath := r.URL.Path

	// Log de operaciones WebDAV
	if method == "OPTIONS" || method == "PUT" || method == "GET" || method == "DELETE" || method == "MOVE" ||
		method == "MKCOL" || method == "PROPFIND" || method == "HEAD" || method == "LOCK" || method == "UNLOCK" {
		logger.Log("[WebDAV] ⚡ %s -> %s %s", clientIP, method, urlPath)
	}

	// 1. Aceptar cualquier Basic Auth eliminando la cabecera para que sea procesada
	r.Header.Del("Authorization")

	// 2. Prevenir doble descarga en GET y sniffing en HEAD pre-configurando Content-Type
	if method == "GET" || method == "HEAD" {
		ext := filepath.Ext(urlPath)
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
	}

	// 3. Interceptar PROPFIND para inyectar cuotas RFC 4331 si las solicitan (Hyper Backup / Duplicati)
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
		if strings.Contains(rawXML, "<d:") {
			prefix = "d"
		}

		quotaXML := fmt.Sprintf("<%[1]s:quota-available-bytes>%[2]d</%[1]s:quota-available-bytes><%[1]s:quota-used-bytes>%[3]d</%[1]s:quota-used-bytes>", prefix, availBytes, usedBytes)

		// 1. Limpiar cualquier tag vacío de cuota
		rawXML = emptyQuotaRegex.ReplaceAllString(rawXML, "")

		// 2. Limpiar cualquier <D:propstat> 404 que haya quedado con <D:prop></D:prop> vacío
		rawXML = empty404Regex.ReplaceAllString(rawXML, "")

		// 3. Inyectar quotaXML
		has200OK := strings.Contains(strings.ToLower(rawXML), "http/1.1 200 ok")
		if has200OK {
			re200 := regexp.MustCompile(fmt.Sprintf(`(?i)</%[1]s:prop>\s*<%[1]s:status>HTTP/1\.1 200 OK</%[1]s:status>`, prefix))
			if re200.MatchString(rawXML) {
				rawXML = re200.ReplaceAllString(rawXML, quotaXML+"</"+prefix+":prop><"+prefix+":status>HTTP/1.1 200 OK</"+prefix+":status>")
			} else {
				// Fallback: reemplazar en el primer </D:prop>
				rawXML = strings.Replace(rawXML, "</"+prefix+":prop>", quotaXML+"</"+prefix+":prop>", 1)
			}
		} else {
			// Inyectar un bloque completo propstat 200 OK antes del primer </D:response>
			newPropstat := fmt.Sprintf("<%[1]s:propstat><%[1]s:prop>%[2]s</%[1]s:prop><%[1]s:status>HTTP/1.1 200 OK</%[1]s:status></%[1]s:propstat>", prefix, quotaXML)
			reResp := regexp.MustCompile(fmt.Sprintf(`(?i)</%s:response>`, prefix))
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
