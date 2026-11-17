package auth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
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
// UTLS_CLIENT_HELLO para pruebas A/B (o2, chrome_auto, firefox_auto, safari_auto, ios,
// android, edge, chrome_120, golang, randomized). En el perfil desktop la huella por
// defecto es la del cliente oficial de Windows (HelloO2Windows).
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
	case "chrome", "chrome_auto":
		return utls.HelloChrome_Auto
	case "chrome_120":
		return utls.HelloChrome_120
	case "golang":
		return utls.HelloGolang
	case "randomized":
		return utls.HelloRandomized
	case "o2", "o2_windows", "openssl":
		return HelloO2Windows
	default:
		if IsDesktopProfile() {
			// Perfil desktop: el ClientHello del cliente oficial (OpenSSL 1.1.1, sin
			// ALPN). Con la huella de Chrome y el User-Agent "omh windows client"
			// CloudFront acaba devolviendo 401 vacíos; HelloGolang (UTLS_CLIENT_HELLO=golang)
			// también pasa y queda como alternativa.
			return HelloO2Windows
		}
		return utls.HelloChrome_Auto
	}
}

// HelloO2Windows es el ClientHello del cliente oficial de Windows de O2
// (O2 Cloud.exe 32.1.3, Qt 5 + OpenSSL 1.1.1), capturado en claro con tcpdump
// (sin MITM) el 17/11/2026: JA3 57fbe0aefee44901190849b0e877a5e1, sin ALPN
// (el servidor responde en HTTP/1.1). Es la huella por defecto del perfil desktop.
var HelloO2Windows = utls.ClientHelloID{Client: "O2Windows", Version: "32.1.3"}

