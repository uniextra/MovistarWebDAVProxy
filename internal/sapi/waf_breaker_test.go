package sapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
)

const cloudFrontBlockPage = `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN" "http://www.w3.org/TR/html4/loose.dtd">
<HTML><HEAD><META HTTP-EQUIV="Content-Type" CONTENT="text/html; charset=iso-8859-1">
<TITLE>ERROR: The request could not be satisfied</TITLE>
</HEAD><BODY><H1>403 ERROR</H1></BODY></HTML>`

// servidor que responde lo que diga modo: "waf" (403 de CloudFront), "sapi403" (403 JSON
// propio de SAPI) u "ok".
func wafTestServer(t *testing.T, modo *atomic.Value, hits *int32) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		switch modo.Load().(string) {
		case "waf":
			w.Header().Set("Server", "CloudFront")
			w.Header().Set("X-Cache", "Error from cloudfront")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, cloudFrontBlockPage)
		case "sapi403":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"COM-1003","description":"forbidden"}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(config.AccountConfig{RefreshToken: "r", ValidationKey: "vk"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURLs(srv.URL+"/sapi", srv.URL)
	return c
}

func get(t *testing.T, c *Client) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", c.baseURL+"/media?action=get", nil)
	resp, err := c.DoRequest(req)
	if resp != nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(string(b)))
	}
	return resp, err
}

// Tras el primer 403 de CloudFront no se envía ninguna petición más a O2 hasta que pasa
// la pausa; la siguiente hace de sonda y, si va bien, todo vuelve a la normalidad.
func TestWAFBreaker_PausaTrasElPrimer403YSondaAlAcabar(t *testing.T) {
	var modo atomic.Value
	var hits int32
	modo.Store("waf")
	c := wafTestServer(t, &modo, &hits)
	now := time.Now()
	c.waf.now = func() time.Time { return now }

	resp, err := get(t, c)
	if err != nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("la primera petición debe llegar a O2 y devolver su 403: resp=%v err=%v", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), "could not be satisfied") {
		t.Errorf("el cuerpo del 403 debe seguir legible para el llamante, leído: %q", body)
	}
	if c.WAFBlockedUntil().IsZero() {
		t.Fatal("tras el 403 del WAF el cliente debe quedar en pausa")
	}

	for i := 0; i < 20; i++ {
		if _, err := get(t, c); !errors.Is(err, ErrWAFBlocked) {
			t.Fatalf("petición %d durante la pausa: esperado ErrWAFBlocked, obtenido %v", i, err)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("durante la pausa no debe salir ninguna petición a O2; salieron %d en total", n)
	}

	// Pasada la pausa, O2 ya responde: la sonda pasa y se vuelve a la normalidad.
	modo.Store("ok")
	now = now.Add(wafCooldownBase + time.Second)
	if resp, err := get(t, c); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("tras la pausa la sonda debe salir y funcionar: resp=%v err=%v", resp, err)
	}
	if !c.WAFBlockedUntil().IsZero() || c.waf.strikes != 0 {
		t.Errorf("tras una respuesta normal no debe quedar pausa ni bloqueos acumulados (strikes=%d)", c.waf.strikes)
	}
}

// Si al acabar la pausa O2 sigue bloqueando, la siguiente pausa es el doble (con tope).
func TestWAFBreaker_PausaCreceSiSigueBloqueado(t *testing.T) {
	var modo atomic.Value
	var hits int32
	modo.Store("waf")
	c := wafTestServer(t, &modo, &hits)
	now := time.Now()
	c.waf.now = func() time.Time { return now }

	want := wafCooldownBase
	for i := 0; i < 6; i++ {
		_, _ = get(t, c)
		until := c.WAFBlockedUntil()
		if got := until.Sub(now); got != want {
			t.Fatalf("bloqueo %d: pausa esperada %s, obtenida %s", i+1, want, got)
		}
		now = until.Add(time.Second)
		want *= 2
		if want > wafCooldownMax {
			want = wafCooldownMax
		}
	}
}

// Un 403 propio de SAPI (JSON, p. ej. permisos) no es el cortafuegos: no pausa nada.
func TestWAFBreaker_403DeSAPINoEsElCortafuegos(t *testing.T) {
	var modo atomic.Value
	var hits int32
	modo.Store("sapi403")
	c := wafTestServer(t, &modo, &hits)

	for i := 0; i < 3; i++ {
		if _, err := get(t, c); err != nil {
			t.Fatalf("un 403 de SAPI no debe convertirse en ErrWAFBlocked: %v", err)
		}
	}
	if !c.WAFBlockedUntil().IsZero() {
		t.Error("un 403 JSON de SAPI no debe activar la pausa del WAF")
	}
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Errorf("esperadas 3 peticiones a O2, hubo %d", n)
	}
}
