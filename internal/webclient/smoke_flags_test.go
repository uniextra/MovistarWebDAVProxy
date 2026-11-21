package webclient

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Prueba manual (CHROME_SMOKE=1): Chromium arranca con las opciones de ahorro y
// carga la página de inicio de sesión de Movistar Cloud.
func TestSmokeChromiumFlags(t *testing.T) {
	if os.Getenv("CHROME_SMOKE") == "" {
		t.Skip("CHROME_SMOKE no definido")
	}
	s := &Session{opts: Options{ProfileDir: t.TempDir()}}
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(s.ctx, 40*time.Second)
	defer cancel()
	time.Sleep(6 * time.Second)
	title, err := chromedp.Run(ctx, chromedp.Evaluate[string]("document.title"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := chromedp.Run(ctx, chromedp.Evaluate[string]("document.body.innerText.slice(0,200)"))
	t.Logf("título %q, texto %q", title, body)
	if title == "" {
		t.Fatal("la página no cargó")
	}
}
