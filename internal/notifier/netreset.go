package notifier

import (
	"crypto/tls"
	"net/http"
	"time"
)

// transport es el de todas las peticiones a Telegram: propio y solo HTTP/1.1. Con
// HTTP/2 todos los envíos comparten una única conexión; si esa conexión muere sin que
// el cliente lo note (corte de red, cambio de salida), los avisos y su reintento se
// mandaban por ella hasta agotar el plazo. Con HTTP/1.1, el reintento (tras cerrar las
// conexiones ociosas) abre una conexión nueva.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	// La copia hereda la configuración TLS que anuncia «h2»: sin quitarlo, el servidor
	// contesta en HTTP/2 y aquí se espera HTTP/1.1.
	if t.TLSClientConfig != nil {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
	} else {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return t
}()

func newClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport}
}
