package webclient

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Transport es un http.RoundTripper que reparte las peticiones del cliente SAPI
// igual que lo hace la web:
//
//   - llamadas a la API (micloud…/sapi/*): desde la pestaña de Chromium con la
//     sesión de la web, para que el WAF las vea como tráfico normal de la web;
//   - descargas (/sapi/download/*): directas, la URL va firmada y no necesita sesión;
//   - subidas (upload.*): directas, con las cookies y la validationKey de la sesión.
type Transport struct {
	Session *Session
	Base    http.RoundTripper // transporte para las peticiones directas
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	portal, _ := url.Parse(PortalURL)
	switch {
	case strings.HasPrefix(req.URL.Host, "upload."):
		return t.upload(req)
	case req.URL.Host == portal.Host && strings.HasPrefix(req.URL.Path, "/sapi/download/"):
		return t.direct(req)
	case req.URL.Host == portal.Host && strings.HasPrefix(req.URL.Path, "/sapi/"):
		return t.viaBrowser(req)
	default:
		return t.direct(req)
	}
}

// direct envía la petición tal cual, sin credenciales del cliente de escritorio.
func (t *Transport) direct(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	r.Header.Del("X-deviceid")
	r.Header.Del("X-devicename")
	r.Header.Set("User-Agent", t.Session.UserAgent())
	return t.base().RoundTrip(r)
}

// upload envía la subida directa a upload.* con la sesión de la web.
func (t *Transport) upload(req *http.Request) (*http.Response, error) {
	cookie, vk, err := t.Session.Credentials(req.URL.Scheme + "://" + req.URL.Host)
	if err != nil {
		return nil, fmt.Errorf("credenciales de la sesión web: %w", err)
	}
	r := req.Clone(req.Context())
	q := r.URL.Query()
	q.Del("validationkey")
	if vk != "" {
		q.Set("validationkey", vk)
	}
	r.URL.RawQuery = q.Encode()
	r.Header.Del("Authorization")
	r.Header.Del("X-devicename")
	r.Header.Set("Cookie", cookie)
	r.Header.Set("X-deviceid", "web-"+t.Session.DeviceID())
	r.Header.Set("User-Agent", t.Session.UserAgent())
	r.Header.Set("Origin", PortalURL)
	r.Header.Set("Referer", PortalURL+"/")
	resp, err := t.base().RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.Session.setLoggedIn(false)
	}
	return resp, err
}

// viaBrowser ejecuta la llamada a la API con fetch() dentro de la web.
func (t *Transport) viaBrowser(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	// La validationkey la pone la pestaña con la de su sesión.
	q := req.URL.Query()
	q.Del("validationkey")
	path := req.URL.Path
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}

	r, err := t.Session.Do(req.Context(), req.Method, path, body, req.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("SAPI vía sesión web (%s %s): %w", req.Method, req.URL.Path, err)
	}
	h := make(http.Header)
	if r.ContentType != "" {
		h.Set("Content-Type", r.ContentType)
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", r.Status, http.StatusText(r.Status)),
		StatusCode:    r.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(strings.NewReader(r.Body)),
		ContentLength: int64(len(r.Body)),
		Request:       req,
	}, nil
}

// NewDirectTransport crea el transporte para las descargas y subidas directas.
func NewDirectTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	t.MaxIdleConnsPerHost = 20
	t.ResponseHeaderTimeout = 60 * time.Second
	return t
}
