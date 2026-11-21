package auth

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ChaCha20 va delante, tras el GREASE; sin los AES de TLS 1.3; el resto en su orden.
func TestChachaFirst(t *testing.T) {
	in := []uint16{0x1a1a, utls.TLS_AES_128_GCM_SHA256, utls.TLS_AES_256_GCM_SHA384, utls.TLS_CHACHA20_POLY1305_SHA256,
		utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305}
	want := []uint16{0x1a1a, utls.TLS_CHACHA20_POLY1305_SHA256, utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}
	got := chachaFirst(in)
	if len(got) != len(want) {
		t.Fatalf("%x", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orden %x, esperado %x", got, want)
		}
	}
}

// Prueba real contra Movistar (MOVISTAR_TLS_LIVE=1): con ChaCha delante se negocia
// ChaCha20 con la huella del modo web.
func TestChachaLiveMovistar(t *testing.T) {
	if os.Getenv("MOVISTAR_TLS_LIVE") == "" {
		t.Skip("MOVISTAR_TLS_LIVE no definido")
	}
	t.Setenv("TLS_PREFER_CHACHA", "true")
	t.Setenv("UTLS_CLIENT_HELLO", "chrome") // la huella del modo web (en la Pi: «Chrome-133»)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := dialTLSContextWithUTLSHTTP1(ctx, "tcp", "micloud.movistar.es:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := c.(*utls.UConn).ConnectionState()
	t.Logf("cifrado negociado: %s", utls.CipherSuiteName(st.CipherSuite))
	if st.CipherSuite != utls.TLS_CHACHA20_POLY1305_SHA256 {
		t.Fatalf("esperado ChaCha20, negociado %s", utls.CipherSuiteName(st.CipherSuite))
	}
}

// Las subidas (upload.*) también con ChaCha20 y HTTP/1.1 (MOVISTAR_TLS_LIVE=1): sin
// sesión el servidor debe responder como siempre, no con un 403 del WAF.
func TestChachaLiveMovistarUpload(t *testing.T) {
	if os.Getenv("MOVISTAR_TLS_LIVE") == "" {
		t.Skip("MOVISTAR_TLS_LIVE no definido")
	}
	t.Setenv("TLS_PREFER_CHACHA", "true")
	t.Setenv("UTLS_CLIENT_HELLO", "chrome")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := dialTLSContextHTTP1Only(ctx, "tcp", "upload.micloud.movistar.es:443")
	if err != nil {
		t.Fatal(err)
	}
	st := c.(*utls.UConn).ConnectionState()
	_ = c.Close()
	t.Logf("cifrado negociado: %s, ALPN %q", utls.CipherSuiteName(st.CipherSuite), st.NegotiatedProtocol)
	if st.CipherSuite != utls.TLS_CHACHA20_POLY1305_SHA256 {
		t.Fatalf("esperado ChaCha20, negociado %s", utls.CipherSuiteName(st.CipherSuite))
	}

	tr := NewFingerprintedTransport().(interface{ HTTP1Transport() *http.Transport }).HTTP1Transport()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://upload.micloud.movistar.es/sapi/upload?action=save", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	t.Logf("respuesta sin sesión: %d", resp.StatusCode)
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("403: el WAF rechaza la huella")
	}
}

// En el perfil desktop la huella del cliente oficial queda intacta salvo que se pida.
func TestPreferChaCha_DesktopOnlyExplicit(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("TLS_PREFER_CHACHA", "")
	if preferChaCha() {
		t.Fatal("desktop sin TLS_PREFER_CHACHA: no debe tocar la huella")
	}
	t.Setenv("TLS_PREFER_CHACHA", "true")
	if !preferChaCha() {
		t.Fatal("desktop con TLS_PREFER_CHACHA=true: debe aplicarlo")
	}
	t.Setenv("SAPI_CLIENT_PROFILE", "browser")
	t.Setenv("TLS_PREFER_CHACHA", "")
	if preferChaCha() != !hasAESHardware() {
		t.Fatal("browser: por defecto según el procesador")
	}
}
