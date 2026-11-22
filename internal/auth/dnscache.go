package auth

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/config"
)

// Caché de DNS de las conexiones con la nube (SAPI_DNS_CACHE_SEC). Go no guarda las
// respuestas DNS: sin caché, cada conexión nueva pregunta otra vez por cloud.* o
// upload.*. Con caché, la dirección se reutiliza durante el plazo configurado; si una
// conexión a ella falla, se descarta y se pregunta de nuevo, así que un cambio de
// dirección del lado de O2 se nota como mucho al primer fallo.

type dnsEntry struct {
	addrs   []net.IPAddr
	expires time.Time
}

var dnsCache = struct {
	sync.Mutex
	m map[string]dnsEntry
}{m: map[string]dnsEntry{}}

// lookupHost pregunta al DNS (sustituible en los tests).
var lookupHost = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func dnsCacheGet(host string, now time.Time) ([]net.IPAddr, bool) {
	dnsCache.Lock()
	defer dnsCache.Unlock()
	e, ok := dnsCache.m[host]
	if !ok || now.After(e.expires) {
		return nil, false
	}
	return e.addrs, true
}

func dnsCachePut(host string, addrs []net.IPAddr, ttl time.Duration) {
	dnsCache.Lock()
	dnsCache.m[host] = dnsEntry{addrs: addrs, expires: time.Now().Add(ttl)}
	dnsCache.Unlock()
}

func dnsCacheDrop(host string) {
	dnsCache.Lock()
	delete(dnsCache.m, host)
	dnsCache.Unlock()
}

// dialContext abre la conexión TCP con la nube (con TCP_USER_TIMEOUT en Linux) y, con
// SAPI_DNS_CACHE_SEC, usa la caché de DNS.
func dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := newDialer()
	ttl := config.DNSCacheTTL()
	host, port, err := net.SplitHostPort(addr)
	if ttl <= 0 || err != nil || net.ParseIP(host) != nil {
		return d.DialContext(ctx, network, addr)
	}
	addrs, cached := dnsCacheGet(host, time.Now())
	if !cached {
		if addrs, err = lookupHost(ctx, host); err != nil {
			return nil, err
		}
		dnsCachePut(host, addrs, ttl)
	}
	conn, err := dialAddrs(ctx, d, network, addrs, port)
	if err == nil {
		return conn, nil
	}
	// La dirección guardada ya no vale (o ninguna respondió): fuera de la caché y, si
	// venía de ella, se pregunta otra vez.
	dnsCacheDrop(host)
	if !cached || ctx.Err() != nil {
		return nil, err
	}
	if addrs, err = lookupHost(ctx, host); err != nil {
		return nil, err
	}
	dnsCachePut(host, addrs, ttl)
	if conn, err = dialAddrs(ctx, d, network, addrs, port); err != nil {
		dnsCacheDrop(host)
	}
	return conn, err
}

// dialAddrs prueba las direcciones en orden, primero las IPv4 (dentro de Docker no
// suele haber IPv6).
func dialAddrs(ctx context.Context, d *net.Dialer, network string, addrs []net.IPAddr, port string) (net.Conn, error) {
	list := append([]net.IPAddr(nil), addrs...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].IP.To4() != nil && list[j].IP.To4() == nil })
	var lastErr error = &net.AddrError{Err: "sin direcciones", Addr: port}
	for _, a := range list {
		if network == "tcp4" && a.IP.To4() == nil || network == "tcp6" && a.IP.To4() != nil {
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}
