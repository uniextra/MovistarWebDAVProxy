package auth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// clientHelloIDFromEnv resuelve la huella TLS (ClientHelloID) que debe imitar el
// proxy. El cliente oficial de Windows enlaza OpenSSL 1.1 (libssl-1_1.dll) y negocia
// HTTP/2 contra CloudFront; la huella JA3/JA4 que emite crypto/tls de Go es
// catalogada como cliente automatizado por AWS WAF Bot Control (403). Por eso se
// imita un navegador convencional en lugar de usar crypto/tls. Configurable con
// UTLS_CLIENT_HELLO para pruebas A/B (chrome_auto, firefox_auto, safari_auto, ios,
// android, edge, chrome_120, golang, randomized).
func clientHelloIDFromEnv() utls.ClientHelloID {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UTLS_CLIENT_HELLO"))) {
	case "firefox", "firefox_auto":
		return utls.HelloFirefox_Auto
	case "safari", "safari_auto":
		return utls.HelloSafari_Auto
	case "ios":
		return utls.HelloIOS_Auto
	case "android":
		return utls.HelloAndroid_11_OkHttp
	case "edge":
		return utls.HelloEdge_Auto
	case "chrome_120":
		return utls.HelloChrome_120
	case "golang":
		return utls.HelloGolang
	case "randomized":
		return utls.HelloRandomized
	default:
		return utls.HelloChrome_Auto
	}
}

// FingerprintName devuelve la etiqueta de la huella TLS seleccionada (para logs).
func FingerprintName() string {
	id := clientHelloIDFromEnv()
	return id.Str()
}

// dialTLSContextWithUTLS realiza el dial TCP y envuelve la conexión en una UConn de
// uTLS para que el ClientHello imite al perfil elegido en lugar del de crypto/tls.
// Firma compatible con http2.Transport.DialTLSContext.
func dialTLSContextWithUTLS(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
	plainConn, err := (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = addr
	}

	helloID := clientHelloIDFromEnv()
	uConn := utls.UClient(plainConn, &utls.Config{
		ServerName: host,
		NextProtos: []string{"h2", "http/1.1"},
	}, helloID)

	if err := uConn.HandshakeContext(ctx); err != nil {
		_ = plainConn.Close()
		return nil, fmt.Errorf("utls handshake with %s: %w", host, err)
	}
	return uConn, nil
}

// fingerprintedTransport envía los hosts SAPI (lectura, borrado, metadatos) por
// HTTP/2 con uTLS, y deja las subidas (upload.*) por HTTP/1.1, que ya funcionaba.
// Mezclar un handshake h2 con un stack HTTP/1.x provoca exactamente el error
// "malformed HTTP response \x00\x00\x12\x04..." (frame SETTINGS de HTTP/2).
type fingerprintedTransport struct {
	h1 *http.Transport
	h2 *http2.Transport
}

func (t *fingerprintedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" && !strings.HasPrefix(req.URL.Hostname(), "upload.") {
		return t.h2.RoundTrip(req)
	}
	return t.h1.RoundTrip(req)
}

// HTTP1Transport expone el transporte HTTP/1.1 subyacente (subidas y URLs planas)
// para verificaciones de configuración en tests.
func (t *fingerprintedTransport) HTTP1Transport() *http.Transport {
	return t.h1
}

// NewFingerprintedTransport construye un RoundTripper cuyo ClientHello lo emite
// uTLS (imitando un navegador convencional). Los hosts SAPI van por HTTP/2 real;
// upload.* y las URLs planas (tests httptest) siguen por HTTP/1.1.
func NewFingerprintedTransport() http.RoundTripper {
	h2 := &http2.Transport{
		DialTLSContext: dialTLSContextWithUTLS,
		IdleConnTimeout: 90 * time.Second,
	}

	h1 := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
	}

	return &fingerprintedTransport{h1: h1, h2: h2}
}