// o2WindowsSpec reproduce ese ClientHello extensión a extensión y en el mismo orden.
func o2WindowsSpec() utls.ClientHelloSpec {
	return utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS10,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			0x1302, 0x1303, 0x1301, 0xc02c, 0xc030, 0x009f, 0xcca9, 0xcca8, 0xccaa, 0xc02b,
			0xc02f, 0x009e, 0xc024, 0xc028, 0x006b, 0xc023, 0xc027, 0x0067, 0xc00a, 0xc014,
			0x0039, 0xc009, 0xc013, 0x0033, 0x00ad, 0x00ab, 0xccae, 0xccad, 0xccac, 0x009d,
			0x00a9, 0xccab, 0x00ac, 0x00aa, 0x009c, 0x00a8, 0x003d, 0x003c, 0xc038, 0xc036,
			0x00b7, 0x00b3, 0x0095, 0x0091, 0x0035, 0x00af, 0x008d, 0xc037, 0xc035, 0x00b6,
			0x00b2, 0x0094, 0x0090, 0x002f, 0x00ae, 0x008c, 0x00ff,
		},
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0, 1, 2}},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519, utls.CurveP256, utls.CurveID(0x001e), utls.CurveP521, utls.CurveP384,
			}},
			&utls.SessionTicketExtension{},
			&utls.GenericExtension{Id: 22}, // encrypt_then_mac
			&utls.ExtendedMasterSecretExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0503, 0x0603, 0x0807, 0x0808, 0x0809, 0x080a, 0x080b, 0x0804, 0x0805,
				0x0806, 0x0401, 0x0501, 0x0601, 0x0303, 0x0203, 0x0301, 0x0201, 0x0302, 0x0202,
				0x0402, 0x0502, 0x0602,
			}},
			&utls.SupportedVersionsExtension{Versions: []uint16{
				utls.VersionTLS13, utls.VersionTLS12, utls.VersionTLS11, utls.VersionTLS10,
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}

// helloSpecFor devuelve la especificación a medida de las huellas propias
// (HelloO2Windows); ok es false para los perfiles de uTLS.
func helloSpecFor(helloID utls.ClientHelloID) (utls.ClientHelloSpec, bool) {
	if helloID == HelloO2Windows {
		return o2WindowsSpec(), true
	}
	return utls.ClientHelloSpec{}, false
}

// newUClient crea la UConn para helloID, aplicando la especificación a medida si
// la huella es propia.
func newUClient(conn net.Conn, cfg *utls.Config, helloID utls.ClientHelloID) (*utls.UConn, error) {
	spec, custom := helloSpecFor(helloID)
	if !custom {
		return utls.UClient(conn, cfg, helloID), nil
	}
	uConn := utls.UClient(conn, cfg, utls.HelloCustom)
	if err := uConn.ApplyPreset(&spec); err != nil {
		return nil, fmt.Errorf("utls preset %s: %w", helloID.Str(), err)
	}
	return uConn, nil
}

// FingerprintName devuelve la etiqueta de la huella TLS seleccionada (para logs).
func FingerprintName() string {
	id := clientHelloIDFromEnv()
	return id.Str()
}

// dialTLSContextWithUTLS realiza el dial TCP y envuelve la conexión en una UConn de
// uTLS para que el ClientHello imite al perfil elegido en lugar del de crypto/tls.
// Firma compatible con http2.Transport.DialTLSContext.
func dialTLSContextWithUTLSALPN(ctx context.Context, network, addr string, nextProtos []string) (net.Conn, error) {
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
	uConn, err := newUClient(plainConn, &utls.Config{
		ServerName: host,
		NextProtos: nextProtos,
	}, helloID)
	if err != nil {
		_ = plainConn.Close()
		return nil, err
	}

	if err := uConn.HandshakeContext(ctx); err != nil {
		_ = plainConn.Close()
		return nil, fmt.Errorf("utls handshake with %s: %w", host, err)
	}
	return uConn, nil
}

func dialTLSContextWithUTLS(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
	return dialTLSContextWithUTLSALPN(ctx, network, addr, []string{"h2", "http/1.1"})
}

func dialTLSContextWithUTLSHTTP1(ctx context.Context, network, addr string) (net.Conn, error) {
	return dialTLSContextWithUTLSALPN(ctx, network, addr, []string{"http/1.1"})
}

// dialTLSContextHTTP1Only es como dialTLSContextWithUTLSHTTP1 pero garantiza que el
// ClientHello anuncie solo "http/1.1". Con los perfiles de navegador de uTLS
// (Chrome, Firefox, Edge…) la extensión ALPN del preset ("h2", "http/1.1") manda
// sobre Config.NextProtos, y CloudFront elige h2 aunque el transporte sea
// HTTP/1.1 ("malformed HTTP response \x00\x00\x12\x04…"). Aquí se toma el
// preset, se cambia su ALPN y se aplica como ClientHello a medida.
// http1OnlySpec devuelve el ClientHello del perfil indicado con la extensión ALPN
// reducida a "http/1.1".
func http1OnlySpec(helloID utls.ClientHelloID) (utls.ClientHelloSpec, error) {
	if spec, ok := helloSpecFor(helloID); ok {
		// Sin ALPN, como el cliente oficial: el servidor responde en HTTP/1.1.
		return spec, nil
	}
	spec, err := utls.UTLSIdToSpec(helloID)
	if err != nil {
		return spec, err
	}
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
	return spec, nil
}

func dialTLSContextHTTP1Only(ctx context.Context, network, addr string) (net.Conn, error) {
	helloID := clientHelloIDFromEnv()
	spec, err := http1OnlySpec(helloID)
	if err != nil {
		// Perfiles sin especificación fija (p. ej. HelloGolang): NextProtos basta.
		return dialTLSContextWithUTLSALPN(ctx, network, addr, []string{"http/1.1"})
	}

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
	uConn := utls.UClient(plainConn, &utls.Config{ServerName: host, NextProtos: []string{"http/1.1"}}, utls.HelloCustom)
	if err := uConn.ApplyPreset(&spec); err != nil {
		_ = plainConn.Close()
		return nil, fmt.Errorf("utls preset %s: %w", helloID.Str(), err)
	}
	if err := uConn.HandshakeContext(ctx); err != nil {
		_ = plainConn.Close()
		return nil, fmt.Errorf("utls handshake with %s: %w", host, err)
	}
	if proto := uConn.ConnectionState().NegotiatedProtocol; proto != "" && proto != "http/1.1" {
		_ = uConn.Close()
		return nil, fmt.Errorf("utls handshake with %s: el servidor eligió %q en lugar de http/1.1", host, proto)
	}
	return uConn, nil
}

func dialTLSContextWithUTLSNoALPN(ctx context.Context, network, addr string) (net.Conn, error) {
	return dialTLSContextWithUTLSALPN(ctx, network, addr, nil)
}

// UploadResponseTimeout es lo que se espera la respuesta del servidor de subidas una vez
// enviado el archivo. Con archivos de cientos de MB, O2 tarda a veces más de 60 s en
// responder tras recibirlos (15/11/2026: 189 MB, dos intentos cortados por "timeout
// awaiting response headers"; otro de 153 MB respondió a los 87 s). Por defecto 5 min,
// configurable con UPLOAD_RESPONSE_TIMEOUT_SEC.
func UploadResponseTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("UPLOAD_RESPONSE_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Minute
}

// fingerprintedTransport envía los hosts SAPI (lectura, borrado, metadatos) por
// HTTP/2 con uTLS, y deja las subidas (upload.*) por HTTP/1.1, que ya funcionaba.
// Mezclar un handshake h2 con un stack HTTP/1.x provoca exactamente el error
// "malformed HTTP response \x00\x00\x12\x04..." (frame SETTINGS de HTTP/2).
type fingerprintedTransport struct {
	h1 *http.Transport
	h2 *http2.Transport
	// sapiH1, si no es nil, sustituye a h2 para los hosts SAPI: el perfil
	// "desktop" habla HTTP/1.1, como el cliente oficial (Qt 5). CloudFront corta
	// las peticiones de un dispositivo "fol-" que llegan por HTTP/2.
	sapiH1 *http.Transport
}

func (t *fingerprintedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" && !strings.HasPrefix(req.URL.Hostname(), "upload.") {
		if t.sapiH1 != nil {
			return t.sapiH1.RoundTrip(req)
		}
		return t.h2.RoundTrip(req)
	}
	return t.h1.RoundTrip(req)
}

