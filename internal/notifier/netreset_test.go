package notifier

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Un envío que falla por la red se reintenta una vez (la conexión vieja queda atrás).
func TestSendRetriesOnceAfterNetworkError(t *testing.T) {
	var n int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close() // como una conexión muerta: sin respuesta
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.Start()
	defer srv.Close()
	oldBase, oldDelay := apiBase, sendRetryDelay
	apiBase, sendRetryDelay = srv.URL, time.Millisecond
	t.Cleanup(func() { apiBase, sendRetryDelay = oldBase, oldDelay })
	tn := &TelegramNotifier{botToken: "1:a", chatID: "42", client: newClient(5 * time.Second), lastAlerts: map[string]time.Time{}}
	if err := tn.SendAlert("", "🔌", "La Pi vuelve al cable", "x", ""); err != nil {
		t.Fatalf("debía enviarse al reintentar: %v", err)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("intentos = %d, quería 2", got)
	}
}

// Telegram va por HTTP/1.1: el reintento de un aviso no reutiliza una conexión HTTP/2 muerta.
func TestTransportIsHTTP1(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	// Solo se añade la CA del servidor de prueba: el resto, tal cual el de producción.
	tr := transport.Clone()
	tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Fatalf("protocolo = %s, quería HTTP/1.1", resp.Proto)
	}
}
