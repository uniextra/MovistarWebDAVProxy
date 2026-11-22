package auth

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func desktopTransports(t *testing.T) (h1, sapi *http.Transport) {
	t.Helper()
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	ft, ok := NewFingerprintedTransport().(*fingerprintedTransport)
	if !ok || ft.sapiH1 == nil {
		t.Fatalf("transporte del perfil desktop inesperado: %T", ft)
	}
	return ft.h1, ft.sapiH1
}

// Como la app: una conexión nueva por petición con cloud.* y upload.*, sin añadir
// «Connection: close» (DisableKeepAlives), que la app no envía.
func TestDesktopConnectionsOnePerRequestLikeTheApp(t *testing.T) {
	h1, sapi := desktopTransports(t)
	for name, tr := range map[string]*http.Transport{"upload": h1, "cloud": sapi} {
		if tr.DisableKeepAlives {
			t.Errorf("%s: DisableKeepAlives añadiría «Connection: close»", name)
		}
		if tr.MaxIdleConnsPerHost >= 0 {
			t.Errorf("%s: MaxIdleConnsPerHost = %d, debe ser negativo (sin conexiones ociosas)", name, tr.MaxIdleConnsPerHost)
		}
	}
}

func TestDesktopConnectionsKeepaliveOption(t *testing.T) {
	t.Setenv("SAPI_KEEPALIVE", "true")
	t.Setenv("SAPI_KEEPALIVE_IDLE_SEC", "3")
	h1, sapi := desktopTransports(t)
	for name, tr := range map[string]*http.Transport{"upload": h1, "cloud": sapi} {
		if tr.DisableKeepAlives || tr.MaxIdleConnsPerHost <= 0 {
			t.Errorf("%s: con SAPI_KEEPALIVE debe reutilizar (DisableKeepAlives=%v, MaxIdleConnsPerHost=%d)", name, tr.DisableKeepAlives, tr.MaxIdleConnsPerHost)
		}
		if tr.IdleConnTimeout != 3*time.Second {
			t.Errorf("%s: IdleConnTimeout = %v, quería 3s", name, tr.IdleConnTimeout)
		}
	}
}

func stubLookup(t *testing.T, fn func(host string) []net.IPAddr) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := lookupHost
	lookupHost = func(_ context.Context, host string) ([]net.IPAddr, error) {
		n.Add(1)
		return fn(host), nil
	}
	t.Cleanup(func() {
		lookupHost = prev
		dnsCache.Lock()
		dnsCache.m = map[string]dnsEntry{}
		dnsCache.Unlock()
	})
	return &n
}

func listen(t *testing.T, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("no se puede escuchar en %s: %v", addr, err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return l
}

func TestDNSCacheOffAsksEveryTime(t *testing.T) {
	t.Setenv("SAPI_DNS_CACHE_SEC", "0")
	l := listen(t, "127.0.0.1:0")
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	n := stubLookup(t, func(string) []net.IPAddr { return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}} })
	// Con SAPI_DNS_CACHE_SEC=0 no se usa la caché (ni el lookup sustituible).
	for i := 0; i < 2; i++ {
		c, err := dialContext(context.Background(), "tcp", net.JoinHostPort("localhost", port))
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}
	if n.Load() != 0 {
		t.Fatalf("sin caché no debería pasar por ella (%d consultas)", n.Load())
	}
}

func TestDNSCacheReusesAddress(t *testing.T) {
	t.Setenv("SAPI_DNS_CACHE_SEC", "60")
	l := listen(t, "127.0.0.1:0")
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	n := stubLookup(t, func(string) []net.IPAddr { return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}} })
	for i := 0; i < 3; i++ {
		c, err := dialContext(context.Background(), "tcp", net.JoinHostPort("upload.cloud.o2online.es", port))
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}
	if n.Load() != 1 {
		t.Fatalf("3 conexiones deberían hacer 1 consulta DNS, hicieron %d", n.Load())
	}
}

// Si O2 cambia de dirección, la guardada falla: se descarta y se pregunta de nuevo.
func TestDNSCacheDropsStaleAddress(t *testing.T) {
	t.Setenv("SAPI_DNS_CACHE_SEC", "60")
	l1 := listen(t, "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(l1.Addr().String())
	var nueva atomic.Bool
	n := stubLookup(t, func(string) []net.IPAddr {
		if nueva.Load() {
			return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 2)}}
		}
		return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}
	})
	addr := net.JoinHostPort("cloud.o2online.es", port)
	c, err := dialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	_ = l1.Close() // la dirección vieja deja de responder
	l2 := listen(t, net.JoinHostPort("127.0.0.2", port))
	defer l2.Close()
	nueva.Store(true)

	c, err = dialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("debería haber vuelto a preguntar y conectado a la nueva: %v", err)
	}
	if got := c.RemoteAddr().(*net.TCPAddr).IP.String(); got != "127.0.0.2" {
		t.Fatalf("conectó a %s", got)
	}
	_ = c.Close()
	if n.Load() != 2 {
		t.Fatalf("consultas DNS = %d, quería 2", n.Load())
	}
}