// UsesHTTP1ForSAPI indica si los hosts SAPI van por HTTP/1.1 (perfil "desktop").
func (t *fingerprintedTransport) UsesHTTP1ForSAPI() bool {
	return t.sapiH1 != nil
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
		DialTLSContext:  dialTLSContextWithUTLS,
		IdleConnTimeout: 90 * time.Second,
	}

	h1 := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// El host de subida devuelve HTTP 408 ("Server timeout waiting for the
		// HTTP request from the client") si se reutiliza una conexión keep-alive
		// que el servidor ya consideraba caducada. Forzamos conexión nueva por
		// subida; el coste es despreciable frente a los ficheros de HyperBackup.
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: UploadResponseTimeout(),
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
	}

	ft := &fingerprintedTransport{h1: h1, h2: h2}
	if IsDesktopProfile() {
		ft.sapiH1 = newSAPIHTTP1Transport()
	}
	return ft
}

// newSAPIHTTP1Transport es el transporte de los hosts SAPI en el perfil "desktop":
// HTTP/1.1 con keep-alive (como el cliente oficial) y ALPN "http/1.1".
func newSAPIHTTP1Transport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DialTLSContext:        dialTLSContextHTTP1Only,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
	}
}

// NewHTTP1FingerprintedTransport mantiene la huella uTLS pero fuerza ALPN
// HTTP/1.1. Se usa para comparar el comportamiento del cliente oficial cuando
// CloudFront trata el login de forma distinta a los endpoints SAPI normales.
func NewHTTP1FingerprintedTransport() http.RoundTripper {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DialTLSContext:        dialTLSContextWithUTLSHTTP1,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
	}
}

// NewNoALPNHTTP1Transport fuerza HTTP/1.1 sin anunciar ALPN. Algunos perfiles
// de CloudFront tratan de forma distinta el ClientHello cuando ALPN está vacío.
func NewNoALPNHTTP1Transport() http.RoundTripper {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DialTLSContext:        dialTLSContextWithUTLSNoALPN,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
	}
}
